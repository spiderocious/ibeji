// Package testharness runs two Ìbejì daemons in one process.
//
// This is the highest-leverage thing in the build. Every feature of the product
// is device-to-device, so without it every test needs two physical machines and
// every bug costs a walk across the room. With it, pairing, permissions, exec
// and sync are all testable in milliseconds — against the same interfaces the
// real transport implements, so a test that passes here is testing the real
// code paths rather than a simulation of them.
//
// What it does simulate, because these are what break logic: latency, a peer
// going away mid-operation, a peer that was never there, and an identity
// changing under a pinned key.
//
// What it deliberately does not simulate: TLS and the mDNS wire format. Those
// have their own tests; mixing them in here would make every failure ambiguous.
package testharness

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
)

// Device is one daemon in the harness: its identity, its pinned peer, its
// pairing state and its place on the fabric.
type Device struct {
	Name    string
	Address string

	Identity *identity.Identity
	Store    *identity.Store
	Pairer   *link.Pairer
	Secrets  *identity.MemoryStore

	fabric   *link.MemoryFabric
	listener link.Listener
	dialer   link.Dialer

	mu       sync.Mutex
	conns    []link.Conn
	accepted chan link.Conn
	stopped  chan struct{}
	stopOnce sync.Once
}

// Pair is two devices on one fabric, as a test sees them.
type Pair struct {
	Laptop  *Device
	Desktop *Device
	Fabric  *link.MemoryFabric

	t *testing.T
}

// New builds an unpaired Laptop and Desktop, each with its own identity and its
// own secret store — two daemons in one process must never share a keychain,
// or a test would pass for the wrong reason.
func New(t *testing.T) *Pair {
	t.Helper()
	f := link.NewMemoryFabric()
	p := &Pair{
		Fabric:  f,
		t:       t,
		Laptop:  newDevice(t, f, "Laptop", "192.168.1.24"),
		Desktop: newDevice(t, f, "Desktop", "192.168.1.31"),
	}
	t.Cleanup(p.Close)
	return p
}

func newDevice(t *testing.T, f *link.MemoryFabric, name, address string) *Device {
	t.Helper()
	secrets := identity.NewMemoryStore()
	id, err := identity.Load(secrets)
	if err != nil {
		t.Fatalf("harness: load identity for %s: %v", name, err)
	}
	l, err := f.Listen(address)
	if err != nil {
		t.Fatalf("harness: listen for %s: %v", name, err)
	}
	d := &Device{
		Name:     name,
		Address:  address,
		Identity: id,
		Store:    identity.NewStore(id, name),
		Pairer:   link.NewPairer(id, name),
		Secrets:  secrets,
		fabric:   f,
		listener: l,
		dialer:   f.Dialer(address),
		accepted: make(chan link.Conn, 4),
		stopped:  make(chan struct{}),
	}
	go d.acceptLoop()
	return d
}

// acceptLoop takes inbound connections and verifies the pinned key before
// handing them to the test — the same order the real daemon uses, so a test
// cannot accidentally exercise an unauthenticated path.
func (d *Device) acceptLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-d.stopped
		cancel()
	}()

	for {
		c, err := d.listener.Accept(ctx)
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns = append(d.conns, c)
		d.mu.Unlock()
		select {
		case d.accepted <- c:
		case <-d.stopped:
			c.Close()
			return
		}
	}
}

// Connect dials the other device and returns both ends, after the dialing side
// has verified the peer's pinned key.
//
// It returns an error rather than failing the test, because "the peer is not
// reachable" is a case several tests assert on deliberately.
func (d *Device) Connect(ctx context.Context, peer *Device) (link.Conn, link.Conn, error) {
	// The real transport verifies during the TLS handshake. Here the check is
	// explicit, so the harness enforces the same rule rather than skipping it.
	if err := d.Store.Verify(peer.Identity.Public()); err != nil {
		return nil, nil, fmt.Errorf("harness: %s will not connect to %s: %w", d.Name, peer.Name, err)
	}

	out, err := d.dialer.Dial(ctx, peer.Address)
	if err != nil {
		return nil, nil, err
	}
	d.mu.Lock()
	d.conns = append(d.conns, out)
	d.mu.Unlock()

	select {
	case in := <-peer.accepted:
		// The accepting side verifies too: trust is mutual, and a device that
		// has unpaired must refuse an inbound connection from its ex-peer.
		if err := peer.Store.Verify(d.Identity.Public()); err != nil {
			out.Close()
			in.Close()
			return nil, nil, fmt.Errorf("harness: %s refused the inbound connection from %s: %w", peer.Name, d.Name, err)
		}
		return out, in, nil
	case <-ctx.Done():
		out.Close()
		return nil, nil, ctx.Err()
	case <-time.After(5 * time.Second):
		out.Close()
		return nil, nil, fmt.Errorf("harness: %s never accepted a connection from %s", peer.Name, d.Name)
	}
}

// Stop takes this device off the fabric, as a lid closing would. Connections
// die and further dials to it fail as unreachable.
func (d *Device) Stop() {
	d.stopOnce.Do(func() {
		close(d.stopped)
		d.listener.Close()
		d.mu.Lock()
		conns := d.conns
		d.conns = nil
		d.mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
}

// Pair pins both sides to each other, the one-line path to "two trusted
// machines" for tests that want a paired fixture.
//
// It is NOT the real ceremony: it calls Begin on both devices itself, whereas
// over a connection each side raises its own session — the dialler in
// BeginPairing, the accepter in servePairing. A break between those two is
// invisible here, which is how v0.1.3 shipped with a ceremony that showed a
// code on one machine and nothing on the other. The ceremony over a real
// connection is covered in internal/device/pairing_network_test.go.
func (p *Pair) Pair() {
	p.t.Helper()

	ls, err := p.Laptop.Pairer.Begin(p.Desktop.Pairer.Offer(p.Desktop.Address))
	if err != nil {
		p.t.Fatalf("harness: laptop begin pairing: %v", err)
	}
	ds, err := p.Desktop.Pairer.Begin(p.Laptop.Pairer.Offer(p.Laptop.Address))
	if err != nil {
		p.t.Fatalf("harness: desktop begin pairing: %v", err)
	}
	if ls.Code != ds.Code {
		p.t.Fatalf("harness: pairing codes differ (%q vs %q); the ceremony is broken", ls.Code, ds.Code)
	}

	peer, err := p.Laptop.Pairer.Confirm(ls.ID)
	if err != nil {
		p.t.Fatalf("harness: laptop confirm: %v", err)
	}
	if _, err := p.Laptop.Store.Pin(peer.PublicKey, peer.DeviceName, peer.Address); err != nil {
		p.t.Fatalf("harness: laptop pin: %v", err)
	}

	peer, err = p.Desktop.Pairer.Confirm(ds.ID)
	if err != nil {
		p.t.Fatalf("harness: desktop confirm: %v", err)
	}
	if _, err := p.Desktop.Store.Pin(peer.PublicKey, peer.DeviceName, peer.Address); err != nil {
		p.t.Fatalf("harness: desktop pin: %v", err)
	}
}

// Link pairs if needed, connects, and returns both ends. The common fixture.
func (p *Pair) Link() (laptopSide, desktopSide link.Conn) {
	p.t.Helper()
	if !p.Laptop.Store.Paired() {
		p.Pair()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, in, err := p.Laptop.Connect(ctx, p.Desktop)
	if err != nil {
		p.t.Fatalf("harness: connect: %v", err)
	}
	return out, in
}

// SetLatency makes every frame take this long, so a test can expose a race
// that an instant pipe would hide. This is the knob the sync-barrier tests use.
func (p *Pair) SetLatency(d time.Duration) {
	p.Fabric.Latency = d
}

// Reinstall gives a device a brand-new identity while keeping its name and
// address — exactly what a reinstalled machine looks like from the other side,
// and indistinguishable from an impersonation. The peer must reject it.
func (p *Pair) Reinstall(d *Device) {
	p.t.Helper()
	secrets := identity.NewMemoryStore()
	id, err := identity.Load(secrets)
	if err != nil {
		p.t.Fatalf("harness: reinstall %s: %v", d.Name, err)
	}
	d.Identity = id
	d.Secrets = secrets
	d.Store = identity.NewStore(id, d.Name)
	d.Pairer = link.NewPairer(id, d.Name)
}

// Close stops both devices. Registered with t.Cleanup by New.
func (p *Pair) Close() {
	p.Laptop.Stop()
	p.Desktop.Stop()
}
