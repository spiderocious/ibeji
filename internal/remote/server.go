package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/feranmi/ibeji/internal/link"
	"github.com/feranmi/ibeji/internal/permission"
)

// Server answers the peer's requests on this machine.
//
// Every side-effecting request passes through the permission engine before it
// touches anything. There is one place that happens — guard(), below — rather
// than a check in each handler, because a check that has to be remembered in
// every new handler is a check that will eventually be forgotten in one.

// Server handles inbound remote operations.
type Server struct {
	Runner *Runner
	Perms  *permission.Engine
	Logger *slog.Logger

	// PeerName is the device asking, which the prompt must name.
	PeerName func() string

	// Trace records what happened, if set.
	Trace func(ev TraceEvent)
}

// TraceEvent is what the server reports about a request.
type TraceEvent struct {
	Type       string
	Op         Op
	Provenance string
	Device     string
	Request    any
	Response   any
	Code       string
	Error      string
	Hint       string
	DurationMS int64
}

// NewServer builds a server.
func NewServer(runner *Runner, perms *permission.Engine, logger *slog.Logger) *Server {
	if runner == nil {
		runner = NewRunner()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Runner: runner, Perms: perms, Logger: logger}
}

func (s *Server) peer() string {
	if s.PeerName != nil {
		if n := s.PeerName(); n != "" {
			return n
		}
	}
	return "the paired device"
}

func (s *Server) trace(ev TraceEvent) {
	if s.Trace != nil {
		ev.Device = s.peer()
		s.Trace(ev)
	}
}

// Serve handles one inbound remote stream.
func (s *Server) Serve(ctx context.Context, stream link.Stream) {
	defer stream.Close()
	start := time.Now()

	// Bound the read of the request frame: a peer that never finishes sending
	// one must not hold a goroutine open.
	if err := stream.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}

	var req Request
	dec := json.NewDecoder(io.LimitReader(stream, 1<<20))
	if err := dec.Decode(&req); err != nil {
		s.fail(stream, "bad_request", fmt.Sprintf("malformed request: %v", err), "")
		return
	}
	// Clear the deadline: a command may legitimately run for minutes.
	_ = stream.SetReadDeadline(time.Time{})

	if !req.Op.Known() {
		// Refused rather than guessed at, so a newer peer gets a clear answer.
		s.fail(stream, "bad_request", fmt.Sprintf("unknown operation %q", req.Op),
			"This device is running an older version of Ìbejì.")
		return
	}

	// THE CHOKEPOINT. Every side effect passes here; reads never do.
	if req.Op.Mutating() {
		decision, err := s.guard(ctx, req)
		s.trace(TraceEvent{
			Type: "perm.check", Op: req.Op, Provenance: req.Provenance,
			Response:   map[string]any{"verdict": string(decision.Verdict), "rule": decision.Rule},
			DurationMS: decision.WaitedMS,
		})
		if err != nil {
			code, hint := permissionError(err, s.peer())
			s.fail(stream, code, err.Error(), hint)
			s.trace(TraceEvent{
				Type: "perm.refused", Op: req.Op, Provenance: req.Provenance,
				Code: code, Error: err.Error(), Hint: hint,
			})
			return
		}
	}

	err := s.dispatch(ctx, stream, req)
	ev := TraceEvent{
		Type:       string(req.Op),
		Op:         req.Op,
		Provenance: req.Provenance,
		Request:    req,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		ev.Code = "internal"
		ev.Error = err.Error()
	}
	s.trace(ev)
}

// guard asks the permission engine. One place, so a new handler cannot
// accidentally skip it.
func (s *Server) guard(ctx context.Context, req Request) (permission.Decision, error) {
	a := permission.Action{
		Device:     s.peer(),
		Provenance: req.Provenance,
	}
	switch req.Op {
	case OpExec:
		if req.Exec == nil {
			return permission.Decision{}, fmt.Errorf("%w: exec request has no command", ErrMalformed)
		}
		a.Kind = permission.KindExec
		a.Effect = req.Exec.Command
		a.Cwd = req.Exec.Cwd
	case OpWrite:
		a.Kind = permission.KindWrite
		// The prompt states exactly what will happen: which file, how much.
		if req.Write != nil && req.Write.Mkdir {
			a.Effect = "create directory " + req.Path
		} else if req.Write != nil {
			a.Effect = fmt.Sprintf("write %s (%d bytes)", req.Path, req.Write.Bytes)
		} else {
			a.Effect = "write " + req.Path
		}
	default:
		return permission.Decision{}, fmt.Errorf("remote: %q is not a guarded operation", req.Op)
	}

	if s.Perms == nil {
		// No engine means nothing is guarding this machine. Refuse rather than
		// run: a missing permission engine must never read as permission.
		return permission.Decision{}, fmt.Errorf("remote: no permission engine configured")
	}
	return s.Perms.Decide(ctx, a)
}

// permissionError maps a refusal to a stable code and a useful hint.
func permissionError(err error, device string) (code, hint string) {
	switch {
	case errors.Is(err, permission.ErrDeclined):
		return "declined", ""
	case errors.Is(err, permission.ErrTimedOut):
		// The hint is what saves an agent a round of guessing.
		return "timed_out", fmt.Sprintf(
			"Nobody answered on %s within the timeout. Its window may be closed, "+
				"or the machine may be locked.", device)
	default:
		return "internal", ""
	}
}

// dispatch runs the operation.
func (s *Server) dispatch(ctx context.Context, stream link.Stream, req Request) error {
	switch req.Op {
	case OpList:
		entries, err := List(req.Path)
		if err != nil {
			return s.fail(stream, "not_found", err.Error(), "")
		}
		return writeResponse(stream, Response{Entries: entries})

	case OpStat:
		info, err := Stat(req.Path)
		if err != nil {
			return s.fail(stream, "not_found", err.Error(), "")
		}
		return writeResponse(stream, Response{Stat: info})

	case OpRead:
		rc, size, err := Open(req.Path)
		if err != nil {
			return s.fail(stream, "not_found", err.Error(), "")
		}
		defer rc.Close()
		if err := writeResponse(stream, Response{Bytes: size}); err != nil {
			return err
		}
		_, err = io.Copy(stream, rc)
		return err

	case OpSearch:
		if req.Search == nil {
			return s.fail(stream, "bad_request", "search request has no query", "")
		}
		matches, truncated, err := Search(ctx, *req.Search)
		if err != nil {
			return s.fail(stream, "bad_request", err.Error(), "")
		}
		return writeResponse(stream, Response{Matches: matches, Truncated: truncated})

	case OpWrite:
		if req.Write == nil {
			return s.fail(stream, "bad_request", "write request has no body", "")
		}
		if err := Write(req.Path, *req.Write, stream); err != nil {
			return s.fail(stream, "internal", err.Error(), "")
		}
		return writeResponse(stream, Response{})

	case OpInfo:
		return writeResponse(stream, Response{Info: Info(ctx)})

	case OpCheck:
		if req.Check == nil {
			return s.fail(stream, "bad_request", "check request has no target", "")
		}
		return writeResponse(stream, Response{Check: Probe(ctx, *req.Check)})

	case OpExec:
		return s.serveExec(ctx, stream, req)

	default:
		return s.fail(stream, "bad_request", fmt.Sprintf("unhandled operation %q", req.Op), "")
	}
}

// serveExec runs a command, streaming its output back as it happens.
func (s *Server) serveExec(ctx context.Context, stream link.Stream, req Request) error {
	if req.Exec == nil {
		return s.fail(stream, "bad_request", "exec request has no command", "")
	}

	if req.Exec.Detach {
		p, err := s.Runner.Start(*req.Exec)
		if err != nil {
			return s.fail(stream, "internal", err.Error(), "")
		}
		return writeResponse(stream, Response{Exec: &ExecStarted{
			ProcessID: p.ID, PID: p.PID, Shell: s.Runner.shell(), Cwd: p.Cwd,
		}})
	}

	if err := writeResponse(stream, Response{Exec: &ExecStarted{
		Shell: s.Runner.shell(), Cwd: req.Exec.Cwd,
	}}); err != nil {
		return err
	}

	// Output frames: a one-byte kind, a four-byte length, then the bytes.
	// Raw rather than JSON, because this is the hot path of a build's output
	// and base64 would cost a third more over the link for no gain.
	sink := SinkFunc(func(kind OutputKind, b []byte) error {
		return writeOutputFrame(stream, kind, b)
	})

	res, err := s.Runner.Run(ctx, *req.Exec, sink)
	if err != nil {
		return s.fail(stream, "internal", err.Error(), "")
	}

	exit, err := json.Marshal(ExitStatus{
		Code: res.ExitCode, DurationMS: res.DurationMS, TimedOut: res.TimedOut,
	})
	if err != nil {
		return err
	}
	return writeOutputFrame(stream, OutExit, exit)
}

// fail sends an error response.
func (s *Server) fail(stream link.Stream, code, message, hint string) error {
	return writeResponse(stream, Response{Code: code, Message: message, Hint: hint})
}

// writeResponse sends a JSON response frame.
func writeResponse(w io.Writer, resp Response) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("remote: encode response: %w", err)
	}
	return writeFrame(w, body)
}

// writeFrame writes a length-prefixed frame.
func writeFrame(w io.Writer, body []byte) error {
	var hdr [4]byte
	hdr[0] = byte(len(body) >> 24)
	hdr[1] = byte(len(body) >> 16)
	hdr[2] = byte(len(body) >> 8)
	hdr[3] = byte(len(body))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// writeOutputFrame writes one chunk of command output.
func writeOutputFrame(w io.Writer, kind OutputKind, b []byte) error {
	var hdr [5]byte
	hdr[0] = byte(kind)
	hdr[1] = byte(len(b) >> 24)
	hdr[2] = byte(len(b) >> 16)
	hdr[3] = byte(len(b) >> 8)
	hdr[4] = byte(len(b))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(b) > 0 {
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// Probe asks whether this machine can reach a target.
//
// The "whose network is broken" answer: run on the machine that owns the VPN,
// it says in one call what would otherwise take a walk across the room.
func Probe(ctx context.Context, req CheckRequest) *CheckResult {
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := &CheckResult{Target: req.Target}
	target := req.Target
	start := time.Now()

	// A bare host:port is probed with TCP; anything else as HTTP.
	if !strings.Contains(target, "://") {
		host := target
		if _, _, err := net.SplitHostPort(target); err != nil {
			host = net.JoinHostPort(target, "443")
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", host)
		res.LatencyMS = time.Since(start).Milliseconds()
		if err != nil {
			res.Error = err.Error()
			return res
		}
		res.Reachable = true
		if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
			res.ResolvedIP = addr.IP.String()
		}
		conn.Close()
		return res
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// Do not follow redirects: "it answered 302" is a more useful answer than
	// wherever the redirect led.
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(httpReq)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Reachable = true
	res.StatusCode = resp.StatusCode
	return res
}
