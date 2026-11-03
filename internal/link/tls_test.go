package link

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

// handshake dials a real TLS connection over a loopback listener and returns
// both sides' results. Using a real socket rather than a net.Pipe matters: TLS
// handshake failures surface differently, and the dialer must see the rejection
// rather than only the accepter.
func handshake(t *testing.T, clientCfg, serverCfg *tls.Config) (clientErr, serverErr error, client *tls.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type res struct {
		conn *tls.Conn
		err  error
	}
	serverCh := make(chan res, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			serverCh <- res{nil, err}
			return
		}
		c := tls.Server(raw, serverCfg)
		err = c.HandshakeContext(ctxWithTimeout(t))
		serverCh <- res{c, err}
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := tls.Client(raw, clientCfg)
	cErr := c.HandshakeContext(ctxWithTimeout(t))

	s := <-serverCh
	t.Cleanup(func() {
		c.Close()
		if s.conn != nil {
			s.conn.Close()
		}
	})
	return cErr, s.err, c
}

func ctxWithTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPinnedHandshakeSucceeds(t *testing.T) {
	laptop, desktop := mustID(t), mustID(t)

	clientCfg, err := ClientConfig(laptop, "Laptop", desktop.Public())
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	serverCfg, err := ServerConfig(desktop, "Desktop", laptop.Public())
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}

	cErr, sErr, conn := handshake(t, clientCfg, serverCfg)
	if cErr != nil {
		t.Fatalf("client handshake: %v", cErr)
	}
	if sErr != nil {
		t.Fatalf("server handshake: %v", sErr)
	}

	// The authenticated peer key is readable afterwards and is the right one.
	got, err := PeerKey(conn)
	if err != nil {
		t.Fatalf("PeerKey: %v", err)
	}
	if !got.Equal(desktop.Public()) {
		t.Fatal("PeerKey returned a key that is not the peer's")
	}
	if v := conn.ConnectionState().Version; v != tls.VersionTLS13 {
		t.Fatalf("negotiated TLS version %x, want 1.3", v)
	}
	if p := conn.ConnectionState().NegotiatedProtocol; p != "ibeji/1" {
		t.Fatalf("ALPN = %q, want ibeji/1", p)
	}
}

// The property the link's security rests on: a device whose key is not the
// pinned one cannot complete a handshake, however valid its certificate looks.
func TestImpostorIsRejectedAtTheHandshake(t *testing.T) {
	laptop, desktop, impostor := mustID(t), mustID(t), mustID(t)

	// Laptop pinned Desktop, but the impostor answers instead.
	clientCfg, err := ClientConfig(laptop, "Laptop", desktop.Public())
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	// The impostor presents a perfectly well-formed certificate of its own.
	serverCfg, err := ServerConfig(impostor, "Desktop", laptop.Public())
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}

	cErr, _, _ := handshake(t, clientCfg, serverCfg)
	if cErr == nil {
		t.Fatal("the handshake succeeded against an impostor; the pin is not being enforced")
	}
	if !errors.Is(cErr, identity.ErrKeyMismatch) {
		t.Fatalf("handshake error = %v, want identity.ErrKeyMismatch so the daemon can raise a re-pair prompt", cErr)
	}
	// The message must carry both fingerprints: the key-mismatch dialog shows
	// them side by side, and a bare "mismatch" would make that impossible.
	if msg := cErr.Error(); !contains(msg, "SHA256:") {
		t.Fatalf("mismatch error %q does not carry a fingerprint", msg)
	}
}

// Trust is mutual: the accepting side must also refuse a stranger, or a device
// that unpaired could still be connected to by its ex-peer.
func TestServerRejectsUnpinnedClient(t *testing.T) {
	laptop, desktop, stranger := mustID(t), mustID(t), mustID(t)

	clientCfg, err := ClientConfig(stranger, "Stranger", desktop.Public())
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	serverCfg, err := ServerConfig(desktop, "Desktop", laptop.Public())
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}

	_, sErr, _ := handshake(t, clientCfg, serverCfg)
	if sErr == nil {
		t.Fatal("the server accepted a client it had not pinned")
	}
	if !errors.Is(sErr, identity.ErrKeyMismatch) {
		t.Fatalf("server error = %v, want identity.ErrKeyMismatch", sErr)
	}
}

// A reinstalled machine presents a new key under the same name. It must fail
// exactly like an impostor, because from here the two are indistinguishable.
func TestReinstalledPeerFailsTheHandshake(t *testing.T) {
	laptop, desktopBefore := mustID(t), mustID(t)
	desktopAfter := mustID(t) // same machine, fresh install

	clientCfg, err := ClientConfig(laptop, "Laptop", desktopBefore.Public())
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	serverCfg, err := ServerConfig(desktopAfter, "Desktop", laptop.Public())
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}

	cErr, _, _ := handshake(t, clientCfg, serverCfg)
	if !errors.Is(cErr, identity.ErrKeyMismatch) {
		t.Fatalf("reinstalled peer = %v, want identity.ErrKeyMismatch", cErr)
	}
}

// During pairing no key is pinned yet. The handshake must still complete — the
// ceremony needs a connection to run over — but a certificate is still
// required, so there is always a fingerprint to show beside the code.
func TestPairingHandshakeAcceptsAnyKeyButRequiresOne(t *testing.T) {
	laptop, desktop := mustID(t), mustID(t)

	clientCfg, err := ClientConfig(laptop, "Laptop", nil)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	serverCfg, err := ServerConfig(desktop, "Desktop", nil)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}

	cErr, sErr, conn := handshake(t, clientCfg, serverCfg)
	if cErr != nil || sErr != nil {
		t.Fatalf("pairing handshake failed: client=%v server=%v", cErr, sErr)
	}

	// And the key is readable, so the ceremony can show its fingerprint.
	got, err := PeerKey(conn)
	if err != nil {
		t.Fatalf("PeerKey: %v", err)
	}
	if !got.Equal(desktop.Public()) {
		t.Fatal("the key read after pairing handshake is not the peer's")
	}
	if identity.Fingerprint(got) == "" {
		t.Fatal("no fingerprint available to show beside the pairing code")
	}
}

// A client that presents no certificate at all must be refused, so an
// unauthenticated inbound connection never reaches the daemon's logic.
func TestServerRequiresAClientCertificate(t *testing.T) {
	desktop, laptop := mustID(t), mustID(t)

	serverCfg, err := ServerConfig(desktop, "Desktop", laptop.Public())
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	// A bare TLS client with no certificate of its own.
	clientCfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // deliberately permissive: this is the attacker
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"ibeji/1"},
	}

	_, sErr, _ := handshake(t, clientCfg, serverCfg)
	if sErr == nil {
		t.Fatal("the server completed a handshake with a client that presented no certificate")
	}
}

func TestCertificateCarriesTheIdentityKey(t *testing.T) {
	id := mustID(t)
	cert, err := selfSignedCert(id, "Laptop")
	if err != nil {
		t.Fatalf("selfSignedCert: %v", err)
	}
	if len(cert.Certificate) != 1 {
		t.Fatalf("certificate has %d entries, want 1 (self-signed, no chain)", len(cert.Certificate))
	}
	parsed, err := parseLeaf(cert)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	pub, ok := parsed.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("certificate holds a %T, want an Ed25519 key", parsed.PublicKey)
	}
	if !pub.Equal(id.Public()) {
		t.Fatal("the certificate does not carry this device's identity key")
	}
	if parsed.Subject.CommonName != "Laptop" {
		t.Fatalf("CommonName = %q, want Laptop", parsed.Subject.CommonName)
	}
	// Valid now, and valid long enough that a year of uptime or a drifting
	// clock does not make a device unreachable.
	now := time.Now()
	if parsed.NotBefore.After(now) {
		t.Fatal("certificate is not yet valid; clock skew would break pairing")
	}
	if parsed.NotAfter.Before(now.Add(5 * 365 * 24 * time.Hour)) {
		t.Fatalf("certificate expires at %v, sooner than expected", parsed.NotAfter)
	}
}

func TestPeerKeyBeforeHandshakeFails(t *testing.T) {
	id := mustID(t)
	cfg, err := ClientConfig(id, "Laptop", nil)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	// A conn whose handshake never ran.
	c := tls.Client(&nopConn{}, cfg)
	if _, err := PeerKey(c); err == nil {
		t.Fatal("PeerKey returned a key before the handshake completed")
	}
}

func TestVerifyPinnedRejectsEmptyState(t *testing.T) {
	expect := mustID(t).Public()
	err := verifyPinned(expect)(tls.ConnectionState{})
	if !errors.Is(err, ErrNoPeerCertificate) {
		t.Fatalf("verifyPinned with no certificate = %v, want ErrNoPeerCertificate", err)
	}
}

func TestTLSEchoOverAPinnedLink(t *testing.T) {
	laptop, desktop := mustID(t), mustID(t)
	clientCfg, _ := ClientConfig(laptop, "Laptop", desktop.Public())
	serverCfg, _ := ServerConfig(desktop, "Desktop", laptop.Public())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		s := tls.Server(raw, serverCfg)
		defer s.Close()
		if err := s.HandshakeContext(context.Background()); err != nil {
			return
		}
		buf := make([]byte, 64)
		n, err := s.Read(buf)
		if err != nil {
			return
		}
		_, _ = s.Write(buf[:n])
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := tls.Client(raw, clientCfg)
	defer c.Close()
	if err := c.HandshakeContext(context.Background()); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	msg := []byte("git push origin main")
	if _, err := c.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echoed %q, want %q", got, msg)
	}
}

// --- helpers ---------------------------------------------------------------

// nopConn is a net.Conn that does nothing, for testing pre-handshake state.
type nopConn struct{}

func (nopConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (nopConn) Write(b []byte) (int, error)      { return len(b), nil }
func (nopConn) Close() error                     { return nil }
func (nopConn) LocalAddr() net.Addr              { return memAddr("local") }
func (nopConn) RemoteAddr() net.Addr             { return memAddr("remote") }
func (nopConn) SetDeadline(time.Time) error      { return nil }
func (nopConn) SetReadDeadline(time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(time.Time) error { return nil }

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
