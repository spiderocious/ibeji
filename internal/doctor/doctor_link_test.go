package doctor

import (
	"context"
	"strings"
	"testing"
)

// Doctor used to print two contradictory causes in one report: "macOS is
// blocking Ìbejì from this network", and three lines later "usually this just
// means the other machine is asleep — check it is awake and on the same
// Wi-Fi". The second is the advice that cost a day of debugging a machine that
// was never at fault.
//
// These assert the wording, because the wording is the product here: a check
// that is right but reads as "go look at the other Mac" does the same damage
// as one that is wrong.

func TestLinkDoesNotBlameThePeerForALocalProblem(t *testing.T) {
	// Runs on a real machine, so the LAN verdict is whatever this machine's
	// network actually is. Assert the invariant that holds either way: the
	// advice must never simultaneously claim the network is blocked here and
	// tell the reader to go check the other machine.
	c := Link(fakeLink{paired: true, keyOK: true, up: false, name: "OTHER-MAC"})(context.Background())

	// A warning, not a failure: an asleep peer is ordinary on a LAN. The
	// severity is the point, not OK.
	if c.Severity != Warn {
		t.Fatalf("an unreachable peer should warn, got severity %v", c.Severity)
	}
	if !strings.Contains(c.Detail, "OTHER-MAC") {
		t.Errorf("the check should name the peer, got %q", c.Detail)
	}

	blocked, _ := lanBlocked(context.Background())
	if blocked {
		// The permission is the cause, so the fix must point here, not there.
		if !strings.Contains(c.Fix, "not the other machine") {
			t.Errorf("with the LAN blocked, the fix must say the peer is not the cause, got %q", c.Fix)
		}
		if strings.Contains(c.Fix, "asleep") {
			t.Errorf("with the LAN blocked, the fix must not send the reader to the other Mac, got %q", c.Fix)
		}
	} else {
		// No local denial, so the ordinary explanation is right — and should
		// mention the address change, which is what actually happened.
		if !strings.Contains(c.Fix, "asleep") {
			t.Errorf("without a local denial, the ordinary cause should lead, got %q", c.Fix)
		}
		if !strings.Contains(c.Fix, "sweep") {
			t.Errorf("a moved peer is a likely cause and sweep finds it, got %q", c.Fix)
		}
	}
}

// A key mismatch is never transient and must never be softened into a
// network-ish warning, whatever the LAN is doing.
func TestLinkKeyMismatchIsStillFatal(t *testing.T) {
	c := Link(fakeLink{paired: true, keyOK: false, up: false, name: "OTHER-MAC"})(context.Background())
	if c.OK {
		t.Fatal("a key mismatch must not pass")
	}
	if c.Severity != Fail {
		t.Errorf("a key mismatch must be a failure, got severity %v", c.Severity)
	}
	if strings.Contains(c.Fix, "asleep") || strings.Contains(c.Fix, "not the other machine") {
		t.Errorf("a key mismatch must not be explained as a network problem, got %q", c.Fix)
	}
}
