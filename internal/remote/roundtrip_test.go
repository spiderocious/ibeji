package remote

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/link"
	"github.com/feranmi/ibeji/internal/permission"
)

// Client and server over the in-memory fabric, as the daemon will run them.
//
// The unit tests check each side against a hand-built frame; these check the
// two halves against each other, which is where a protocol mismatch actually
// shows up.
func pair(t *testing.T, mode permission.Mode) (*Client, *permission.Engine) {
	t.Helper()

	engine := permission.New(nil)
	if err := engine.SetMode(mode); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	server := NewServer(nil, engine, quiet())
	server.PeerName = func() string { return "Laptop" }
	server.Shells = NewShellManager()
	t.Cleanup(server.Shells.CloseAll)

	fabric := link.NewMemoryFabric()
	ln, err := fabric.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		for {
			stream, err := conn.Accept(ctx)
			if err != nil {
				return
			}
			go server.Serve(ctx, stream)
		}
	}()

	conn, err := fabric.Dialer("laptop").Dial(ctx, "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return &Client{Conn: func() link.Conn { return conn }, Provenance: "cli"}, engine
}

func TestRoundTripListStatRead(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)
	dir := t.TempDir()
	content := "the file contents\n"
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := c.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("List = %+v", entries)
	}

	info, err := c.Stat(context.Background(), path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("Stat size = %d", info.Size)
	}

	var buf bytes.Buffer
	n, err := c.Read(context.Background(), path, &buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != int64(len(content)) || buf.String() != content {
		t.Fatalf("Read %d bytes: %q", n, buf.String())
	}
}

func TestRoundTripSearch(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("// TODO: fix\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	matches, truncated, err := c.Search(context.Background(), SearchRequest{Root: dir, Query: "TODO"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if truncated {
		t.Error("a one-match search reported truncation")
	}
	if len(matches) != 1 || !strings.Contains(matches[0].Text, "TODO") {
		t.Fatalf("Search = %+v", matches)
	}
}

func TestRoundTripWrite(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)
	target := filepath.Join(t.TempDir(), "nested", "out.txt")
	content := "written across the link\n"

	err := c.Write(context.Background(), target,
		WriteRequest{Bytes: int64(len(content))}, strings.NewReader(content))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Fatalf("wrote %q", got)
	}
}

// The whole point of milestone 2: a command runs on the far machine and its
// output and exit code come back.
func TestRoundTripExec(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)

	var stdout, stderr strings.Builder
	sink := SinkFunc(func(kind OutputKind, b []byte) error {
		if kind == OutStdout {
			stdout.Write(b)
		} else {
			stderr.Write(b)
		}
		return nil
	})

	exit, started, err := c.Exec(context.Background(),
		ExecRequest{Command: "echo out; echo err 1>&2; exit 7"}, sink)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if started == nil || started.Shell == "" {
		t.Fatalf("no acknowledgement: %+v", started)
	}
	if !strings.Contains(stdout.String(), "out") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "err") {
		t.Errorf("stderr = %q", stderr.String())
	}
	// The remote exit code is what makes the CLI composable in scripts.
	if exit.Code != 7 {
		t.Fatalf("exit code = %d, want 7", exit.Code)
	}
	if exit.DurationMS < 0 {
		t.Errorf("duration = %d", exit.DurationMS)
	}
}

// Output must arrive while the command runs, not in a lump at the end.
func TestRoundTripExecStreams(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)

	first := make(chan time.Time, 1)
	sink := SinkFunc(func(kind OutputKind, b []byte) error {
		if len(strings.TrimSpace(string(b))) > 0 {
			select {
			case first <- time.Now():
			default:
			}
		}
		return nil
	})

	start := time.Now()
	if _, _, err := c.Exec(context.Background(),
		ExecRequest{Command: "echo now; sleep 1"}, sink); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	select {
	case at := <-first:
		if d := at.Sub(start); d > 900*time.Millisecond {
			t.Fatalf("first output arrived after %v; it is not streaming across the link", d)
		}
	default:
		t.Fatal("no output arrived")
	}
}

// A declined command comes back with a code the caller can branch on.
func TestRoundTripDeclined(t *testing.T) {
	c, engine := pair(t, permission.ModePrompt)
	engine.Timeout = 5 * time.Second

	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if p := engine.Pending(); len(p) > 0 {
				_ = engine.Answer(p[0].ID, false, false)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	_, _, err := c.Exec(context.Background(), ExecRequest{Command: "echo hi"}, nil)
	if err == nil {
		t.Fatal("a declined command reported success")
	}
	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("error is not a RemoteError: %v", err)
	}
	if re.Code != "declined" {
		t.Fatalf("code = %q, want declined", re.Code)
	}
}

// Expiry must stay distinguishable from decline all the way to the caller.
func TestRoundTripTimedOut(t *testing.T) {
	c, engine := pair(t, permission.ModePrompt)
	engine.Timeout = 120 * time.Millisecond

	_, _, err := c.Exec(context.Background(), ExecRequest{Command: "echo hi"}, nil)
	if err == nil {
		t.Fatal("an unanswered command reported success")
	}
	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("error is not a RemoteError: %v", err)
	}
	if re.Code != "timed_out" {
		t.Fatalf("code = %q, want timed_out", re.Code)
	}
	if re.Hint == "" {
		t.Error("a timeout carries no hint; an agent would have to guess why")
	}
}

// Approving once with "remember" must mean the next identical command runs
// without asking — the flow that makes agent work possible under Prompt mode.
func TestRoundTripRememberedRule(t *testing.T) {
	c, engine := pair(t, permission.ModePrompt)
	engine.Timeout = 5 * time.Second

	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if p := engine.Pending(); len(p) > 0 {
				_ = engine.Answer(p[0].ID, true, true) // accept and remember
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	req := ExecRequest{Command: "echo approved", Cwd: os.TempDir()}
	if _, _, err := c.Exec(context.Background(), req, nil); err != nil {
		t.Fatalf("first Exec: %v", err)
	}

	// Now nobody is answering, and the timeout is tiny. If it prompted again
	// this would fail rather than hang.
	engine.Timeout = 80 * time.Millisecond
	exit, _, err := c.Exec(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("second Exec was not covered by the remembered rule: %v", err)
	}
	if exit.Code != 0 {
		t.Fatalf("exit = %d", exit.Code)
	}
}

func TestRoundTripInfoAndCheck(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)

	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.OS == "" || info.Shell == "" {
		t.Fatalf("info = %+v", info)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()

	res, err := c.Check(context.Background(), CheckRequest{Target: ln.Addr().String(), TimeoutSeconds: 3})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Reachable {
		t.Fatalf("probe said unreachable: %+v", res)
	}
}

// With no link there is nothing to carry the request. On a LAN that is normal
// — the other machine is asleep — so it must be a clean, nameable failure.
func TestClientOfflineIsCleanlyReported(t *testing.T) {
	c := &Client{Conn: func() link.Conn { return nil }}
	if _, err := c.List(context.Background(), "~"); err == nil {
		t.Fatal("List succeeded with no link")
	}
	if _, _, err := c.Exec(context.Background(), ExecRequest{Command: "echo hi"}, nil); err == nil {
		t.Fatal("Exec succeeded with no link")
	}
}

func TestRoundTripDetach(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)

	_, started, err := c.Exec(context.Background(),
		ExecRequest{Command: "sleep 10", Detach: true}, nil)
	if err != nil {
		t.Fatalf("Exec detached: %v", err)
	}
	if started == nil || started.ProcessID == "" {
		t.Fatalf("a detached command returned no process id: %+v", started)
	}
	if started.PID == 0 {
		t.Error("a detached command returned no pid")
	}
}

// --- the Shell tab over the link ------------------------------------------

// readShell follows a remote shell from the live edge, returning what it says.
func readShell(t *testing.T, c *Client, id string, since int, want string, budget time.Duration) (string, int) {
	t.Helper()
	var sb strings.Builder
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		out, next, done := func() ([]ShellOutput, int, bool) {
			o, n, d, err := c.ShellRead(context.Background(), id, since, 400*time.Millisecond)
			if err != nil {
				t.Fatalf("ShellRead: %v", err)
			}
			return o, n, d
		}()
		since = next
		for _, o := range out {
			sb.WriteString(o.Data)
		}
		if strings.Contains(sb.String(), want) || done {
			return sb.String(), since
		}
	}
	return sb.String(), since
}

// The point of a remote shell: state has to carry across commands. If it did
// not, exec would already be enough and this whole path would be redundant.
func TestRoundTripShellKeepsState(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)

	sess, err := c.ShellOpen(context.Background(), "")
	if err != nil {
		t.Fatalf("ShellOpen: %v", err)
	}
	if sess.ID == "" || sess.PID == 0 {
		t.Fatalf("ShellOpen returned an empty session: %+v", sess)
	}
	defer c.ShellClose(context.Background(), sess.ID)

	_, seq, _, err := c.ShellRead(context.Background(), sess.ID, 0, 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	if err := c.ShellWrite(context.Background(), sess.ID, "cd /tmp"); err != nil {
		t.Fatalf("ShellWrite cd: %v", err)
	}
	if err := c.ShellWrite(context.Background(), sess.ID, "pwd"); err != nil {
		t.Fatalf("ShellWrite pwd: %v", err)
	}
	got, seq := readShell(t, c, sess.ID, seq, "/tmp", 10*time.Second)
	if !strings.Contains(got, "/tmp") {
		t.Fatalf("cd did not carry across the link:\n%q", got)
	}

	if err := c.ShellWrite(context.Background(), sess.ID, "export RT_VAR=carried"); err != nil {
		t.Fatalf("ShellWrite export: %v", err)
	}
	if err := c.ShellWrite(context.Background(), sess.ID, "echo \"[$RT_VAR]\""); err != nil {
		t.Fatalf("ShellWrite echo: %v", err)
	}
	got, _ = readShell(t, c, sess.ID, seq, "[carried]", 10*time.Second)
	if !strings.Contains(got, "[carried]") {
		t.Fatalf("an exported variable did not carry across the link:\n%q", got)
	}
}

// Several shells at once, because the tab has sub-tabs.
func TestRoundTripShellListAndClose(t *testing.T) {
	c, _ := pair(t, permission.ModeBypass)
	ctx := context.Background()

	a, err := c.ShellOpen(ctx, "")
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	b, err := c.ShellOpen(ctx, "/tmp")
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("two shells share an id")
	}

	list, err := c.ShellList(ctx)
	if err != nil {
		t.Fatalf("ShellList: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ShellList returned %d shells, want 2", len(list))
	}

	if err := c.ShellClose(ctx, a.ID); err != nil {
		t.Fatalf("ShellClose: %v", err)
	}
	list, _ = c.ShellList(ctx)
	if len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("after closing a, the list is %+v", list)
	}
	_ = c.ShellClose(ctx, b.ID)
}

// Prompt mode must gate what a shell runs, exactly as it gates exec.
// A shell that bypassed the permission engine would be a hole straight
// through it.
func TestRoundTripShellIsGatedInPromptMode(t *testing.T) {
	c, engine := pair(t, permission.ModePrompt)
	ctx := context.Background()

	// Decline whatever is asked.
	go func() {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			for _, p := range engine.Pending() {
				_ = engine.Answer(p.ID, false, false)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	if _, err := c.ShellOpen(ctx, ""); err == nil {
		t.Fatal("opening a shell was allowed in Prompt mode without approval")
	}
}
