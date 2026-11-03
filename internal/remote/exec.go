package remote

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Commands run inside the user's login shell, as the logged-in user, with no
// privilege escalation.
//
// This is a product requirement, not an implementation detail. A command run
// without a login shell gets a PATH that misses nvm, pnpm and homebrew, and a
// git that cannot find its credential helper — so the first `git push` fails
// with something that looks nothing like its cause. The PRD calls it out for
// that reason, and `ibeji doctor` checks for it.

// LoginShell reports the shell commands run in.
//
// It reads the user's actual shell rather than assuming zsh: a machine where
// the user switched to fish or bash would otherwise get a PATH assembled by a
// shell they do not configure.
func LoginShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		// On macOS the login shell is in the directory service, which
		// `dscl` reads; falling back to zsh is right for every modern Mac.
		if runtime.GOOS == "darwin" {
			return "/bin/zsh"
		}
	}
	return "/bin/sh"
}

// Runner executes commands on this machine.
type Runner struct {
	// Shell overrides the login shell. Empty means LoginShell().
	Shell string

	// Env is extra environment for every command, on top of the login
	// shell's own. Used for tests; empty in production, because the whole
	// point is to inherit what the user's shell provides.
	Env []string

	mu        sync.Mutex
	processes map[string]*Process
	nextID    int
}

// NewRunner builds a runner.
func NewRunner() *Runner {
	return &Runner{processes: map[string]*Process{}}
}

func (r *Runner) shell() string {
	if r.Shell != "" {
		return r.Shell
	}
	return LoginShell()
}

// Process is a command this runner started.
type Process struct {
	ID      string
	Command string
	Cwd     string
	PID     int
	Started time.Time

	cmd    *exec.Cmd
	cancel context.CancelFunc

	mu       sync.Mutex
	exited   bool
	exitCode int
	// ring holds recent output so an attaching client sees context rather
	// than starting mid-sentence.
	ring *ringBuffer
}

// Result is what a finished command reports.
type Result struct {
	ExitCode   int
	DurationMS int64
	TimedOut   bool
}

// Sink receives a command's output as it happens.
type Sink interface {
	// Write is called with each chunk and which stream it came from.
	Write(kind OutputKind, b []byte) error
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(kind OutputKind, b []byte) error

// Write implements Sink.
func (f SinkFunc) Write(kind OutputKind, b []byte) error { return f(kind, b) }

// Run executes a command, streaming output to the sink, and returns its result.
//
// Output streams as it is produced rather than being buffered to the end: a
// caller watching a build must see it build, and a test suite that takes two
// minutes must not look like a hang.
func (r *Runner) Run(ctx context.Context, req ExecRequest, sink Sink) (Result, error) {
	if strings.TrimSpace(req.Command) == "" {
		return Result{}, fmt.Errorf("%w: no command", ErrMalformed)
	}

	cwd, err := resolveCwd(req.Cwd)
	if err != nil {
		return Result{}, err
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// `-l` makes it a login shell, which is the whole point: it reads
	// ~/.zprofile and assembles the PATH the user actually has.
	cmd := exec.CommandContext(runCtx, r.shell(), "-lc", req.Command)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), r.Env...)

	// Own process group, so killing the command kills what it spawned. A dev
	// server that survives its parent is a process you have to walk over to
	// the other machine to stop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("remote: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("remote: stderr pipe: %w", err)
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("remote: start %q: %w", req.Command, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(stdout, OutStdout, sink) }()
	go func() { defer wg.Done(); pump(stderr, OutStderr, sink) }()
	wg.Wait()

	waitErr := cmd.Wait()
	elapsed := time.Since(start)

	res := Result{DurationMS: elapsed.Milliseconds()}
	// A timeout is reported as such rather than as a generic non-zero exit:
	// "the command overran" and "the command failed" lead somewhere different.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	res.ExitCode = exitCodeOf(waitErr)
	return res, nil
}

// Start runs a command detached, returning immediately.
//
// A dev server started from the other machine must outlive the call that
// started it, and must still be killable without walking over there.
func (r *Runner) Start(req ExecRequest) (*Process, error) {
	if strings.TrimSpace(req.Command) == "" {
		return nil, fmt.Errorf("%w: no command", ErrMalformed)
	}
	cwd, err := resolveCwd(req.Cwd)
	if err != nil {
		return nil, err
	}

	// Detached processes are not bounded by the request's timeout: that is
	// what detaching means. They end when they end, or when killed.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, r.shell(), "-lc", req.Command)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), r.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("remote: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("remote: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("remote: start %q: %w", req.Command, err)
	}

	r.mu.Lock()
	r.nextID++
	id := fmt.Sprintf("p-%03x", r.nextID)
	p := &Process{
		ID:      id,
		Command: req.Command,
		Cwd:     cwd,
		PID:     cmd.Process.Pid,
		Started: time.Now(),
		cmd:     cmd,
		cancel:  cancel,
		ring:    newRingBuffer(200),
	}
	r.processes[id] = p
	r.mu.Unlock()

	// Keep recent output so attaching shows context rather than starting
	// mid-sentence.
	go pump(stdout, OutStdout, SinkFunc(func(k OutputKind, b []byte) error {
		p.ring.add(k, b)
		return nil
	}))
	go pump(stderr, OutStderr, SinkFunc(func(k OutputKind, b []byte) error {
		p.ring.add(k, b)
		return nil
	}))
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exited = true
		p.exitCode = exitCodeOf(err)
		p.mu.Unlock()
	}()

	return p, nil
}

// Processes lists what this runner started.
func (r *Runner) Processes() []*Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Process, 0, len(r.processes))
	for _, p := range r.processes {
		out = append(out, p)
	}
	return out
}

// Process returns one by id.
func (r *Runner) Process(id string) (*Process, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.processes[id]
	return p, ok
}

// Kill stops a process and everything it spawned.
func (r *Runner) Kill(id string) error {
	r.mu.Lock()
	p, ok := r.processes[id]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("remote: no process %q", id)
	}
	p.cancel()
	return nil
}

// Forget drops a finished process from the table.
func (r *Runner) Forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.processes, id)
}

// Exited reports whether the process has finished, and its code.
func (p *Process) Exited() (bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited, p.exitCode
}

// Replay returns the recent output, for a client attaching mid-run.
func (p *Process) Replay() []OutputChunk { return p.ring.snapshot() }

// OutputChunk is one piece of remembered output.
type OutputChunk struct {
	Kind OutputKind
	Text string
}

// --- helpers ---

// pump copies a pipe into the sink, chunk by chunk as it arrives.
func pump(r io.Reader, kind OutputKind, sink Sink) {
	// A modest buffer: large enough not to syscall per byte, small enough
	// that a line appears as it is written rather than when a buffer fills.
	buf := make([]byte, 8192)
	br := bufio.NewReaderSize(r, 8192)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			if werr := sink.Write(kind, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// resolveCwd expands ~ and verifies the directory exists.
//
// A command that would run in a missing directory should fail saying so,
// rather than running somewhere unexpected — the shell's own error for a bad
// `cd` arrives mixed into output and is easy to miss.
func resolveCwd(cwd string) (string, error) {
	if cwd == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("remote: locate home directory: %w", err)
		}
		return home, nil
	}
	expanded, err := ExpandPath(cwd)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(expanded)
	if err != nil {
		return "", fmt.Errorf("remote: working directory %q: %w", cwd, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("remote: working directory %q is not a directory", cwd)
	}
	return expanded, nil
}

// ExpandPath expands a leading ~ and makes the path absolute.
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty path", ErrMalformed)
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("remote: locate home directory: %w", err)
		}
		if p == "~" {
			return home, nil
		}
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:]), nil
		}
		// ~otheruser is not supported; saying so beats resolving it wrongly.
		return "", fmt.Errorf("%w: cannot expand %q", ErrMalformed, p)
	}
	return filepath.Abs(p)
}

// exitCodeOf extracts the exit status from a Wait error.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	// Could not run at all — a missing shell, a permissions problem. 127 is
	// the shell's own convention for "command not found", which is the
	// closest honest answer.
	return 127
}

// ringBuffer keeps the most recent output chunks.
type ringBuffer struct {
	mu    sync.Mutex
	items []OutputChunk
	limit int
}

func newRingBuffer(limit int) *ringBuffer {
	return &ringBuffer{limit: limit}
}

func (b *ringBuffer) add(kind OutputKind, p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = append(b.items, OutputChunk{Kind: kind, Text: string(p)})
	if len(b.items) > b.limit {
		b.items = b.items[len(b.items)-b.limit:]
	}
}

func (b *ringBuffer) snapshot() []OutputChunk {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]OutputChunk, len(b.items))
	copy(out, b.items)
	return out
}
