package link

import (
	"crypto/ed25519"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

func mustID(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id
}

// The property the whole trust model rests on: both devices, starting from the
// same two keys, derive the same six digits — whichever of them initiated.
func TestCodeIsSymmetric(t *testing.T) {
	a, b := mustID(t), mustID(t)

	fromA, err := DeriveCode(a.Public(), b.Public())
	if err != nil {
		t.Fatalf("DeriveCode(a,b): %v", err)
	}
	fromB, err := DeriveCode(b.Public(), a.Public())
	if err != nil {
		t.Fatalf("DeriveCode(b,a): %v", err)
	}
	if fromA != fromB {
		t.Fatalf("code is not symmetric: %q vs %q — the two screens would never match", fromA, fromB)
	}
}

func TestCodeIsSixDigits(t *testing.T) {
	re := regexp.MustCompile(`^[0-9]{6}$`)
	// Many pairs, because a formatting bug (a code that renders as "4182" when
	// the value is small) would only show up on some keys.
	for i := 0; i < 200; i++ {
		a, b := mustID(t), mustID(t)
		code, err := DeriveCode(a.Public(), b.Public())
		if err != nil {
			t.Fatalf("DeriveCode: %v", err)
		}
		if !re.MatchString(code) {
			t.Fatalf("code %q is not six digits", code)
		}
	}
}

// A machine in the middle that substitutes its own key must produce a different
// number on one of the two screens. That difference is the entire defence.
func TestSubstitutedKeyChangesTheCode(t *testing.T) {
	a, b, attacker := mustID(t), mustID(t), mustID(t)

	honest, err := DeriveCode(a.Public(), b.Public())
	if err != nil {
		t.Fatalf("DeriveCode: %v", err)
	}
	// A sees the attacker's key instead of B's.
	tampered, err := DeriveCode(a.Public(), attacker.Public())
	if err != nil {
		t.Fatalf("DeriveCode: %v", err)
	}
	if honest == tampered {
		t.Fatal("substituting a key produced the same code; the ceremony would not detect a machine in the middle")
	}
}

func TestCodeIsStableAcrossCalls(t *testing.T) {
	a, b := mustID(t), mustID(t)
	first, _ := DeriveCode(a.Public(), b.Public())
	for i := 0; i < 5; i++ {
		again, err := DeriveCode(a.Public(), b.Public())
		if err != nil {
			t.Fatalf("DeriveCode: %v", err)
		}
		if again != first {
			t.Fatalf("code changed between calls: %q then %q", first, again)
		}
	}
}

func TestDeriveCodeRejectsMalformedKeys(t *testing.T) {
	good := mustID(t).Public()
	bad := ed25519.PublicKey("short")
	if _, err := DeriveCode(good, bad); err == nil {
		t.Fatal("DeriveCode accepted a malformed peer key")
	}
	if _, err := DeriveCode(bad, good); err == nil {
		t.Fatal("DeriveCode accepted a malformed self key")
	}
}

func TestFormatCode(t *testing.T) {
	if got := FormatCode("418207"); got != "418 207" {
		t.Fatalf("FormatCode(418207) = %q, want \"418 207\"", got)
	}
	// Anything unexpected passes through rather than panicking — this runs in
	// the render path of the ceremony screen.
	if got := FormatCode("123"); got != "123" {
		t.Fatalf("FormatCode(123) = %q, want it unchanged", got)
	}
}

// --- the ceremony end to end ------------------------------------------------

func TestBothSidesSeeTheSameCode(t *testing.T) {
	laptopID, desktopID := mustID(t), mustID(t)
	laptop := NewPairer(laptopID, "Laptop")
	desktop := NewPairer(desktopID, "Desktop")

	ls, err := laptop.Begin(desktop.Offer("192.168.1.31"))
	if err != nil {
		t.Fatalf("laptop.Begin: %v", err)
	}
	ds, err := desktop.Begin(laptop.Offer("192.168.1.24"))
	if err != nil {
		t.Fatalf("desktop.Begin: %v", err)
	}

	if ls.Code != ds.Code {
		t.Fatalf("the two screens show different codes: %q and %q", ls.Code, ds.Code)
	}

	// Each side confirms, and each learns the other's real key.
	gotDesktop, err := laptop.Confirm(ls.ID)
	if err != nil {
		t.Fatalf("laptop.Confirm: %v", err)
	}
	if !gotDesktop.PublicKey.Equal(desktopID.Public()) {
		t.Fatal("laptop confirmed but did not receive Desktop's key")
	}
	if gotDesktop.DeviceName != "Desktop" {
		t.Fatalf("peer name = %q, want Desktop", gotDesktop.DeviceName)
	}

	gotLaptop, err := desktop.Confirm(ds.ID)
	if err != nil {
		t.Fatalf("desktop.Confirm: %v", err)
	}
	if !gotLaptop.PublicKey.Equal(laptopID.Public()) {
		t.Fatal("desktop confirmed but did not receive Laptop's key")
	}
}

// The pinning that follows a confirmation is what makes the pair trusted, so
// the two halves are tested together: confirm, pin, then verify.
func TestConfirmThenPinMakesThePairTrusted(t *testing.T) {
	laptopID, desktopID := mustID(t), mustID(t)
	laptop := NewPairer(laptopID, "Laptop")
	desktop := NewPairer(desktopID, "Desktop")

	laptopStore := identity.NewStore(laptopID, "Laptop")
	desktopStore := identity.NewStore(desktopID, "Desktop")

	ls, _ := laptop.Begin(desktop.Offer("192.168.1.31"))
	ds, _ := desktop.Begin(laptop.Offer("192.168.1.24"))

	peer, err := laptop.Confirm(ls.ID)
	if err != nil {
		t.Fatalf("laptop.Confirm: %v", err)
	}
	if _, err := laptopStore.Pin(peer.PublicKey, peer.DeviceName, peer.Address); err != nil {
		t.Fatalf("laptop Pin: %v", err)
	}

	peer, err = desktop.Confirm(ds.ID)
	if err != nil {
		t.Fatalf("desktop.Confirm: %v", err)
	}
	if _, err := desktopStore.Pin(peer.PublicKey, peer.DeviceName, peer.Address); err != nil {
		t.Fatalf("desktop Pin: %v", err)
	}

	// Each now verifies the other and rejects anyone else.
	if err := laptopStore.Verify(desktopID.Public()); err != nil {
		t.Fatalf("laptop does not trust Desktop after pairing: %v", err)
	}
	if err := desktopStore.Verify(laptopID.Public()); err != nil {
		t.Fatalf("desktop does not trust Laptop after pairing: %v", err)
	}
	stranger := mustID(t)
	if err := laptopStore.Verify(stranger.Public()); !errors.Is(err, identity.ErrKeyMismatch) {
		t.Fatalf("laptop accepted a stranger: %v", err)
	}
}

func TestSessionCarriesWhatTheScreenNeeds(t *testing.T) {
	laptop := NewPairer(mustID(t), "Laptop")
	desktopID := mustID(t)
	desktop := NewPairer(desktopID, "Desktop")

	s, err := laptop.Begin(desktop.Offer("192.168.1.31"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// The ceremony screen shows the code, the peer's name, its address and its
	// fingerprint. All four must be on the session.
	if s.Peer.DeviceName != "Desktop" {
		t.Fatalf("peer name = %q", s.Peer.DeviceName)
	}
	if s.Peer.Address != "192.168.1.31" {
		t.Fatalf("peer address = %q", s.Peer.Address)
	}
	if s.Fingerprint() != identity.Fingerprint(desktopID.Public()) {
		t.Fatal("session fingerprint does not match the peer's key")
	}
	if s.ID == "" {
		t.Fatal("session has no id")
	}
	if !s.ExpiresAt.After(s.StartedAt) {
		t.Fatal("session expires before it starts")
	}
}

func TestConfirmIsSingleUse(t *testing.T) {
	laptop := NewPairer(mustID(t), "Laptop")
	desktop := NewPairer(mustID(t), "Desktop")

	s, _ := laptop.Begin(desktop.Offer("192.168.1.31"))
	if _, err := laptop.Confirm(s.ID); err != nil {
		t.Fatalf("first Confirm: %v", err)
	}
	// A replayed confirmation must not succeed.
	if _, err := laptop.Confirm(s.ID); !errors.Is(err, ErrPairingUnknown) {
		t.Fatalf("second Confirm = %v, want ErrPairingUnknown", err)
	}
}

func TestConfirmUnknownSession(t *testing.T) {
	laptop := NewPairer(mustID(t), "Laptop")
	if _, err := laptop.Confirm("pair-does-not-exist"); !errors.Is(err, ErrPairingUnknown) {
		t.Fatalf("Confirm of an unknown id = %v, want ErrPairingUnknown", err)
	}
}

// A code left on an unattended screen must stop being confirmable.
func TestExpiredSessionCannotBeConfirmed(t *testing.T) {
	laptop := NewPairer(mustID(t), "Laptop")
	desktop := NewPairer(mustID(t), "Desktop")

	now := time.Now()
	laptop.SetClock(func() time.Time { return now })

	s, err := laptop.Begin(desktop.Offer("192.168.1.31"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if s.Expired(now) {
		t.Fatal("a fresh session reports itself expired")
	}

	// Step past the window.
	now = now.Add(PairingWindow + time.Second)
	if !s.Expired(now) {
		t.Fatal("session did not expire after its window")
	}
	if _, err := laptop.Confirm(s.ID); !errors.Is(err, ErrPairingExpired) {
		t.Fatalf("Confirm after expiry = %v, want ErrPairingExpired", err)
	}
	// And it is consumed, so it cannot be retried once the clock is wrong.
	if _, err := laptop.Confirm(s.ID); !errors.Is(err, ErrPairingUnknown) {
		t.Fatalf("retry after expiry = %v, want ErrPairingUnknown", err)
	}
}

func TestCancel(t *testing.T) {
	laptop := NewPairer(mustID(t), "Laptop")
	desktop := NewPairer(mustID(t), "Desktop")

	s, _ := laptop.Begin(desktop.Offer("192.168.1.31"))
	if laptop.Active() != 1 {
		t.Fatalf("Active = %d, want 1", laptop.Active())
	}
	if err := laptop.Cancel(s.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if laptop.Active() != 0 {
		t.Fatalf("Active after Cancel = %d, want 0", laptop.Active())
	}
	if err := laptop.Cancel(s.ID); !errors.Is(err, ErrPairingUnknown) {
		t.Fatalf("Cancel twice = %v, want ErrPairingUnknown", err)
	}
	if _, err := laptop.Confirm(s.ID); !errors.Is(err, ErrPairingUnknown) {
		t.Fatalf("Confirm after Cancel = %v, want ErrPairingUnknown", err)
	}
}

func TestSweepDropsExpiredSessionsOnly(t *testing.T) {
	laptop := NewPairer(mustID(t), "Laptop")
	now := time.Now()
	laptop.SetClock(func() time.Time { return now })

	old, _ := laptop.Begin(NewPairer(mustID(t), "A").Offer("192.168.1.2"))
	now = now.Add(PairingWindow + time.Second)
	fresh, _ := laptop.Begin(NewPairer(mustID(t), "B").Offer("192.168.1.3"))

	if n := laptop.Sweep(); n != 1 {
		t.Fatalf("Sweep removed %d sessions, want 1", n)
	}
	if _, err := laptop.Session(old.ID); !errors.Is(err, ErrPairingUnknown) {
		t.Fatal("Sweep did not remove the expired session")
	}
	if _, err := laptop.Session(fresh.ID); err != nil {
		t.Fatalf("Sweep removed a live session: %v", err)
	}
}

func TestBeginRejectsMalformedOffer(t *testing.T) {
	laptop := NewPairer(mustID(t), "Laptop")
	if _, err := laptop.Begin(PairingOffer{DeviceName: "Bad", PublicKey: ed25519.PublicKey("short")}); err == nil {
		t.Fatal("Begin accepted an offer with a malformed key")
	}
}

func TestOfferCarriesSelfIdentity(t *testing.T) {
	id := mustID(t)
	p := NewPairer(id, "Laptop")
	o := p.Offer("192.168.1.24")
	if o.DeviceName != "Laptop" {
		t.Fatalf("offer name = %q, want Laptop", o.DeviceName)
	}
	if !o.PublicKey.Equal(id.Public()) {
		t.Fatal("offer does not carry this device's public key")
	}
	if o.Address != "192.168.1.24" {
		t.Fatalf("offer address = %q", o.Address)
	}
}

// Two different pairs must not collide onto one code often enough to matter;
// more importantly, a device pairing with two different peers must see two
// different codes.
func TestDifferentPeersGiveDifferentCodes(t *testing.T) {
	me := mustID(t)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		peer := mustID(t)
		code, err := DeriveCode(me.Public(), peer.Public())
		if err != nil {
			t.Fatalf("DeriveCode: %v", err)
		}
		seen[code] = true
	}
	// 100 draws from a million values: a handful of collisions would be
	// surprising, and zero distinct codes would mean the hash ignores an input.
	if len(seen) < 95 {
		t.Fatalf("only %d distinct codes from 100 peers; the derivation may be ignoring the peer key", len(seen))
	}
}
