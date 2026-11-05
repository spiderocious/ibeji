package remote

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A shell session: one long-lived login shell on this machine, with stdin
// held open so the caller can type into it.
//
// Exec (above) spawns `zsh -lc <command>` per call, which is right for
// `ibeji run`: one command, one exit code, nothing left behind. It cannot
// back a terminal, because every call gets a fresh process — `cd ~/projects`
// then `ls` lists the wrong directory, and `export FOO=1` is gone by the next
// line. A shell someone types into has to be one process that stays.
//
// This is deliberately NOT a PTY. There is no terminal device, so no curses
// program, no password prompt, no line editing. What it gives is a persistent
// shell: state carries across commands, long-running commands keep streaming,
// and an interrupt reaches the foreground job. Anything needing a real
// terminal needs a PTY, which is a larger piece of work and honestly
// advertised as absent rather than half-present.

// ShellOutput is one chunk of output from a session.
type ShellOutput struct {
	Stream string `json:"stream"` // "stdout" or "stderr"
	Data   string `json:"data"`
}

// ShellSession is one running shell.
type ShellSession struct {
	ID      string    `json:"id"`
	Shell   string    `json:"shell"`
	Cwd     string    `json:"cwd"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`

	cmd   *exec.Cmd
	stdin io.WriteCloser

	ready chan struct{}

	// log, when set, reports what an interrupt actually did. Signalling is
	// invisible otherwise: the command either stops or it does not, and
	// there is nothing to look at when it does not.
	log func(msg string, args ...any)

	mu      sync.Mutex
	out     []ShellOutput
	seq     int
	done    bool
	exitErr error
	waiters []chan struct{}
}

// scrollbackLimit bounds what one session keeps in memory.
//
// A shell left running `tail -f` would otherwise grow without limit inside
// the daemon, which holds the link for the whole product. The window keeps
// its own scrollback; this is only what a reconnecting reader can catch up on.
const scrollbackLimit = 2000

// ShellManager owns the sessions on this machine.
type ShellManager struct {
	// Shell is the login shell to run. Defaults to the user's SHELL.
	Shell string

	// Logger, when set, records what interrupts did.
	Logger *slog.Logger

	mu   sync.Mutex
	next int
	byID map[string]*ShellSession
}

// quietZDotDir writes a .zshrc that defers to the user's own and then clears
// the prompt, returning the directory to point ZDOTDIR at.
//
// Best-effort: if it cannot be written the shell still works, it just echoes
// a prompt the window then has to live with.
func (m *ShellManager) quietZDotDir() (string, error) {
	dir, err := os.MkdirTemp("", "ibeji-zdot-")
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	rc := fmt.Sprintf(`# Written by Ìbejì for its Shell tab. Sources your own
# configuration first, so aliases, functions and PATH are unchanged, then
# silences the prompt because the window draws its own.
ZDOTDIR=%q
[ -f %q ] && source %q
PS1=''
PS2=''
RPS1=''
unsetopt PROMPT_SP 2>/dev/null
precmd() { :; }
`, home, home+"/.zshrc", home+"/.zshrc")
	if err := os.WriteFile(dir+"/.zshrc", []byte(rc), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// NewShellManager builds an empty manager.
func NewShellManager() *ShellManager {
	return &ShellManager{byID: map[string]*ShellSession{}}
}

func (m *ShellManager) shellPath() string {
	if m.Shell != "" {
		return m.Shell
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/zsh"
}

// Open starts a session in cwd, or the home directory when cwd is empty.
func (m *ShellManager) Open(cwd string) (*ShellSession, error) {
	dir, err := resolveCwd(cwd)
	if err != nil {
		return nil, err
	}

	sh := m.shellPath()

	// Silence the shell's own prompt by giving zsh an rc directory of ours
	// that sources the user's files and then clears PS1.
	//
	// Neither PS1 in the environment nor a write to stdin works: an
	// interactive zsh sets its prompt in .zshrc, which runs after the
	// environment is read and before the first thing we could type. The pane
	// filled with "feranmi@Mac ~ %" between every command. ZDOTDIR is the
	// supported way in, and sourcing the user's own .zshrc first keeps their
	// aliases, functions and PATH exactly as they are.
	zdot, zerr := m.quietZDotDir()
	// -i, not -l: an interactive shell reads the rc file, so aliases and
	// functions behave the way they do in a terminal. Login files are read
	// too for the common case of zsh, which is what puts nvm and homebrew on
	// PATH.
	cmd := exec.Command(sh, "-i")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		// No terminal: stop the shell drawing colours or line editing into a
		// pipe. TERM=dumb is what well-behaved tools check.
		"TERM=dumb",
		"IBEJI_SHELL=1",
	)
	if zerr == nil && zdot != "" && strings.Contains(sh, "zsh") {
		cmd.Env = append(cmd.Env, "ZDOTDIR="+zdot)
	}
	// Its own process group, so an interrupt reaches the whole job and not
	// the daemon.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("remote: shell stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("remote: shell stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("remote: shell stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("remote: start %s: %w", sh, err)
	}

	m.mu.Lock()
	m.next++
	id := fmt.Sprintf("sh-%03x", m.next)
	s := &ShellSession{
		ID:      id,
		Shell:   sh,
		Cwd:     dir,
		PID:     cmd.Process.Pid,
		Started: time.Now(),
		cmd:     cmd,
		stdin:   stdin,
	}
	if m.Logger != nil {
		s.log = func(msg string, args ...any) { m.Logger.Info(msg, args...) }
	}
	m.byID[id] = s
	m.mu.Unlock()

	go s.pump(stdout, "stdout")
	go s.pump(stderr, "stderr")

	// Wait for the shell to be ready before anyone can type into it.
	//
	// An interactive zsh sources .zshrc before it reads the first line of
	// stdin. A command written into that window is not queued reliably — it
	// can be swallowed outright — so a Shell tab that sent a command the
	// instant it opened would sometimes do nothing at all. Echoing a marker
	// and waiting for it to come back is the only honest signal that the
	// shell has reached its first read.
	s.ready = make(chan struct{})
	go s.waitReady()
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.done = true
		s.exitErr = err
		s.wakeLocked()
		s.mu.Unlock()
	}()

	return s, nil
}

// readyMarker is echoed once at startup and waited for. Distinctive enough
// not to collide with real output, and filtered out before anyone sees it.
const readyMarker = "__ibeji_shell_ready__"

// waitReady writes the marker and blocks until it comes back, then opens the
// session for business.
func (s *ShellSession) waitReady() {
	defer close(s.ready)
	if _, err := io.WriteString(s.stdin, "echo "+readyMarker+"\n"); err != nil {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	seq := 0
	for time.Now().Before(deadline) {
		out, next, done := s.Read(seq, 500*time.Millisecond)
		seq = next
		for _, o := range out {
			if strings.Contains(o.Data, readyMarker) {
				s.dropMarker()
				return
			}
		}
		if done {
			return
		}
	}
}

// dropMarker blanks the readiness echo, so the first thing a user sees in a
// new tab is not our own bookkeeping.
//
// It REPLACES rather than removes: seq counts appends and Read maps a
// sequence onto a slice index with `s.seq - len(s.out)`. Shortening the
// slice shifts that mapping under every existing reader, which sent them
// past the end of the stream and made the pane look dead.
func (s *ShellSession) dropMarker() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.out {
		if strings.Contains(o.Data, readyMarker) {
			s.out[i].Data = ""
		}
	}
}

// awaitReady blocks until the shell has sourced its rc files, or the timeout
// passes. Every write goes through it.
func (s *ShellSession) awaitReady() {
	if s.ready == nil {
		return
	}
	select {
	case <-s.ready:
	case <-time.After(10 * time.Second):
	}
}

// pump copies one of the shell's streams into the session's scrollback.
func (s *ShellSession) pump(r io.Reader, stream string) {
	br := bufio.NewReader(r)
	buf := make([]byte, 4096)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			s.append(ShellOutput{Stream: stream, Data: string(buf[:n])})
		}
		if err != nil {
			return
		}
	}
}

func (s *ShellSession) append(o ShellOutput) {
	s.mu.Lock()
	s.out = append(s.out, o)
	if len(s.out) > scrollbackLimit {
		s.out = s.out[len(s.out)-scrollbackLimit:]
	}
	s.seq++
	s.wakeLocked()
	s.mu.Unlock()
}

// wakeLocked releases everyone waiting on new output. Caller holds s.mu.
func (s *ShellSession) wakeLocked() {
	for _, ch := range s.waiters {
		close(ch)
	}
	s.waiters = nil
}

// Write sends a line to the shell. A trailing newline is added when missing,
// because a line without one simply sits in the shell's buffer and the caller
// sees nothing happen at all.
func (s *ShellSession) Write(line string) error {
	// Never write into a shell that is still starting: the line races the rc
	// files and can be swallowed, which looks like a tab that ignored you.
	s.awaitReady()

	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done {
		return fmt.Errorf("remote: this shell has exited")
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := io.WriteString(s.stdin, line); err != nil {
		return fmt.Errorf("remote: write to shell: %w", err)
	}
	return nil
}

// Interrupt stops the running command without killing the shell.
//
// Signalling the whole process group (-pid) is what a terminal does, but a
// terminal's shell is job-control-aware and ignores the SIGINT meant for its
// foreground job. Ours is not a session leader with a controlling terminal,
// so the group signal hits the shell too: it survived, stopped reading stdin,
// and the tab went deaf — the command stopped, and so did everything after.
//
// So signal the shell's CHILDREN and leave the shell alone. Without a PTY
// there is no foreground-job concept to consult, so this interrupts every
// direct child, which for a shell waiting at a prompt is exactly the command
// that is running.
func (s *ShellSession) Interrupt() error {
	s.awaitReady()
	if s.cmd.Process == nil {
		return nil
	}
	pids, err := childPIDs(s.cmd.Process.Pid)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		// Nothing running: a bare Ctrl-C at a prompt does nothing, which is
		// also what a terminal does.
		return nil
	}
	if s.log != nil {
		s.log("shell: interrupting", "shell", s.ID, "children", pids)
	}
	for _, pid := range pids {
		// Negative pid: the child may have started its own group (a dev
		// server and its workers), and stopping only the parent would leave
		// those behind. Fall back to the bare pid when it leads no group.
		if err := syscall.Kill(-pid, syscall.SIGINT); err != nil {
			_ = syscall.Kill(pid, syscall.SIGINT)
		}
	}
	return nil
}

// childPIDs lists the direct children of pid, via ps.
//
// There is no portable syscall for this and the daemon is macOS-only, so ps
// is the honest way rather than reading a /proc that does not exist here.
func childPIDs(pid int) ([]int, error) {
	out, err := exec.Command("ps", "-o", "pid=", "-g", fmt.Sprint(pid)).Output()
	if err != nil {
		// No matching processes is an exit status, not a failure.
		return nil, nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p int
		if _, err := fmt.Sscanf(line, "%d", &p); err != nil || p == pid {
			continue
		}
		pids = append(pids, p)
	}
	return pids, nil
}

// Close ends the session and everything it started.
func (s *ShellSession) Close() error {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd.Process == nil {
		return nil
	}
	// The whole group: a shell that started a dev server must not leave it
	// running after the tab is closed.
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	go func() {
		time.Sleep(2 * time.Second)
		if s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	return nil
}

// Read returns output from `since` onward, with the sequence to ask for next.
//
// It blocks for up to `wait` for something to arrive, so a reader can long-poll
// rather than spin. Returning the sequence rather than consuming lets a
// reconnecting window replay what it missed.
func (s *ShellSession) Read(since int, wait time.Duration) (out []ShellOutput, seq int, done bool) {
	deadline := time.Now().Add(wait)
	for {
		s.mu.Lock()
		// seq counts appends; the slice holds at most scrollbackLimit of
		// them, so the oldest readable index floats up as output is dropped.
		first := s.seq - len(s.out)
		if since < first {
			since = first
		}
		if since < s.seq {
			from := since - first
			out = append([]ShellOutput(nil), s.out[from:]...)
			seq, done = s.seq, s.done
			s.mu.Unlock()
			return out, seq, done
		}
		if s.done {
			seq, done = s.seq, true
			s.mu.Unlock()
			return nil, seq, true
		}
		if wait <= 0 || !time.Now().Before(deadline) {
			seq = s.seq
			s.mu.Unlock()
			return nil, seq, false
		}
		ch := make(chan struct{})
		s.waiters = append(s.waiters, ch)
		s.mu.Unlock()

		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
		}
	}
}

// Running reports whether the shell is still alive.
func (s *ShellSession) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.done
}

// Get returns a session by id.
func (m *ShellManager) Get(id string) (*ShellSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	return s, ok
}

// List returns every session, oldest first.
func (m *ShellManager) List() []*ShellSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*ShellSession, 0, len(m.byID))
	for _, s := range m.byID {
		out = append(out, s)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Started.Before(out[i].Started) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// CloseAll ends every session. Called when the link drops or on unpair: a
// shell is only reachable through the pair, so an orphan serves nobody.
func (m *ShellManager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*ShellSession, 0, len(m.byID))
	for id, s := range m.byID {
		sessions = append(sessions, s)
		delete(m.byID, id)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

// Remove closes one session and forgets it.
func (m *ShellManager) Remove(id string) error {
	m.mu.Lock()
	s, ok := m.byID[id]
	delete(m.byID, id)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("remote: no shell %q", id)
	}
	return s.Close()
}
