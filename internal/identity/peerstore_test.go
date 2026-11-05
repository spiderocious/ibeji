package identity

import (
	"errors"
	"testing"
)

// These tests exist because every other pairing test in the repo shares one
// long-lived Store, so none of them could see that Pin wrote to memory and
// nowhere else. The product lost its pairing on the first daemon restart and
// the whole suite stayed green.
//
// The shape that catches it: pin, THROW THE STORE AWAY, build a new one from
// the same secret store, and assert the peer came back.

func newSelf(t *testing.T) *Identity {
	t.Helper()
	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id
}

func TestPinSurvivesARestart(t *testing.T) {
	ks := NewMemoryStore()
	self := newSelf(t)
	peerKey := newSelf(t).Public()

	first, err := LoadStore(self, "Laptop", ks)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if _, err := first.Pin(peerKey, "Desktop", "192.168.1.39:7749"); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	// The daemon exits here. Nothing of `first` survives.
	second, err := LoadStore(self, "Laptop", ks)
	if err != nil {
		t.Fatalf("LoadStore after restart: %v", err)
	}

	if !second.Paired() {
		t.Fatal("the pairing did not survive a restart: the daemon would come back unpaired " +
			"and the user would find their machines disconnected with nothing to explain it")
	}
	got := second.Peer()
	if got == nil {
		t.Fatal("Paired() is true but Peer() is nil")
	}
	if !got.PublicKey.Equal(peerKey) {
		t.Error("the restored peer has a different key than the one pinned")
	}
	if got.Name != "Desktop" {
		t.Errorf("restored name = %q, want Desktop", got.Name)
	}
	if got.LastAddress != "192.168.1.39:7749" {
		t.Errorf("restored address = %q", got.LastAddress)
	}
	if got.PairedAt.IsZero() {
		t.Error("restored peer has no PairedAt")
	}

	// And the restored pin must actually gate connections, not merely display.
	if err := second.Verify(peerKey); err != nil {
		t.Errorf("Verify with the pinned key after restart: %v", err)
	}
	if err := second.Verify(newSelf(t).Public()); !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("Verify with a stranger's key = %v, want ErrKeyMismatch", err)
	}
}

func TestUnpairSurvivesARestart(t *testing.T) {
	ks := NewMemoryStore()
	self := newSelf(t)
	peerKey := newSelf(t).Public()

	first, _ := LoadStore(self, "Laptop", ks)
	if _, err := first.Pin(peerKey, "Desktop", "192.168.1.39:7749"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := first.Unpair(); err != nil {
		t.Fatalf("Unpair: %v", err)
	}

	second, err := LoadStore(self, "Laptop", ks)
	if err != nil {
		t.Fatalf("LoadStore after restart: %v", err)
	}
	if second.Paired() {
		t.Fatal("a revoked device came back after a restart — unpairing must be permanent")
	}
}

// Re-pinning the same key is idempotent, and the refreshed name and address
// must persist too: the peer renames itself and DHCP moves it.
func TestRepinUpdatesAndPersists(t *testing.T) {
	ks := NewMemoryStore()
	self := newSelf(t)
	peerKey := newSelf(t).Public()

	first, _ := LoadStore(self, "Laptop", ks)
	if _, err := first.Pin(peerKey, "Desktop", "192.168.1.39:7749"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if _, err := first.Pin(peerKey, "Studio", "192.168.1.147:7749"); err != nil {
		t.Fatalf("re-Pin the same key: %v", err)
	}

	second, _ := LoadStore(self, "Laptop", ks)
	got := second.Peer()
	if got == nil {
		t.Fatal("no peer after restart")
	}
	if got.Name != "Studio" || got.LastAddress != "192.168.1.147:7749" {
		t.Errorf("restored peer = %q at %q, want Studio at 192.168.1.147:7749",
			got.Name, got.LastAddress)
	}
}

// A different key must still be refused, and refusing must not disturb what
// is already stored.
func TestPinRefusesASecondDeviceAndKeepsTheFirst(t *testing.T) {
	ks := NewMemoryStore()
	self := newSelf(t)
	first := newSelf(t).Public()
	stranger := newSelf(t).Public()

	store, _ := LoadStore(self, "Laptop", ks)
	if _, err := store.Pin(first, "Desktop", ""); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if _, err := store.Pin(stranger, "Stranger", ""); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("pinning a second device = %v, want ErrAlreadyPaired", err)
	}

	after, _ := LoadStore(self, "Laptop", ks)
	got := after.Peer()
	if got == nil || !got.PublicKey.Equal(first) {
		t.Error("a refused second pairing disturbed the stored first one")
	}
}

// A store with no secret store still works — tests use it — it just does not
// persist. This asserts it does not panic or error.
func TestStoreWithoutSecretsStillPins(t *testing.T) {
	store := NewStore(newSelf(t), "Laptop")
	if _, err := store.Pin(newSelf(t).Public(), "Desktop", ""); err != nil {
		t.Fatalf("Pin on a store with no secrets: %v", err)
	}
	if !store.Paired() {
		t.Error("not paired after Pin")
	}
}

// Corruption must be reported, not silently treated as "unpaired" — those
// two states need different responses from the user.
func TestCorruptStoredPairingIsReported(t *testing.T) {
	ks := NewMemoryStore()
	if err := ks.Set(accountPeer, []byte("{not json")); err != nil {
		t.Fatalf("seed corruption: %v", err)
	}
	if _, err := LoadStore(newSelf(t), "Laptop", ks); err == nil {
		t.Fatal("a corrupt stored pairing loaded without error; the daemon would " +
			"start unpaired and look like it had forgotten the other machine")
	}
}
