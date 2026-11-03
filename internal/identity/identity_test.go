package identity

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
)

func TestGenerateAndRoundTripSeed(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(id.Public()) != PublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", len(id.Public()), PublicKeySize)
	}

	again, err := FromSeed(id.Seed())
	if err != nil {
		t.Fatalf("FromSeed: %v", err)
	}
	if !id.Public().Equal(again.Public()) {
		t.Fatal("identity reconstructed from its own seed has a different public key")
	}
}

func TestFromSeedRejectsWrongLength(t *testing.T) {
	for _, n := range []int{0, 1, 31, 33, 64} {
		if _, err := FromSeed(make([]byte, n)); err == nil {
			t.Fatalf("FromSeed accepted a %d-byte seed; it must not", n)
		}
	}
}

func TestSignVerify(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	msg := []byte("418207")
	sig := id.Sign(msg)

	if !Verify(id.Public(), msg, sig) {
		t.Fatal("a signature from this identity did not verify against its own public key")
	}
	if Verify(id.Public(), []byte("418208"), sig) {
		t.Fatal("a signature verified against a different message")
	}

	other, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if Verify(other.Public(), msg, sig) {
		t.Fatal("a signature verified against an unrelated public key")
	}
}

func TestVerifyRejectsMalformedKey(t *testing.T) {
	id, _ := Generate()
	sig := id.Sign([]byte("x"))
	if Verify(ed25519.PublicKey("short"), []byte("x"), sig) {
		t.Fatal("Verify accepted a malformed public key")
	}
}

func TestFingerprintIsStableAndComparable(t *testing.T) {
	id, _ := Generate()
	a, b := id.Fingerprint(), Fingerprint(id.Public())
	if a != b {
		t.Fatalf("fingerprint is unstable: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "SHA256:") {
		t.Fatalf("fingerprint %q lacks the SHA256: prefix the UI expects", a)
	}
	// Grouped in fours so it can be compared by eye during a key-mismatch
	// confirmation; 8 groups of 4 hex chars plus 7 spaces.
	body := strings.TrimPrefix(a, "SHA256:")
	groups := strings.Split(body, " ")
	if len(groups) != 8 {
		t.Fatalf("fingerprint has %d groups, want 8: %q", len(groups), a)
	}
	for _, g := range groups {
		if len(g) != 4 {
			t.Fatalf("fingerprint group %q is not 4 chars: %q", g, a)
		}
	}

	other, _ := Generate()
	if other.Fingerprint() == a {
		t.Fatal("two independently generated identities share a fingerprint")
	}
}

func TestEncodeDecodePublic(t *testing.T) {
	id, _ := Generate()
	enc := EncodePublic(id.Public())
	got, err := DecodePublic(enc)
	if err != nil {
		t.Fatalf("DecodePublic: %v", err)
	}
	if !got.Equal(id.Public()) {
		t.Fatal("public key did not survive encode/decode")
	}
	// mDNS TXT records arrive with whitespace sometimes; tolerate it.
	if _, err := DecodePublic("  " + enc + "\n"); err != nil {
		t.Fatalf("DecodePublic rejected a padded value: %v", err)
	}
	for _, bad := range []string{"", "!!!!", EncodePublic(id.Public())[:10]} {
		if _, err := DecodePublic(bad); err == nil {
			t.Fatalf("DecodePublic accepted %q", bad)
		}
	}
}

// --- Store: the pinning contract -------------------------------------------

func newStore(t *testing.T) (*Store, *Identity) {
	t.Helper()
	self, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return NewStore(self, "Laptop"), self
}

func TestStoreStartsUnpaired(t *testing.T) {
	s, _ := newStore(t)
	if s.Paired() {
		t.Fatal("a fresh store reports itself as paired")
	}
	if s.Peer() != nil {
		t.Fatal("a fresh store returned a peer")
	}
	if err := s.Verify(make(ed25519.PublicKey, PublicKeySize)); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Verify on an unpaired store = %v, want ErrNotPaired", err)
	}
	if err := s.Unpair(); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Unpair on an unpaired store = %v, want ErrNotPaired", err)
	}
}

func TestPinThenVerify(t *testing.T) {
	s, _ := newStore(t)
	peerID, _ := Generate()

	p, err := s.Pin(peerID.Public(), "Desktop", "192.168.1.31")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if p.Name != "Desktop" || p.LastAddress != "192.168.1.31" {
		t.Fatalf("Pin returned %+v, want name/address as given", p)
	}
	if p.PairedAt.IsZero() {
		t.Fatal("Pin did not record PairedAt")
	}
	if !s.Paired() {
		t.Fatal("store does not report itself paired after Pin")
	}
	if err := s.Verify(peerID.Public()); err != nil {
		t.Fatalf("Verify of the pinned key failed: %v", err)
	}
}

// This is the contract that matters most in the whole package: a key that is
// not the pinned key must be rejected, and rejected with a distinguishable
// error so the caller can show a re-pair prompt rather than a generic failure.
func TestVerifyRejectsMismatchedKey(t *testing.T) {
	s, _ := newStore(t)
	paired, _ := Generate()
	impostor, _ := Generate()

	if _, err := s.Pin(paired.Public(), "Desktop", "192.168.1.31"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	err := s.Verify(impostor.Public())
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Verify of an unpinned key = %v, want ErrKeyMismatch", err)
	}
}

// A reinstalled peer presents the same name and address with a new key. The
// product must not treat that as the same device.
func TestReinstalledPeerIsAMismatchNotATrust(t *testing.T) {
	s, _ := newStore(t)
	before, _ := Generate()
	after, _ := Generate() // same machine, fresh install, new identity

	if _, err := s.Pin(before.Public(), "Desktop", "192.168.1.31"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := s.Verify(after.Public()); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("a reinstalled peer verified as trusted (err=%v); it must be a mismatch", err)
	}
	// And pinning over the top must be refused: replacing a trusted key has to
	// go through an explicit Unpair so it is always separately confirmed.
	if _, err := s.Pin(after.Public(), "Desktop", "192.168.1.31"); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("Pin over an existing different key = %v, want ErrAlreadyPaired", err)
	}
}

// v1 is exactly two devices. A second, different peer must not pin.
func TestPinSecondPeerIsRefused(t *testing.T) {
	s, _ := newStore(t)
	first, _ := Generate()
	second, _ := Generate()

	if _, err := s.Pin(first.Public(), "Desktop", ""); err != nil {
		t.Fatalf("Pin first: %v", err)
	}
	if _, err := s.Pin(second.Public(), "Studio Mac", ""); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("Pin second peer = %v, want ErrAlreadyPaired", err)
	}
}

// Re-pinning the identical key is idempotent, so a retried confirmation during
// pairing does not fail confusingly.
func TestPinIsIdempotentForTheSameKey(t *testing.T) {
	s, _ := newStore(t)
	peerID, _ := Generate()

	if _, err := s.Pin(peerID.Public(), "Desktop", "192.168.1.31"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	p, err := s.Pin(peerID.Public(), "Desktop-renamed", "192.168.1.40")
	if err != nil {
		t.Fatalf("re-Pin of the same key failed: %v", err)
	}
	if p.Name != "Desktop-renamed" || p.LastAddress != "192.168.1.40" {
		t.Fatalf("re-Pin did not refresh name/address: %+v", p)
	}
}

func TestPinRejectsMalformedKey(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Pin(ed25519.PublicKey("too short"), "Desktop", ""); err == nil {
		t.Fatal("Pin accepted a malformed public key")
	}
}

func TestUnpairThenRepair(t *testing.T) {
	s, _ := newStore(t)
	first, _ := Generate()
	second, _ := Generate()

	if _, err := s.Pin(first.Public(), "Desktop", ""); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := s.Unpair(); err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	if s.Paired() {
		t.Fatal("store still reports paired after Unpair")
	}
	if err := s.Verify(first.Public()); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Verify after Unpair = %v, want ErrNotPaired", err)
	}
	// After unpairing, a different device may pin — that is the recovery path
	// for a genuine reinstall.
	if _, err := s.Pin(second.Public(), "Desktop", ""); err != nil {
		t.Fatalf("Pin after Unpair: %v", err)
	}
}

func TestPeerIsReturnedByValue(t *testing.T) {
	s, _ := newStore(t)
	peerID, _ := Generate()
	if _, err := s.Pin(peerID.Public(), "Desktop", "192.168.1.31"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	got := s.Peer()
	got.Name = "mutated by caller"
	if s.Peer().Name != "Desktop" {
		t.Fatal("mutating the returned Peer changed the store's copy")
	}
}

func TestRenameAndPeerMetadata(t *testing.T) {
	s, _ := newStore(t)
	if s.Name() != "Laptop" {
		t.Fatalf("Name = %q, want Laptop", s.Name())
	}
	s.SetName("Laptop 16")
	if s.Name() != "Laptop 16" {
		t.Fatalf("SetName did not take: %q", s.Name())
	}

	peerID, _ := Generate()
	if _, err := s.Pin(peerID.Public(), "Desktop", "192.168.1.31"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	s.SetPeerName("Mac Studio")
	s.SetPeerAddress("192.168.1.44")
	s.Touch()
	p := s.Peer()
	if p.Name != "Mac Studio" || p.LastAddress != "192.168.1.44" {
		t.Fatalf("peer metadata not updated: %+v", p)
	}
	if p.LastSeen.IsZero() {
		t.Fatal("Touch did not record LastSeen")
	}
	// Empty updates must not blank existing values — the link layer calls these
	// opportunistically and may not know a name or address yet.
	s.SetPeerName("")
	s.SetPeerAddress("")
	p = s.Peer()
	if p.Name != "Mac Studio" || p.LastAddress != "192.168.1.44" {
		t.Fatalf("an empty update blanked peer metadata: %+v", p)
	}
}

// --- loader -----------------------------------------------------------------

func TestLoadGeneratesOnceThenReuses(t *testing.T) {
	ks := NewMemoryStore()

	first, err := Load(ks)
	if err != nil {
		t.Fatalf("Load (first run): %v", err)
	}
	second, err := Load(ks)
	if err != nil {
		t.Fatalf("Load (second run): %v", err)
	}
	if !first.Public().Equal(second.Public()) {
		t.Fatal("Load generated a new identity on the second call; the device would appear to have been replaced")
	}
}

func TestLoadRejectsCorruptSeed(t *testing.T) {
	ks := NewMemoryStore()
	if err := ks.Set(accountSeed, []byte("not a 32 byte seed")); err != nil {
		t.Fatalf("seed the store: %v", err)
	}
	if _, err := Load(ks); err == nil {
		t.Fatal("Load silently accepted a corrupt seed; it must fail loudly rather than regenerate")
	}
}

func TestAPITokenGeneratesOnceThenReuses(t *testing.T) {
	ks := NewMemoryStore()

	a, err := APIToken(ks)
	if err != nil {
		t.Fatalf("APIToken: %v", err)
	}
	if len(a) < 32 {
		t.Fatalf("API token is only %d chars; too short to resist guessing", len(a))
	}
	b, err := APIToken(ks)
	if err != nil {
		t.Fatalf("APIToken (second call): %v", err)
	}
	if a != b {
		t.Fatal("APIToken rotated the token on a second call; every existing client would break")
	}
}

func TestAPITokenRejectsShortStoredValue(t *testing.T) {
	ks := NewMemoryStore()
	if err := ks.Set(accountAPIToken, []byte("tiny")); err != nil {
		t.Fatalf("seed the store: %v", err)
	}
	if _, err := APIToken(ks); err == nil {
		t.Fatal("APIToken accepted a suspiciously short stored token")
	}
}

func TestForgetClearsBothSecrets(t *testing.T) {
	ks := NewMemoryStore()
	if _, err := Load(ks); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := APIToken(ks); err != nil {
		t.Fatalf("APIToken: %v", err)
	}
	if err := Forget(ks); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := ks.Get(accountSeed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seed survived Forget (err=%v)", err)
	}
	if _, err := ks.Get(accountAPIToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("API token survived Forget (err=%v)", err)
	}
}

func TestMemoryStoreCopiesValues(t *testing.T) {
	ks := NewMemoryStore()
	secret := []byte("original")
	if err := ks.Set("k", secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	secret[0] = 'X'
	got, err := ks.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("stored value aliased the caller's slice: %q", got)
	}
	got[0] = 'Y'
	again, _ := ks.Get("k")
	if string(again) != "original" {
		t.Fatalf("returned value aliased the stored slice: %q", again)
	}
}
