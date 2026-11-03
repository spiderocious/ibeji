package device

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
)

// The supervisor keeps the link to the peer alive.
//
// On a LAN the peer is unreachable most of the day: the lid is shut, the Wi-Fi
// dropped, the machine renewed its lease. None of that is a failure, so the
// supervisor retries quietly and forever rather than reporting errors — the
// device row says "unreachable" and that is the whole story the user needs.
//
// The one thing it does not retry is a pinned-key mismatch. That is never
// transient and must never be papered over by a later attempt succeeding, so
// it stops dialling until the user re-pairs.

const (
	// minBackoff is how soon a dropped link is retried. Short, because a
	// laptop waking from sleep should reconnect before the user notices.
	minBackoff = 1 * time.Second

	// maxBackoff caps the retry interval. A minute is frequent enough that a
	// machine waking up is picked up quickly, and rare enough that a peer
	// switched off overnight costs nothing.
	maxBackoff = 60 * time.Second
)

// Supervise maintains the outbound link until ctx is cancelled.
func (d *Daemon) Supervise(ctx context.Context) {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		// Nothing to connect to until a device is paired.
		if !d.store.Paired() {
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}

		// A key mismatch is terminal until the user acts. Retrying would
		// hammer a machine we will never trust and would bury the one message
		// that matters.
		if d.KeyMismatch() {
			if !sleep(ctx, 10*time.Second) {
				return
			}
			continue
		}

		if d.Conn() != nil {
			if !sleep(ctx, time.Second) {
				return
			}
			continue
		}

		// Only one side dials, or the two displace each other's connections
		// forever. The other side waits for the inbound link.
		if !d.shouldDial() {
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}

		conn, err := d.dialPeer(ctx)
		if err != nil {
			if errors.Is(err, identity.ErrKeyMismatch) {
				d.NoteKeyMismatch()
				continue
			}
			if ctx.Err() != nil {
				return
			}
			// Unreachable is the normal case; log it once per backoff step at
			// debug level rather than filling the log with it.
			d.Logger.Debug("device: peer unreachable", "err", err, "retry_in", backoff)
			if !sleep(ctx, jitter(backoff)) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = minBackoff
		d.Logger.Info("device: linked", "peer", d.PeerName(), "address", conn.RemoteAddr())
		d.SetConn(conn)

		// Tell the peer our name and learn theirs, so a rename propagates.
		name, err := exchangeNames(ctx, conn, d.store.Name(), true)
		if errors.Is(err, ErrPeerUnpaired) {
			// The other machine revoked us. Keep our own pin — the user may
			// want to see the device and re-pair — but do not pretend to be
			// linked to something that will not talk to us.
			d.Logger.Info("device: the other machine has unpaired from this one", "peer", name)
			conn.Close()
			d.SetConn(nil)
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}
		if err == nil {
			d.store.SetPeerName(name)
		}

		// A heartbeat notices a peer that went away without closing — a lid
		// shutting, a process killed, a network vanishing. TCP alone would not.
		linkCtx, cancelLink := context.WithCancel(ctx)
		go func() {
			if err := heartbeat(linkCtx, conn, d.store.Name(), d.store.Paired); err != nil && linkCtx.Err() == nil {
				d.Logger.Info("device: peer stopped answering", "err", err)
				conn.Close()
			}
		}()

		// Hold until the link drops, then loop round and redial.
		select {
		case <-conn.Done():
			d.Logger.Info("device: link dropped")
			cancelLink()
			d.SetConn(nil)
		case <-ctx.Done():
			cancelLink()
			conn.Close()
			d.SetConn(nil)
			return
		}
	}
}

// dialPeer connects to the pinned peer, trying mDNS first and the last known
// address second.
//
// Both paths matter. mDNS finds a machine that changed IP; the last known
// address works on a network where multicast is blocked, which is exactly the
// network this product is most useful on.
func (d *Daemon) dialPeer(ctx context.Context) (link.Conn, error) {
	peer := d.store.Peer()
	if peer == nil {
		return nil, identity.ErrNotPaired
	}

	transport := &link.TCPTransport{
		Identity: d.store.Self(),
		Name:     d.store.Name(),
		Expect:   peer.PublicKey,
	}

	// Addresses to try, freshest first, without duplicates.
	var addrs []string
	seen := map[string]bool{}
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			addrs = append(addrs, a)
		}
	}
	fp := identity.Fingerprint(peer.PublicKey)
	if c, ok := d.registry.Lookup(fp); ok {
		add(c.Endpoint())
	}
	add(peer.LastAddress)

	if len(addrs) == 0 {
		return nil, link.ErrNotLinked
	}

	var lastErr error
	for _, addr := range addrs {
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := transport.Dial(dialCtx, addr)
		cancel()
		if err == nil {
			// Remember where it answered, so the next reconnect tries there
			// first even if mDNS is unavailable.
			d.store.SetPeerAddress(addr)
			return conn, nil
		}
		// A key mismatch is conclusive: stop, do not try the next address.
		if errors.Is(err, identity.ErrKeyMismatch) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// shouldDial reports whether this device is the one that opens the connection.
//
// Both devices listen, but only one dials: the device whose public key sorts
// lower. Without a rule, both dial at once, each accepts the other, and each
// new connection displaces the previous one — a hot loop that logged 46,000
// link drops in fifteen seconds and would have pegged a core on both machines,
// wrecking the "idle CPU near zero" requirement.
//
// Comparing keys is deterministic and needs no negotiation: both sides already
// know both keys, and they always reach the same answer.
func (d *Daemon) shouldDial() bool {
	peer := d.store.Peer()
	if peer == nil {
		return false
	}
	return bytes.Compare(d.store.Self().Public(), peer.PublicKey) < 0
}

// ServeInbound accepts connections from the peer until ctx is cancelled.
//
// Both devices listen; only the lower-keyed one dials. The listener stays open
// on both so either can be the one that was asleep.
func (d *Daemon) ServeInbound(ctx context.Context, ln link.Listener) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, identity.ErrKeyMismatch) {
				d.NoteKeyMismatch()
				continue
			}
			d.Logger.Debug("device: inbound connection failed", "err", err)
			if !sleep(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}

		// While unpaired, an inbound connection is a pairing conversation, not
		// a link: the other machine is asking who we are so its ceremony can
		// show our name beside the code. Treating it as a link was a bug —
		// SetConn plus a heartbeat tore the connection down before the peer
		// could read the reply, so every ceremony fell back to showing a bare
		// IP address instead of the device's name.
		if !d.store.Paired() {
			go d.servePairing(ctx, conn)
			continue
		}

		// A connection while one is already live happens when both sides
		// dialled at once. Keep the existing one; a second link would mean two
		// of everything.
		if d.Conn() != nil {
			conn.Close()
			continue
		}

		d.Logger.Info("device: accepted link", "from", conn.RemoteAddr())
		d.SetConn(conn)
		go d.serveStreams(ctx, conn)
		// The accepting side heartbeats too: either end may be the one whose
		// peer vanishes.
		go func() {
			if err := heartbeat(ctx, conn, d.store.Name(), d.store.Paired); err != nil && ctx.Err() == nil {
				d.Logger.Info("device: peer stopped answering", "err", err)
				conn.Close()
			}
		}()
	}
}

// servePairing answers an inbound connection from an unpaired device.
//
// It serves control streams only, for as long as the other side keeps the
// connection open, and claims no link: nothing is trusted yet, so nothing but
// the name exchange is allowed, and the daemon's device list stays empty until
// the ceremony completes.
func (d *Daemon) servePairing(ctx context.Context, conn link.Conn) {
	defer conn.Close()

	// A ceremony that has not finished within the pairing window is over.
	ctx, cancel := context.WithTimeout(ctx, link.PairingWindow)
	defer cancel()

	for {
		stream, err := conn.Accept(ctx)
		if err != nil {
			return
		}
		if stream.Kind() != link.KindControl {
			// Nothing else is permitted before trust exists.
			stream.Close()
			continue
		}
		go func() {
			// Unpaired here, so Paired is false: that is how the other side
			// learns we have revoked it rather than silently relinking.
			if err := serveControl(stream, d.store.Name(), d.store.Paired()); err != nil {
				d.Logger.Debug("device: pairing control stream failed", "err", err)
			}
		}()
	}
}

// serveStreams handles what the peer opens on an established link.
func (d *Daemon) serveStreams(ctx context.Context, conn link.Conn) {
	defer func() {
		conn.Close()
		// Only clear the connection if it is still the live one: by now the
		// supervisor may have replaced it.
		if d.Conn() == conn {
			d.SetConn(nil)
		}
	}()

	for {
		stream, err := conn.Accept(ctx)
		if err != nil {
			return
		}
		switch stream.Kind() {
		case link.KindControl:
			go func() {
				if err := serveControl(stream, d.store.Name(), d.store.Paired()); err != nil {
					d.Logger.Debug("device: control stream failed", "err", err)
				}
			}()
		default:
			// Kinds whose handlers land in later milestones. Closing is the
			// honest answer: the peer gets a clear end rather than a hang.
			d.Logger.Debug("device: no handler for stream kind yet", "kind", stream.Kind())
			stream.Close()
		}
	}
}

// sleep waits unless the context is cancelled. Reports false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// nextBackoff doubles up to the cap.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// jitter spreads retries so two machines waking together do not lock into
// dialling each other at the same instant and missing every time.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := time.Duration(rand.Int63n(int64(d / 4)))
	return d - (d / 8) + delta
}
