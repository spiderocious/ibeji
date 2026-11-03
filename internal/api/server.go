package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Version is the API's major version, carried in the path.
const Version = "v1"

// HandoffName is the file the daemon writes so local clients can find it.
//
// The window is launched from the Dock and the CLI from a shell; neither
// inherits the daemon's environment, so an environment variable cannot carry
// the endpoint and token. A 0600 file in the user's home directory can.
const HandoffName = "api.json"

// Handoff is what that file contains.
type Handoff struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
}

// Server is the daemon's local HTTP API.
type Server struct {
	// Token authenticates every request. From the keychain.
	Token string

	// StateDir is where the handoff file is written. Defaults to ~/.ibeji.
	StateDir string

	// Logger receives request and lifecycle logs.
	Logger *slog.Logger

	mux      *http.ServeMux
	srv      *http.Server
	listener net.Listener

	// Tracer, when set, records every request. Wired in by the daemon; nil in
	// tests that are not about tracing.
	Tracer Tracer

	// Barrier, when set, blocks until a space has converged on both devices.
	// Wired in by the daemon; nil where spaces do not exist.
	Barrier func(r *http.Request, space string, timeout time.Duration) error

	mu      sync.RWMutex
	routes  []Route
	handoff string // path actually written, for cleanup
	started bool
	events  *eventHub
}

// Tracer records what the API did. Satisfied by internal/trace.
//
// Declared as an interface here rather than importing the trace package so the
// dependency runs one way: trace knows nothing about HTTP, and the API knows
// nothing about SQLite.
type Tracer interface {
	Record(ev TraceEvent)
}

// TraceEvent is one recorded API call.
type TraceEvent struct {
	At         time.Time
	Type       string
	Method     string
	Path       string
	Provenance string
	Status     int
	Code       Code
	DurationMS int64
	Request    any
	Response   any
	Err        string
}

// Route is one endpoint, as registered. The surface description that
// `ibeji help --json` emits is generated from these, so the CLI, the MCP tool
// descriptions and the human help text cannot drift from what is served.
type Route struct {
	Method  string
	Path    string
	Summary string

	// Mutating marks endpoints with side effects, which the trace records in
	// full and which permission checks apply to.
	Mutating bool
}

// New builds a server. The token must be non-empty: an API with no token is
// reachable by any process on the machine.
func New(token string, logger *slog.Logger) (*Server, error) {
	if token == "" {
		return nil, errors.New("api: refusing to start without a token")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		Token:  token,
		Logger: logger,
		mux:    http.NewServeMux(),
		events: newEventHub(),
	}
	return s, nil
}

// Handle registers an endpoint.
func (s *Server) Handle(r Route, h http.HandlerFunc) {
	s.mu.Lock()
	s.routes = append(s.routes, r)
	s.mu.Unlock()
	s.mux.HandleFunc(r.Method+" /"+Version+r.Path, h)
}

// Routes returns the registered surface, for help generation.
func (s *Server) Routes() []Route {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Route, len(s.routes))
	copy(out, s.routes)
	return out
}

// Events returns the hub that broadcasts to SSE subscribers.
func (s *Server) Events() *eventHub { return s.events }

// Start binds to loopback and begins serving.
//
// addr is usually "127.0.0.1:0" so the OS picks a free port — a fixed port
// would collide with whatever else the user is running, and the handoff file
// makes a fixed port unnecessary.
func (s *Server) Start(addr string) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("api: already started")
	}
	s.started = true
	s.mu.Unlock()

	if err := assertLoopback(addr); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("api: listen on %s: %w", addr, err)
	}
	s.listener = ln

	s.srv = &http.Server{
		Handler:           s.middleware(s.mux),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: exec streams output for as long as the command
		// runs, and an SSE subscription lives for the life of the window.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Logger.Error("api: serve stopped", "err", err)
		}
	}()

	if err := s.writeHandoff(); err != nil {
		_ = s.Stop(context.Background())
		return err
	}
	s.Logger.Info("api: listening", "endpoint", s.Endpoint())
	return nil
}

// assertLoopback refuses to bind anywhere the network can reach.
//
// The PRD makes LAN-only a hard boundary, and the local API is the one part
// that must not be on the LAN at all: it is the unauthenticated-by-design
// control surface for this machine. Binding it to 0.0.0.0 by a typo would hand
// the whole product to the network, so the check is here rather than in review.
func assertLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("api: malformed address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("api: %q is not an IP address; the local API binds to loopback only", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("api: refusing to bind to %s — the local API binds to loopback only", host)
	}
	return nil
}

// Endpoint is the base URL clients use.
func (s *Server) Endpoint() string {
	if s.listener == nil {
		return ""
	}
	return "http://" + s.listener.Addr().String()
}

// Addr returns the bound address.
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// stateDir resolves where the handoff file goes.
func (s *Server) stateDir() (string, error) {
	if s.StateDir != "" {
		return s.StateDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("api: locate home directory: %w", err)
	}
	return filepath.Join(home, ".ibeji"), nil
}

// writeHandoff publishes the endpoint and token for local clients.
func (s *Server) writeHandoff() error {
	dir, err := s.stateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("api: create state dir: %w", err)
	}
	path := filepath.Join(dir, HandoffName)

	body, err := json.MarshalIndent(Handoff{
		Endpoint: s.Endpoint(),
		Token:    s.Token,
		PID:      os.Getpid(),
		Version:  Version,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("api: encode handoff: %w", err)
	}

	// Write-then-rename, so a client never reads a half-written file and
	// concludes the daemon is broken.
	tmp, err := os.CreateTemp(dir, ".api-*.json")
	if err != nil {
		return fmt.Errorf("api: create temp handoff: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("api: chmod handoff: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("api: write handoff: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("api: close handoff: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("api: install handoff: %w", err)
	}

	s.mu.Lock()
	s.handoff = path
	s.mu.Unlock()
	return nil
}

// Stop shuts down and removes the handoff file, so a client does not go on
// believing a dead daemon is reachable.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.RLock()
	path := s.handoff
	s.mu.RUnlock()
	if path != "" {
		_ = os.Remove(path)
	}
	s.events.closeAll()
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// middleware authenticates, records, and recovers.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if v := recover(); v != nil {
				// A panic must not take the daemon down: it holds the link,
				// the watchers and the permission state for the whole product.
				s.Logger.Error("api: panic in handler", "path", r.URL.Path, "panic", v)
				if !rec.wrote {
					WriteError(rec, Errf(CodeInternal, "internal error"))
				}
			}
			if s.Tracer != nil {
				s.Tracer.Record(TraceEvent{
					At:         start,
					Type:       "api.request",
					Method:     r.Method,
					Path:       r.URL.Path,
					Provenance: provenanceOf(r),
					Status:     rec.status,
					Code:       rec.code,
					DurationMS: time.Since(start).Milliseconds(),
				})
			}
		}()

		if !s.authorised(r) {
			// No detail: a wrong token learns nothing about whether the path
			// exists.
			WriteError(rec, Errf(CodeUnauthorized, "missing or invalid API token"))
			return
		}
		next.ServeHTTP(rec, r)
	})
}

// authorised checks the bearer token in constant time.
func (s *Server) authorised(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) == 1
}

// provenanceOf reports who is calling: the app, the CLI, an agent via MCP, or
// something else using the API directly.
//
// This is what makes a command Claude Code ran distinguishable from one the
// user ran, in both the log and the trace. It is self-declared and therefore
// not a security boundary — a script could claim any value — which is stated
// plainly in the PRD. It is for legibility, not enforcement.
func provenanceOf(r *http.Request) string {
	switch p := r.Header.Get("X-Ibeji-Provenance"); p {
	case "app", "cli", "mcp", "api":
		return p
	default:
		return "api"
	}
}

// statusRecorder captures what was written, for the trace.
type statusRecorder struct {
	http.ResponseWriter
	status int
	code   Code
	wrote  bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer, which SSE needs.
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// WriteJSON sends a successful response.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if v == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := json.Marshal(v)
	if err != nil {
		WriteError(w, Errf(CodeInternal, "encode response: %v", err))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// WriteError sends a failure, carrying the stable code.
func WriteError(w http.ResponseWriter, err error) {
	e := AsError(err)
	if rec, ok := w.(*statusRecorder); ok {
		rec.code = e.Code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code.Status())
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    e.Code,
		"message": e.Message,
		"hint":    e.Hint,
	})
}

// DecodeBody reads a JSON request body into v.
func DecodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Errf(CodeBadRequest, "malformed request body: %v", err)
	}
	return nil
}
