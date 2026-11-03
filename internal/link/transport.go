// Package link carries every device-to-device feature over one authenticated,
// encrypted connection.
//
// There is exactly one connection between the two devices. Sync, tunnels, exec,
// handoff and the clipboard are all multiplexed streams on it, rather than each
// opening its own socket: one connection means one place where pinned-key
// verification happens, one reconnect policy, and one answer to "are we
// linked?" for the whole product.
package link

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

var (
	// ErrClosed is returned by operations on a link that has been closed.
	ErrClosed = errors.New("link: closed")

	// ErrNotLinked is returned when an operation needs a live link and the peer
	// is unreachable. On a LAN this is normal, not a failure — the peer sleeps —
	// so callers surface it as "unreachable", never as an error state.
	ErrNotLinked = errors.New("link: peer is not reachable")

	// ErrStreamRejected is returned when the peer declines to open a stream of
	// the requested kind, e.g. an unknown kind from a newer version.
	ErrStreamRejected = errors.New("link: peer rejected the stream")
)

// StreamKind identifies what a multiplexed stream carries. The peer routes an
// incoming stream by this value, so the set is a wire contract: values are
// never renumbered, and an unknown kind is rejected rather than guessed at.
type StreamKind uint16

const (
	// KindControl carries device metadata: name propagation, mode changes,
	// heartbeats. Always available while linked.
	KindControl StreamKind = 1

	// KindRPC carries a request/response API call — list, read, search, stat.
	KindRPC StreamKind = 2

	// KindExec carries a command execution with streamed stdout and stderr.
	KindExec StreamKind = 3

	// KindSync carries space synchronisation: manifests and file chunks.
	KindSync StreamKind = 4

	// KindTunnel carries one forwarded TCP connection for Port Lens.
	KindTunnel StreamKind = 5

	// KindTransfer carries a file drop into the peer's inbox.
	KindTransfer StreamKind = 6
)

// String renders a stream kind for logs and the trace.
func (k StreamKind) String() string {
	switch k {
	case KindControl:
		return "control"
	case KindRPC:
		return "rpc"
	case KindExec:
		return "exec"
	case KindSync:
		return "sync"
	case KindTunnel:
		return "tunnel"
	case KindTransfer:
		return "transfer"
	default:
		return "unknown"
	}
}

// Known reports whether this kind is one this version understands. An unknown
// kind from a newer peer is rejected, never silently dropped: a newer device
// asking for something this one cannot do must get a clear refusal.
func (k StreamKind) Known() bool {
	switch k {
	case KindControl, KindRPC, KindExec, KindSync, KindTunnel, KindTransfer:
		return true
	default:
		return false
	}
}

// Stream is one multiplexed conversation over the link. It behaves like a
// net.Conn: both directions, deadlines, and a close that the peer observes.
type Stream interface {
	io.ReadWriteCloser

	// Kind reports what this stream carries.
	Kind() StreamKind

	// SetDeadline and friends bound a stream's lifetime, so a stuck peer cannot
	// hold a goroutine forever.
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// Conn is an established, authenticated connection to the paired device.
//
// Authentication has already happened by the time a Conn exists: the pinned key
// was verified during the handshake, so no method here needs to re-check it.
type Conn interface {
	// Open starts a new outbound stream of the given kind.
	Open(ctx context.Context, kind StreamKind) (Stream, error)

	// Accept returns the next inbound stream the peer opened.
	Accept(ctx context.Context) (Stream, error)

	// RemoteAddr reports where the peer is, for the device view.
	RemoteAddr() net.Addr

	// Close tears down the connection and every stream on it.
	Close() error

	// Done is closed when the connection is no longer usable, so supervisors
	// can wait on it rather than polling.
	Done() <-chan struct{}
}

// Dialer establishes a connection to a peer at an address.
//
// It is an interface so the two-daemon test harness can substitute an
// in-process pipe for TLS over TCP. Everything above this line — pairing,
// permissions, sync, exec — is then testable without two physical machines,
// which is the single highest-leverage thing in the build.
type Dialer interface {
	Dial(ctx context.Context, address string) (Conn, error)
}

// Listener accepts inbound connections from a peer.
type Listener interface {
	// Accept blocks until a peer connects and the handshake completes.
	Accept(ctx context.Context) (Conn, error)

	// Addr reports where this listener is bound.
	Addr() net.Addr

	// Close stops accepting.
	Close() error
}
