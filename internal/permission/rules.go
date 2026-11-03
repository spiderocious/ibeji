package permission

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// Remembered rules are what make Prompt mode livable.
//
// Bypass gives an agent the whole machine; Prompt asks you to approve every
// `git push` forever. A rule is the third answer: approve this exact command,
// in this directory, from this device, once — and it never asks again, while
// everything else still asks.
//
// v1 matches exactly: no wildcards, no path prefixes, no command patterns. A
// pattern language is where permission systems go to die, and `git push` in
// one repo is the real need. A rule that does not match simply prompts, which
// is the safe failure.

// Rule is a remembered approval.
type Rule struct {
	ID string `json:"id"`

	// Kind, Device, Effect and Cwd are the rule's identity. All four must
	// match for the rule to fire.
	Kind   Kind   `json:"kind"`
	Device string `json:"device"`
	Effect string `json:"effect"`
	Cwd    string `json:"cwd,omitempty"`

	// CreatedAt is when it was approved.
	CreatedAt time.Time `json:"createdAt"`

	// FireCount is how often it has been used since. Shown in the rules list,
	// because a rule that has fired 41 times is one to be confident about and
	// a rule that fired once is one to revoke.
	FireCount int `json:"fireCount"`

	// LastFired is when it last matched.
	LastFired time.Time `json:"lastFired,omitempty"`
}

// RuleSet holds the remembered rules for a device.
type RuleSet struct {
	mu    sync.RWMutex
	rules map[string]*Rule
	now   func() time.Time
}

// NewRuleSet builds an empty set.
func NewRuleSet() *RuleSet {
	return &RuleSet{rules: map[string]*Rule{}, now: time.Now}
}

// SetClock replaces the time source, for tests.
func (s *RuleSet) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// normalise reduces a command to its comparable form.
//
// Whitespace only. `git push` and `git  push` are the same command typed
// twice; `git push` and `git push --force` are not, and must never match the
// same rule. Nothing else is normalised: flag reordering and path
// canonicalisation look helpful and are how a rule starts matching commands
// the user never approved.
func normalise(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ruleKey is the identity of a rule: all four parts, hashed for a short id.
func ruleKey(kind Kind, device, effect, cwd string) string {
	h := sha256.New()
	h.Write([]byte("ibeji-rule-v1"))
	for _, part := range []string{string(kind), device, normalise(effect), normalise(cwd)} {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return "rule-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// Match finds a rule for this action.
func (s *RuleSet) Match(a Action) (*Rule, bool) {
	// Only side effects that can be remembered are. File writes into a space
	// are governed by the space, not by rules — the PRD is explicit.
	if a.Kind != KindExec && a.Kind != KindLaunch && a.Kind != KindHandoff {
		return nil, false
	}
	id := ruleKey(a.Kind, a.Device, a.Effect, a.Cwd)

	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rules[id]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// Remember creates a rule from an approved action.
func (s *RuleSet) Remember(a Action) *Rule {
	id := ruleKey(a.Kind, a.Device, a.Effect, a.Cwd)

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.rules[id]; ok {
		cp := *existing
		return &cp
	}
	r := &Rule{
		ID:        id,
		Kind:      a.Kind,
		Device:    a.Device,
		Effect:    normalise(a.Effect),
		Cwd:       normalise(a.Cwd),
		CreatedAt: s.now(),
	}
	s.rules[id] = r
	cp := *r
	return &cp
}

// Fired records a rule matching, for the count the rules list shows.
func (s *RuleSet) Fired(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rules[id]; ok {
		r.FireCount++
		r.LastFired = s.now()
	}
}

// List returns every rule, most recently created first.
func (s *RuleSet) List() []Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Revoke removes a rule. One click, per the PRD.
func (s *RuleSet) Revoke(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[id]; !ok {
		return false
	}
	delete(s.rules, id)
	return true
}

// RevokeAll removes every rule, for unpairing — a revoked device's remembered
// approvals must not survive to greet it if it is ever paired again.
func (s *RuleSet) RevokeAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.rules)
	s.rules = map[string]*Rule{}
	return n
}

// Len reports how many rules are held.
func (s *RuleSet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rules)
}

// Load restores rules from storage.
func (s *RuleSet) Load(rules []Rule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = make(map[string]*Rule, len(rules))
	for i := range rules {
		r := rules[i]
		// Re-derive the id rather than trusting the stored one: a rule whose
		// id does not match its contents would match actions it was never
		// approved for.
		r.ID = ruleKey(r.Kind, r.Device, r.Effect, r.Cwd)
		s.rules[r.ID] = &r
	}
}
