package device

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/api"
	"github.com/feranmi/ibeji/internal/discovery"
	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newDaemon(t *testing.T, name string) *Daemon {
	t.Helper()
	d, err := New(Options{
		Version:  "test",
		Logger:   quiet(),
		Secrets:  identity.NewMemoryStore(),
		Name:     name,
		Registry: discovery.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("New(%s): %v", name, err)
	}
	return d
}

// pairDaemons pins two daemons to each other, for tests that want a paired
// fixture rather than the ceremony itself.
//
// It stages both halves by hand, which the real network path does NOT do: the
// dialling daemon raises its session in BeginPairing and the accepting one
// raises its own in servePairing. So this helper cannot see a break in that
// handshake — it did not see the one that shipped in v0.1.3. The ceremony as
// it actually runs over a connection is covered in pairing_network_test.go,
// and that is where a change to it must be tested.
func pairDaemons(t *testing.T, a, b *Daemon) {
	t.Helper()
	as, err := a.Pairer().Begin(b.Pairer().Offer("192.168.1.31"))
	if err != nil {
		t.Fatalf("a.Begin: %v", err)
	}
	bs, err := b.Pairer().Begin(a.Pairer().Offer("192.168.1.24"))
	if err != nil {
		t.Fatalf("b.Begin: %v", err)
	}
	if as.Code != bs.Code {
		t.Fatalf("codes differ: %q vs %q", as.Code, bs.Code)
	}
	if _, err := a.ConfirmPairing(as.ID); err != nil {
		t.Fatalf("a.ConfirmPairing: %v", err)
	}
	if _, err := b.ConfirmPairing(bs.ID); err != nil {
		t.Fatalf("b.ConfirmPairing: %v", err)
	}
}

func TestDaemonStartsUnpaired(t *testing.T) {
	d := newDaemon(t, "Laptop")

	self := d.Self()
	if self.Name != "Laptop" {
		t.Fatalf("name = %q", self.Name)
	}
	if self.Paired {
		t.Fatal("a fresh daemon reports itself paired")
	}
	if self.Fingerprint == "" {
		t.Fatal("no fingerprint")
	}
	if _, ok := d.Paired(); ok {
		t.Fatal("Paired() reported a device")
	}
	// Prompt is the default: a machine that asks first is the safe start, and
	// Bypass is a deliberate, guarded choice.
	if d.Mode() != api.ModePrompt {
		t.Fatalf("default mode = %q, want prompt", d.Mode())
	}
}

func TestPairThenReportLive(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	pairDaemons(t, laptop, desktop)

	dev, ok := laptop.Paired()
	if !ok {
		t.Fatal("not paired after the ceremony")
	}
	if dev.Name != "Desktop" {
		t.Fatalf("peer name = %q", dev.Name)
	}
	// Address is a required slot in the design system; a response without one
	// makes a compliant device row impossible to render.
	if dev.Address == "" {
		t.Fatal("paired device has no address")
	}
	// Not connected yet, so unreachable — which is a status, not an error.
	if dev.Status != api.StatusUnreachable {
		t.Fatalf("status = %q, want unreachable before the link comes up", dev.Status)
	}
}

// Regression: unpairing left the half-finished ceremony in place, which kept
// the listener willing to accept a stranger. The ex-peer's supervisor
// reconnected within a second and both sides showed "live" again, so revoking
// a device did nothing visible. Found by running two real daemons.
func TestUnpairClearsPairingSessions(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	pairDaemons(t, laptop, desktop)

	// A second, unconfirmed attempt — exactly what a retried pairing leaves.
	stranger := newDaemon(t, "Stranger")
	if _, err := laptop.Pairer().Begin(stranger.Pairer().Offer("192.168.1.99")); err != nil {
		t.Fatalf("Begin a second session: %v", err)
	}
	if laptop.Pairer().Active() == 0 {
		t.Fatal("no session in flight; the test is not reproducing the bug")
	}
	if !laptop.PairingOpen() {
		t.Fatal("PairingOpen() is false with a session in flight")
	}

	dev, _ := laptop.Paired()
	if err := laptop.Unpair(dev.ID); err != nil {
		t.Fatalf("Unpair: %v", err)
	}

	if laptop.Pairer().Active() != 0 {
		t.Fatalf("%d pairing sessions survived unpair", laptop.Pairer().Active())
	}
	// This is the property that matters: an unpaired device with no ceremony
	// open accepts nobody, so the ex-peer cannot walk back in.
	if laptop.PairingOpen() {
		t.Fatal("PairingOpen() is still true after unpair; the ex-peer would reconnect immediately")
	}
	if laptop.CurrentPeerKey() != nil {
		t.Fatal("CurrentPeerKey() still returns a key after unpair")
	}
}

// Regression: the listener captured the pinned key at startup, so unpairing
// did not stop it accepting the revoked device. The pin must be read at every
// handshake.
func TestCurrentPeerKeyTracksPairing(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")

	if laptop.CurrentPeerKey() != nil {
		t.Fatal("an unpaired daemon offers a key to pin against")
	}

	pairDaemons(t, laptop, desktop)
	key := laptop.CurrentPeerKey()
	if key == nil {
		t.Fatal("no key after pairing")
	}
	if !key.Equal(desktop.Store().Self().Public()) {
		t.Fatal("CurrentPeerKey is not the peer's key")
	}

	dev, _ := laptop.Paired()
	if err := laptop.Unpair(dev.ID); err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	if laptop.CurrentPeerKey() != nil {
		t.Fatal("CurrentPeerKey survived unpair; the listener would still accept the revoked device")
	}
}

// Unpairing must close the live link at once: the PRD requires every tunnel,
// sync and session to end, and everything rides on that one connection.
func TestUnpairClosesTheLink(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	pairDaemons(t, laptop, desktop)

	fabric := link.NewMemoryFabric()
	ln, err := fabric.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() { _, _ = ln.Accept(context.Background()) }()

	conn, err := fabric.Dialer("laptop").Dial(context.Background(), "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	laptop.SetConn(conn)

	dev, _ := laptop.Paired()
	if dev.Status != api.StatusLive {
		t.Fatalf("status = %q with a live conn, want live", dev.Status)
	}

	if err := laptop.Unpair(dev.ID); err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the link stayed open after unpair")
	}
	if laptop.Conn() != nil {
		t.Fatal("the daemon still holds a connection after unpair")
	}
}

// v1 is exactly two devices: confirming a stale session after a pair exists
// must not quietly replace it.
func TestConfirmPairingRefusedWhenAlreadyPaired(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	stranger := newDaemon(t, "Stranger")

	// Start a session with the stranger, then pair with Desktop first.
	s, err := laptop.Pairer().Begin(stranger.Pairer().Offer("192.168.1.99"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	pairDaemons(t, laptop, desktop)

	if _, err := laptop.ConfirmPairing(s.ID); !errors.Is(err, identity.ErrAlreadyPaired) {
		t.Fatalf("confirming a stale session while paired = %v, want ErrAlreadyPaired", err)
	}
	// And the real pair survived.
	dev, ok := laptop.Paired()
	if !ok || dev.Name != "Desktop" {
		t.Fatal("the established pair was replaced by a stale confirmation")
	}
}

func TestBeginPairingRefusedWhenAlreadyPaired(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	pairDaemons(t, laptop, desktop)

	if _, err := laptop.BeginPairing("192.168.1.99"); !errors.Is(err, identity.ErrAlreadyPaired) {
		t.Fatalf("BeginPairing while paired = %v, want ErrAlreadyPaired", err)
	}
}

func TestSetConnPublishesOnlineAndOffline(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	events := &recordingPublisher{}
	laptop.events = events
	pairDaemons(t, laptop, desktop)

	fabric := link.NewMemoryFabric()
	ln, _ := fabric.Listen("desktop")
	defer ln.Close()
	go func() { _, _ = ln.Accept(context.Background()) }()
	conn, err := fabric.Dialer("laptop").Dial(context.Background(), "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	laptop.SetConn(conn)
	laptop.SetConn(conn) // idempotent: must not publish twice
	laptop.SetConn(nil)

	if events.count(api.EventDeviceOnline) != 1 {
		t.Fatalf("online published %d times, want 1", events.count(api.EventDeviceOnline))
	}
	if events.count(api.EventDeviceOffline) != 1 {
		t.Fatalf("offline published %d times, want 1", events.count(api.EventDeviceOffline))
	}
}

// A key mismatch is never transient, so it must not be cleared by anything
// short of re-pairing — a retry that happened to succeed against something
// else would bury the one message the user has to see.
func TestKeyMismatchIsSticky(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	events := &recordingPublisher{}
	laptop.events = events
	pairDaemons(t, laptop, desktop)

	if laptop.KeyMismatch() {
		t.Fatal("a freshly paired daemon reports a key mismatch")
	}
	if !laptop.KeyMatches() {
		t.Fatal("KeyMatches() is false after pairing")
	}

	laptop.NoteKeyMismatch()
	laptop.NoteKeyMismatch() // must publish once, not per attempt

	if !laptop.KeyMismatch() {
		t.Fatal("the mismatch did not stick")
	}
	if laptop.KeyMatches() {
		t.Fatal("KeyMatches() is true during a mismatch")
	}
	if n := events.count(api.EventKeyMismatch); n != 1 {
		t.Fatalf("key_mismatch published %d times, want 1", n)
	}

	// Unpairing clears it: the user has acted.
	dev, _ := laptop.Paired()
	if err := laptop.Unpair(dev.ID); err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	if laptop.KeyMismatch() {
		t.Fatal("the mismatch survived unpairing")
	}
}

// mDNS answers our own advertisement, once per interface. Offering to pair
// with ourselves would be absurd and confusing.
func TestCandidatesExcludeSelf(t *testing.T) {
	d := newDaemon(t, "Laptop")

	d.Registry().Add(discovery.Candidate{
		Name: "Laptop", Address: "192.168.1.24", Port: 7749,
		PublicKey:  d.Store().Self().Public(),
		APIVersion: discovery.APIVersion, Source: discovery.SourceMDNS,
	})
	other, _ := identity.Generate()
	d.Registry().Add(discovery.Candidate{
		Name: "Desktop", Address: "192.168.1.31", Port: 7749,
		PublicKey:  other.Public(),
		APIVersion: discovery.APIVersion, Source: discovery.SourceMDNS,
	})

	got := d.Candidates()
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1 (self excluded)", len(got))
	}
	if got[0].Name != "Desktop" {
		t.Fatalf("candidate = %q, want Desktop", got[0].Name)
	}
}

func TestSetModeRequiresAPairedDevice(t *testing.T) {
	d := newDaemon(t, "Laptop")
	if err := d.SetMode("", api.ModeBypass); !errors.Is(err, identity.ErrNotPaired) {
		t.Fatalf("SetMode while unpaired = %v, want ErrNotPaired", err)
	}

	desktop := newDaemon(t, "Desktop")
	pairDaemons(t, d, desktop)
	if err := d.SetMode("", api.ModeBypass); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if d.Mode() != api.ModeBypass {
		t.Fatalf("mode = %q, want bypass", d.Mode())
	}
	// A wrong device id must not silently change the real device's mode.
	if err := d.SetMode("SHA256:not-this-one", api.ModePrompt); err == nil {
		t.Fatal("SetMode accepted an unknown device id")
	}
	if d.Mode() != api.ModeBypass {
		t.Fatal("a rejected SetMode changed the mode anyway")
	}
}

func TestUnpairRejectsUnknownDeviceID(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	pairDaemons(t, laptop, desktop)

	if err := laptop.Unpair("SHA256:some-other-device"); err == nil {
		t.Fatal("Unpair accepted an unknown device id")
	}
	if _, ok := laptop.Paired(); !ok {
		t.Fatal("a rejected Unpair revoked the real device")
	}
}

// --- name handling ---

// A peer's name is untrusted input: it arrives from another machine, and
// control characters would corrupt a terminal listing devices.
func TestSanitiseName(t *testing.T) {
	cases := map[string]string{
		"Desktop":         "Desktop",
		"  Desktop  ":     "Desktop",
		"Desk\x00top":     "Desktop",
		"Desk\ntop":       "Desktop",
		"Desktop\x1b[31m": "Desktop[31m",
		"":                "",
		"\x00\x01":        "",
	}
	for in, want := range cases {
		if got := sanitiseName(in); got != want {
			t.Errorf("sanitiseName(%q) = %q, want %q", in, got, want)
		}
	}
	// And it is bounded, so a peer cannot break the device row's layout.
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'a'
	}
	if got := sanitiseName(string(long)); len(got) > 63 {
		t.Fatalf("sanitiseName returned %d characters; it must be bounded", len(got))
	}
}

func TestDefaultDeviceNameIsUsable(t *testing.T) {
	name := defaultDeviceName()
	if name == "" {
		t.Fatal("default device name is empty")
	}
	if len(name) > 63 {
		t.Fatalf("default device name is %d characters", len(name))
	}
}

// --- helpers ---

type recordingPublisher struct {
	events []string
}

func (r *recordingPublisher) Publish(typ string, data any) {
	r.events = append(r.events, typ)
}

func (r *recordingPublisher) count(typ string) int {
	n := 0
	for _, e := range r.events {
		if e == typ {
			n++
		}
	}
	return n
}

// Regression: a successful TLS handshake proves the peer is the device we
// pinned, but not that it still pins us. An unpaired device accepts
// connections so a new ceremony can start, so without the Paired flag in the
// hello an ex-peer reconnected and both sides reported themselves linked a
// second after one revoked the other. Found by running two real daemons.
func TestControlHelloCarriesPairingState(t *testing.T) {
	fabric := link.NewMemoryFabric()
	ln, err := fabric.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	// The far side has unpaired: it answers, but reports Paired false.
	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			stream, err := conn.Accept(context.Background())
			if err != nil {
				return
			}
			go func() { _, _ = serveControl(stream, "Desktop", false) }()
		}
	}()

	conn, err := fabric.Dialer("laptop").Dial(context.Background(), "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// We still think we are paired; the peer says it is not.
	name, err := exchangeNames(context.Background(), conn, "Laptop", true)
	if !errors.Is(err, ErrPeerUnpaired) {
		t.Fatalf("exchangeNames with an unpaired peer = %v, want ErrPeerUnpaired", err)
	}
	// The name still comes back, so the UI can say which device revoked us.
	if name != "Desktop" {
		t.Fatalf("name = %q, want Desktop even when the peer has unpaired", name)
	}
}

// During the ceremony neither side is paired yet, so an unpaired answer is
// expected and must not be treated as a revocation.
func TestPairingExchangeToleratesUnpairedPeer(t *testing.T) {
	fabric := link.NewMemoryFabric()
	ln, err := fabric.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			stream, err := conn.Accept(context.Background())
			if err != nil {
				return
			}
			go func() { _, _ = serveControl(stream, "Desktop", false) }()
		}
	}()

	conn, err := fabric.Dialer("laptop").Dial(context.Background(), "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// selfPaired false: this is the ceremony, not a link.
	name, err := exchangeNames(context.Background(), conn, "Laptop", false)
	if err != nil {
		t.Fatalf("pairing-time exchange failed: %v", err)
	}
	if name != "Desktop" {
		t.Fatalf("name = %q, want Desktop", name)
	}
}

// Regression: both devices dialled and both accepted, so each new connection
// displaced the previous one — a hot loop that logged 46,000 link drops in
// fifteen seconds and pegged a core on both machines. Exactly one side must
// dial, decided without negotiation.
func TestExactlyOneSideDials(t *testing.T) {
	laptop, desktop := newDaemon(t, "Laptop"), newDaemon(t, "Desktop")
	pairDaemons(t, laptop, desktop)

	a, b := laptop.shouldDial(), desktop.shouldDial()
	if a == b {
		t.Fatalf("both sides agree on dialling (%v); one dials, the other listens", a)
	}

	// The rule is deterministic: both arrive at the same answer from the same
	// two keys, with no negotiation and no coin flip.
	for i := 0; i < 20; i++ {
		if laptop.shouldDial() != a || desktop.shouldDial() != b {
			t.Fatal("shouldDial is not deterministic")
		}
	}
	// And it is the lower key that dials, on both sides' reckoning.
	lower := bytes.Compare(laptop.Store().Self().Public(), desktop.Store().Self().Public()) < 0
	if a != lower {
		t.Fatal("the dialer is not the device with the lower key")
	}
}

func TestUnpairedDeviceDoesNotDial(t *testing.T) {
	d := newDaemon(t, "Laptop")
	if d.shouldDial() {
		t.Fatal("an unpaired daemon wants to dial; there is nobody to dial")
	}
}
