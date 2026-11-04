package device

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// These tests drive pairing the way the product does: one daemon calls
// BeginPairing against another daemon's real listener, over real TCP.
//
// They exist because the ceremony shipped broken in v0.1.3 while every
// pairing test passed. The older helpers call Pairer.Begin on *both* daemons
// by hand, which is a step the network path never performed — the receiving
// daemon answered the name exchange and raised no session, so it displayed
// nothing and the ceremony could not be completed from either end. A test
// that stages both halves itself cannot see that, so these drive one side
// only and let the other side react.

// serveDaemon starts a daemon's peer listener on loopback and returns its
// address, exactly as cmd/ibeji-daemon does.
func serveDaemon(t *testing.T, d *Daemon) string {
	t.Helper()

	transport := &link.TCPTransport{
		Identity:       d.Store().Self(),
		Name:           d.Store().Name(),
		CurrentKey:     d.CurrentPeerKey,
		AcceptUnpaired: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ln, err := transport.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for %s: %v", d.Store().Name(), err)
	}
	t.Cleanup(func() { ln.Close() })

	d.SetListenAddr(ln.Addr().String())
	go d.ServeInbound(ctx, ln)

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split %q: %v", ln.Addr(), err)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// waitForPending polls for the ceremony the daemon is showing, the way the
// window does.
func waitForPending(t *testing.T, d *Daemon, within time.Duration) *link.PairingSession {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if v := d.PendingPairing(); v != nil {
			s, err := d.Pairer().Session(v.ID)
			if err == nil {
				return s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// TestPairingOverTheNetworkShowsACodeOnBothMachines is the regression test for
// the v0.1.3 bug. Only the laptop is told to pair; the desktop must raise its
// own ceremony in response, showing the same six digits.
func TestPairingOverTheNetworkShowsACodeOnBothMachines(t *testing.T) {
	laptop := newDaemon(t, "Laptop")
	desktop := newDaemon(t, "Desktop")

	serveDaemon(t, laptop)
	desktopAddr := serveDaemon(t, desktop)

	// Exactly what typing an IP into the window does, and nothing else.
	view, err := laptop.BeginPairing(desktopAddr)
	if err != nil {
		t.Fatalf("laptop.BeginPairing(%s): %v", desktopAddr, err)
	}

	// The dialling side names its peer rather than showing a bare address.
	if view.PeerName != "Desktop" {
		t.Errorf("laptop shows peer %q, want %q", view.PeerName, "Desktop")
	}

	// The whole point: the other machine must be showing a ceremony too.
	got := waitForPending(t, desktop, 5*time.Second)
	if got == nil {
		t.Fatal("the desktop shows no pairing ceremony: the machine that was " +
			"dialled never raised its own session, so there is nothing for the " +
			"user to compare the six digits against")
	}

	if got.Code != view.Code {
		t.Fatalf("codes differ: laptop %q, desktop %q", view.Code, got.Code)
	}
	if got.Peer.DeviceName != "Laptop" {
		t.Errorf("desktop shows peer %q, want %q", got.Peer.DeviceName, "Laptop")
	}

	// And both sides can complete, which is what the user clicking confirm on
	// each machine does.
	if _, err := laptop.ConfirmPairing(view.ID); err != nil {
		t.Fatalf("laptop.ConfirmPairing: %v", err)
	}
	if _, err := desktop.ConfirmPairing(got.ID); err != nil {
		t.Fatalf("desktop.ConfirmPairing: %v", err)
	}

	if !laptop.Store().Paired() {
		t.Error("the laptop is not paired after confirming")
	}
	if !desktop.Store().Paired() {
		t.Error("the desktop is not paired after confirming")
	}

	// Each must have pinned the other's actual key, not merely recorded a name.
	lp := laptop.Store().Peer()
	if lp == nil {
		t.Fatal("the laptop pinned no peer")
	} else if !lp.PublicKey.Equal(desktop.Store().Self().Public()) {
		t.Error("the laptop pinned a key that is not the desktop's")
	}
	dp := desktop.Store().Peer()
	if dp == nil {
		t.Fatal("the desktop pinned no peer")
	} else if !dp.PublicKey.Equal(laptop.Store().Self().Public()) {
		t.Error("the desktop pinned a key that is not the laptop's")
	}
}

// TestInboundPairingPinsADialableAddress is the regression test for the
// ephemeral-port bug.
//
// The accepting side sees the peer's *source* port on the connection, which is
// a random high port the kernel picked for that one dial — nothing is bound to
// it. Pinning that made the link come up only when the other machine happened
// to dial first, so pairing looked fine and then the devices showed each other
// as unreachable about a third of the time.
func TestInboundPairingPinsADialableAddress(t *testing.T) {
	laptop := newDaemon(t, "Laptop")
	desktop := newDaemon(t, "Desktop")

	laptopAddr := serveDaemon(t, laptop)
	desktopAddr := serveDaemon(t, desktop)

	view, err := laptop.BeginPairing(desktopAddr)
	if err != nil {
		t.Fatalf("BeginPairing: %v", err)
	}
	got := waitForPending(t, desktop, 5*time.Second)
	if got == nil {
		t.Fatal("the desktop raised no ceremony")
	}
	if _, err := laptop.ConfirmPairing(view.ID); err != nil {
		t.Fatalf("laptop confirm: %v", err)
	}
	if _, err := desktop.ConfirmPairing(got.ID); err != nil {
		t.Fatalf("desktop confirm: %v", err)
	}

	// What the desktop pinned must be the laptop's listener, not whatever
	// source port that dial happened to use.
	peer := desktop.Store().Peer()
	if peer == nil {
		t.Fatal("the desktop pinned no peer")
	}
	if peer.LastAddress != laptopAddr {
		t.Fatalf("the desktop pinned %q, want the laptop's listener %q",
			peer.LastAddress, laptopAddr)
	}

	// And it must actually be dialable, which is the property that matters.
	conn, err := net.DialTimeout("tcp", peer.LastAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("the pinned address %q is not dialable: %v", peer.LastAddress, err)
	}
	conn.Close()
}

// TestInboundPairingRaisesOneSessionPerPeer guards the retry loop: the dialler
// attempts the name exchange up to five times, and each attempt opens a
// control stream. One ceremony must result, not five.
func TestInboundPairingRaisesOneSessionPerPeer(t *testing.T) {
	laptop := newDaemon(t, "Laptop")
	desktop := newDaemon(t, "Desktop")

	serveDaemon(t, laptop)
	desktopAddr := serveDaemon(t, desktop)

	if _, err := laptop.BeginPairing(desktopAddr); err != nil {
		t.Fatalf("BeginPairing: %v", err)
	}
	if waitForPending(t, desktop, 5*time.Second) == nil {
		t.Fatal("the desktop raised no ceremony")
	}
	// Let any further control streams land.
	time.Sleep(300 * time.Millisecond)

	if n := desktop.Pairer().Active(); n != 1 {
		t.Errorf("the desktop has %d ceremonies in flight, want 1", n)
	}
}

// TestPairingFromEitherDirectionAgrees checks the sorted-key property over the
// wire: it must not matter which machine the user starts from.
func TestPairingFromEitherDirectionAgrees(t *testing.T) {
	laptop := newDaemon(t, "Laptop")
	desktop := newDaemon(t, "Desktop")

	laptopAddr := serveDaemon(t, laptop)
	serveDaemon(t, desktop)

	// Desktop dials this time, the reverse of the first test.
	view, err := desktop.BeginPairing(laptopAddr)
	if err != nil {
		t.Fatalf("desktop.BeginPairing(%s): %v", laptopAddr, err)
	}
	got := waitForPending(t, laptop, 5*time.Second)
	if got == nil {
		t.Fatal("the laptop shows no ceremony when the desktop dials")
	}
	if got.Code != view.Code {
		t.Fatalf("codes differ when dialled in reverse: desktop %q, laptop %q",
			view.Code, got.Code)
	}
}

// TestInboundPairingIgnoredOnceAlreadyPaired makes sure a stranger connecting
// to a paired machine cannot raise a ceremony on its screen.
func TestInboundPairingIgnoredOnceAlreadyPaired(t *testing.T) {
	laptop := newDaemon(t, "Laptop")
	desktop := newDaemon(t, "Desktop")
	pairDaemons(t, laptop, desktop)

	stranger := newDaemon(t, "Stranger")
	serveDaemon(t, stranger)
	desktopAddr := serveDaemon(t, desktop)

	// The stranger tries. The desktop is already paired, so it must refuse to
	// put anything on screen.
	_, _ = stranger.BeginPairing(desktopAddr)
	time.Sleep(500 * time.Millisecond)

	if n := desktop.Pairer().Active(); n != 0 {
		t.Errorf("a paired desktop raised %d ceremonies for a stranger, want 0", n)
	}
	if v := desktop.PendingPairing(); v != nil {
		t.Errorf("a paired desktop shows a ceremony for %q", v.PeerName)
	}
}
