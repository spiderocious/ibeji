package device

import (
	"errors"
	"strings"
	"testing"
)

// The timeline exists to tell apart failures that previously collapsed into
// one sentence. These tests assert that distinction directly, because it is
// the whole product value of the file: a timeout and a refusal must not
// produce the same advice, and the timeout must offer the sweep, since a
// moved peer is the case the daemon can resolve by itself.

func TestDiagnoseDistinguishesTimeoutFromRefusal(t *testing.T) {
	timeout := diagnosePair("connect", "192.168.1.147:7749",
		errors.New("dial tcp 192.168.1.147:7749: i/o timeout"))
	refused := diagnosePair("connect", "192.168.1.39:7749",
		errors.New("dial tcp 192.168.1.39:7749: connect: connection refused"))

	if timeout.cause == refused.cause {
		t.Fatal("a timeout and a refusal produced the same cause; they are different problems")
	}
	if !timeout.scan {
		t.Error("a timeout should offer the subnet sweep: a moved peer is the likeliest reason and the one we can fix")
	}
	if refused.scan {
		t.Error("a refusal means the machine is there, so sweeping the subnet is not the answer")
	}
	// The address belongs in the sentence: "nothing answered" without saying
	// where is what made the old message useless.
	if !strings.Contains(timeout.cause, "192.168.1.147:7749") {
		t.Errorf("the timeout cause should name the address that went unanswered, got %q", timeout.cause)
	}
	if !strings.Contains(strings.ToLower(refused.fix), "doctor") {
		t.Errorf("a refusal should point at the other Mac's daemon, got %q", refused.fix)
	}
}

func TestDiagnoseNamesThePermissionBlock(t *testing.T) {
	// "no route to host" is what a denied Local Network permission looks
	// like, and it is indistinguishable from a dead network from this side.
	// Saying so is the only honest answer.
	c := diagnosePair("connect", "192.168.1.39:7749",
		errors.New("dial tcp 192.168.1.39:7749: connect: no route to host"))
	if !strings.Contains(c.fix, "Local Network") {
		t.Errorf("a no-route error should send the reader to Local Network settings, got %q", c.fix)
	}
	if !strings.Contains(strings.ToLower(c.cause), "macos") {
		t.Errorf("the cause should name macOS as the blocker, got %q", c.cause)
	}
}

func TestDiagnoseTLSMeansSomethingElseHoldsThePort(t *testing.T) {
	c := diagnosePair("tls", "192.168.1.39:7749",
		errors.New("tls: first record does not look like a TLS handshake"))
	if c.scan {
		t.Error("a TLS failure means something answered; sweeping is not the fix")
	}
	if !strings.Contains(strings.ToLower(c.cause), "not as") &&
		!strings.Contains(strings.ToLower(c.cause), "not ìbejì") {
		t.Errorf("the cause should say the thing answering is not Ìbejì, got %q", c.cause)
	}
}

func TestDiagnoseAlwaysGivesACauseAndAFix(t *testing.T) {
	// An unrecognised error must still produce something actionable. A blank
	// cause on the timeline is the old failure mode wearing a new coat.
	for _, step := range []string{"ready", "address", "connect", "tls", "key", "name", "code", "confirm"} {
		c := diagnosePair(step, "10.0.0.2:7749", errors.New("something nobody has seen before"))
		if c.cause == "" {
			t.Errorf("step %q: no cause for an unknown error", step)
		}
		if c.fix == "" {
			t.Errorf("step %q: no fix for an unknown error", step)
		}
	}
}

func TestAttemptMarksTheFailingStepAndSkipsTheRest(t *testing.T) {
	a := newPairAttempt("attempt-1", "192.168.1.147:7749")
	a.ok("ready", "not paired yet")
	a.ok("address", "192.168.1.147:7749")
	a.begin("connect")
	a.fail("connect", errors.New("i/o timeout"))

	v := a.View()
	if v.Failed != "connect" {
		t.Fatalf("expected the connect step to be flagged, got %q", v.Failed)
	}
	if v.OK {
		t.Error("an attempt that failed should not report OK")
	}
	if !v.Done {
		t.Error("a failed attempt is finished")
	}

	byKey := map[string]PairStep{}
	for _, s := range v.Steps {
		byKey[s.Key] = s
	}
	if byKey["address"].State != StepOK {
		t.Errorf("the address step succeeded, got %q", byKey["address"].State)
	}
	if byKey["connect"].State != StepFailed {
		t.Errorf("the connect step failed, got %q", byKey["connect"].State)
	}
	// Everything downstream was never attempted. Reporting those as pending
	// would leave a timeline that looks like it is still working.
	for _, k := range []string{"tls", "key", "name", "code", "confirm"} {
		if byKey[k].State != StepSkipped {
			t.Errorf("step %q should be skipped after an earlier failure, got %q", k, byKey[k].State)
		}
	}
	if byKey["connect"].Cause == "" {
		t.Error("the failing step should carry a cause")
	}
	if !byKey["connect"].Scan {
		t.Error("a timeout on connect should offer the sweep")
	}
}

func TestAttemptKeepsItsOwnLog(t *testing.T) {
	a := newPairAttempt("attempt-1", "192.168.1.39:7749")
	a.logf("pairing with %s", "192.168.1.39:7749")
	a.begin("connect")
	a.ok("connect", "reached it")
	a.fail("key", errors.New("no key presented"))

	v := a.View()
	if len(v.Log) == 0 {
		t.Fatal("the attempt recorded no log; the Copy button would hand over nothing")
	}
	joined := strings.Join(v.Log, "\n")
	// The log is what a person pastes into a bug report, so the error and the
	// diagnosis both have to be in it — not only on screen.
	if !strings.Contains(joined, "no key presented") {
		t.Error("the log should carry the underlying error verbatim")
	}
	if !strings.Contains(joined, "cause:") {
		t.Error("the log should carry the diagnosis, since that is what gets pasted elsewhere")
	}
}

func TestAttemptWarnDoesNotFailTheCeremony(t *testing.T) {
	// A peer that will not say its name is still pairable: the fingerprint
	// beside the code carries the assurance. Treating that as fatal would
	// refuse a perfectly safe pairing.
	a := newPairAttempt("attempt-1", "192.168.1.39:7749")
	for _, k := range []string{"ready", "address", "connect", "tls", "key"} {
		a.ok(k, "fine")
	}
	a.warn("name", "it would not say", errors.New("hello timed out"))
	a.ok("code", "983620")
	a.finish(true, "paired with MONB-1375-LP")

	v := a.View()
	if v.Failed != "" {
		t.Errorf("a name warning must not flag the attempt as failed, got %q", v.Failed)
	}
	if !v.OK {
		t.Error("every step resolved, so the attempt is OK")
	}
}

func TestNormalizePairAddress(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		// The common case: a person types an IP and nothing else.
		{in: "192.168.1.39", want: "192.168.1.39:7749"},
		{in: "192.168.1.39:7749", want: "192.168.1.39:7749"},
		{in: "  192.168.1.39:7749  ", want: "192.168.1.39:7749"},
		// Pasted out of a browser, which people do.
		{in: "http://192.168.1.39:7749/", want: "192.168.1.39:7749"},
		{in: "other.local", want: "other.local:7749"},
		{in: "", wantErr: true},
		{in: "192.168.1.39:0", wantErr: true},
		{in: "192.168.1.39:notaport", wantErr: true},
		{in: "192.168.1.39:99999", wantErr: true},
		// A sentence typed into the address box used to sail through and fail
		// fifteen seconds later as a DNS lookup, which reads as a network
		// fault rather than a typo. The e2e run caught it.
		{in: "not an address at all", wantErr: true},
		{in: "192.168.1.39 :7749", wantErr: true},
		{in: ".local", wantErr: true},
		{in: "other..local", wantErr: true},
	} {
		got, err := normalizePairAddress(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizePairAddress(%q) should have failed, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizePairAddress(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizePairAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestConnectOrTLSPlacesTheFailure(t *testing.T) {
	// Dial covers reaching the machine and shaking hands with it. Putting the
	// failure on the right step is what tells "nothing is there" apart from
	// "something is there and it is not us".
	if got := connectOrTLS(errors.New("dial tcp: i/o timeout")); got != "connect" {
		t.Errorf("a timeout belongs to connect, got %q", got)
	}
	if got := connectOrTLS(errors.New("remote error: tls: bad certificate")); got != "tls" {
		t.Errorf("a TLS error belongs to tls, got %q", got)
	}
	if got := connectOrTLS(nil); got != "connect" {
		t.Errorf("no error defaults to connect, got %q", got)
	}
}

func TestDiagnoseAlreadyPairedIsNotAnAddressProblem(t *testing.T) {
	// The e2e run caught this: an "already paired" refusal was reported as a
	// malformed address, which is nonsense and sends the reader to check
	// something that is perfectly fine. It gets its own step.
	c := diagnosePair("ready", "192.168.1.39:7749", errors.New("identity: already paired"))
	if strings.Contains(strings.ToLower(c.cause), "address") {
		t.Errorf("an already-paired refusal must not blame the address, got %q", c.cause)
	}
	if !strings.Contains(strings.ToLower(c.cause), "already paired") {
		t.Errorf("the cause should say this Mac is already paired, got %q", c.cause)
	}
	// Reconnect is the right answer when the peer merely moved, and it is the
	// thing the user could not find when their peer changed address.
	if !strings.Contains(c.fix, "Reconnect") {
		t.Errorf("the fix should mention Reconnect for a peer that moved, got %q", c.fix)
	}
}
