package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
)

const testToken = "test-token-long-enough-to-be-real"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newServer starts a server on loopback with a throwaway state dir.
func newServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, err := New(testToken, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.StateDir = t.TempDir()
	if err := s.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	return s, s.Endpoint()
}

func do(t *testing.T, method, url string, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, out
}

// --- the two things that keep this API from being a hole in the machine ---

// Loopback only. A typo binding this to 0.0.0.0 would hand the product to the
// network, so the refusal is enforced rather than reviewed.
func TestRefusesToBindOffLoopback(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:0",
		"192.168.1.35:0",
		"[::]:0",
		"example.com:0",
	} {
		s, err := New(testToken, quietLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		s.StateDir = t.TempDir()
		if err := s.Start(addr); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = s.Stop(ctx)
			cancel()
			t.Fatalf("Start(%q) succeeded; the local API must bind to loopback only", addr)
		}
	}

	// And the forms that are loopback do work.
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		s, err := New(testToken, quietLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		s.StateDir = t.TempDir()
		if err := s.Start(addr); err != nil {
			t.Fatalf("Start(%q) failed: %v", addr, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.Stop(ctx)
		cancel()
	}
}

// The token is what keeps other local processes out — a browser tab on this
// machine can POST to localhost, and must not be able to drive the other one.
func TestRejectsRequestsWithoutAValidToken(t *testing.T) {
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	for _, tok := range []string{"", "wrong", testToken + "x", strings.ToUpper(testToken)} {
		res, body := do(t, "GET", endpoint+"/v1/self", tok, nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q got %d, want 401", tok, res.StatusCode)
		}
		var e struct{ Code Code }
		_ = json.Unmarshal(body, &e)
		if e.Code != CodeUnauthorized {
			t.Fatalf("token %q got code %q, want %q", tok, e.Code, CodeUnauthorized)
		}
	}

	// And the right token works.
	res, _ := do(t, "GET", endpoint+"/v1/self", testToken, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("valid token got %d, want 200", res.StatusCode)
	}
}

// An unauthorised caller must not learn whether a path exists.
func TestUnauthorisedLeaksNothingAboutRouting(t *testing.T) {
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	real, _ := do(t, "GET", endpoint+"/v1/self", "wrong", nil)
	fake, _ := do(t, "GET", endpoint+"/v1/no-such-endpoint", "wrong", nil)
	if real.StatusCode != fake.StatusCode {
		t.Fatalf("a real path returns %d and a fake one %d; routing is observable without a token",
			real.StatusCode, fake.StatusCode)
	}
}

func TestRefusesToStartWithoutAToken(t *testing.T) {
	if _, err := New("", quietLogger()); err == nil {
		t.Fatal("New succeeded with an empty token; the API would be open to every local process")
	}
}

// --- the handoff file ---

func TestHandoffFileIsWrittenAndRemoved(t *testing.T) {
	s, endpoint := newServer(t)
	path := filepath.Join(s.StateDir, HandoffName)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("handoff file not written: %v", err)
	}
	// 0600: it holds the token.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("handoff file mode is %o, want 600 — it carries the API token", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read handoff: %v", err)
	}
	var h Handoff
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("handoff is not valid JSON: %v", err)
	}
	if h.Endpoint != endpoint {
		t.Fatalf("handoff endpoint = %q, want %q", h.Endpoint, endpoint)
	}
	if h.Token != testToken {
		t.Fatal("handoff does not carry the token")
	}
	if h.PID != os.Getpid() {
		t.Fatalf("handoff PID = %d, want %d", h.PID, os.Getpid())
	}

	// On shutdown it must go, so a client does not keep believing a dead
	// daemon is reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("handoff file survived shutdown; clients would dial a dead daemon")
	}
}

// --- error mapping ---

// The same domain condition must report under one code everywhere, or an agent
// branching on the code cannot behave consistently.
func TestDomainErrorsMapToStableCodes(t *testing.T) {
	cases := []struct {
		err  error
		code Code
	}{
		{identity.ErrKeyMismatch, CodeKeyMismatch},
		{identity.ErrNotPaired, CodeNotPaired},
		{identity.ErrAlreadyPaired, CodeAlreadyPaired},
		{link.ErrNotLinked, CodePeerOffline},
		{link.ErrClosed, CodePeerOffline},
		{link.ErrPairingExpired, CodePairingExpired},
		{link.ErrPairingUnknown, CodeNotFound},
		{link.ErrCodeMismatch, CodeCodeMismatch},
	}
	for _, c := range cases {
		got := AsError(c.err)
		if got.Code != c.code {
			t.Errorf("AsError(%v) = %q, want %q", c.err, got.Code, c.code)
		}
		// Wrapped errors must map too: handlers rarely return a bare sentinel.
		wrapped := fmt.Errorf("doing the thing: %w", c.err)
		if got := AsError(wrapped).Code; got != c.code {
			t.Errorf("AsError(wrapped %v) = %q, want %q", c.err, got, c.code)
		}
	}
}

func TestKeyMismatchIsNotUnauthorized(t *testing.T) {
	// A client that retries with better credentials after a key mismatch is
	// fixing the wrong thing: the request was authorised, the peer is wrong.
	if got := CodeKeyMismatch.Status(); got == http.StatusUnauthorized {
		t.Fatal("key_mismatch maps to 401; clients would retry with credentials instead of re-pairing")
	}
}

// timed_out and declined must stay distinguishable: nobody said no, nobody was
// there, and an agent should behave differently in each case.
func TestTimedOutAndDeclinedAreDistinct(t *testing.T) {
	if CodeTimedOut == CodeDeclined {
		t.Fatal("timed_out and declined are the same code")
	}
	if CodeTimedOut.Status() == CodeDeclined.Status() {
		t.Fatalf("timed_out and declined share status %d; they are different situations",
			CodeTimedOut.Status())
	}
	// And exec_timeout is about the command, not the approval.
	if CodeExecTimeout == CodeTimedOut {
		t.Fatal("exec_timeout and timed_out are the same code")
	}
}

func TestErrorCarriesHintAndUnwraps(t *testing.T) {
	base := Errf(CodeTimedOut, "timed out awaiting approval on Desktop")
	withHint := base.WithHint("Desktop's window was closed; the prompt was never seen")

	if withHint.Hint == "" {
		t.Fatal("WithHint did not attach a hint")
	}
	if base.Hint != "" {
		t.Fatal("WithHint mutated the original error")
	}
	if !strings.Contains(withHint.Error(), "never seen") {
		t.Fatalf("Error() drops the hint: %q", withHint.Error())
	}

	wrapped := Errf(CodePeerOffline, "unreachable").Wrapping(link.ErrNotLinked)
	if !errors.Is(wrapped, link.ErrNotLinked) {
		t.Fatal("Wrapping lost the cause for errors.Is")
	}
	// Matching by code alone is what handlers do.
	if !errors.Is(wrapped, &Error{Code: CodePeerOffline}) {
		t.Fatal("errors.Is cannot match an Error by its code")
	}
	if errors.Is(wrapped, &Error{Code: CodeDeclined}) {
		t.Fatal("errors.Is matched the wrong code")
	}
}

func TestErrorResponseShape(t *testing.T) {
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{failRename: identity.ErrNotPaired})

	res, body := do(t, "POST", endpoint+"/v1/self/name", testToken, map[string]any{"name": "Laptop"})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
	var e struct {
		Code    Code   `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, body)
	}
	if e.Code != CodeNotPaired {
		t.Fatalf("code = %q, want %q", e.Code, CodeNotPaired)
	}
	if e.Message == "" {
		t.Fatal("error carries no message for a human to read")
	}
}

// --- handlers ---

func TestSelfAndDevices(t *testing.T) {
	st := &fakeState{
		self: SelfInfo{Name: "Laptop", Fingerprint: "SHA256:aaaa", Version: "0.1.0", Paired: true},
		device: &Device{
			ID: "SHA256:bbbb", Name: "Desktop", Address: "192.168.1.31",
			Status: StatusLive, Mode: ModePrompt, Fingerprint: "SHA256:bbbb",
		},
	}
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(st)

	_, body := do(t, "GET", endpoint+"/v1/self", testToken, nil)
	var self SelfInfo
	if err := json.Unmarshal(body, &self); err != nil {
		t.Fatalf("unmarshal self: %v", err)
	}
	if self.Name != "Laptop" || !self.Paired {
		t.Fatalf("self = %+v", self)
	}

	_, body = do(t, "GET", endpoint+"/v1/devices", testToken, nil)
	var devs struct {
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(body, &devs); err != nil {
		t.Fatalf("unmarshal devices: %v", err)
	}
	if len(devs.Devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs.Devices))
	}
	// The address is a required slot in the design system; a response without
	// it makes a compliant device row impossible to render.
	if devs.Devices[0].Address == "" {
		t.Fatal("device response omits the address")
	}
}

// An unpaired daemon returns an empty array, not null: a client doing
// `devices.map(...)` must not crash on first run.
func TestDevicesReturnsEmptyArrayWhenUnpaired(t *testing.T) {
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	_, body := do(t, "GET", endpoint+"/v1/devices", testToken, nil)
	if !strings.Contains(string(body), `"devices":[]`) {
		t.Fatalf("unpaired devices response = %s, want an empty array", body)
	}
}

func TestDiscoveryCarriesAnEmptyStateHint(t *testing.T) {
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	_, body := do(t, "GET", endpoint+"/v1/discovery", testToken, nil)
	var out struct {
		Candidates []Candidate `json:"candidates"`
		Hint       string      `json:"hint"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Candidates == nil {
		t.Fatal("candidates is null, not an empty array")
	}
	// Nothing found and no traffic seen: the hint must point at blocked
	// multicast and adding by IP, which is the whole point of it.
	if !strings.Contains(out.Hint, "IP address") {
		t.Fatalf("hint does not suggest adding by IP: %q", out.Hint)
	}
}

func TestRenameValidation(t *testing.T) {
	st := &fakeState{}
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(st)

	bad := []string{
		"",
		strings.Repeat("a", maxDeviceNameLen+1),
		"Laptop\x00",
		"Laptop\nDesktop",
		"Laptop\x1b[31m", // an escape sequence would colour a terminal listing
	}
	for _, name := range bad {
		res, _ := do(t, "POST", endpoint+"/v1/self/name", testToken, map[string]any{"name": name})
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("rename to %q got %d, want 400", name, res.StatusCode)
		}
	}

	res, _ := do(t, "POST", endpoint+"/v1/self/name", testToken, map[string]any{"name": "Laptop 16"})
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		t.Fatalf("valid rename got %d", res.StatusCode)
	}
	if st.renamedTo != "Laptop 16" {
		t.Fatalf("state saw rename to %q", st.renamedTo)
	}
}

func TestSetModeValidation(t *testing.T) {
	st := &fakeState{device: &Device{ID: "d", Address: "192.168.1.31"}}
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(st)

	res, _ := do(t, "POST", endpoint+"/v1/devices/mode", testToken,
		map[string]any{"deviceId": "d", "mode": "sometimes"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid mode got %d, want 400", res.StatusCode)
	}

	for _, m := range []Mode{ModePrompt, ModeBypass} {
		res, _ := do(t, "POST", endpoint+"/v1/devices/mode", testToken,
			map[string]any{"deviceId": "d", "mode": m})
		if res.StatusCode >= 400 {
			t.Fatalf("mode %q got %d", m, res.StatusCode)
		}
		if st.mode != m {
			t.Fatalf("state has mode %q, want %q", st.mode, m)
		}
	}
}

func TestMalformedBodyIsBadRequest(t *testing.T) {
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	req, _ := http.NewRequest("POST", endpoint+"/v1/self/name", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body got %d, want 400", res.StatusCode)
	}
}

// A typo in a client's field name must fail loudly rather than silently doing
// nothing — the failure mode where a rename appears to work and does not.
func TestUnknownFieldsAreRejected(t *testing.T) {
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	res, _ := do(t, "POST", endpoint+"/v1/self/name", testToken,
		map[string]any{"nmae": "Laptop"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("misspelled field got %d, want 400", res.StatusCode)
	}
}

// A panic in one handler must not take down the daemon: it holds the link, the
// watchers and the permission state for the whole product.
func TestPanicInHandlerDoesNotKillTheDaemon(t *testing.T) {
	s, endpoint := newServer(t)
	s.Handle(Route{Method: "GET", Path: "/boom", Summary: "panics"},
		func(w http.ResponseWriter, r *http.Request) {
			panic("handler exploded")
		})
	s.RegisterDeviceRoutes(&fakeState{})

	res, _ := do(t, "GET", endpoint+"/v1/boom", testToken, nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("panicking handler returned %d, want 500", res.StatusCode)
	}
	// The server is still serving.
	res, _ = do(t, "GET", endpoint+"/v1/self", testToken, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("server stopped serving after a panic: %d", res.StatusCode)
	}
}

// --- pairing over HTTP ---

func TestPairingEndpoints(t *testing.T) {
	st := &fakeState{
		pairing: &PairingView{
			ID: "pair-1", Code: "418207", CodeFormatted: "418 207",
			PeerName: "Desktop", PeerAddress: "192.168.1.31",
			PeerFingerprint: "SHA256:bbbb", ExpiresAt: time.Now().Add(3 * time.Minute),
		},
	}
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(st)

	_, body := do(t, "POST", endpoint+"/v1/pair/start", testToken,
		map[string]any{"address": "192.168.1.31"})
	var view PairingView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("unmarshal pairing: %v", err)
	}
	// The ceremony screen needs all four: the code, who it is, where, and the
	// fingerprint to compare.
	if view.Code != "418207" || view.CodeFormatted != "418 207" {
		t.Fatalf("code = %q / %q", view.Code, view.CodeFormatted)
	}
	if view.PeerName == "" || view.PeerAddress == "" || view.PeerFingerprint == "" {
		t.Fatalf("pairing view is missing what the screen shows: %+v", view)
	}

	res, _ := do(t, "POST", endpoint+"/v1/pair/start", testToken, map[string]any{"address": ""})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty address got %d, want 400", res.StatusCode)
	}

	_, body = do(t, "POST", endpoint+"/v1/pair/confirm", testToken,
		map[string]any{"sessionId": "pair-1"})
	var confirmed struct {
		Device Device `json:"device"`
	}
	if err := json.Unmarshal(body, &confirmed); err != nil {
		t.Fatalf("unmarshal confirm: %v", err)
	}
	if confirmed.Device.Address == "" {
		t.Fatal("confirmed device omits its address")
	}

	st.failConfirm = link.ErrPairingExpired
	res, _ = do(t, "POST", endpoint+"/v1/pair/confirm", testToken,
		map[string]any{"sessionId": "pair-1"})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expired pairing got %d, want 409", res.StatusCode)
	}
}

// --- events ---

func TestEventStreamDeliversAndCleansUp(t *testing.T) {
	s, endpoint := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Wait for the subscription to register before publishing.
	deadline := time.Now().Add(5 * time.Second)
	for s.Events().Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Events().Subscribers() != 1 {
		t.Fatalf("subscribers = %d, want 1", s.Events().Subscribers())
	}

	s.Events().Publish(EventDeviceOnline, map[string]string{"name": "Desktop"})

	scanner := bufio.NewScanner(res.Body)
	var gotEvent, gotData string
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			gotEvent = after
		}
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			gotData = after
			break
		}
	}
	if gotEvent != EventDeviceOnline {
		t.Fatalf("event type = %q, want %q", gotEvent, EventDeviceOnline)
	}
	if !strings.Contains(gotData, "Desktop") {
		t.Fatalf("event data = %q, want it to carry the payload", gotData)
	}

	// Disconnecting must deregister, or subscribers leak for the life of the
	// daemon.
	cancel()
	res.Body.Close()
	deadline = time.Now().Add(5 * time.Second)
	for s.Events().Subscribers() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := s.Events().Subscribers(); n != 0 {
		t.Fatalf("subscribers = %d after disconnect, want 0", n)
	}
}

// A client that stopped reading must never stall the daemon: it holds the link
// and the watchers for the whole product.
func TestSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	hub := newEventHub()
	_, unsubscribe := hub.subscribe() // never drained
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			hub.Publish(EventDeviceChanged, i)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}
}

func TestEventHubIsConcurrencySafe(t *testing.T) {
	hub := newEventHub()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := hub.subscribe()
			go func() {
				for range ch {
				}
			}()
			for j := 0; j < 50; j++ {
				hub.Publish(EventDeviceChanged, j)
			}
			unsub()
		}()
	}
	wg.Wait()
	hub.closeAll()
	hub.closeAll() // must be idempotent
}

// --- routes are the surface description ---

func TestRoutesAreRecordedForHelpGeneration(t *testing.T) {
	s, _ := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	routes := s.Routes()
	if len(routes) == 0 {
		t.Fatal("no routes recorded; `ibeji help --json` would describe nothing")
	}
	seen := map[string]bool{}
	for _, r := range routes {
		if r.Summary == "" {
			t.Errorf("route %s %s has no summary; an agent cannot learn what it does", r.Method, r.Path)
		}
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("route %s registered twice", key)
		}
		seen[key] = true
	}
	for _, want := range []string{"GET /self", "GET /devices", "POST /pair/start", "GET /events"} {
		if !seen[want] {
			t.Errorf("route %q is not registered", want)
		}
	}
}

func TestMutatingRoutesAreMarked(t *testing.T) {
	s, _ := newServer(t)
	s.RegisterDeviceRoutes(&fakeState{})

	byPath := map[string]Route{}
	for _, r := range s.Routes() {
		byPath[r.Method+" "+r.Path] = r
	}
	// Reads must not be marked mutating, or they would later be made to prompt.
	for _, p := range []string{"GET /self", "GET /devices", "GET /discovery", "GET /events"} {
		if byPath[p].Mutating {
			t.Errorf("%s is marked mutating; reads must never prompt", p)
		}
	}
	// Side effects must be marked, or they would bypass the permission layer.
	for _, p := range []string{"POST /unpair", "POST /devices/mode", "POST /self/name", "POST /pair/confirm"} {
		if !byPath[p].Mutating {
			t.Errorf("%s is not marked mutating; it would skip the permission layer", p)
		}
	}
}

// --- fake state -------------------------------------------------------------

type fakeState struct {
	self     SelfInfo
	device   *Device
	cands    []Candidate
	pairing  *PairingView
	attempt  any
	sweep    any
	sweepErr error

	renamedTo string
	mode      Mode

	failRename  error
	failConfirm error
}

func (f *fakeState) Self() SelfInfo { return f.self }

func (f *fakeState) Paired() (*Device, bool) {
	if f.device == nil {
		return nil, false
	}
	return f.device, true
}

func (f *fakeState) Candidates() []Candidate { return f.cands }

func (f *fakeState) Rename(name string) error {
	if f.failRename != nil {
		return f.failRename
	}
	f.renamedTo = name
	return nil
}

func (f *fakeState) SetMode(deviceID string, mode Mode) error {
	f.mode = mode
	return nil
}

func (f *fakeState) Unpair(deviceID string) error {
	f.device = nil
	return nil
}

func (f *fakeState) ReconnectAt(address string) (*Device, error) {
	if f.device == nil {
		return nil, errors.New("not paired")
	}
	f.device.Address = address
	f.device.Status = StatusLive
	return f.device, nil
}

func (f *fakeState) PendingPairing() *PairingView { return f.pairing }

func (f *fakeState) LastPairAttempt() any { return f.attempt }

func (f *fakeState) SweepNetwork(ctx context.Context) (any, error) {
	if f.sweepErr != nil {
		return nil, f.sweepErr
	}
	return f.sweep, nil
}

func (f *fakeState) BeginPairing(address string) (*PairingView, error) {
	if f.pairing == nil {
		return nil, link.ErrNotLinked
	}
	return f.pairing, nil
}

func (f *fakeState) ConfirmPairing(sessionID string) (*Device, error) {
	if f.failConfirm != nil {
		return nil, f.failConfirm
	}
	return &Device{
		ID: "SHA256:bbbb", Name: "Desktop", Address: "192.168.1.31",
		Status: StatusLive, Mode: ModePrompt, Fingerprint: "SHA256:bbbb",
	}, nil
}

func (f *fakeState) CancelPairing(sessionID string) error { return nil }
