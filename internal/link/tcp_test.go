package link

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

// tcpPair brings up a real listener and a real dialer over loopback, each
// pinning the other, and returns both ends of the established link.
func tcpPair(t *testing.T) (dialSide, acceptSide Conn) {
	t.Helper()
	laptop, desktop := mustID(t), mustID(t)

	server := &TCPTransport{Identity: desktop, Name: "Desktop", Expect: laptop.Public()}
	ln, err := server.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	type accepted struct {
		c   Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept(context.Background())
		ch <- accepted{c, err}
	}()

	client := &TCPTransport{Identity: laptop, Name: "Laptop", Expect: desktop.Public()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := client.Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	res := <-ch
	if res.err != nil {
		t.Fatalf("Accept: %v", res.err)
	}
	t.Cleanup(func() { out.Close(); res.c.Close() })
	return out, res.c
}

func TestTCPStreamRoundTrip(t *testing.T) {
	a, b := tcpPair(t)
	ctx := context.Background()

	sa, err := a.Open(ctx, KindExec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := sa.Write([]byte("git push")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	sb, err := b.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if sb.Kind() != KindExec {
		t.Fatalf("kind = %v, want exec", sb.Kind())
	}
	if got := string(readN(t, sb, 8)); got != "git push" {
		t.Fatalf("read %q", got)
	}

	// And back the other way.
	if _, err := sb.Write([]byte("ok")); err != nil {
		t.Fatalf("Write back: %v", err)
	}
	if got := string(readN(t, sa, 2)); got != "ok" {
		t.Fatalf("read back %q", got)
	}
}

// A chunk larger than one frame must be split and reassembled, not refused:
// the sync engine moves files through these streams.
func TestTCPLargeWriteIsChunked(t *testing.T) {
	a, b := tcpPair(t)
	ctx := context.Background()

	sa, _ := a.Open(ctx, KindSync)
	sb, _ := b.Accept(ctx)

	payload := make([]byte, 3*maxFrame+1234)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	go func() {
		_, _ = sa.Write(payload)
		sa.Close()
	}()

	sb.SetReadDeadline(time.Now().Add(30 * time.Second))
	got, err := io.ReadAll(sb)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload corrupted: got %d bytes, want %d", len(got), len(payload))
	}
}

// A peer claiming an enormous frame would otherwise make us allocate it.
func TestTCPRejectsOversizedFrameHeader(t *testing.T) {
	laptop, desktop := mustID(t), mustID(t)
	server := &TCPTransport{Identity: desktop, Name: "Desktop", Expect: laptop.Public()}
	ln, err := server.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	ch := make(chan Conn, 1)
	go func() {
		c, err := ln.Accept(context.Background())
		if err == nil {
			ch <- c
		}
	}()

	client := &TCPTransport{Identity: laptop, Name: "Laptop", Expect: desktop.Public()}
	out, err := client.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer out.Close()

	in := <-ch
	defer in.Close()

	// Claim a 4 GiB frame. The reader must close the connection rather than
	// allocate it.
	tc := out.(*tcpConn)
	var hdr [11]byte
	hdr[0] = frameData
	hdr[1], hdr[2], hdr[3], hdr[4] = 0, 0, 0, 1
	hdr[7], hdr[8], hdr[9], hdr[10] = 0xff, 0xff, 0xff, 0xff
	tc.writeMu.Lock()
	_, _ = tc.tls.Write(hdr[:])
	tc.writeMu.Unlock()

	select {
	case <-in.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open after a peer claimed a 4 GiB frame")
	}
}

// The dialer must refuse a peer whose key is not the pinned one, over a real
// socket — this is the production path for the property tls_test checks.
func TestTCPDialRejectsWrongKey(t *testing.T) {
	laptop, desktop, impostor := mustID(t), mustID(t), mustID(t)

	// The impostor is listening where Desktop should be.
	server := &TCPTransport{Identity: impostor, Name: "Desktop", Expect: laptop.Public()}
	ln, err := server.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() { _, _ = ln.Accept(context.Background()) }()

	client := &TCPTransport{Identity: laptop, Name: "Laptop", Expect: desktop.Public()}
	_, err = client.Dial(context.Background(), ln.Addr().String())
	if !errors.Is(err, identity.ErrKeyMismatch) {
		t.Fatalf("Dial to an impostor = %v, want identity.ErrKeyMismatch", err)
	}
}

// A sleeping peer reads as unreachable, not as an error: that is the normal
// case on a LAN and the UI renders it as a status.
func TestTCPDialToNothingIsNotLinked(t *testing.T) {
	laptop, desktop := mustID(t), mustID(t)
	client := &TCPTransport{Identity: laptop, Name: "Laptop", Expect: desktop.Public()}

	// Bind and immediately release, so the port is almost certainly free.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.Dial(ctx, addr)
	if !errors.Is(err, ErrNotLinked) {
		t.Fatalf("Dial to a closed port = %v, want ErrNotLinked", err)
	}
}

// A user typing a bare IP must not also have to know the port.
func TestWithDefaultPort(t *testing.T) {
	cases := map[string]string{
		"192.168.1.31":      "192.168.1.31:7749",
		"192.168.1.31:9000": "192.168.1.31:9000",
		"fd00::1":           "[fd00::1]:7749",
		"[fd00::1]:9000":    "[fd00::1]:9000",
		"desktop.local":     "desktop.local:7749",
	}
	for in, want := range cases {
		if got := withDefaultPort(in); got != want {
			t.Errorf("withDefaultPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTCPClosingConnectionEndsStreams(t *testing.T) {
	a, b := tcpPair(t)
	ctx := context.Background()

	s, err := a.Open(ctx, KindRPC)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := b.Accept(ctx); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	read := make(chan error, 1)
	go func() {
		_, err := s.Read(make([]byte, 8))
		read <- err
	}()

	b.Close()

	select {
	case err := <-read:
		if err == nil {
			t.Fatal("a blocked read returned nil after the peer closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked read hung after the peer closed")
	}
}

func TestTCPUnknownKindIsRejected(t *testing.T) {
	a, _ := tcpPair(t)
	if _, err := a.Open(context.Background(), StreamKind(4242)); !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("Open with an unknown kind = %v, want ErrStreamRejected", err)
	}
}

func TestTCPConcurrentStreamsStaySeparate(t *testing.T) {
	a, b := tcpPair(t)
	ctx := context.Background()

	const n = 6
	go func() {
		for i := 0; i < n; i++ {
			s, err := b.Accept(ctx)
			if err != nil {
				return
			}
			go func(s Stream) {
				buf := make([]byte, 32)
				read, err := s.Read(buf)
				if err != nil {
					return
				}
				_, _ = s.Write(buf[:read])
			}(s)
		}
	}()

	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			want := bytes.Repeat([]byte{byte('a' + i)}, 16)
			s, err := a.Open(ctx, KindRPC)
			if err != nil {
				errs <- err
				return
			}
			defer s.Close()
			if _, err := s.Write(want); err != nil {
				errs <- err
				return
			}
			s.SetReadDeadline(time.Now().Add(10 * time.Second))
			got := make([]byte, 16)
			if _, err := io.ReadFull(s, got); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, want) {
				errs <- errors.New("stream crosstalk: got " + string(got))
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent stream: %v", err)
		}
	}
}

// During pairing no key is pinned, so both sides accept any key but still
// require one — and the key is readable afterwards so the ceremony can show
// its fingerprint.
func TestTCPPairingModeExposesPeerKey(t *testing.T) {
	laptop, desktop := mustID(t), mustID(t)

	server := &TCPTransport{Identity: desktop, Name: "Desktop"} // Expect nil
	ln, err := server.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	ch := make(chan Conn, 1)
	go func() {
		c, err := ln.Accept(context.Background())
		if err == nil {
			ch <- c
		}
	}()

	client := &TCPTransport{Identity: laptop, Name: "Laptop"} // Expect nil
	out, err := client.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("pairing-mode Dial: %v", err)
	}
	defer out.Close()

	in := <-ch
	defer in.Close()

	// The dialer learns the peer's key, which is what the ceremony shows a
	// fingerprint for.
	key, err := out.(*tcpConn).PeerKey()
	if err != nil {
		t.Fatalf("PeerKey: %v", err)
	}
	if !key.Equal(desktop.Public()) {
		t.Fatal("the dialer did not learn the peer's real key")
	}
	// And the accepter learns the dialer's.
	key, err = in.(*tcpConn).PeerKey()
	if err != nil {
		t.Fatalf("PeerKey (accepter): %v", err)
	}
	if !key.Equal(laptop.Public()) {
		t.Fatal("the accepter did not learn the dialer's key")
	}
}

func TestTCPAcceptRespectsContextCancellation(t *testing.T) {
	desktop, laptop := mustID(t), mustID(t)
	server := &TCPTransport{Identity: desktop, Name: "Desktop", Expect: laptop.Public()}
	ln, err := server.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ln.Accept(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Accept with a cancelled context = %v, want context.Canceled", err)
	}
}

// The whole point of the transport abstraction: the real one and the harness
// one satisfy the same contracts, so the layers above cannot tell them apart.
func TestBothTransportsSatisfyTheSameContracts(t *testing.T) {
	var _ Dialer = (*TCPTransport)(nil)
	var _ Dialer = (*memDialer)(nil)
	var _ Listener = (*tcpListener)(nil)
	var _ Listener = (*memListener)(nil)
	var _ Conn = (*tcpConn)(nil)
	var _ Conn = (*memConn)(nil)
	var _ Stream = (*tcpStream)(nil)
	var _ Stream = (*memStream)(nil)
}
