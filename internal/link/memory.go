package link

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// MemoryFabric connects daemons running in one process, for tests.
//
// It implements the same Dialer/Listener/Conn/Stream contracts as the real TLS
// transport, so every layer above — pairing, permissions, exec, sync — runs
// unmodified against it. That is what lets the suite test two machines without
// two machines, and it is why the transport is an interface at all.
//
// It deliberately does NOT simulate encryption or key pinning: those live in
// the TLS transport and have their own tests. What it does simulate is the
// things that break logic — latency, a peer going away mid-stream, and streams
// closing independently of the connection.
type MemoryFabric struct {
	mu        sync.Mutex
	listeners map[string]*memListener

	// Latency, when set, delays every write by this much, so a test can expose
	// a race that a zero-latency pipe would hide.
	Latency time.Duration
}

// NewMemoryFabric builds an empty fabric.
func NewMemoryFabric() *MemoryFabric {
	return &MemoryFabric{listeners: map[string]*memListener{}}
}

// Listen registers a listener at a synthetic address.
func (f *MemoryFabric) Listen(address string) (Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.listeners[address]; exists {
		return nil, fmt.Errorf("link: %q is already listening", address)
	}
	l := &memListener{
		fabric:   f,
		address:  address,
		incoming: make(chan *memConn, 4),
		closed:   make(chan struct{}),
	}
	f.listeners[address] = l
	return l, nil
}

// Dialer returns a Dialer onto this fabric. The address identifies the dialer
// itself, so the accepted side can report a sensible RemoteAddr.
func (f *MemoryFabric) Dialer(selfAddress string) Dialer {
	return &memDialer{fabric: f, selfAddress: selfAddress}
}

func (f *MemoryFabric) listener(address string) (*memListener, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.listeners[address]
	return l, ok
}

func (f *MemoryFabric) removeListener(address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.listeners, address)
}

func (f *MemoryFabric) latency() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Latency
}

// --- addresses --------------------------------------------------------------

type memAddr string

func (a memAddr) Network() string { return "memory" }
func (a memAddr) String() string  { return string(a) }

// --- dialer and listener ----------------------------------------------------

type memDialer struct {
	fabric      *MemoryFabric
	selfAddress string
}

func (d *memDialer) Dial(ctx context.Context, address string) (Conn, error) {
	l, ok := d.fabric.listener(address)
	if !ok {
		// The real transport reports this as "peer is not reachable", which on
		// a LAN means asleep rather than broken.
		return nil, fmt.Errorf("%w: nothing listening at %q", ErrNotLinked, address)
	}

	// Two conns sharing two pipes: what A writes, B reads.
	aToB := newPipe()
	bToA := newPipe()

	dialSide := newMemConn(d.fabric, memAddr(address), bToA, aToB, true)
	acceptSide := newMemConn(d.fabric, memAddr(d.selfAddress), aToB, bToA, false)

	select {
	case l.incoming <- acceptSide:
		return dialSide, nil
	case <-l.closed:
		dialSide.Close()
		acceptSide.Close()
		return nil, fmt.Errorf("%w: listener at %q closed", ErrNotLinked, address)
	case <-ctx.Done():
		dialSide.Close()
		acceptSide.Close()
		return nil, ctx.Err()
	}
}

type memListener struct {
	fabric    *MemoryFabric
	address   string
	incoming  chan *memConn
	closed    chan struct{}
	closeOnce sync.Once
}

func (l *memListener) Accept(ctx context.Context) (Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.closed:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *memListener) Addr() net.Addr { return memAddr(l.address) }

func (l *memListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.fabric.removeListener(l.address)
	})
	return nil
}

// --- connection -------------------------------------------------------------

// memConn multiplexes streams over a pair of pipes using a tiny frame header:
// stream id, kind, and payload length. The real transport gets this from its
// mux library; here it is hand-rolled because the harness must not depend on
// the production mux being correct in order to test the layers above it.
type memConn struct {
	fabric *MemoryFabric
	remote net.Addr

	in  *pipe
	out *pipe

	// Odd stream ids for the dialer, even for the accepter, so the two sides
	// never allocate the same id.
	mu       sync.Mutex
	nextID   uint32
	streams  map[uint32]*memStream
	accepted chan *memStream

	writeMu sync.Mutex

	done      chan struct{}
	closeOnce sync.Once
}

func newMemConn(f *MemoryFabric, remote net.Addr, in, out *pipe, dialer bool) *memConn {
	c := &memConn{
		fabric:   f,
		remote:   remote,
		in:       in,
		out:      out,
		streams:  map[uint32]*memStream{},
		accepted: make(chan *memStream, 8),
		done:     make(chan struct{}),
	}
	if dialer {
		c.nextID = 1
	} else {
		c.nextID = 2
	}
	go c.readLoop()
	return c
}

// Frame types on the wire.
const (
	frameOpen  byte = 1
	frameData  byte = 2
	frameClose byte = 3
)

func (c *memConn) Open(ctx context.Context, kind StreamKind) (Stream, error) {
	if !kind.Known() {
		return nil, fmt.Errorf("%w: unknown kind %d", ErrStreamRejected, kind)
	}
	select {
	case <-c.done:
		return nil, ErrClosed
	default:
	}

	c.mu.Lock()
	id := c.nextID
	c.nextID += 2
	s := newMemStream(c, id, kind)
	c.streams[id] = s
	c.mu.Unlock()

	if err := c.writeFrame(frameOpen, id, uint16(kind), nil); err != nil {
		c.mu.Lock()
		delete(c.streams, id)
		c.mu.Unlock()
		return nil, err
	}
	return s, nil
}

func (c *memConn) Accept(ctx context.Context) (Stream, error) {
	select {
	case s := <-c.accepted:
		return s, nil
	case <-c.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *memConn) RemoteAddr() net.Addr { return c.remote }

func (c *memConn) Done() <-chan struct{} { return c.done }

func (c *memConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		streams := make([]*memStream, 0, len(c.streams))
		for _, s := range c.streams {
			streams = append(streams, s)
		}
		c.streams = map[uint32]*memStream{}
		c.mu.Unlock()
		// Every stream on a dead connection ends, which is what lets callers
		// wait on a stream rather than also watching the connection.
		for _, s := range streams {
			s.remoteClosed(ErrClosed)
		}
		c.in.Close()
		c.out.Close()
	})
	return nil
}

// writeFrame serialises one frame. Header: type(1) id(4) kind(2) len(4).
func (c *memConn) writeFrame(typ byte, id uint32, kind uint16, payload []byte) error {
	if d := c.fabric.latency(); d > 0 {
		time.Sleep(d)
	}
	var hdr [11]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], id)
	binary.BigEndian.PutUint16(hdr[5:7], kind)
	binary.BigEndian.PutUint32(hdr[7:11], uint32(len(payload)))

	// One writer at a time, or two goroutines interleave their frames.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.out.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		return c.out.Write(payload)
	}
	return nil
}

func (c *memConn) readLoop() {
	defer c.Close()
	for {
		hdr, err := c.in.ReadFull(11)
		if err != nil {
			return
		}
		typ := hdr[0]
		id := binary.BigEndian.Uint32(hdr[1:5])
		kind := StreamKind(binary.BigEndian.Uint16(hdr[5:7]))
		n := binary.BigEndian.Uint32(hdr[7:11])

		var payload []byte
		if n > 0 {
			payload, err = c.in.ReadFull(int(n))
			if err != nil {
				return
			}
		}

		switch typ {
		case frameOpen:
			if !kind.Known() {
				// Refuse rather than guess: a newer peer must get a clear no.
				_ = c.writeFrame(frameClose, id, uint16(kind), nil)
				continue
			}
			s := newMemStream(c, id, kind)
			c.mu.Lock()
			c.streams[id] = s
			c.mu.Unlock()
			select {
			case c.accepted <- s:
			case <-c.done:
				return
			}
		case frameData:
			c.mu.Lock()
			s := c.streams[id]
			c.mu.Unlock()
			if s != nil {
				s.deliver(payload)
			}
		case frameClose:
			c.mu.Lock()
			s := c.streams[id]
			delete(c.streams, id)
			c.mu.Unlock()
			if s != nil {
				s.remoteClosed(nil)
			}
		}
	}
}

// --- stream -----------------------------------------------------------------

type memStream struct {
	conn *memConn
	id   uint32
	kind StreamKind

	mu       sync.Mutex
	buf      []byte
	readable chan struct{} // signalled when buf gains data or the stream ends
	eofErr   error         // nil means open; io.EOF or ErrClosed once ended
	closed   bool

	deadline      time.Time
	readDeadline  time.Time
	writeDeadline time.Time
}

func newMemStream(c *memConn, id uint32, kind StreamKind) *memStream {
	return &memStream{
		conn:     c,
		id:       id,
		kind:     kind,
		readable: make(chan struct{}, 1),
	}
}

func (s *memStream) Kind() StreamKind { return s.kind }

func (s *memStream) signal() {
	select {
	case s.readable <- struct{}{}:
	default:
	}
}

func (s *memStream) deliver(p []byte) {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	s.mu.Unlock()
	s.signal()
}

func (s *memStream) remoteClosed(err error) {
	s.mu.Lock()
	if s.eofErr == nil {
		if err == nil {
			s.eofErr = io.EOF
		} else {
			s.eofErr = err
		}
	}
	s.mu.Unlock()
	s.signal()
}

func (s *memStream) effectiveReadDeadline() time.Time {
	if !s.readDeadline.IsZero() {
		return s.readDeadline
	}
	return s.deadline
}

func (s *memStream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if len(s.buf) > 0 {
			n := copy(p, s.buf)
			s.buf = s.buf[n:]
			remaining := len(s.buf) > 0
			s.mu.Unlock()
			if remaining {
				s.signal()
			}
			return n, nil
		}
		err := s.eofErr
		dl := s.effectiveReadDeadline()
		s.mu.Unlock()

		if err != nil {
			return 0, err
		}

		var timeout <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, timeoutError{}
			}
			t := time.NewTimer(d)
			defer t.Stop()
			timeout = t.C
		}

		select {
		case <-s.readable:
		case <-timeout:
			return 0, timeoutError{}
		case <-s.conn.done:
			return 0, ErrClosed
		}
	}
}

func (s *memStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	closed := s.closed
	dl := s.writeDeadline
	if dl.IsZero() {
		dl = s.deadline
	}
	s.mu.Unlock()

	if closed {
		return 0, ErrClosed
	}
	if !dl.IsZero() && time.Now().After(dl) {
		return 0, timeoutError{}
	}
	select {
	case <-s.conn.done:
		return 0, ErrClosed
	default:
	}

	// Copy: the caller may reuse its buffer the moment Write returns, and the
	// frame is handed to another goroutine.
	cp := make([]byte, len(p))
	copy(cp, p)
	if err := s.conn.writeFrame(frameData, s.id, uint16(s.kind), cp); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *memStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.conn.mu.Lock()
	delete(s.conn.streams, s.id)
	s.conn.mu.Unlock()

	select {
	case <-s.conn.done:
		return nil
	default:
	}
	return s.conn.writeFrame(frameClose, s.id, uint16(s.kind), nil)
}

func (s *memStream) SetDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *memStream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.readDeadline = t
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *memStream) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.writeDeadline = t
	s.mu.Unlock()
	return nil
}

// timeoutError matches net.Error so callers can branch on Timeout() exactly as
// they would against the real transport.
type timeoutError struct{}

func (timeoutError) Error() string   { return "link: deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}

// --- pipe -------------------------------------------------------------------

// pipe is an unbounded byte pipe. Unbounded on purpose: a bounded one would
// deadlock the hand-rolled mux when both sides write large frames at once, and
// flow control is the production transport's job, not the harness's.
type pipe struct {
	mu     sync.Mutex
	buf    []byte
	wake   chan struct{}
	closed bool
}

func newPipe() *pipe {
	return &pipe{wake: make(chan struct{}, 1)}
}

func (p *pipe) Write(b []byte) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	p.buf = append(p.buf, b...)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

// ReadFull returns exactly n bytes, or an error.
func (p *pipe) ReadFull(n int) ([]byte, error) {
	out := make([]byte, 0, n)
	for len(out) < n {
		p.mu.Lock()
		if len(p.buf) > 0 {
			take := min(n-len(out), len(p.buf))
			out = append(out, p.buf[:take]...)
			p.buf = p.buf[take:]
			more := len(p.buf) > 0
			p.mu.Unlock()
			if more {
				select {
				case p.wake <- struct{}{}:
				default:
				}
			}
			continue
		}
		closed := p.closed
		p.mu.Unlock()
		if closed {
			if len(out) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		}
		<-p.wake
	}
	return out, nil
}

func (p *pipe) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
