package permission

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func exec(cmd, cwd string) Action {
	return Action{Kind: KindExec, Device: "Laptop", Effect: cmd, Cwd: cwd, Provenance: "mcp"}
}

// answerIn resolves a prompt after a short delay, as a user would.
func answerIn(t *testing.T, e *Engine, delay time.Duration, accept, remember bool) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if pending := e.Pending(); len(pending) > 0 {
				time.Sleep(delay)
				_ = e.Answer(pending[0].ID, accept, remember)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// --- the chokepoint ---

func TestBypassRunsWithoutAsking(t *testing.T) {
	e := New(nil)
	if err := e.SetMode(ModeBypass); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	d, err := e.Decide(context.Background(), exec("rm -rf /tmp/x", "~"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Verdict != Bypassed {
		t.Fatalf("verdict = %q, want bypassed", d.Verdict)
	}
	if !d.Verdict.Allowed() {
		t.Fatal("bypassed is not allowed")
	}
	if len(e.Pending()) != 0 {
		t.Fatal("Bypass raised a prompt")
	}
}

func TestPromptIsTheDefault(t *testing.T) {
	if got := New(nil).Mode(); got != ModePrompt {
		t.Fatalf("default mode = %q, want prompt — a machine that asks first is the safe start", got)
	}
}

func TestAcceptedPromptRuns(t *testing.T) {
	e := New(nil)
	answerIn(t, e, 10*time.Millisecond, true, false)

	d, err := e.Decide(context.Background(), exec("npm test", "~/projects/medcord"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Verdict != Accepted {
		t.Fatalf("verdict = %q, want accepted", d.Verdict)
	}
	if d.WaitedMS < 0 {
		t.Fatalf("waited = %d", d.WaitedMS)
	}
}

// Decline must return a distinguishable error naming the machine, so a caller
// can tell "you said no" from a crash or a network failure.
func TestDeclinedReturnsADistinguishableError(t *testing.T) {
	e := New(nil)
	answerIn(t, e, 10*time.Millisecond, false, false)

	d, err := e.Decide(context.Background(), exec("rm -rf dist", "~/projects"))
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("error = %v, want ErrDeclined", err)
	}
	if d.Verdict != Declined {
		t.Fatalf("verdict = %q", d.Verdict)
	}
	if d.Verdict.Allowed() {
		t.Fatal("declined is allowed")
	}
	if !strings.Contains(err.Error(), "Laptop") {
		t.Fatalf("the error does not name the machine that declined: %v", err)
	}
}

// Nobody said no; nobody was there. An agent should behave differently in each
// case, so the two must never collapse into one.
func TestExpiryIsDistinctFromDecline(t *testing.T) {
	e := New(nil)
	e.Timeout = 120 * time.Millisecond

	start := time.Now()
	d, err := e.Decide(context.Background(), exec("npm run deploy", "~/projects"))

	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("error = %v, want ErrTimedOut", err)
	}
	if errors.Is(err, ErrDeclined) {
		t.Fatal("an expired prompt reports itself as declined; nobody said no")
	}
	if d.Verdict != Expired {
		t.Fatalf("verdict = %q, want expired", d.Verdict)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the prompt took %v to expire", elapsed)
	}
	if !strings.Contains(err.Error(), "Laptop") {
		t.Fatalf("the error does not name the machine: %v", err)
	}
}

func TestDefaultTimeoutIsSixtySeconds(t *testing.T) {
	// The decision we settled on, asserted so it cannot drift silently.
	if DefaultTimeout != 60*time.Second {
		t.Fatalf("default timeout is %v, want 60s", DefaultTimeout)
	}
}

// An agent that disconnects should not leave a prompt hanging forever.
func TestCallerGivingUpResolvesThePrompt(t *testing.T) {
	e := New(nil)
	e.Timeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.Decide(ctx, exec("sleep 60", "~"))
	}()

	// Wait for the prompt, then give up.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(e.Pending()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Decide did not return when its caller gave up")
	}
}

// --- required slots ---

// The design system makes Prompt.Asker and Prompt.Effect required slots so an
// unattributed prompt cannot render. The engine enforces the same rule, so an
// unattributable action cannot be approved either: a LAN tool that asks
// "allow this?" without naming the asker is how you approve the wrong machine.
func TestAnActionWithoutAnAskerOrEffectCannotBeApproved(t *testing.T) {
	e := New(nil)
	e.Timeout = 50 * time.Millisecond

	cases := map[string]Action{
		"no device":    {Kind: KindExec, Effect: "git push"},
		"blank device": {Kind: KindExec, Device: "  ", Effect: "git push"},
		"no effect":    {Kind: KindExec, Device: "Laptop"},
		"blank effect": {Kind: KindExec, Device: "Laptop", Effect: "\t"},
		"no kind":      {Device: "Laptop", Effect: "git push"},
	}
	for name, a := range cases {
		if _, err := e.Decide(context.Background(), a); err == nil {
			t.Errorf("%s: Decide approved an action that cannot be rendered as a prompt", name)
		}
	}
}

func TestPromptExposesItsRequiredSlots(t *testing.T) {
	e := New(nil)
	e.Timeout = 2 * time.Second

	go func() { _, _ = e.Decide(context.Background(), exec("git push", "~/projects/medcord")) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := e.Pending(); len(p) > 0 {
			if p[0].Asker() != "Laptop" {
				t.Fatalf("Asker() = %q", p[0].Asker())
			}
			if p[0].Effect() != "git push" {
				t.Fatalf("Effect() = %q", p[0].Effect())
			}
			if p[0].Action.Cwd == "" {
				t.Fatal("the prompt does not carry the working directory")
			}
			if p[0].Action.Provenance != "mcp" {
				t.Fatal("the prompt does not carry provenance; an agent's action must be legible")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no prompt appeared")
}

// --- remembered rules ---

// The feature that makes Prompt mode livable for agent work.
func TestRememberedRuleRunsWithoutAskingAgain(t *testing.T) {
	e := New(nil)
	answerIn(t, e, 10*time.Millisecond, true, true)

	a := exec("git push", "~/projects/medcord")
	d, err := e.Decide(context.Background(), a)
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if d.Verdict != Accepted || d.Rule == "" {
		t.Fatalf("first decision = %+v, want accepted with a rule created", d)
	}

	// The same command again must not prompt. Timeout is tiny, so if it
	// prompted this would fail rather than hang.
	e.Timeout = 50 * time.Millisecond
	d, err = e.Decide(context.Background(), a)
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if d.Verdict != RuleMatched {
		t.Fatalf("second decision = %q, want rule-matched", d.Verdict)
	}
}

// A rule covers one command in one directory from one device. Everything else
// still asks — that is what distinguishes it from Bypass.
func TestARuleDoesNotCoverAnythingElse(t *testing.T) {
	e := New(nil)
	answerIn(t, e, 10*time.Millisecond, true, true)
	if _, err := e.Decide(context.Background(), exec("git push", "~/projects/medcord")); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	e.Timeout = 60 * time.Millisecond
	others := map[string]Action{
		"different command":   exec("git push --force", "~/projects/medcord"),
		"different directory": exec("git push", "~/projects/solon"),
		"different device":    {Kind: KindExec, Device: "Desktop", Effect: "git push", Cwd: "~/projects/medcord"},
		"different kind":      {Kind: KindLaunch, Device: "Laptop", Effect: "git push", Cwd: "~/projects/medcord"},
	}
	for name, a := range others {
		d, err := e.Decide(context.Background(), a)
		if d.Verdict == RuleMatched {
			t.Errorf("%s: matched a rule it should not have", name)
		}
		if !errors.Is(err, ErrTimedOut) {
			t.Errorf("%s: expected it to prompt and expire, got %v", name, err)
		}
	}
}

// `git push` and `git  push` are the same command typed twice.
func TestWhitespaceIsNormalisedButNothingElseIs(t *testing.T) {
	e := New(nil)
	answerIn(t, e, 10*time.Millisecond, true, true)
	if _, err := e.Decide(context.Background(), exec("git push origin main", "~/p")); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	e.Timeout = 50 * time.Millisecond
	d, err := e.Decide(context.Background(), exec("git   push   origin  main", "~/p"))
	if err != nil {
		t.Fatalf("whitespace-different command: %v", err)
	}
	if d.Verdict != RuleMatched {
		t.Fatalf("verdict = %q; the same command with different spacing should match", d.Verdict)
	}

	// But flag order is NOT normalised: that would start matching commands
	// the user never approved.
	d, _ = e.Decide(context.Background(), exec("git push main origin", "~/p"))
	if d.Verdict == RuleMatched {
		t.Fatal("reordered arguments matched a rule; only whitespace is normalised")
	}
}

// Declining must not create a rule, even if "remember" was somehow set.
func TestDecliningNeverCreatesARule(t *testing.T) {
	e := New(nil)
	answerIn(t, e, 10*time.Millisecond, false, true)

	if _, err := e.Decide(context.Background(), exec("git push", "~/p")); !errors.Is(err, ErrDeclined) {
		t.Fatalf("Decide: %v", err)
	}
	if n := e.Rules().Len(); n != 0 {
		t.Fatalf("declining created %d rules", n)
	}
}

func TestRuleFireCountAndRevoke(t *testing.T) {
	e := New(nil)
	answerIn(t, e, 10*time.Millisecond, true, true)
	a := exec("npm test", "~/projects/medcord")
	if _, err := e.Decide(context.Background(), a); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	e.Timeout = 50 * time.Millisecond
	for i := 0; i < 3; i++ {
		if _, err := e.Decide(context.Background(), a); err != nil {
			t.Fatalf("Decide %d: %v", i, err)
		}
	}

	rules := e.Rules().List()
	if len(rules) != 1 {
		t.Fatalf("got %d rules", len(rules))
	}
	// The count is what tells you whether to trust a rule or revoke it.
	if rules[0].FireCount != 3 {
		t.Fatalf("fire count = %d, want 3", rules[0].FireCount)
	}
	if rules[0].LastFired.IsZero() {
		t.Fatal("the rule does not record when it last fired")
	}

	if !e.Rules().Revoke(rules[0].ID) {
		t.Fatal("Revoke returned false for a rule that exists")
	}
	if e.Rules().Revoke(rules[0].ID) {
		t.Fatal("Revoke returned true twice for the same rule")
	}

	// Revoked means it asks again.
	d, err := e.Decide(context.Background(), a)
	if d.Verdict == RuleMatched {
		t.Fatal("a revoked rule still matched")
	}
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("after revoking, expected a prompt: %v", err)
	}
}

// A revoked device's remembered approvals must not survive to greet it if it
// is ever paired again.
func TestRevokeAll(t *testing.T) {
	s := NewRuleSet()
	s.Remember(exec("a", "~"))
	s.Remember(exec("b", "~"))
	if n := s.RevokeAll(); n != 2 {
		t.Fatalf("RevokeAll removed %d, want 2", n)
	}
	if s.Len() != 0 {
		t.Fatal("rules survived RevokeAll")
	}
}

// Writes into a space are governed by the space, not by rules — the PRD is
// explicit, and a rule that silently covered future writes would be a
// different product.
func TestWritesAreNotRemembered(t *testing.T) {
	s := NewRuleSet()
	a := Action{Kind: KindWrite, Device: "Laptop", Effect: "~/projects/a.txt"}
	s.Remember(a)
	if _, ok := s.Match(a); ok {
		t.Fatal("a file write matched a remembered rule; writes are governed by the space")
	}
}

func TestLaunchAndHandoffAreRemembered(t *testing.T) {
	s := NewRuleSet()
	for _, kind := range []Kind{KindLaunch, KindHandoff} {
		a := Action{Kind: kind, Device: "Laptop", Effect: "code ~/projects"}
		s.Remember(a)
		if _, ok := s.Match(a); !ok {
			t.Fatalf("%s was not remembered; the PRD says rules cover launches and handoffs", kind)
		}
	}
}

// A rule whose id does not match its contents would match actions it was
// never approved for.
func TestLoadRederivesRuleIDs(t *testing.T) {
	s := NewRuleSet()
	s.Load([]Rule{{
		ID:     "rule-tampered",
		Kind:   KindExec,
		Device: "Laptop",
		Effect: "git push",
		Cwd:    "~/projects/medcord",
	}})
	rules := s.List()
	if len(rules) != 1 {
		t.Fatalf("loaded %d rules", len(rules))
	}
	if rules[0].ID == "rule-tampered" {
		t.Fatal("Load trusted a stored id rather than re-deriving it from the contents")
	}
	// And it matches the action it describes.
	if _, ok := s.Match(exec("git push", "~/projects/medcord")); !ok {
		t.Fatal("a loaded rule does not match its own action")
	}
}

// --- prompts ---

func TestAnsweringAnUnknownPrompt(t *testing.T) {
	e := New(nil)
	if err := e.Answer("ask-nope", true, false); !errors.Is(err, ErrUnknownPrompt) {
		t.Fatalf("Answer on an unknown id = %v, want ErrUnknownPrompt", err)
	}
}

// A replayed answer must not take effect.
func TestAnsweringTwice(t *testing.T) {
	e := New(nil)
	e.Timeout = 3 * time.Second

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.Decide(context.Background(), exec("npm test", "~"))
	}()

	deadline := time.Now().Add(3 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		if p := e.Pending(); len(p) > 0 {
			id = p[0].ID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("no prompt appeared")
	}

	if err := e.Answer(id, true, false); err != nil {
		t.Fatalf("first Answer: %v", err)
	}
	if err := e.Answer(id, false, false); !errors.Is(err, ErrUnknownPrompt) {
		t.Fatalf("second Answer = %v, want ErrUnknownPrompt", err)
	}
	<-done
}

// A prompt nobody can see expires in sixty seconds and makes the product look
// broken, so the daemon has to be able to reach a closed window.
func TestNotifierIsToldAboutPrompts(t *testing.T) {
	n := &recordingNotifier{}
	e := New(nil)
	e.Notify = n
	e.Timeout = 100 * time.Millisecond

	_, _ = e.Decide(context.Background(), exec("npm test", "~"))

	if n.raised != 1 {
		t.Fatalf("PromptRaised called %d times, want 1", n.raised)
	}
	if n.resolved != 1 {
		t.Fatalf("PromptResolved called %d times, want 1", n.resolved)
	}
	if n.lastVerdict != Expired {
		t.Fatalf("resolved with %q, want expired", n.lastVerdict)
	}
}

func TestPendingExcludesExpiredPrompts(t *testing.T) {
	e := New(nil)
	e.Timeout = 50 * time.Millisecond
	_, _ = e.Decide(context.Background(), exec("npm test", "~"))
	if n := len(e.Pending()); n != 0 {
		t.Fatalf("%d prompts pending after expiry", n)
	}
}

func TestSetModeRejectsNonsense(t *testing.T) {
	e := New(nil)
	if err := e.SetMode("sometimes"); err == nil {
		t.Fatal("SetMode accepted an unknown mode")
	}
	if e.Mode() != ModePrompt {
		t.Fatalf("a rejected SetMode changed the mode to %q", e.Mode())
	}
}

// The engine is the chokepoint for every side effect, so it must be safe to
// call from many goroutines at once.
func TestConcurrentDecisions(t *testing.T) {
	e := New(nil)
	if err := e.SetMode(ModeBypass); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := e.Decide(context.Background(), exec("echo hi", "~")); err != nil {
					t.Errorf("Decide: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// --- helpers ---

type recordingNotifier struct {
	mu          sync.Mutex
	raised      int
	resolved    int
	lastVerdict Verdict
}

func (n *recordingNotifier) PromptRaised(p *Prompt) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.raised++
}

func (n *recordingNotifier) PromptResolved(p *Prompt, d Decision) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.resolved++
	n.lastVerdict = d.Verdict
}
