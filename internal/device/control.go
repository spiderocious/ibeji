package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// The control stream carries the small facts two devices tell each other:
// names, and later permission-mode changes and heartbeats.
//
// It is deliberately the simplest thing on the link — one JSON message each
// way, with a deadline — because it runs during pairing, before any trust
// exists, and a complicated protocol there is a complicated attack surface
// reachable by anything that can open a TCP connection.

// controlHello is the first message on a control stream.
type controlHello struct {
	DeviceName string `json:"deviceName"`
	Version    string `json:"version"`

	// Paired reports whether the sender still has the recipient pinned.
	//
	// A successful TLS handshake proves the peer is the device we pinned. It
	// does not prove the peer still pins *us*: an unpaired device accepts
	// connections so a new ceremony can start, so without this an ex-peer
	// would reconnect and both sides would report themselves linked a second
	// after one of them revoked the other.
	Paired bool `json:"paired"`

	// PeerPort is where the sender *listens*, which is not where it dialled
	// from. The accepting side of a ceremony sees only an ephemeral source
	// port on the connection — 54603, say — and pinning that gives an address
	// nothing is bound to, so it can never dial back. Zero from an older peer,
	// in which case the default port is the best guess available.
	PeerPort int `json:"peerPort,omitempty"`
}

// peerAddress rebuilds a dialable address from a connection's remote host and
// the port the peer says it listens on.
//
// The host comes from the connection, never from the message: a peer must not
// be able to point this machine at a third address.
func peerAddress(remote string, port int) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if port <= 0 {
		port = link.DefaultPort
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// ErrPeerUnpaired means the peer answered but no longer has us paired.
var ErrPeerUnpaired = errors.New("device: the other machine has unpaired from this one")

// controlTimeout bounds a control exchange. Short: both sides are already
// connected, and a peer that cannot answer in this long is not going to.
const controlTimeout = 10 * time.Second

// heartbeatInterval is how often the link is checked.
//
// Without it a half-open connection — the peer's process killed, its lid shut,
// its Wi-Fi gone — reads as live indefinitely, because TCP does not notice
// until something is written. That was a real bug: a device showed "live" for
// a minute after its peer had unpaired and closed.
const heartbeatInterval = 10 * time.Second

// exchangeNames tells the peer our name and learns theirs.
//
// Used during pairing so the ceremony can show "Desktop" rather than an IP,
// and after reconnecting so a rename propagates. selfPaired tells the peer
// whether we still have it pinned.
func exchangeNames(ctx context.Context, conn link.Conn, selfName string, selfPaired bool) (string, error) {
	name, _, err := exchangeHello(ctx, conn, selfName, selfPaired, 0)
	return name, err
}

// exchangeHello is exchangeNames plus the peer's listening port.
func exchangeHello(ctx context.Context, conn link.Conn, selfName string, selfPaired bool, selfPort int) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()

	stream, err := conn.Open(ctx, link.KindControl)
	if err != nil {
		return "", 0, fmt.Errorf("device: open control stream: %w", err)
	}
	defer stream.Close()

	deadline := time.Now().Add(controlTimeout)
	if err := stream.SetDeadline(deadline); err != nil {
		return "", 0, err
	}

	if err := json.NewEncoder(stream).Encode(controlHello{
		DeviceName: selfName,
		Paired:     selfPaired,
		PeerPort:   selfPort,
	}); err != nil {
		return "", 0, fmt.Errorf("device: send hello: %w", err)
	}

	var peer controlHello
	// Bound the decode: a peer streaming an endless name must not be able to
	// exhaust memory here, and this runs before any trust exists.
	dec := json.NewDecoder(&limitedReader{r: stream, n: 8 << 10})
	if err := dec.Decode(&peer); err != nil {
		return "", 0, fmt.Errorf("device: read hello: %w", err)
	}
	name := sanitiseName(peer.DeviceName)
	if name == "" {
		return "", 0, fmt.Errorf("device: peer sent no usable name")
	}
	// If we think we are paired but the peer does not, this is not a link.
	if selfPaired && !peer.Paired {
		return name, peer.PeerPort, ErrPeerUnpaired
	}
	return name, peer.PeerPort, nil
}

// serveControl answers an inbound control stream, reporting the peer's name.
//
// The name is returned rather than discarded because the accepting side of a
// pairing ceremony needs it: it has to raise a session of its own, and that
// session is what puts "Laptop" rather than a bare IP beside the six digits.
func serveControl(stream link.Stream, selfName string, selfPaired bool) (string, error) {
	name, _, err := serveHello(stream, selfName, selfPaired, 0)
	return name, err
}

// serveHello is serveControl plus the ports: it tells the peer where this
// machine listens, and reports where the peer says it listens.
func serveHello(stream link.Stream, selfName string, selfPaired bool, selfPort int) (string, int, error) {
	defer stream.Close()
	if err := stream.SetDeadline(time.Now().Add(controlTimeout)); err != nil {
		return "", 0, err
	}

	var peer controlHello
	dec := json.NewDecoder(&limitedReader{r: stream, n: 8 << 10})
	if err := dec.Decode(&peer); err != nil {
		return "", 0, fmt.Errorf("device: read hello: %w", err)
	}
	if err := json.NewEncoder(stream).Encode(controlHello{
		DeviceName: selfName,
		Paired:     selfPaired,
		PeerPort:   selfPort,
	}); err != nil {
		return sanitiseName(peer.DeviceName), peer.PeerPort, fmt.Errorf("device: send hello: %w", err)
	}
	return sanitiseName(peer.DeviceName), peer.PeerPort, nil
}

// sanitiseName makes a peer-supplied name safe to render.
//
// It arrives from another machine, so it is untrusted input: control
// characters would corrupt a terminal listing devices, and an unbounded name
// would break the device row's layout.
func sanitiseName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 63 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// limitedReader caps how much a peer can make us read.
type limitedReader struct {
	r link.Stream
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, fmt.Errorf("device: peer sent more than the allowed %d bytes", 8<<10)
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// defaultDeviceName names this machine before the user picks something.
//
// The hostname is what they would type anyway, minus the .local suffix macOS
// adds and the hyphenation it does to a name with spaces.
func defaultDeviceName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "This Mac"
	}
	host = strings.TrimSuffix(host, ".local")
	host = strings.TrimSuffix(host, ".lan")
	if name := sanitiseName(host); name != "" {
		return name
	}
	return "This Mac"
}

// heartbeat keeps checking that the peer is still there.
//
// It opens a control stream on an interval and waits for the answer. A peer
// whose process died, whose lid shut, or whose network vanished fails this
// within one interval; without it the link would read as live until something
// tried to use it, which on a LAN product could be hours.
func heartbeat(ctx context.Context, conn link.Conn, selfName string, selfPaired func() bool) error {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Done():
			return nil
		case <-t.C:
			if _, err := exchangeNames(ctx, conn, selfName, selfPaired()); err != nil {
				return fmt.Errorf("device: heartbeat failed: %w", err)
			}
		}
	}
}
