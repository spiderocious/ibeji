package device

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/feranmi/ibeji/internal/api"
	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
)

// Reconnecting a pinned peer that moved.
//
// A changed IP address is not a trust event. The keys are already pinned, so
// demanding the six-digit ceremony again would be asking the user to re-prove
// something that has not changed — and until this existed, the device row for
// an unreachable peer offered exactly two buttons: switch its permission
// mode, or unpair. So a peer that moved, or one the daemon could not reach
// because macOS was blocking the local network, left unpairing as the only
// thing to press. That is what a real user did, and it threw away a working
// pairing to work around a network problem.
//
// Re-pairing is still required for exactly one case: the peer presents a
// different key. That is not a machine that moved, it is a different machine,
// and the refusal is the product working.

// ReconnectAt dials the pinned peer at a new address, verifies it is the same
// machine, and remembers where it answered.
func (d *Daemon) ReconnectAt(address string) (*api.Device, error) {
	peer := d.store.Peer()
	if peer == nil {
		return nil, identity.ErrNotPaired
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Expect the pinned key: the handshake itself is the proof that this is
	// the same machine, so a stranger at that address is rejected by TLS
	// before anything else happens.
	transport := &link.TCPTransport{
		Identity: d.store.Self(),
		Name:     d.store.Name(),
		Expect:   peer.PublicKey,
	}

	conn, err := transport.Dial(ctx, address)
	if err != nil {
		if errors.Is(err, identity.ErrKeyMismatch) {
			d.NoteKeyMismatch()
			return nil, fmt.Errorf(
				"%w: the machine at %s is not %s — its key does not match the one pinned. "+
					"If you replaced or reinstalled that Mac, unpair and pair again",
				identity.ErrKeyMismatch, address, peer.Name)
		}
		return nil, err
	}

	// Learn its current name while we are here: a machine that moved may also
	// have been renamed, and the device row should not keep showing the old one.
	name := peer.Name
	if got, _, err := exchangeHello(ctx, conn, d.store.Name(), true, d.peerPort()); err == nil && got != "" {
		name = got
	}

	// Hand the live connection to the supervisor rather than closing it: the
	// point of reconnecting is to be connected, and making the user wait for
	// the next backoff tick would look like it had not worked.
	d.store.SetPeerAddress(address)
	d.store.SetPeerName(name)
	d.SetConn(conn)

	linkCtx, cancelLink := context.WithCancel(context.Background())
	go d.serveStreams(linkCtx, conn)
	go func() {
		if err := heartbeat(linkCtx, conn, d.store.Name(), d.store.Paired); err != nil && linkCtx.Err() == nil {
			conn.Close()
		}
	}()
	go func() {
		<-conn.Done()
		cancelLink()
		if d.Conn() == conn {
			d.SetConn(nil)
		}
	}()

	d.Logger.Info("device: reconnected", "peer", name, "address", address)
	return d.pairedDevice(), nil
}
