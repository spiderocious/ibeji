package device

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// A record of what happened during one pairing attempt.
//
// Pairing used to be a single opaque call: it returned a session or an error,
// and the window showed a spinner that became either a six-digit code or the
// words "the paired device is not reachable". That sentence is true and almost
// useless. It does not say whether the address was malformed, whether the TCP
// connection was refused (something is there, but not Ìbejì), whether it timed
// out (nothing is there at all), whether TLS failed, or whether the machine
// answered as somebody else. Those are five different problems with five
// different answers, and a person staring at one sentence cannot tell them
// apart.
//
// Worse, that one sentence sent a user chasing a macOS permission dialog for a
// day when the real cause was a stale IP: the other Mac had moved from
// 192.168.1.147 to 192.168.1.39 after the network changed subnets. "Not
// reachable" was accurate about the symptom and silent about the cause.
//
// So the daemon now writes down every step, keeps the log of the attempt, and
// serves both. The window draws it as a timeline. When a step fails it is the
// step that is marked, carrying the real error and — where one exists — the
// specific next move.

// StepState is how a step turned out.
type StepState string

const (
	StepPending StepState = "pending"
	StepRunning StepState = "running"
	StepOK      StepState = "ok"
	StepFailed  StepState = "failed"
	// StepSkipped marks a step that could not be reached because an earlier
	// one failed. Rendering these greyed rather than hiding them keeps the
	// shape of the ceremony visible, so a reader can see how far it got.
	StepSkipped StepState = "skipped"
)

// PairStep is one stage of the ceremony.
type PairStep struct {
	// Key is stable and machine-readable, for the UI to match against.
	Key string `json:"key"`
	// Title is what the timeline row reads.
	Title string `json:"title"`
	// Detail is what happened, in a sentence, once it has happened.
	Detail string    `json:"detail,omitempty"`
	State  StepState `json:"state"`
	// Error is the underlying error, verbatim. Shown beneath the step, and
	// kept raw: a paraphrase of a network error loses the part that
	// identifies it.
	Error string `json:"error,omitempty"`
	// Cause is this daemon's reading of WHY, in plain words.
	Cause string `json:"cause,omitempty"`
	// Fix is the concrete next move, when there is one.
	Fix string `json:"fix,omitempty"`
	// Scan asks the window to offer a subnet sweep: set when the failure is
	// consistent with the peer having moved.
	Scan bool `json:"scan,omitempty"`

	StartedAt time.Time  `json:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

// Took reports how long the step ran, for the timeline's right-hand column.
func (s *PairStep) Took() time.Duration {
	if s.StartedAt.IsZero() || s.EndedAt == nil {
		return 0
	}
	return s.EndedAt.Sub(s.StartedAt)
}

// The steps of the ceremony, in order. Declared as data rather than built up
// in the flow so that the window can draw the whole timeline — including the
// steps not yet reached — the instant pairing begins. A timeline that grows a
// row at a time cannot show you how far there is left to go.
var pairStepPlan = []struct{ Key, Title string }{
	// Checked before anything else: v1 pins exactly one peer, so a daemon
	// that is already paired cannot pair again. This was briefly folded into
	// the address step, which produced the nonsense of an "already paired"
	// refusal being reported as a malformed address — a mis-attribution the
	// e2e test caught, and exactly the kind of wrong answer the timeline
	// exists to stop.
	{"ready", "Check this Mac can pair"},
	{"address", "Read the address"},
	{"connect", "Open a connection"},
	{"tls", "Verify the encrypted channel"},
	{"key", "Read the other Mac's key"},
	{"name", "Ask its name"},
	{"code", "Derive the six digits"},
	{"confirm", "Wait for both sides to confirm"},
}

// PairAttempt is one attempt, and the log it produced.
type PairAttempt struct {
	mu sync.Mutex

	id      string
	address string
	started time.Time
	ended   *time.Time
	steps   []PairStep
	log     []string
	// sessionID ties a finished attempt to the ceremony it opened, so the
	// window can keep showing the right timeline beside the code.
	sessionID string
}

// PairAttemptView is the attempt as the API serves it.
type PairAttemptView struct {
	ID        string     `json:"id"`
	Address   string     `json:"address"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	SessionID string     `json:"sessionId,omitempty"`
	Steps     []PairStep `json:"steps"`
	Log       []string   `json:"log"`
	// Failed is the key of the step that broke, empty when none did. The
	// window uses it to scroll the timeline to the point of failure rather
	// than making someone hunt for the one red row.
	Failed string `json:"failed,omitempty"`
	Done   bool   `json:"done"`
	OK     bool   `json:"ok"`
}

func newPairAttempt(id, address string) *PairAttempt {
	a := &PairAttempt{id: id, address: address, started: time.Now()}
	for _, p := range pairStepPlan {
		a.steps = append(a.steps, PairStep{Key: p.Key, Title: p.Title, State: StepPending})
	}
	return a
}

// logf appends to the attempt's own log.
//
// Separate from the daemon log on purpose. The daemon log is everything the
// daemon did; what a person debugging one failed pairing wants is the twenty
// lines belonging to that attempt, which is what the Copy button on the modal
// hands them.
func (a *PairAttempt) logf(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appendLogLocked(format, args...)
}

func (a *PairAttempt) appendLogLocked(format string, args ...any) {
	line := fmt.Sprintf("%s  %s",
		time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	a.log = append(a.log, line)
	// Bounded: an attempt is seconds long, but a retry loop that somehow ran
	// away must not grow without limit.
	const maxLines = 400
	if len(a.log) > maxLines {
		a.log = a.log[len(a.log)-maxLines:]
	}
}

func (a *PairAttempt) stepLocked(key string) *PairStep {
	for i := range a.steps {
		if a.steps[i].Key == key {
			return &a.steps[i]
		}
	}
	return nil
}

// begin marks a step as running.
func (a *PairAttempt) begin(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.stepLocked(key); s != nil {
		s.State = StepRunning
		s.StartedAt = time.Now()
		a.appendLogLocked("→ %s", s.Title)
	}
}

// ok marks a step as finished, with a sentence saying what it found.
func (a *PairAttempt) ok(key, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.stepLocked(key); s != nil {
		now := time.Now()
		s.State = StepOK
		s.Detail = detail
		s.EndedAt = &now
		if detail != "" {
			a.appendLogLocked("✓ %s · %s", s.Title, detail)
		} else {
			a.appendLogLocked("✓ %s", s.Title)
		}
	}
}

// warn records a step that did not succeed but does not stop the ceremony.
//
// The name exchange is the live example: a peer that will not say its name is
// still pairable, because the fingerprint beside the code is what carries the
// assurance. Marking that failure as fatal would refuse a pairing that is
// perfectly safe to make.
func (a *PairAttempt) warn(key, detail string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.stepLocked(key); s != nil {
		now := time.Now()
		s.State = StepOK
		s.Detail = detail
		s.EndedAt = &now
		if err != nil {
			s.Error = err.Error()
			a.appendLogLocked("! %s · %s · %v", s.Title, detail, err)
			return
		}
		a.appendLogLocked("! %s · %s", s.Title, detail)
	}
}

// fail marks the step that broke, and skips the ones after it.
func (a *PairAttempt) fail(key string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	hit := false
	for i := range a.steps {
		s := &a.steps[i]
		if s.Key == key {
			hit = true
			s.State = StepFailed
			s.EndedAt = &now
			if s.StartedAt.IsZero() {
				s.StartedAt = now
			}
			if err != nil {
				s.Error = err.Error()
			}
			c := diagnosePair(key, a.address, err)
			s.Cause, s.Fix, s.Scan = c.cause, c.fix, c.scan
			a.appendLogLocked("✗ %s · %v", s.Title, err)
			if c.cause != "" {
				a.appendLogLocked("  cause: %s", c.cause)
			}
			if c.fix != "" {
				a.appendLogLocked("  fix: %s", c.fix)
			}
			continue
		}
		// Everything after the failure was never attempted. Saying so is
		// more honest than leaving them looking pending forever.
		if hit && s.State == StepPending {
			s.State = StepSkipped
		}
	}
	a.ended = &now
}

// succeed closes the attempt once the code is on screen.
func (a *PairAttempt) succeed(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionID = sessionID
}

// finish closes the attempt after the ceremony resolves.
func (a *PairAttempt) finish(ok bool, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.ended = &now
	s := a.stepLocked("confirm")
	if s == nil {
		return
	}
	s.EndedAt = &now
	if s.StartedAt.IsZero() {
		s.StartedAt = now
	}
	if ok {
		s.State = StepOK
		s.Detail = detail
		a.appendLogLocked("✓ %s · %s", s.Title, detail)
		return
	}
	s.State = StepFailed
	s.Detail = detail
	a.appendLogLocked("✗ %s · %s", s.Title, detail)
}

// View renders the attempt for the API.
func (a *PairAttempt) View() *PairAttemptView {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := &PairAttemptView{
		ID:        a.id,
		Address:   a.address,
		StartedAt: a.started,
		EndedAt:   a.ended,
		SessionID: a.sessionID,
		Steps:     make([]PairStep, len(a.steps)),
		Log:       append([]string(nil), a.log...),
	}
	copy(v.Steps, a.steps)
	ok := true
	for i := range a.steps {
		switch a.steps[i].State {
		case StepFailed:
			if v.Failed == "" {
				v.Failed = a.steps[i].Key
			}
			ok = false
		case StepPending, StepRunning, StepSkipped:
			ok = false
		}
	}
	v.OK = ok
	v.Done = a.ended != nil
	return v
}

// cause is a reading of one failure.
type cause struct {
	cause string
	fix   string
	scan  bool
}

// diagnosePair turns an error into words a person can act on.
//
// This is the function that would have saved a day. The error text for a peer
// that has moved and a peer that is switched off is the same — a timeout — but
// the move is far more likely on a home network where DHCP hands out a new
// address overnight, and it is the one case the daemon can actually resolve by
// itself: sweep the subnet for anything answering the Ìbejì port. So a timeout
// says so, and offers the sweep.
func diagnosePair(step, address string, err error) cause {
	msg := ""
	if err != nil {
		msg = strings.ToLower(err.Error())
	}

	switch {
	case strings.Contains(msg, "own address"):
		return cause{
			cause: "That is this Mac's address, not the other one's. A machine cannot pair with itself.",
			fix:   "Open Ìbejì on your other Mac — its own address is shown under “This Mac is at”. Type that one here.",
		}

	case step == "ready", strings.Contains(msg, "already paired"):
		return cause{
			cause: "This Mac is already paired with a device, and Ìbejì pins exactly one.",
			fix:   "Unpair the current device on the Devices tab first, then pair again. If the other Mac has simply moved, use Reconnect instead — that keeps the pairing and only changes the address.",
		}

	case step == "address":
		return cause{
			cause: "That address is not in a form the daemon can dial.",
			fix:   "An address looks like 192.168.1.39:7749 — the other Mac's IP, then a colon, then the port Ìbejì listens on.",
		}

	case strings.Contains(msg, "timeout"), strings.Contains(msg, "timed out"),
		strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "i/o timeout"):
		return cause{
			cause: fmt.Sprintf("Nothing answered at %s. The connection was not refused — it went unanswered, which means no machine is listening there at all.", address),
			fix:   "The usual reason is that the other Mac's IP address changed: a router hands out a new one overnight, or the network moved to a different subnet. Sweep the network to find where it went.",
			scan:  true,
		}

	case strings.Contains(msg, "connection refused"):
		return cause{
			cause: fmt.Sprintf("A machine is at %s, but nothing is listening on that port — it refused the connection outright.", address),
			fix:   "Ìbejì's daemon is probably not running on the other Mac. Open Ìbejì there, or run `ibeji doctor` on it. Check the port too: the default is 7749.",
		}

	case strings.Contains(msg, "no route to host"):
		return cause{
			cause: "macOS blocked this connection. A denial of the Local Network permission looks exactly like a dead network from here — the same error either way.",
			fix:   "Open System Settings → Privacy & Security → Local Network and switch Ìbejì on. macOS re-asks after every update, because each new build counts as a new app.",
		}

	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "name resolution"), strings.Contains(msg, "server misbehaving"):
		return cause{
			cause: "That name could not be resolved to an address.",
			fix:   "Use the other Mac's IP address rather than its name — names depend on mDNS, which plenty of routers block.",
			scan:  true,
		}

	case strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "network is down"):
		return cause{
			cause: "This Mac has no route to that network at all.",
			fix:   "Check that this Mac is on Wi-Fi or Ethernet, and that both machines are on the same network.",
		}

	case step == "tls", strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"),
		strings.Contains(msg, "handshake"):
		return cause{
			cause: "Something is listening there, and it answered — but not as Ìbejì. The encrypted handshake did not complete.",
			fix:   "Check the port: another program may hold 7749 on the other Mac. If both machines run Ìbejì, make sure the versions match — run `ibeji doctor` on each.",
		}

	case step == "key":
		return cause{
			cause: "The other Mac connected but never presented an identity key, so there is nothing to derive the six digits from.",
			fix:   "Restart Ìbejì on the other Mac and try again. If it persists, its keystore may be damaged — `ibeji doctor` on that machine will say.",
		}

	case strings.Contains(msg, "closed"), strings.Contains(msg, "reset by peer"),
		strings.Contains(msg, "broken pipe"), strings.Contains(msg, "eof"):
		return cause{
			cause: "The other Mac accepted the connection and then dropped it mid-ceremony.",
			fix:   "Usually its daemon restarted at that moment. Try again; if it keeps happening, check the log on that machine.",
		}
	}

	return cause{
		cause: "The connection to the other Mac did not complete.",
		fix:   "Try again. If it keeps failing, run `ibeji doctor` on both machines — the log below carries the exact error.",
	}
}
