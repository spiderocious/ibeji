package link

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// The memory fabric is what every later test stands on, so it is tested
// harder than a test helper usually would be: if it has a bug, every suite
// above it reports a failure in the wrong place.

func dialPair(t *testing.T) (Conn, Conn, *MemoryFabric) {
	t.Helper()
	f := NewMemoryFabric()
	l, err := f.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	type accepted struct {
		c   Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := l.Accept(ctx)
		ch <- accepted{c, err}
	}()

	dialer := f.Dialer("laptop")
	a, err := dialer.Dial(ctx, "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	res := <-ch
	if res.err != nil {
		t.Fatalf("Accept: %v", res.err)
	}
	t.Cleanup(func() { a.Close(); res.c.Close() })
	return a, res.c, f
}

func TestDialToNowhereIsNotLinked(t *testing.T) {
	f := NewMemoryFabric()
	_, err := f.Dialer("laptop").Dial(context.Background(), "desktop")
	if !errors.Is(err, ErrNotLinked) {
		t.Fatalf("Dial to an absent listener = %v, want ErrNotLinked", err)
	}
}

func TestListenTwiceOnOneAddressFails(t *testing.T) {
	f := NewMemoryFabric()
	l, err := f.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()
	if _, err := f.Listen("desktop"); err == nil {
		t.Fatal("Listen succeeded twice on one address")
	}
	// After closing, the address is free again — a daemon restart must be able
	// to rebind.
	l.Close()
	l2, err := f.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen after Close: %v", err)
	}
	l2.Close()
}

func TestAddressesAreReported(t *testing.T) {
	a, b, _ := dialPair(t)
	if got := a.RemoteAddr().String(); got != "desktop" {
		t.Fatalf("dialer's RemoteAddr = %q, want desktop", got)
	}
	if got := b.RemoteAddr().String(); got != "laptop" {
		t.Fatalf("accepter's RemoteAddr = %q, want laptop", got)
	}
	if got := a.RemoteAddr().Network(); got != "memory" {
		t.Fatalf("Network = %q, want memory", got)
	}
}

func TestStreamRoundTrip(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()

	sa, err := a.Open(ctx, KindRPC)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if sa.Kind() != KindRPC {
		t.Fatalf("Kind = %v, want rpc", sa.Kind())
	}

	sb, err := b.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if sb.Kind() != KindRPC {
		t.Fatalf("accepted Kind = %v, want rpc", sb.Kind())
	}

	// Both directions.
	if _, err := sa.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := readN(t, sb, 4)
	if string(got) != "ping" {
		t.Fatalf("read %q, want ping", got)
	}

	if _, err := sb.Write([]byte("pong")); err != nil {
		t.Fatalf("Write back: %v", err)
	}
	got = readN(t, sa, 4)
	if string(got) != "pong" {
		t.Fatalf("read %q, want pong", got)
	}
}

func TestWriteDoesNotAliasCallerBuffer(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()

	sa, _ := a.Open(ctx, KindRPC)
	sb, _ := b.Accept(ctx)

	buf := []byte("original")
	if _, err := sa.Write(buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The caller reuses its buffer immediately, as real callers do.
	copy(buf, "MUTATED!")

	got := readN(t, sb, 8)
	if string(got) != "original" {
		t.Fatalf("peer read %q; Write aliased the caller's buffer", got)
	}
}

func TestConcurrentStreamsStaySeparate(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()

	const n = 8
	// Accept in the background and echo each stream's own payload back.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s, err := b.Accept(ctx)
			if err != nil {
				return
			}
			go func(s Stream) {
				buf := make([]byte, 64)
				read, err := s.Read(buf)
				if err != nil {
					return
				}
				_, _ = s.Write(buf[:read])
			}(s)
		}
	}()

	// Open n streams at once, each with a distinct payload, and confirm no
	// frame lands on the wrong stream.
	var mu sync.Mutex
	errs := []error{}
	var cwg sync.WaitGroup
	for i := 0; i < n; i++ {
		cwg.Add(1)
		go func(i int) {
			defer cwg.Done()
			payload := bytes.Repeat([]byte{byte('a' + i)}, 16)
			s, err := a.Open(ctx, KindExec)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			defer s.Close()
			if _, err := s.Write(payload); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			s.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 16)
			if _, err := io.ReadFull(s, buf); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if !bytes.Equal(buf, payload) {
				mu.Lock()
				errs = append(errs, errors.New("stream crosstalk: got "+string(buf)+" want "+string(payload)))
				mu.Unlock()
			}
		}(i)
	}
	cwg.Wait()
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent stream: %v", err)
	}
}

func TestStreamIDsDoNotCollideBetweenSides(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()

	// Both sides open at once. With a shared id space these would collide and
	// one side's data would land on the other's stream.
	sa, err := a.Open(ctx, KindRPC)
	if err != nil {
		t.Fatalf("a.Open: %v", err)
	}
	sb, err := b.Open(ctx, KindRPC)
	if err != nil {
		t.Fatalf("b.Open: %v", err)
	}

	// Each side accepts the other's stream.
	accA, err := a.Accept(ctx)
	if err != nil {
		t.Fatalf("a.Accept: %v", err)
	}
	accB, err := b.Accept(ctx)
	if err != nil {
		t.Fatalf("b.Accept: %v", err)
	}

	if _, err := sa.Write([]byte("from-a")); err != nil {
		t.Fatalf("write from a: %v", err)
	}
	if _, err := sb.Write([]byte("from-b")); err != nil {
		t.Fatalf("write from b: %v", err)
	}
	if got := string(readN(t, accB, 6)); got != "from-a" {
		t.Fatalf("b's accepted stream read %q, want from-a", got)
	}
	if got := string(readN(t, accA, 6)); got != "from-b" {
		t.Fatalf("a's accepted stream read %q, want from-b", got)
	}
}

func TestClosingAStreamGivesPeerEOF(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()

	sa, _ := a.Open(ctx, KindTransfer)
	sb, _ := b.Accept(ctx)

	if _, err := sa.Write([]byte("tail")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sa.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Buffered data must still be readable before EOF — closing must not
	// discard bytes already in flight.
	got := readN(t, sb, 4)
	if string(got) != "tail" {
		t.Fatalf("read %q, want tail", got)
	}
	sb.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := sb.Read(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Fatalf("read after peer Close = %v, want io.EOF", err)
	}
}

func TestClosingAStreamIsIdempotent(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()
	sa, _ := a.Open(ctx, KindRPC)
	_, _ = b.Accept(ctx)

	if err := sa.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sa.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := sa.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}
}

// A peer that goes away mid-stream is the normal case on a LAN — a lid closes.
// Every open stream must end, so a caller waiting on a read does not hang.
func TestClosingTheConnectionEndsEveryStream(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()

	var streams []Stream
	for i := 0; i < 3; i++ {
		s, err := a.Open(ctx, KindSync)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := b.Accept(ctx); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		streams = append(streams, s)
	}

	// A reader blocked on each stream must be released, not left hanging.
	done := make(chan error, len(streams))
	for _, s := range streams {
		go func(s Stream) {
			_, err := s.Read(make([]byte, 8))
			done <- err
		}(s)
	}

	a.Close()

	for i := 0; i < len(streams); i++ {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a blocked read returned nil after the connection closed")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a blocked read did not return after the connection closed")
		}
	}

	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() was not closed after Close()")
	}
}

func TestConnDoneClosesOnPeerClose(t *testing.T) {
	a, b, _ := dialPair(t)
	b.Close()
	select {
	case <-a.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the local side's Done() did not close when the peer closed")
	}
	if _, err := a.Open(context.Background(), KindRPC); !errors.Is(err, ErrClosed) {
		t.Fatalf("Open on a dead connection = %v, want ErrClosed", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	a, _, _ := dialPair(t)
	if err := a.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestUnknownStreamKindIsRejected(t *testing.T) {
	a, _, _ := dialPair(t)
	// A kind from a hypothetical newer peer. It must be refused, not guessed at.
	if _, err := a.Open(context.Background(), StreamKind(9999)); !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("Open with an unknown kind = %v, want ErrStreamRejected", err)
	}
}

func TestReadDeadline(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()
	sa, _ := a.Open(ctx, KindRPC)
	_, _ = b.Accept(ctx)

	if err := sa.SetReadDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	start := time.Now()
	_, err := sa.Read(make([]byte, 4))
	if err == nil {
		t.Fatal("Read past its deadline returned no error")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read error %v does not report Timeout(); callers branch on that", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Read took %v to honour an 80ms deadline", elapsed)
	}
}

func TestDeadlineAppliesWhenReadDeadlineUnset(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()
	sa, _ := a.Open(ctx, KindRPC)
	_, _ = b.Accept(ctx)

	if err := sa.SetDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := sa.Read(make([]byte, 4)); err == nil {
		t.Fatal("SetDeadline did not bound a read")
	}
}

func TestPartialReadsReassemble(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()
	sa, _ := a.Open(ctx, KindSync)
	sb, _ := b.Accept(ctx)

	// Several small writes, one large read: the reader must see a byte stream,
	// not framed messages.
	for _, part := range []string{"in", "-", "sync"} {
		if _, err := sa.Write([]byte(part)); err != nil {
			t.Fatalf("Write %q: %v", part, err)
		}
	}
	sb.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len("in-sync"))
	if _, err := io.ReadFull(sb, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != "in-sync" {
		t.Fatalf("reassembled %q, want in-sync", got)
	}
}

func TestLargePayloadSurvives(t *testing.T) {
	a, b, _ := dialPair(t)
	ctx := context.Background()
	sa, _ := a.Open(ctx, KindSync)
	sb, _ := b.Accept(ctx)

	// 1 MiB, the size a file chunk might be.
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	go func() {
		_, _ = sa.Write(payload)
		sa.Close()
	}()

	sb.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(sb)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("1 MiB payload did not survive: got %d bytes, want %d", len(got), len(payload))
	}
}

// Latency is what the fabric exists to simulate: a race that a zero-latency
// pipe hides. This is the knob the sync-barrier tests will use.
func TestLatencyIsApplied(t *testing.T) {
	f := NewMemoryFabric()
	f.Latency = 20 * time.Millisecond
	l, err := f.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	ctx := context.Background()
	ch := make(chan Conn, 1)
	go func() {
		c, _ := l.Accept(ctx)
		ch <- c
	}()
	a, err := f.Dialer("laptop").Dial(ctx, "desktop")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer a.Close()
	b := <-ch
	defer b.Close()

	sa, _ := a.Open(ctx, KindRPC)
	sb, _ := b.Accept(ctx)

	start := time.Now()
	if _, err := sa.Write([]byte("slow")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = readN(t, sb, 4)
	// Two frames (open + data) at 20ms each; allow slack but require that the
	// latency took effect at all.
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("write completed in %v with 20ms latency configured", elapsed)
	}
}

func TestAcceptRespectsContextCancellation(t *testing.T) {
	a, _, _ := dialPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Accept(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Accept with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestListenerAcceptAfterCloseFails(t *testing.T) {
	f := NewMemoryFabric()
	l, err := f.Listen("desktop")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	l.Close()
	if _, err := l.Accept(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Accept after Close = %v, want ErrClosed", err)
	}
	if got := l.Addr().String(); got != "desktop" {
		t.Fatalf("Addr = %q, want desktop", got)
	}
}

func TestStreamKindStrings(t *testing.T) {
	cases := map[StreamKind]string{
		KindControl:   "control",
		KindRPC:       "rpc",
		KindExec:      "exec",
		KindSync:      "sync",
		KindTunnel:    "tunnel",
		KindTransfer:  "transfer",
		StreamKind(0): "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("StreamKind(%d).String() = %q, want %q", k, got, want)
		}
	}
	for _, k := range []StreamKind{KindControl, KindRPC, KindExec, KindSync, KindTunnel, KindTransfer} {
		if !k.Known() {
			t.Errorf("%v is not Known()", k)
		}
	}
	for _, k := range []StreamKind{0, 7, 9999} {
		if k.Known() {
			t.Errorf("StreamKind(%d) reports Known()", k)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func readN(t *testing.T, s Stream, n int) []byte {
	t.Helper()
	if err := s.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("ReadFull(%d): %v", n, err)
	}
	return buf
}
