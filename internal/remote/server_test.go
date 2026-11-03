package remote

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/link"
	"github.com/feranmi/ibeji/internal/permission"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serve runs one request against a server over the in-memory fabric and
// returns the response frame.
func serve(t *testing.T, s *Server, req Request) (Response, link.Stream) {
	t.Helper()

	fabric := link.NewMemoryFabric()
	ln, err := fabric.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		stream, err := conn.Accept(context.Background())
		if err != nil {
			return
		}
		s.Serve(context.Background(), stream)
	}()

	conn, err := fabric.Dialer("laptop").Dial(context.Background(), "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	stream, err := conn.Open(context.Background(), link.KindRPC)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := stream.Write(body); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp := readResponseT(t, stream)
	return resp, stream
}

func readResponseT(t *testing.T, stream link.Stream) Response {
	t.Helper()
	if err := stream.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(stream, hdr[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	body := make([]byte, n)
	if _, err := io.ReadFull(stream, body); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, body)
	}
	return resp
}

func bypassing(t *testing.T) *Server {
	t.Helper()
	e := permission.New(nil)
	if err := e.SetMode(permission.ModeBypass); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	s := NewServer(nil, e, quiet())
	s.PeerName = func() string { return "Laptop" }
	return s
}

// THE test the whole permission model rests on.
//
// It reads the dispatch function's source and asserts that every mutating
// operation is handled only after the guard has run. A check that has to be
// remembered in every new handler is a check that will eventually be forgotten
// in one, so this asserts the property by construction rather than by review.
func TestNoSideEffectCanSkipTheChokepoint(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}

	var serveFn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Serve" {
			serveFn = fn
			return false
		}
		return true
	})
	if serveFn == nil {
		t.Fatal("Serve not found in server.go")
	}

	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	body := string(src[fset.Position(serveFn.Pos()).Offset:fset.Position(serveFn.End()).Offset])

	guardAt := strings.Index(body, "s.guard(")
	dispatchAt := strings.Index(body, "s.dispatch(")
	if guardAt == -1 {
		t.Fatal("Serve does not call guard(); nothing is checking permissions")
	}
	if dispatchAt == -1 {
		t.Fatal("Serve does not call dispatch()")
	}
	if guardAt > dispatchAt {
		t.Fatal("Serve dispatches before guarding; a side effect could run unapproved")
	}
	if !strings.Contains(body, "req.Op.Mutating()") {
		t.Fatal("Serve does not branch on Op.Mutating(); the guard may not cover every side effect")
	}

	// And every op the protocol declares mutating must be handled by guard().
	guardSrc := funcSource(t, "guard")
	for _, op := range []Op{OpExec, OpWrite} {
		if !strings.Contains(guardSrc, string(opConstName(op))) {
			t.Errorf("guard() does not handle %s; that side effect would be refused or unguarded", op)
		}
	}
}

func funcSource(t *testing.T, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			out = string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
			return false
		}
		return true
	})
	if out == "" {
		t.Fatalf("%s not found", name)
	}
	return out
}

func opConstName(op Op) string {
	switch op {
	case OpExec:
		return "OpExec"
	case OpWrite:
		return "OpWrite"
	default:
		return string(op)
	}
}

// A missing permission engine must never read as permission.
func TestNoEngineMeansNothingRuns(t *testing.T) {
	s := NewServer(nil, nil, quiet())
	s.PeerName = func() string { return "Laptop" }

	resp, _ := serve(t, s, Request{
		Op:   OpExec,
		Exec: &ExecRequest{Command: "echo should-not-run"},
	})
	if resp.Code == "" {
		t.Fatal("a server with no permission engine ran a command")
	}
}

// --- reads never prompt ---

// The PRD is explicit, and it is what makes the API usable for an agent: a
// read that started prompting would make Prompt mode unusable.
func TestReadsNeverPrompt(t *testing.T) {
	// An engine in Prompt mode with a timeout so short that any prompt would
	// expire and fail the request.
	e := permission.New(nil)
	e.Timeout = 50 * time.Millisecond
	s := NewServer(nil, e, quiet())
	s.PeerName = func() string { return "Laptop" }

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	reads := map[string]Request{
		"list":   {Op: OpList, Path: dir},
		"stat":   {Op: OpStat, Path: filepath.Join(dir, "a.txt")},
		"search": {Op: OpSearch, Search: &SearchRequest{Root: dir, Query: "hello"}},
		"info":   {Op: OpInfo},
	}
	for name, req := range reads {
		resp, _ := serve(t, s, req)
		if resp.Code != "" {
			t.Errorf("%s was refused in Prompt mode (%s); reads must never prompt", name, resp.Code)
		}
	}
	if n := len(e.Pending()); n != 0 {
		t.Fatalf("%d prompts were raised by reads", n)
	}
}

// --- side effects do prompt ---

func TestExecIsGuarded(t *testing.T) {
	e := permission.New(nil)
	e.Timeout = 80 * time.Millisecond // nobody answers
	s := NewServer(nil, e, quiet())
	s.PeerName = func() string { return "Laptop" }

	marker := filepath.Join(t.TempDir(), "should-not-exist")
	resp, _ := serve(t, s, Request{
		Op:   OpExec,
		Exec: &ExecRequest{Command: "touch " + marker},
	})

	if resp.Code != "timed_out" {
		t.Fatalf("code = %q, want timed_out", resp.Code)
	}
	// The hint is what saves an agent a round of guessing.
	if !strings.Contains(resp.Hint, "Laptop") {
		t.Errorf("the hint does not name the machine: %q", resp.Hint)
	}
	// And the command genuinely did not run.
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an unapproved command ran anyway")
	}
}

func TestWriteIsGuarded(t *testing.T) {
	e := permission.New(nil)
	e.Timeout = 80 * time.Millisecond
	s := NewServer(nil, e, quiet())
	s.PeerName = func() string { return "Laptop" }

	target := filepath.Join(t.TempDir(), "out.txt")
	resp, _ := serve(t, s, Request{
		Op:    OpWrite,
		Path:  target,
		Write: &WriteRequest{Bytes: 5},
	})
	if resp.Code != "timed_out" {
		t.Fatalf("code = %q, want timed_out", resp.Code)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("an unapproved write happened anyway")
	}
}

// A decline must come back distinguishable from a crash or a timeout.
func TestDeclinedExecReportsDeclined(t *testing.T) {
	e := permission.New(nil)
	e.Timeout = 5 * time.Second
	s := NewServer(nil, e, quiet())
	s.PeerName = func() string { return "Laptop" }

	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if p := e.Pending(); len(p) > 0 {
				_ = e.Answer(p[0].ID, false, false)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	resp, _ := serve(t, s, Request{Op: OpExec, Exec: &ExecRequest{Command: "echo hi"}})
	if resp.Code != "declined" {
		t.Fatalf("code = %q, want declined", resp.Code)
	}
}

// --- the happy path ---

func TestExecStreamsOutputAndExitCode(t *testing.T) {
	s := bypassing(t)
	resp, stream := serve(t, s, Request{
		Op:   OpExec,
		Exec: &ExecRequest{Command: "echo one; echo two 1>&2; exit 3"},
	})
	if resp.Code != "" {
		t.Fatalf("refused: %s %s", resp.Code, resp.Message)
	}
	if resp.Exec == nil || resp.Exec.Shell == "" {
		t.Fatalf("the acknowledgement does not report the shell: %+v", resp.Exec)
	}

	var stdout, stderr strings.Builder
	var exit ExitStatus
	for {
		kind, body, err := readOutputFrameT(stream)
		if err != nil {
			t.Fatalf("read output: %v", err)
		}
		switch kind {
		case OutStdout:
			stdout.Write(body)
		case OutStderr:
			stderr.Write(body)
		case OutExit:
			if err := json.Unmarshal(body, &exit); err != nil {
				t.Fatalf("exit frame: %v", err)
			}
			if !strings.Contains(stdout.String(), "one") {
				t.Errorf("stdout = %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "two") {
				t.Errorf("stderr = %q", stderr.String())
			}
			// The remote exit code is what makes the CLI composable.
			if exit.Code != 3 {
				t.Fatalf("exit code = %d, want 3", exit.Code)
			}
			return
		}
	}
}

func readOutputFrameT(stream link.Stream) (OutputKind, []byte, error) {
	if err := stream.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return 0, nil, err
	}
	var hdr [5]byte
	if _, err := io.ReadFull(stream, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(hdr[1])<<24 | int(hdr[2])<<16 | int(hdr[3])<<8 | int(hdr[4])
	body := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(stream, body); err != nil {
			return 0, nil, err
		}
	}
	return OutputKind(hdr[0]), body, nil
}

func TestListAndSearchOverTheLink(t *testing.T) {
	s := bypassing(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("// TODO: fix\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, _ := serve(t, s, Request{Op: OpList, Path: dir})
	if resp.Code != "" || len(resp.Entries) != 1 {
		t.Fatalf("list = %+v", resp)
	}

	resp, _ = serve(t, s, Request{Op: OpSearch, Search: &SearchRequest{Root: dir, Query: "TODO"}})
	if resp.Code != "" || len(resp.Matches) != 1 {
		t.Fatalf("search = %+v", resp)
	}
	if resp.Matches[0].Line != 1 {
		t.Fatalf("match line = %d", resp.Matches[0].Line)
	}
}

func TestReadStreamsContents(t *testing.T) {
	s := bypassing(t)
	dir := t.TempDir()
	content := "the file contents\n"
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, stream := serve(t, s, Request{Op: OpRead, Path: path})
	if resp.Code != "" {
		t.Fatalf("read refused: %s", resp.Message)
	}
	if resp.Bytes != int64(len(content)) {
		t.Fatalf("declared %d bytes, file is %d", resp.Bytes, len(content))
	}
	body := make([]byte, resp.Bytes)
	if err := stream.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := io.ReadFull(stream, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != content {
		t.Fatalf("contents = %q", body)
	}
}

func TestDeviceInfoOverTheLink(t *testing.T) {
	s := bypassing(t)
	resp, _ := serve(t, s, Request{Op: OpInfo})
	if resp.Code != "" || resp.Info == nil {
		t.Fatalf("info = %+v", resp)
	}
	if resp.Info.OS == "" || resp.Info.Shell == "" {
		t.Fatalf("info is incomplete: %+v", resp.Info)
	}
}

// An unknown op from a newer peer gets a clear refusal, not a guess.
func TestUnknownOpIsRefusedWithAHint(t *testing.T) {
	s := bypassing(t)
	resp, _ := serve(t, s, Request{Op: Op("fs.destroy")})
	if resp.Code != "bad_request" {
		t.Fatalf("code = %q, want bad_request", resp.Code)
	}
	if !strings.Contains(strings.ToLower(resp.Hint), "older version") {
		t.Errorf("the hint does not explain a version mismatch: %q", resp.Hint)
	}
}

func TestMalformedRequestIsRefused(t *testing.T) {
	s := bypassing(t)
	fabric := link.NewMemoryFabric()
	ln, _ := fabric.Listen("desktop")
	defer ln.Close()
	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		stream, err := conn.Accept(context.Background())
		if err != nil {
			return
		}
		s.Serve(context.Background(), stream)
	}()

	conn, err := fabric.Dialer("laptop").Dial(context.Background(), "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	stream, _ := conn.Open(context.Background(), link.KindRPC)
	if _, err := stream.Write([]byte("{not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp := readResponseT(t, stream)
	if resp.Code != "bad_request" {
		t.Fatalf("code = %q, want bad_request", resp.Code)
	}
}

// --- connectivity probe ---

// The "whose network is broken" answer, which is a daily question when one
// machine owns the VPN.
func TestProbeReportsReachability(t *testing.T) {
	// A listener we control, so this does not depend on the internet.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()

	res := Probe(context.Background(), CheckRequest{Target: ln.Addr().String()})
	if !res.Reachable {
		t.Fatalf("a listening port reported unreachable: %+v", res)
	}
	if res.ResolvedIP == "" {
		t.Error("the probe does not report which address it reached")
	}

	// And something that is not listening.
	addr := ln.Addr().String()
	ln.Close()
	res = Probe(context.Background(), CheckRequest{Target: addr, TimeoutSeconds: 2})
	if res.Reachable {
		t.Fatal("a closed port reported reachable")
	}
	if res.Error == "" {
		t.Error("an unreachable target reports no reason")
	}
}

func TestProbeOverTheLink(t *testing.T) {
	s := bypassing(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()

	resp, _ := serve(t, s, Request{
		Op:    OpCheck,
		Check: &CheckRequest{Target: ln.Addr().String(), TimeoutSeconds: 3},
	})
	if resp.Code != "" || resp.Check == nil {
		t.Fatalf("check = %+v", resp)
	}
	if !resp.Check.Reachable {
		t.Fatalf("probe said unreachable: %+v", resp.Check)
	}
	if resp.Check.LatencyMS < 0 {
		t.Errorf("latency = %d", resp.Check.LatencyMS)
	}
}

func TestPermissionErrorMapping(t *testing.T) {
	code, _ := permissionError(errors.New("something else"), "Desktop")
	if code != "internal" {
		t.Fatalf("an unexpected error mapped to %q", code)
	}
	code, hint := permissionError(permission.ErrTimedOut, "Desktop")
	if code != "timed_out" || !strings.Contains(hint, "Desktop") {
		t.Fatalf("timeout mapped to %q / %q", code, hint)
	}
	code, _ = permissionError(permission.ErrDeclined, "Desktop")
	if code != "declined" {
		t.Fatalf("decline mapped to %q", code)
	}
}
