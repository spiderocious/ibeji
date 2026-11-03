package permission

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The PRD's permission table, transcribed and asserted.
//
// This is the table the whole module exists to implement, so it is tested as a
// table rather than as prose: every row, in both modes, times rule-match and
// no-match, times accept, decline and expiry. A regression in any cell is a
// security regression, and the ones that look least interesting — a read that
// starts prompting, a write that stops — are the ones most likely to slip
// through review.
func TestPermissionMatrix(t *testing.T) {
	type outcome struct {
		verdict Verdict
		allowed bool
	}

	cases := []struct {
		name    string
		mode    Mode
		kind    Kind
		rule    bool // a matching rule exists
		answer  *bool
		want    outcome
		wantErr error
	}{
		// Bypass: everything runs, nothing asks.
		{"bypass/exec", ModeBypass, KindExec, false, nil, outcome{Bypassed, true}, nil},
		{"bypass/write", ModeBypass, KindWrite, false, nil, outcome{Bypassed, true}, nil},
		{"bypass/launch", ModeBypass, KindLaunch, false, nil, outcome{Bypassed, true}, nil},
		{"bypass/handoff", ModeBypass, KindHandoff, false, nil, outcome{Bypassed, true}, nil},

		// Prompt with a matching rule: runs, logged as rule-matched.
		{"prompt/exec/rule", ModePrompt, KindExec, true, nil, outcome{RuleMatched, true}, nil},
		{"prompt/launch/rule", ModePrompt, KindLaunch, true, nil, outcome{RuleMatched, true}, nil},
		{"prompt/handoff/rule", ModePrompt, KindHandoff, true, nil, outcome{RuleMatched, true}, nil},

		// Prompt, accepted.
		{"prompt/exec/accept", ModePrompt, KindExec, false, boolp(true), outcome{Accepted, true}, nil},
		{"prompt/write/accept", ModePrompt, KindWrite, false, boolp(true), outcome{Accepted, true}, nil},

		// Prompt, declined.
		{"prompt/exec/decline", ModePrompt, KindExec, false, boolp(false), outcome{Declined, false}, ErrDeclined},
		{"prompt/write/decline", ModePrompt, KindWrite, false, boolp(false), outcome{Declined, false}, ErrDeclined},

		// Prompt, unanswered.
		{"prompt/exec/expire", ModePrompt, KindExec, false, nil, outcome{Expired, false}, ErrTimedOut},
		{"prompt/launch/expire", ModePrompt, KindLaunch, false, nil, outcome{Expired, false}, ErrTimedOut},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := New(nil)
			e.Timeout = 150 * time.Millisecond
			if err := e.SetMode(c.mode); err != nil {
				t.Fatalf("SetMode: %v", err)
			}

			a := Action{Kind: c.kind, Device: "Laptop", Effect: "the effect", Cwd: "~/projects"}
			if c.rule {
				e.Rules().Remember(a)
			}
			if c.answer != nil {
				accept := *c.answer
				go func() {
					deadline := time.Now().Add(3 * time.Second)
					for time.Now().Before(deadline) {
						if p := e.Pending(); len(p) > 0 {
							_ = e.Answer(p[0].ID, accept, false)
							return
						}
						time.Sleep(5 * time.Millisecond)
					}
				}()
			}

			d, err := e.Decide(context.Background(), a)

			if d.Verdict != c.want.verdict {
				t.Errorf("verdict = %q, want %q", d.Verdict, c.want.verdict)
			}
			if d.Verdict.Allowed() != c.want.allowed {
				t.Errorf("allowed = %v, want %v", d.Verdict.Allowed(), c.want.allowed)
			}
			if c.wantErr == nil && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Errorf("error = %v, want %v", err, c.wantErr)
			}
		})
	}
}

// Writes are never covered by a rule, whatever the mode: they are governed by
// the space. A rule that silently covered future writes would be a different
// product.
func TestWritesNeverMatchARuleInAnyMode(t *testing.T) {
	for _, mode := range []Mode{ModePrompt, ModeBypass} {
		e := New(nil)
		e.Timeout = 80 * time.Millisecond
		if err := e.SetMode(mode); err != nil {
			t.Fatalf("SetMode: %v", err)
		}
		a := Action{Kind: KindWrite, Device: "Laptop", Effect: "~/projects/a.txt"}
		e.Rules().Remember(a)

		d, _ := e.Decide(context.Background(), a)
		if d.Verdict == RuleMatched {
			t.Errorf("%s: a write matched a rule", mode)
		}
	}
}

// Switching to Bypass must take effect at once, and switching back must too:
// a mode that lagged would mean commands running under a policy the user
// thought they had changed.
func TestModeChangesTakeEffectImmediately(t *testing.T) {
	e := New(nil)
	e.Timeout = 80 * time.Millisecond
	a := Action{Kind: KindExec, Device: "Laptop", Effect: "npm test", Cwd: "~"}

	// Prompt: expires.
	if _, err := e.Decide(context.Background(), a); !errors.Is(err, ErrTimedOut) {
		t.Fatalf("in Prompt mode: %v", err)
	}

	// Bypass: runs.
	if err := e.SetMode(ModeBypass); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	d, err := e.Decide(context.Background(), a)
	if err != nil || d.Verdict != Bypassed {
		t.Fatalf("after switching to Bypass: %+v %v", d, err)
	}

	// Back to Prompt: asks again.
	if err := e.SetMode(ModePrompt); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if _, err := e.Decide(context.Background(), a); !errors.Is(err, ErrTimedOut) {
		t.Fatalf("after switching back to Prompt: %v", err)
	}
}

// Bypass must not quietly create rules: turning it off has to restore the
// previous behaviour exactly, or "I'll just try Bypass for a minute" leaves
// permanent approvals behind.
func TestBypassLeavesNoRulesBehind(t *testing.T) {
	e := New(nil)
	if err := e.SetMode(ModeBypass); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := e.Decide(context.Background(),
			Action{Kind: KindExec, Device: "Laptop", Effect: "npm test", Cwd: "~"}); err != nil {
			t.Fatalf("Decide: %v", err)
		}
	}
	if n := e.Rules().Len(); n != 0 {
		t.Fatalf("Bypass created %d rules; turning it off would not restore the previous behaviour", n)
	}
}

func boolp(b bool) *bool { return &b }
