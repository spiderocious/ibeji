package device

import (
	"errors"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

// Reconnecting is the answer to a peer that moved. Until it existed, the only
// action on an unreachable device was Unpair — so a user working around a
// network problem threw away a working pairing, which is exactly what
// happened in the field.

func TestReconnectRefusesWhenUnpaired(t *testing.T) {
	d := newDaemon(t, "Laptop")
	if _, err := d.ReconnectAt("127.0.0.1:1"); !errors.Is(err, identity.ErrNotPaired) {
		t.Fatalf("ReconnectAt while unpaired = %v, want ErrNotPaired", err)
	}
}

func TestReconnectFindsAPeerThatMoved(t *testing.T) {
	laptop := newDaemon(t, "Laptop")
	desktop := newDaemon(t, "Desktop")

	serveDaemon(t, laptop)
	first := serveDaemon(t, desktop)

	view, err := laptop.BeginPairing(first)
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

	// The desktop moves: a second listener, as a new address would be.
	moved := serveDaemon(t, desktop)
	if moved == first {
		t.Skip("the test harness reused the port, so there is nothing to move to")
	}

	dev, err := laptop.ReconnectAt(moved)
	if err != nil {
		t.Fatalf("ReconnectAt(%s): %v", moved, err)
	}
	if dev == nil {
		t.Fatal("ReconnectAt returned no device")
	}
	// The pin must be unchanged — reconnecting is not re-pairing.
	peer := laptop.Store().Peer()
	if peer == nil {
		t.Fatal("the laptop lost its peer while reconnecting")
	}
	if !peer.PublicKey.Equal(desktop.Store().Self().Public()) {
		t.Error("the pinned key changed during a reconnect")
	}
	if peer.LastAddress != moved {
		t.Errorf("LastAddress = %q, want the new address %q", peer.LastAddress, moved)
	}
}

func TestReconnectRefusesAStranger(t *testing.T) {
	laptop := newDaemon(t, "Laptop")
	desktop := newDaemon(t, "Desktop")
	stranger := newDaemon(t, "Stranger")

	serveDaemon(t, laptop)
	serveDaemon(t, desktop)
	strangerAddr := serveDaemon(t, stranger)
	pairDaemons(t, laptop, desktop)

	// A different machine at that address must be refused: this is the one
	// case that genuinely needs a new ceremony.
	_, err := laptop.ReconnectAt(strangerAddr)
	if err == nil {
		t.Fatal("reconnecting to a stranger succeeded; the pinned key must gate it")
	}
	if !errors.Is(err, identity.ErrKeyMismatch) {
		t.Fatalf("got %v, want a key mismatch", err)
	}
	// And the real peer must still be pinned.
	if peer := laptop.Store().Peer(); peer == nil ||
		!peer.PublicKey.Equal(desktop.Store().Self().Public()) {
		t.Error("a refused reconnect disturbed the pinned peer")
	}
}
