package device

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/feranmi/ibeji/internal/api"
	"github.com/feranmi/ibeji/internal/link"
	"github.com/feranmi/ibeji/internal/ports"
	"github.com/feranmi/ibeji/internal/remote"
)

// Port Lens, as the daemon wires it.

// Scanner returns this machine's port scanner.
func (d *Daemon) Scanner() *ports.Scanner {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.scanner == nil {
		d.scanner = ports.NewScanner()
	}
	return d.scanner
}

// Tunnels returns the tunnel manager.
func (d *Daemon) TunnelManager() *ports.Manager {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tunnels == nil {
		d.tunnels = ports.NewManager(d.dialTunnel, d.Logger)
		d.tunnels.OnChange = func() {
			if d.events != nil {
				d.events.Publish(api.EventTunnelChanged, nil)
			}
		}
	}
	return d.tunnels
}

// dialTunnel opens a stream the far daemon will join to its own loopback.
func (d *Daemon) dialTunnel(ctx context.Context, remotePort int) (link.Stream, error) {
	conn := d.Conn()
	if conn == nil {
		return nil, link.ErrNotLinked
	}
	stream, err := conn.Open(ctx, link.KindTunnel)
	if err != nil {
		return nil, err
	}
	// The port number is the first thing on the stream; everything after it
	// is the forwarded connection.
	if _, err := fmt.Fprintf(stream, "%d\n", remotePort); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// LocalPorts lists this machine's listening ports.
func (d *Daemon) LocalPorts(ctx context.Context) ([]remote.PortInfo, error) {
	found, err := d.Scanner().Ports(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]remote.PortInfo, 0, len(found))
	for _, p := range found {
		out = append(out, remote.PortInfo{
			Port: p.Port, Process: p.Process, PID: p.PID,
			Service: p.Service, LoopbackOnly: p.LoopbackOnly,
		})
	}
	return out, nil
}

// AllPorts implements api.PortState: both machines, merged.
func (d *Daemon) AllPorts(r *http.Request) ([]ports.Port, api.PortMeta, error) {
	ctx := r.Context()
	local, err := d.Scanner().Ports(ctx)
	if err != nil {
		d.Logger.Debug("ports: local scan failed", "err", err)
	}

	meta := api.PortMeta{}
	var remotePorts []ports.Port

	client, clientErr := d.Remote()
	if clientErr != nil {
		// Stale renders; stale does not act. The local half is still useful,
		// and the UI says why the other half is missing rather than showing
		// an empty list as though nothing were running over there.
		meta.RemoteStale = true
		meta.Reason = fmt.Sprintf("%s is not reachable right now", d.peerOrDefault())
	} else {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		found, err := client.Ports(rctx)
		cancel()
		if err != nil {
			meta.RemoteStale = true
			meta.Reason = fmt.Sprintf("could not read %s's ports: %v", d.peerOrDefault(), err)
		} else {
			for _, p := range found {
				remotePorts = append(remotePorts, ports.Port{
					Port: p.Port, Process: p.Process, PID: p.PID,
					Service: p.Service, LoopbackOnly: p.LoopbackOnly,
				})
			}
		}
	}

	return ports.Combined(local, remotePorts, d.Self().Name, d.peerOrDefault()), meta, nil
}

func (d *Daemon) peerOrDefault() string {
	if n := d.PeerName(); n != "" {
		return n
	}
	return "the other machine"
}

// Tunnels implements api.PortState.
func (d *Daemon) Tunnels() []ports.Tunnel { return d.TunnelManager().List() }

// OpenTunnel implements api.PortState.
func (d *Daemon) OpenTunnel(r *http.Request, port int, name string) (*ports.Tunnel, error) {
	if d.Conn() == nil {
		return nil, link.ErrNotLinked
	}
	// The tunnel outlives this request: it stays open until closed, so it is
	// started from the daemon's own context rather than the HTTP one.
	d.mu.RLock()
	ctx := d.syncCtx
	d.mu.RUnlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return d.TunnelManager().Open(ctx, port, name)
}

// CloseTunnel implements api.PortState.
func (d *Daemon) CloseTunnel(port int) error { return d.TunnelManager().Close(port) }

// PinTunnel implements api.PortState.
func (d *Daemon) PinTunnel(port int, name string) error {
	return d.TunnelManager().Pin(port, name)
}

// ServeTunnelStream joins an inbound tunnel stream to a local port.
func (d *Daemon) ServeTunnelStream(ctx context.Context, stream link.Stream) {
	defer stream.Close()

	// The port number arrives first, as a single line.
	if err := stream.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	var port int
	buf := make([]byte, 0, 8)
	one := make([]byte, 1)
	for len(buf) < 8 {
		if _, err := stream.Read(one); err != nil {
			return
		}
		if one[0] == '\n' {
			break
		}
		buf = append(buf, one[0])
	}
	if _, err := fmt.Sscanf(string(buf), "%d", &port); err != nil {
		return
	}
	_ = stream.SetReadDeadline(time.Time{})

	if err := ports.ServeTunnel(ctx, stream, port); err != nil {
		d.Logger.Debug("ports: could not serve a tunnel", "port", port, "err", err)
	}
}
