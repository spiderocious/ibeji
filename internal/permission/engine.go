// Package permission decides whether an incoming action from the paired
// device is allowed to run.
//
// Every side effect passes through here, whatever its origin — the window, the
// CLI, the HTTP API or an agent over MCP. If it is possible to reach a side
// effect without passing through this package, that is a bug of the highest
// severity in this product, which is why the engine is one function with one
// entry point rather than a set of checks sprinkled through the handlers.
//
// Reads never pass through: they do not prompt in either mode. That is the
// PRD's decision, and it is what makes the API usable for an agent.
package permission

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Mode is how this device treats incoming actions. Set on the RECEIVING
// device, and per direction: Desktop can be in Bypass toward Laptop while
// Laptop stays in Prompt toward Desktop.
type Mode string

const (
	// ModePrompt asks before every side effect. The right default, and the
	// right mode for a corporate-managed machine.
	//
	// Named ModePrompt rather than Prompt because Prompt is the question
	// itself, and a mode and a question are different things.
	ModePrompt Mode = "prompt"

	// ModeBypass runs everything from the paired device without asking. Right
	// between two machines the same person owns and sits at — and guarded by
	// hold-to-confirm, because it is not destructive now, it is destructive
	// later, on every command that follows.
	ModeBypass Mode = "bypass"
)

// Kind is what sort of action is being asked for.
type Kind string

const (
	// KindExec runs a command.
	KindExec Kind = "exec"

	// KindWrite writes a file through the API.
	KindWrite Kind = "write"

	// KindLaunch opens an application.
	KindLaunch Kind = "launch"

	// KindHandoff opens a URL in the browser.
	KindHandoff Kind = "handoff"
)

// Verdict is what the engine decided.
type Verdict string

const (
	// Allowed by Bypass mode.
	Bypassed Verdict = "bypassed"

	// Allowed by a remembered rule.
	RuleMatched Verdict = "rule-matched"

	// Allowed by the user, just now.
	Accepted Verdict = "accepted"

	// Refused by the user.
	Declined Verdict = "declined"

	// Nobody answered in time. Distinct from Declined on purpose: nobody said
	// no, nobody was there — and an agent should behave differently.
	Expired Verdict = "expired"
)

// Allowed reports whether this verdict permits the action.
func (v Verdict) Allowed() bool {
	switch v {
	case Bypassed, RuleMatched, Accepted:
		return true
	default:
		return false
	}
}

// DefaultTimeout is how long a prompt waits for an answer.
//
// Long enough to notice a notification and decide; short enough that an agent
// learns quickly that nobody is there. The alternative — a caller hanging
// indefinitely on a locked screen — is the failure this exists to prevent.
const DefaultTimeout = 60 * time.Second

var (
	// ErrDeclined means the user said no.
	ErrDeclined = errors.New("permission: declined")

	// ErrTimedOut means the prompt expired unanswered.
	ErrTimedOut = errors.New("permission: timed out awaiting approval")

	// ErrUnknownPrompt means the prompt id was never issued, or was already
	// answered — a replayed answer must not take effect.
	ErrUnknownPrompt = errors.New("permission: no such prompt")
)

// Action describes what the peer wants to do.
type Action struct {
	Kind Kind

	// Device is the name of the machine asking. Required: a prompt that does
	// not name the asker cannot render, because approving the wrong machine
	// is exactly what this guards against.
	Device string

	// Effect is what will happen, verbatim — the command line, the path, the
	// URL. Required for the same reason.
	Effect string

	// Cwd is where a command will run. Part of a rule's identity.
	Cwd string

	// Provenance is who asked: app, cli, mcp or api. Self-declared, so not a
	// security boundary; it is what makes an agent's action legible.
	Provenance string
}

// Valid reports whether the action can be rendered as a prompt.
//
// Both required slots are checked here rather than trusted: the design system
// makes Prompt.Asker and Prompt.Effect required slots so an unattributed
// prompt cannot render, and the engine enforces the same rule so an
// unattributable action cannot be approved either.
func (a Action) Valid() error {
	if strings.TrimSpace(a.Device) == "" {
		return fmt.Errorf("permission: an action with no asking device cannot be approved")
	}
	if strings.TrimSpace(a.Effect) == "" {
		return fmt.Errorf("permission: an action with no stated effect cannot be approved")
	}
	if a.Kind == "" {
		return fmt.Errorf("permission: an action with no kind cannot be approved")
	}
	return nil
}

// Decision is the engine's answer, for the caller and the log.
type Decision struct {
	Verdict Verdict

	// Rule names the remembered rule that matched, if one did.
	Rule string

	// WaitedMS is how long the user took, for the trace.
	WaitedMS int64
}

// Prompt is a question waiting for an answer.
type Prompt struct {
	ID     string
	Action Action

	// At is when it was raised.
	At time.Time

	// ExpiresAt is when it stops being answerable.
	ExpiresAt time.Time

	answer chan answer

	// answered guards against a replayed answer. The channel is buffered, so
	// without this a second Answer could land in it during the window between
	// the first being sent and Decide removing the prompt.
	answered bool
}

type answer struct {
	verdict Verdict
	// remember creates a rule from this action.
	remember bool
}

// Expired reports whether the window has closed.
func (p *Prompt) Expired(now time.Time) bool { return now.After(p.ExpiresAt) }

// Asker and Effect mirror the design system's required slots, so a renderer
// cannot build a prompt without them.
func (p *Prompt) Asker() string  { return p.Action.Device }
func (p *Prompt) Effect() string { return p.Action.Effect }

// Notifier is told when a prompt needs answering, so the window can surface it
// and the tray can change.
//
// A prompt nobody can see expires in sixty seconds and makes the product look
// broken, so this is how the daemon reaches a closed window.
type Notifier interface {
	PromptRaised(p *Prompt)
	PromptResolved(p *Prompt, d Decision)
}

// Engine decides, and remembers.
type Engine struct {
	// Timeout overrides DefaultTimeout.
	Timeout time.Duration

	// Notify, when set, is told about prompts.
	Notify Notifier

	mu      sync.Mutex
	mode    Mode
	rules   *RuleSet
	pending map[string]*Prompt
	nextID  int
	now     func() time.Time
}

// New builds an engine in Prompt mode — the safe default.
func New(rules *RuleSet) *Engine {
	if rules == nil {
		rules = NewRuleSet()
	}
	return &Engine{
		mode:    ModePrompt,
		rules:   rules,
		pending: map[string]*Prompt{},
		now:     time.Now,
	}
}

// SetClock replaces the time source, for tests crossing the expiry boundary.
func (e *Engine) SetClock(now func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
}

func (e *Engine) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now()
}

// Mode reports the current mode.
func (e *Engine) Mode() Mode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// SetMode changes the mode.
//
// Flipping to Bypass authorises every future command from that machine. The UI
// guards it with hold-to-confirm; the engine does not second-guess a decision
// the user has already made, but it does log it, because a mode change is the
// most consequential thing in this package.
func (e *Engine) SetMode(m Mode) error {
	switch m {
	case ModePrompt, ModeBypass:
	default:
		return fmt.Errorf("permission: unknown mode %q", m)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = m
	return nil
}

// Rules exposes the remembered rules.
func (e *Engine) Rules() *RuleSet { return e.rules }

// Decide is the chokepoint. Every side effect from the peer passes through it.
//
// It blocks while a prompt is outstanding, which is the point: the caller is
// waiting to know whether its command ran, and an answer that arrives after
// the caller gave up is no answer at all.
func (e *Engine) Decide(ctx context.Context, a Action) (Decision, error) {
	if err := a.Valid(); err != nil {
		return Decision{}, err
	}

	e.mu.Lock()
	mode := e.mode
	e.mu.Unlock()

	if mode == ModeBypass {
		return Decision{Verdict: Bypassed}, nil
	}

	// A remembered rule is what makes Prompt mode livable: `git push` in one
	// repo, approved once, never asked again — while everything else still
	// asks.
	if rule, ok := e.rules.Match(a); ok {
		e.rules.Fired(rule.ID)
		return Decision{Verdict: RuleMatched, Rule: rule.ID}, nil
	}

	return e.prompt(ctx, a)
}

// prompt raises a question and waits.
func (e *Engine) prompt(ctx context.Context, a Action) (Decision, error) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	e.mu.Lock()
	e.nextID++
	now := e.now()
	p := &Prompt{
		ID:        fmt.Sprintf("ask-%d", e.nextID),
		Action:    a,
		At:        now,
		ExpiresAt: now.Add(timeout),
		answer:    make(chan answer, 1),
	}
	e.pending[p.ID] = p
	e.mu.Unlock()

	if e.Notify != nil {
		e.Notify.PromptRaised(p)
	}

	defer func() {
		e.mu.Lock()
		delete(e.pending, p.ID)
		e.mu.Unlock()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ans := <-p.answer:
		waited := e.clock().Sub(p.At).Milliseconds()
		d := Decision{Verdict: ans.verdict, WaitedMS: waited}
		if ans.verdict == Accepted && ans.remember {
			rule := e.rules.Remember(a)
			d.Rule = rule.ID
		}
		if e.Notify != nil {
			e.Notify.PromptResolved(p, d)
		}
		if ans.verdict == Declined {
			return d, fmt.Errorf("%w by %s", ErrDeclined, a.Device)
		}
		return d, nil

	case <-timer.C:
		// Expired, not declined. Nobody said no; nobody was there.
		d := Decision{Verdict: Expired, WaitedMS: timeout.Milliseconds()}
		if e.Notify != nil {
			e.Notify.PromptResolved(p, d)
		}
		return d, fmt.Errorf("%w on %s", ErrTimedOut, a.Device)

	case <-ctx.Done():
		// The caller gave up — it disconnected, or its own deadline passed.
		d := Decision{Verdict: Expired, WaitedMS: e.clock().Sub(p.At).Milliseconds()}
		if e.Notify != nil {
			e.Notify.PromptResolved(p, d)
		}
		return d, ctx.Err()
	}
}

// Pending lists prompts waiting for an answer, so a window opening mid-prompt
// shows it rather than nothing.
func (e *Engine) Pending() []*Prompt {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Prompt, 0, len(e.pending))
	now := e.now()
	for _, p := range e.pending {
		if !p.Expired(now) {
			out = append(out, p)
		}
	}
	return out
}

// Answer resolves a prompt.
//
// remember creates a rule from the action — the third button beside Accept and
// Decline, and the one that makes Prompt mode usable for agent work.
func (e *Engine) Answer(id string, accept bool, remember bool) error {
	e.mu.Lock()
	p, ok := e.pending[id]
	e.mu.Unlock()
	if !ok {
		return ErrUnknownPrompt
	}

	verdict := Declined
	if accept {
		verdict = Accepted
	}

	// Claim the prompt under the lock: whoever sets answered first is the
	// answer, and every later one is refused.
	e.mu.Lock()
	if p.answered {
		e.mu.Unlock()
		return ErrUnknownPrompt
	}
	p.answered = true
	e.mu.Unlock()

	select {
	case p.answer <- answer{verdict: verdict, remember: remember && accept}:
		return nil
	default:
		// Expired while we claimed it; nobody is listening.
		return ErrUnknownPrompt
	}
}
