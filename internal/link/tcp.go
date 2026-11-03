package link

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

// DefaultPort is the TCP port the daemon listens on for its peer.
//
// Fixed rather than ephemeral: mDNS advertises it, but a user adding a device
// by IP has only an address to type, and asking for a port number too would
// make the manual path — the one that works on networks mDNS does not — worse
// than it needs to be.
const DefaultPort = 7749

// TCPTransport dials and accepts real connections over TLS with pinned keys.
//
// It implements the same Dialer and Listener contracts as the in-memory fabric,
// so everything above it — pairing, permissions, exec, sync — is identical
// whether it is running against a real machine or inside the test harness.
type TCPTransport struct {
	// Identity is this device's keypair.
	Identity *identity.Identity

	// Name is this device's name, used as the certificate's common name.
	Name string

	// Expect is the peer's pinned public key. Nil during pairing, when no key
	// is pinned yet and the ceremony is what establishes trust.
	Expect ed25519.PublicKey

	// CurrentKey, when set, overrides Expect on the LISTENING side and is
	// consulted at every handshake.
	//
	// The daemon's listener outlives the pin: unpairing must take effect at
	// once, and a listener holding a key captured at startup would go on
	// accepting the device the user just revoked.
	CurrentKey KeyProvider

	// AcceptUnpaired lets the listener accept a connection while no key is
	// pinned, so a ceremony can be started from either machine. The connection
	// grants nothing on its own: the daemon serves control streams over it and
	// nothing else until a key is pinned.
	AcceptUnpaired bool
}

// Dial connects to a peer and completes the pinned handshake.
func (t *TCPTransport) Dial(ctx context.Context, address string) (Conn, error) {
	cfg, err := ClientConfig(t.Identity, t.Name, t.Expect)
	if err != nil {
		return nil, err
	}

	addr := withDefaultPort(address)
	d := &net.Dialer{Timeout: 10 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		// A peer that is asleep is the normal case on a LAN, not a failure.
		return nil, fmt.Errorf("%w: %s: %v", ErrNotLinked, addr, err)
	}

	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		// A key mismatch must survive as itself so the daemon raises a re-pair
		// prompt rather than retrying forever against a machine it will never
		// trust.
		if errors.Is(err, identity.ErrKeyMismatch) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: handshake with %s: %v", ErrNotLinked, addr, err)
	}
	return newTCPConn(conn, true), nil
}

// Listen accepts peer connections on the given address.
//
// The listener is long-lived and the pin is not: the daemon pairs, unpairs and
// re-pairs without restarting. So when CurrentKey is set the pin is read at
// every handshake rather than captured here.
func (t *TCPTransport) Listen(ctx context.Context, address string) (Listener, error) {
	var (
		cfg *tls.Config
		err error
	)
	switch {
	case t.CurrentKey != nil:
		provider := t.CurrentKey
		if t.AcceptUnpaired {
			// While unpaired, accept a stranger so either machine can start
			// the ceremony; once a key is pinned, that key alone.
			cfg, err = ServerConfigEither(t.Identity, t.Name, provider)
		} else {
			cfg, err = ServerConfigLive(t.Identity, t.Name, provider)
		}
	default:
		cfg, err = ServerConfig(t.Identity, t.Name, t.Expect)
	}
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("link: listen on %s: %w", address, err)
	}
	return &tcpListener{inner: ln, cfg: cfg}, nil
}

// withDefaultPort appends the default port when the address carries none, so a
// user can type a bare IP.
func withDefaultPort(address string) string {
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	return net.JoinHostPort(address, fmt.Sprint(DefaultPort))
}

type tcpListener struct {
	inner net.Listener
	cfg   *tls.Config
}

func (l *tcpListener) Accept(ctx context.Context) (Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := l.inner.Accept()
		ch <- result{c, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		conn := tls.Server(r.conn, l.cfg)
		hsCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := conn.HandshakeContext(hsCtx); err != nil {
			r.conn.Close()
			if errors.Is(err, identity.ErrKeyMismatch) {
				return nil, err
			}
			return nil, fmt.Errorf("link: inbound handshake: %w", err)
		}
		return newTCPConn(conn, false), nil
	}
}

func (l *tcpListener) Addr() net.Addr { return l.inner.Addr() }
func (l *tcpListener) Close() error   { return l.inner.Close() }

// --- multiplexed connection over one TLS stream ---

// tcpConn multiplexes streams over a single TLS connection using the same
// frame format as the memory fabric: type, stream id, kind, length.
//
// One connection carries everything — sync, tunnels, exec, handoff — so there
// is one place where the pinned key was verified and one answer to "are we
// linked?" for the whole product.
type tcpConn struct {
	tls *tls.Conn

	mu       sync.Mutex
	nextID   uint32
	streams  map[uint32]*tcpStream
	accepted chan *tcpStream

	writeMu sync.Mutex

	done      chan struct{}
	closeOnce sync.Once
}

func newTCPConn(c *tls.Conn, dialer bool) *tcpConn {
	tc := &tcpConn{
		tls:      c,
		streams:  map[uint32]*tcpStream{},
		accepted: make(chan *tcpStream, 8),
		done:     make(chan struct{}),
	}
	// Odd ids for the dialer, even for the accepter, so the two sides never
	// allocate the same id.
	if dialer {
		tc.nextID = 1
	} else {
		tc.nextID = 2
	}
	go tc.readLoop()
	return tc
}

func (c *tcpConn) Open(ctx context.Context, kind StreamKind) (Stream, error) {
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
	s := newTCPStream(c, id, kind)
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

func (c *tcpConn) Accept(ctx context.Context) (Stream, error) {
	select {
	case s := <-c.accepted:
		return s, nil
	case <-c.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *tcpConn) RemoteAddr() net.Addr  { return c.tls.RemoteAddr() }
func (c *tcpConn) Done() <-chan struct{} { return c.done }

// PeerKey reports the authenticated peer's key, for pairing.
func (c *tcpConn) PeerKey() (ed25519.PublicKey, error) { return PeerKey(c.tls) }

func (c *tcpConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		streams := make([]*tcpStream, 0, len(c.streams))
		for _, s := range c.streams {
			streams = append(streams, s)
		}
		c.streams = map[uint32]*tcpStream{}
		c.mu.Unlock()
		for _, s := range streams {
			s.remoteClosed(ErrClosed)
		}
		c.tls.Close()
	})
	return nil
}

const maxFrame = 1 << 20 // 1 MiB, the sync engine's chunk size

func (c *tcpConn) writeFrame(typ byte, id uint32, kind uint16, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("link: frame of %d bytes exceeds the %d limit", len(payload), maxFrame)
	}
	var hdr [11]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], id)
	binary.BigEndian.PutUint16(hdr[5:7], kind)
	binary.BigEndian.PutUint32(hdr[7:11], uint32(len(payload)))

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.tls.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.tls.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (c *tcpConn) readLoop() {
	defer c.Close()
	hdr := make([]byte, 11)
	for {
		if _, err := io.ReadFull(c.tls, hdr); err != nil {
			return
		}
		typ := hdr[0]
		id := binary.BigEndian.Uint32(hdr[1:5])
		kind := StreamKind(binary.BigEndian.Uint16(hdr[5:7]))
		n := binary.BigEndian.Uint32(hdr[7:11])

		// A peer claiming a huge frame would otherwise allocate it for them.
		if n > maxFrame {
			return
		}
		var payload []byte
		if n > 0 {
			payload = make([]byte, n)
			if _, err := io.ReadFull(c.tls, payload); err != nil {
				return
			}
		}

		switch typ {
		case frameOpen:
			if !kind.Known() {
				_ = c.writeFrame(frameClose, id, uint16(kind), nil)
				continue
			}
			s := newTCPStream(c, id, kind)
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

// tcpStream mirrors memStream; the two share the frame format so a bug fixed
// in one is visible in the other.
type tcpStream struct {
	conn *tcpConn
	id   uint32
	kind StreamKind

	mu       sync.Mutex
	buf      []byte
	readable chan struct{}
	eofErr   error
	closed   bool

	deadline      time.Time
	readDeadline  time.Time
	writeDeadline time.Time
}

func newTCPStream(c *tcpConn, id uint32, kind StreamKind) *tcpStream {
	return &tcpStream{conn: c, id: id, kind: kind, readable: make(chan struct{}, 1)}
}

func (s *tcpStream) Kind() StreamKind { return s.kind }

func (s *tcpStream) signal() {
	select {
	case s.readable <- struct{}{}:
	default:
	}
}

func (s *tcpStream) deliver(p []byte) {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	s.mu.Unlock()
	s.signal()
}

func (s *tcpStream) remoteClosed(err error) {
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

func (s *tcpStream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if len(s.buf) > 0 {
			n := copy(p, s.buf)
			s.buf = s.buf[n:]
			more := len(s.buf) > 0
			s.mu.Unlock()
			if more {
				s.signal()
			}
			return n, nil
		}
		err := s.eofErr
		dl := s.readDeadline
		if dl.IsZero() {
			dl = s.deadline
		}
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

func (s *tcpStream) Write(p []byte) (int, error) {
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

	// Chunk to the frame limit so a large write becomes several frames rather
	// than being refused.
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxFrame)
		if err := s.conn.writeFrame(frameData, s.id, uint16(s.kind), p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (s *tcpStream) Close() error {
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

func (s *tcpStream) SetDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *tcpStream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.readDeadline = t
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *tcpStream) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.writeDeadline = t
	s.mu.Unlock()
	return nil
}

// KeyedConn is a connection that can report the authenticated peer's key.
//
// Pairing needs this: it connects without a pinned key, and the key the peer
// presented is what the six digits are derived from. Declared as an interface
// so the daemon does not reach into the transport's concrete types, and so the
// test harness can offer the same capability.
type KeyedConn interface {
	Conn
	// PeerKey reports the key the peer authenticated with. Valid only after
	// the handshake completed, which is guaranteed once a Conn exists.
	PeerKey() (ed25519.PublicKey, error)
}

var _ KeyedConn = (*tcpConn)(nil)
