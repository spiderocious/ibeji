package remote

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A shell session is a shell: state must carry across commands. That is the
// whole reason it exists rather than reusing Exec, which spawns a fresh
// `zsh -lc` per call and so forgets `cd` and `export` immediately.

// reader follows one session, remembering how far it has read.
//
// A helper that restarts from sequence 0 on every call re-reads the whole
// scrollback and so can "find" text that arrived before the thing under test
// — which is how an earlier version of this file reported a working
// interrupt as a failure. The cursor has to be held across calls.
type reader struct {
	s   *ShellSession
	seq int
	buf strings.Builder
}

// follow starts a reader at the CURRENT end of the stream, so it only ever
// sees output produced after this point.
//
// It reads s.seq directly rather than asking Read for it: Read clamps a
// too-large `since` down into the window and hands back data, so using it to
// probe the end position returned a sequence BEHIND the end and the reader
// then replayed history it was supposed to skip.
func follow(s *ShellSession) *reader {
	s.mu.Lock()
	seq := s.seq
	s.mu.Unlock()
	return &reader{s: s, seq: seq}
}

// await reads until `want` appears in output seen SINCE this reader was made,
// or the budget runs out. Returns everything it has seen, for the message.
func (r *reader) await(t *testing.T, want string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		out, next, done := r.s.Read(r.seq, 250*time.Millisecond)
		r.seq = next
		for _, o := range out {
			r.buf.WriteString(o.Data)
		}
		if strings.Contains(r.buf.String(), want) {
			return r.buf.String()
		}
		if done || !time.Now().Before(deadline) {
			return r.buf.String()
		}
	}
}

// drain is the one-shot form, for a reader that does not need a cursor.
func drain(t *testing.T, s *ShellSession, want string, budget time.Duration) string {
	t.Helper()
	var sb strings.Builder
	seq := 0
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		out, next, _ := s.Read(seq, 300*time.Millisecond)
		seq = next
		for _, o := range out {
			sb.WriteString(o.Data)
		}
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
	}
	return sb.String()
}

func TestShellKeepsStateAcrossCommands(t *testing.T) {
	m := NewShellManager()
	s, err := m.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Remove(s.ID)

	// cd must persist: this is the headline difference from exec.
	if err := s.Write("cd /tmp"); err != nil {
		t.Fatalf("Write cd: %v", err)
	}
	if err := s.Write("pwd"); err != nil {
		t.Fatalf("Write pwd: %v", err)
	}
	got := drain(t, s, "/tmp", 8*time.Second)
	if !strings.Contains(got, "/tmp") {
		t.Fatalf("cd did not persist to the next command; output was:\n%s", got)
	}

	// And so must an exported variable.
	if err := s.Write("export IBEJI_TEST_VAR=carried"); err != nil {
		t.Fatalf("Write export: %v", err)
	}
	if err := s.Write("echo \"[$IBEJI_TEST_VAR]\""); err != nil {
		t.Fatalf("Write echo: %v", err)
	}
	got = drain(t, s, "[carried]", 8*time.Second)
	if !strings.Contains(got, "[carried]") {
		t.Fatalf("an exported variable did not survive to the next command; output was:\n%s", got)
	}
}

func TestShellStreamsLongRunningOutput(t *testing.T) {
	m := NewShellManager()
	s, err := m.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Remove(s.ID)

	// Output must arrive while the command is still running, not only at the
	// end — otherwise a dev server looks like it hung.
	if err := s.Write("for i in 1 2 3; do echo tick-$i; sleep 0.4; done"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	first := drain(t, s, "tick-1", 6*time.Second)
	if !strings.Contains(first, "tick-1") {
		t.Fatalf("no output while the command was still running:\n%s", first)
	}
	rest := drain(t, s, "tick-3", 8*time.Second)
	if !strings.Contains(rest, "tick-3") {
		t.Fatalf("the command's later output never arrived:\n%s", rest)
	}
}

func TestShellInterruptStopsTheRunningCommand(t *testing.T) {
	m := NewShellManager()
	s, err := m.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Remove(s.ID)

	if err := s.Write("echo starting; sleep 30; echo SHOULD-NOT-APPEAR"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Assert against the process table, not the scrollback.
	//
	// Earlier versions of this test tracked a cursor through the session's
	// output and kept getting it wrong, reporting a working interrupt as a
	// failure four different ways. What the feature actually promises is
	// simpler and directly observable: the running command dies, the shell
	// does not.
	if !waitForChild(s, "sleep", 8*time.Second) {
		t.Fatal("the command never started")
	}

	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	if !waitForNoChild(s, "sleep", 8*time.Second) {
		t.Fatal("the interrupt did not stop the running command")
	}
	if !s.Running() {
		t.Fatal("the interrupt killed the shell itself; Ctrl-C must stop the job and leave the shell")
	}

	// And the shell still takes work afterwards, which is the whole point of
	// sparing it.
	r := follow(s)
	if err := s.Write("echo after-interrupt"); err != nil {
		t.Fatalf("the shell would not take a command after the interrupt: %v", err)
	}
	if got := r.await(t, "after-interrupt", 10*time.Second); !strings.Contains(got, "after-interrupt") {
		t.Fatalf("the shell did not respond after the interrupt:\n%q", got)
	}
}

// waitForChild reports whether the session has a child whose command line
// contains `name`, within the budget.
func waitForChild(s *ShellSession, name string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if hasChild(s, name) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

func waitForNoChild(s *ShellSession, name string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if !hasChild(s, name) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

func hasChild(s *ShellSession, name string) bool {
	out, err := exec.Command("ps", "-o", "command=", "-g", fmt.Sprint(s.PID)).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, name) && !strings.Contains(line, "zsh") {
			return true
		}
	}
	return false
}

func TestShellOpensInAGivenDirectory(t *testing.T) {
	m := NewShellManager()
	s, err := m.Open("/tmp")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Remove(s.ID)
	if !strings.Contains(s.Cwd, "tmp") {
		t.Errorf("Cwd = %q, want it under /tmp", s.Cwd)
	}
	if err := s.Write("pwd"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := drain(t, s, "tmp", 8*time.Second); !strings.Contains(got, "tmp") {
		t.Errorf("the shell did not start in /tmp:\n%s", got)
	}
}

func TestShellSessionsAreIndependent(t *testing.T) {
	m := NewShellManager()
	a, err := m.Open("")
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	defer m.Remove(a.ID)
	b, err := m.Open("")
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer m.Remove(b.ID)

	if a.ID == b.ID {
		t.Fatal("two sessions share an id")
	}
	// A cd in one tab must not move the other.
	_ = a.Write("cd /tmp")
	_ = a.Write("pwd")
	drain(t, a, "/tmp", 6*time.Second)

	_ = b.Write("pwd")
	got := drain(t, b, "\n", 6*time.Second)
	if strings.Contains(got, "/tmp") && !strings.Contains(a.Cwd, "tmp") {
		t.Errorf("a cd in one shell moved another; b saw:\n%s", got)
	}

	if n := len(m.List()); n != 2 {
		t.Errorf("List() = %d sessions, want 2", n)
	}
}

func TestClosingAShellEndsIt(t *testing.T) {
	m := NewShellManager()
	s, err := m.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Remove(s.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !s.Running() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s.Running() {
		t.Error("the shell is still running after being closed")
	}
	if _, ok := m.Get(s.ID); ok {
		t.Error("a closed shell is still listed")
	}
}

// The window draws its own prompt. A shell echoing "feranmi@Mac ~ %" between
// every command gives the pane two of them, and neither PS1 in the
// environment nor a write to stdin suppresses it — an interactive zsh sets
// its prompt in .zshrc, after the first and before the second.
func TestShellDoesNotEchoAPrompt(t *testing.T) {
	m := NewShellManager()
	s, err := m.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Remove(s.ID)
	time.Sleep(900 * time.Millisecond)

	r := follow(s)
	if err := s.Write("echo hello-there"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := r.await(t, "hello-there", 8*time.Second)
	if strings.Contains(got, "%") && strings.Contains(got, "@") {
		t.Errorf("the shell is drawing its own prompt into the pane:\n%q", got)
	}
}

// The user's own zsh configuration must still apply: silencing the prompt
// must not cost them their aliases, functions or PATH.
func TestShellKeepsTheUsersConfiguration(t *testing.T) {
	m := NewShellManager()
	s, err := m.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Remove(s.ID)
	time.Sleep(900 * time.Millisecond)

	r := follow(s)
	// HOME is the clearest thing to check that survives sourcing .zshrc.
	if err := s.Write("echo \"home=[$HOME]\""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := r.await(t, "home=[", 8*time.Second)
	if !strings.Contains(got, "home=[/") {
		t.Errorf("the shell lost its environment:\n%q", got)
	}
}
