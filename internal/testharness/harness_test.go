package testharness

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
)

// The harness is what every later suite stands on, so it is tested like
// production code: if it is wrong, failures surface in the wrong package.

func TestTwoDevicesStartUnpairedAndDistinct(t *testing.T) {
	p := New(t)

	if p.Laptop.Store.Paired() || p.Desktop.Store.Paired() {
		t.Fatal("devices start paired; they must begin as strangers")
	}
	if p.Laptop.Identity.Public().Equal(p.Desktop.Identity.Public()) {
		t.Fatal("both devices share one identity; they are not two machines")
	}
	// Separate secret stores: a shared keychain would let a test pass for the
	// wrong reason.
	if p.Laptop.Secrets == p.Desktop.Secrets {
		t.Fatal("devices share one secret store")
	}
}

func TestUnpairedDevicesRefuseToConnect(t *testing.T) {
	p := New(t)
	ctx := context.Background()

	_, _, err := p.Laptop.Connect(ctx, p.Desktop)
	if !errors.Is(err, identity.ErrNotPaired) {
		t.Fatalf("connect before pairing = %v, want ErrNotPaired", err)
	}
}

func TestPairThenConnect(t *testing.T) {
	p := New(t)
	p.Pair()

	if !p.Laptop.Store.Paired() || !p.Desktop.Store.Paired() {
		t.Fatal("Pair() did not leave both devices paired")
	}
	// Each pinned the other's real key, and the names propagated.
	if err := p.Laptop.Store.Verify(p.Desktop.Identity.Public()); err != nil {
		t.Fatalf("laptop does not trust desktop: %v", err)
	}
	if err := p.Desktop.Store.Verify(p.Laptop.Identity.Public()); err != nil {
		t.Fatalf("desktop does not trust laptop: %v", err)
	}
	if got := p.Laptop.Store.Peer().Name; got != "Desktop" {
		t.Fatalf("laptop's peer name = %q, want Desktop", got)
	}
	if got := p.Laptop.Store.Peer().LastAddress; got != "192.168.1.31" {
		t.Fatalf("laptop's peer address = %q, want 192.168.1.31", got)
	}

	laptopSide, desktopSide := p.Link()
	if laptopSide.RemoteAddr().String() != "192.168.1.31" {
		t.Fatalf("laptop's remote = %q", laptopSide.RemoteAddr())
	}
	if desktopSide.RemoteAddr().String() != "192.168.1.24" {
		t.Fatalf("desktop's remote = %q", desktopSide.RemoteAddr())
	}
}

func TestStreamsFlowBetweenDevices(t *testing.T) {
	p := New(t)
	laptopSide, desktopSide := p.Link()
	ctx := context.Background()

	out, err := laptopSide.Open(ctx, link.KindRPC)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := out.Write([]byte("ls ~/projects")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	in, err := desktopSide.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	in.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, len("ls ~/projects"))
	if _, err := io.ReadFull(in, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(buf) != "ls ~/projects" {
		t.Fatalf("desktop received %q", buf)
	}
}

// A reinstalled peer presents the same name and the same address with a new
// key. The product cannot tell that apart from an impersonation, so it must
// refuse — this is the scenario the whole pinning model exists for.
func TestReinstalledPeerIsRefused(t *testing.T) {
	p := New(t)
	p.Pair()

	// Confirm it works before the reinstall, so the refusal below is
	// attributable to the new key and nothing else.
	if _, _, err := p.Laptop.Connect(context.Background(), p.Desktop); err != nil {
		t.Fatalf("connect before reinstall: %v", err)
	}

	p.Reinstall(p.Desktop)

	// Same name, same address, new key.
	if p.Desktop.Name != "Desktop" || p.Desktop.Address != "192.168.1.31" {
		t.Fatal("Reinstall changed the device's name or address; it must look like the same machine")
	}
	_, _, err := p.Laptop.Connect(context.Background(), p.Desktop)
	if !errors.Is(err, identity.ErrKeyMismatch) {
		t.Fatalf("connect to a reinstalled peer = %v, want ErrKeyMismatch", err)
	}
}

// Unpairing must take effect at once: the ex-peer cannot reconnect, in either
// direction.
func TestUnpairStopsFurtherConnections(t *testing.T) {
	p := New(t)
	p.Pair()
	ctx := context.Background()

	if _, _, err := p.Laptop.Connect(ctx, p.Desktop); err != nil {
		t.Fatalf("connect while paired: %v", err)
	}

	if err := p.Laptop.Store.Unpair(); err != nil {
		t.Fatalf("Unpair: %v", err)
	}

	if _, _, err := p.Laptop.Connect(ctx, p.Desktop); !errors.Is(err, identity.ErrNotPaired) {
		t.Fatalf("outbound connect after unpair = %v, want ErrNotPaired", err)
	}
	// And the other direction: Desktop still has Laptop pinned, but Laptop must
	// refuse the inbound connection.
	_, _, err := p.Desktop.Connect(ctx, p.Laptop)
	if !errors.Is(err, identity.ErrNotPaired) {
		t.Fatalf("inbound connect after unpair = %v, want the unpaired side to refuse", err)
	}
}

// A lid closing is the normal case on a LAN, not a failure.
func TestStoppedPeerIsUnreachable(t *testing.T) {
	p := New(t)
	p.Pair()

	p.Desktop.Stop()

	_, _, err := p.Laptop.Connect(context.Background(), p.Desktop)
	if !errors.Is(err, link.ErrNotLinked) {
		t.Fatalf("connect to a sleeping peer = %v, want ErrNotLinked", err)
	}
}

// An open link must end when the peer goes away, so a caller blocked on a read
// is released rather than hanging.
func TestPeerGoingAwayEndsTheLink(t *testing.T) {
	p := New(t)
	laptopSide, _ := p.Link()
	ctx := context.Background()

	s, err := laptopSide.Open(ctx, link.KindExec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	read := make(chan error, 1)
	go func() {
		_, err := s.Read(make([]byte, 16))
		read <- err
	}()

	p.Desktop.Stop()

	select {
	case err := <-read:
		if err == nil {
			t.Fatal("a blocked read returned nil after the peer went away")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked read hung after the peer went away")
	}

	select {
	case <-laptopSide.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the link did not report Done() after the peer went away")
	}
}

func TestLatencyKnob(t *testing.T) {
	p := New(t)
	p.SetLatency(15 * time.Millisecond)
	laptopSide, desktopSide := p.Link()
	ctx := context.Background()

	start := time.Now()
	s, err := laptopSide.Open(ctx, link.KindSync)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Write([]byte("chunk")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	in, err := desktopSide.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	in.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(in, make([]byte, 5)); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("round trip took %v with 15ms latency set", elapsed)
	}
}

func TestLinkIsIdempotentAboutPairing(t *testing.T) {
	p := New(t)
	// Link() pairs on demand; calling it twice must not try to pin twice.
	p.Link()
	p.Link()
	if !p.Laptop.Store.Paired() {
		t.Fatal("devices came unpaired")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	p := New(t)
	p.Pair()
	p.Desktop.Stop()
	p.Desktop.Stop() // must not panic on a double close
}

// v1 is exactly two devices: a third must not be able to pin onto either.
func TestThirdDeviceCannotPair(t *testing.T) {
	p := New(t)
	p.Pair()

	third, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := p.Laptop.Store.Pin(third.Public(), "Studio Mac", "192.168.1.9"); !errors.Is(err, identity.ErrAlreadyPaired) {
		t.Fatalf("pinning a third device = %v, want ErrAlreadyPaired", err)
	}
}
