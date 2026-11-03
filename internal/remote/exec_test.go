package remote

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

// collect gathers a command's output by stream.
type collector struct {
	mu     sync.Mutex
	stdout strings.Builder
	stderr strings.Builder
	// chunks records how many separate writes arrived, which is how we tell
	// streaming from buffering.
	chunks int
}

func (c *collector) Write(kind OutputKind, b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chunks++
	switch kind {
	case OutStdout:
		c.stdout.Write(b)
	case OutStderr:
		c.stderr.Write(b)
	}
	return nil
}

func (c *collector) out() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdout.String()
}

func (c *collector) err() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stderr.String()
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	r := NewRunner()
	var c collector

	res, err := r.Run(ctx(t), ExecRequest{Command: "echo hello"}, &c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	if strings.TrimSpace(c.out()) != "hello" {
		t.Fatalf("stdout = %q, want hello", c.out())
	}
	if res.DurationMS < 0 {
		t.Fatalf("duration = %d", res.DurationMS)
	}
}

// The remote exit code is what makes the CLI composable in scripts: `ibeji run
// … && deploy` has to mean what it says.
func TestRunReportsNonZeroExit(t *testing.T) {
	r := NewRunner()
	var c collector

	res, err := r.Run(ctx(t), ExecRequest{Command: "exit 42"}, &c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 42 {
		t.Fatalf("exit code = %d, want 42", res.ExitCode)
	}
}

// stdout and stderr stay separate, so a caller can route them to different
// file descriptors as a terminal does.
func TestStdoutAndStderrAreDistinct(t *testing.T) {
	r := NewRunner()
	var c collector

	if _, err := r.Run(ctx(t),
		ExecRequest{Command: "echo to-stdout; echo to-stderr 1>&2"}, &c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(c.out(), "to-stdout") {
		t.Fatalf("stdout = %q", c.out())
	}
	if strings.Contains(c.out(), "to-stderr") {
		t.Fatal("stderr leaked into stdout")
	}
	if !strings.Contains(c.err(), "to-stderr") {
		t.Fatalf("stderr = %q", c.err())
	}
}

// Output must arrive as it is produced. A caller watching a build has to see
// it build; a two-minute test suite that only prints at the end looks hung.
func TestOutputStreamsRatherThanBuffering(t *testing.T) {
	r := NewRunner()

	first := make(chan time.Time, 1)
	var once sync.Once
	sink := SinkFunc(func(kind OutputKind, b []byte) error {
		if len(strings.TrimSpace(string(b))) > 0 {
			once.Do(func() { first <- time.Now() })
		}
		return nil
	})

	start := time.Now()
	// Prints immediately, then waits. If output were buffered to the end, the
	// first chunk would not arrive for a second.
	if _, err := r.Run(ctx(t),
		ExecRequest{Command: "echo now; sleep 1"}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case at := <-first:
		if delay := at.Sub(start); delay > 800*time.Millisecond {
			t.Fatalf("first output arrived after %v; it is being buffered, not streamed", delay)
		}
	default:
		t.Fatal("no output arrived at all")
	}
}

// The requirement the PRD is explicit about: commands run in a LOGIN shell.
// Without it the PATH misses nvm, pnpm and homebrew, and git cannot find its
// credential helper — so the first `git push` fails with something that looks
// nothing like its cause.
func TestRunsInALoginShell(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the zprofile/zshrc distinction is the macOS case")
	}

	home := t.TempDir()
	marker := "IBEJI_LOGIN_SHELL_MARKER"
	// Only a login shell reads .zprofile.
	if err := os.WriteFile(filepath.Join(home, ".zprofile"),
		[]byte("export "+marker+"=yes\n"), 0o644); err != nil {
		t.Fatalf("write .zprofile: %v", err)
	}
	// A non-login interactive shell would read this instead.
	if err := os.WriteFile(filepath.Join(home, ".zshrc"),
		[]byte("export "+marker+"=no\n"), 0o644); err != nil {
		t.Fatalf("write .zshrc: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)

	r := &Runner{Shell: "/bin/zsh", processes: map[string]*Process{}}
	var c collector
	if _, err := r.Run(ctx(t), ExecRequest{Command: "echo $" + marker, Cwd: home}, &c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.TrimSpace(c.out())
	if got != "yes" {
		t.Fatalf("marker = %q, want \"yes\" — commands are not running in a login shell, "+
			"so the user's PATH from ~/.zprofile is missing", got)
	}
}

// A command in a directory that does not exist should fail saying so. The
// shell's own error for a bad cd arrives mixed into output and is easy to miss.
func TestMissingWorkingDirectoryFailsClearly(t *testing.T) {
	r := NewRunner()
	var c collector

	_, err := r.Run(ctx(t),
		ExecRequest{Command: "pwd", Cwd: "/no/such/directory/anywhere"}, &c)
	if err == nil {
		t.Fatal("Run succeeded with a missing working directory")
	}
	if !strings.Contains(err.Error(), "/no/such/directory") {
		t.Fatalf("error does not name the directory: %v", err)
	}
}

func TestWorkingDirectoryIsHonoured(t *testing.T) {
	dir := t.TempDir()
	r := NewRunner()
	var c collector

	if _, err := r.Run(ctx(t), ExecRequest{Command: "pwd", Cwd: dir}, &c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// macOS reports /private/var for /var, so compare resolved paths.
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(c.out()))
	if got != want {
		t.Fatalf("ran in %q, want %q", got, want)
	}
}

func TestEmptyCommandIsRejected(t *testing.T) {
	r := NewRunner()
	var c collector
	for _, cmd := range []string{"", "   ", "\t\n"} {
		if _, err := r.Run(ctx(t), ExecRequest{Command: cmd}, &c); err == nil {
			t.Fatalf("Run accepted an empty command %q", cmd)
		}
	}
}

// A timeout must be distinguishable from a command that simply failed: "it
// overran" and "it went wrong" lead somewhere different.
func TestTimeoutIsReportedAsSuch(t *testing.T) {
	r := NewRunner()
	var c collector

	start := time.Now()
	res, err := r.Run(ctx(t),
		ExecRequest{Command: "sleep 30", TimeoutSeconds: 1}, &c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("a command that overran its timeout did not report TimedOut")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the timeout took %v to fire", elapsed)
	}
}

// Killing a command must kill what it spawned. A dev server that outlives its
// parent is a process you have to walk to the other machine to stop.
func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	r := NewRunner()
	var c collector

	marker := filepath.Join(t.TempDir(), "child-alive")
	// A child that outlives its parent and keeps touching a file.
	cmd := "( while true; do touch " + marker + "; sleep 0.2; done ) & sleep 30"

	if _, err := r.Run(ctx(t), ExecRequest{Command: cmd, TimeoutSeconds: 1}, &c); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Give the group a moment to die, then confirm the child stopped writing.
	time.Sleep(600 * time.Millisecond)
	before, err := os.Stat(marker)
	if err != nil {
		t.Skip("the child never ran; nothing to check")
	}
	time.Sleep(1200 * time.Millisecond)
	after, err := os.Stat(marker)
	if err != nil {
		return // gone entirely, which is fine
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a child process survived its parent being killed; the process group was not killed")
	}
}

func TestContextCancellationStopsACommand(t *testing.T) {
	r := NewRunner()
	var c collector

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Run(runCtx, ExecRequest{Command: "sleep 30"}, &c)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the context did not stop the command")
	}
}

// --- detached processes ---

func TestStartDetachedAndKill(t *testing.T) {
	r := NewRunner()

	p, err := r.Start(ExecRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.ID == "" || p.PID == 0 {
		t.Fatalf("process has no id or pid: %+v", p)
	}
	if exited, _ := p.Exited(); exited {
		t.Fatal("a just-started process reports itself exited")
	}

	// It is listed, so `ibeji ps` can show it.
	found := false
	for _, q := range r.Processes() {
		if q.ID == p.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the started process is not in the process table")
	}

	if err := r.Kill(p.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exited, _ := p.Exited(); exited {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the process did not die after Kill")
}

func TestStartedProcessRemembersRecentOutput(t *testing.T) {
	r := NewRunner()

	p, err := r.Start(ExecRequest{Command: "echo first; echo second; sleep 5"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer r.Kill(p.ID)

	// Attaching should show context rather than starting mid-sentence.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		text := ""
		for _, c := range p.Replay() {
			text += c.Text
		}
		if strings.Contains(text, "first") && strings.Contains(text, "second") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the process did not remember its recent output for an attaching client")
}

func TestKillUnknownProcess(t *testing.T) {
	r := NewRunner()
	if err := r.Kill("p-nope"); err == nil {
		t.Fatal("Kill accepted an unknown process id")
	}
}

func TestProcessLookupAndForget(t *testing.T) {
	r := NewRunner()
	p, err := r.Start(ExecRequest{Command: "sleep 5"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := r.Process(p.ID); !ok {
		t.Fatal("Process() did not find a started process")
	}
	_ = r.Kill(p.ID)
	r.Forget(p.ID)
	if _, ok := r.Process(p.ID); ok {
		t.Fatal("Forget did not drop the process")
	}
}

// --- paths ---

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	got, err := ExpandPath("~")
	if err != nil || got != home {
		t.Fatalf("ExpandPath(~) = %q, %v; want %q", got, err, home)
	}
	got, err = ExpandPath("~/projects")
	if err != nil || got != filepath.Join(home, "projects") {
		t.Fatalf("ExpandPath(~/projects) = %q, %v", got, err)
	}
	// Relative paths become absolute, so a request cannot depend on the
	// daemon's working directory.
	got, err = ExpandPath("relative/path")
	if err != nil {
		t.Fatalf("ExpandPath(relative): %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("ExpandPath(relative) = %q, want an absolute path", got)
	}
	// ~otheruser is refused rather than resolved wrongly.
	if _, err := ExpandPath("~someone/else"); err == nil {
		t.Fatal("ExpandPath accepted ~otheruser")
	}
	if _, err := ExpandPath(""); err == nil {
		t.Fatal("ExpandPath accepted an empty path")
	}
}

func TestLoginShellIsReal(t *testing.T) {
	sh := LoginShell()
	if sh == "" {
		t.Fatal("LoginShell returned nothing")
	}
	if _, err := os.Stat(sh); err != nil {
		t.Fatalf("LoginShell returned %q, which does not exist: %v", sh, err)
	}
}

// --- ops ---

// Getting this wrong in either direction is serious: a mutating op marked as a
// read bypasses the permission engine entirely, and a read marked mutating
// makes Prompt mode unusable for an agent.
func TestOpMutabilityIsCorrect(t *testing.T) {
	mutating := map[Op]bool{OpWrite: true, OpExec: true}
	for _, op := range []Op{OpList, OpStat, OpRead, OpSearch, OpInfo, OpCheck, OpWrite, OpExec} {
		if got := op.Mutating(); got != mutating[op] {
			t.Errorf("%s.Mutating() = %v, want %v", op, got, mutating[op])
		}
		if !op.Known() {
			t.Errorf("%s is not Known()", op)
		}
	}
	if Op("fs.destroy").Known() {
		t.Error("an unknown op reports itself Known")
	}
}
