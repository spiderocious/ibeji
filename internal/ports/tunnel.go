package ports

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// A tunnel: listen on localhost here, forward over the link, connect to
// localhost there.
//
// The remote server never changes its bind address, no firewall rule is
// needed, and nobody types an IP. From this machine's point of view the other
// machine's dev server simply is local.

// Tunnel is one forwarded port.
type Tunnel struct {
	// RemotePort is the port on the far machine.
	RemotePort int `json:"remotePort"`

	// LocalPort is where it is reachable here. Usually the same number; a
	// different one when that was already taken.
	LocalPort int `json:"localPort"`

	// Name is a friendly label for a pinned tunnel.
	Name string `json:"name,omitempty"`

	// Pinned tunnels persist across restarts and reconnect by themselves.
	Pinned bool `json:"pinned,omitempty"`

	// Remapped marks a tunnel that could not take its own port number.
	//
	// Always shown. A silent remap is how you spend an hour wondering why the
	// wrong app is answering on localhost:3000.
	Remapped bool `json:"remapped"`

	// Connections counts how many are currently forwarded.
	Connections int `json:"connections"`

	OpenedAt time.Time `json:"openedAt"`

	listener net.Listener
	cancel   context.CancelFunc
}

// Address is where to reach the tunnel on this machine.
func (t *Tunnel) Address() string { return fmt.Sprintf("localhost:%d", t.LocalPort) }

// Mapping renders the forward for display: always visible when remapped.
func (t *Tunnel) Mapping(device string) string {
	if !t.Remapped {
		return fmt.Sprintf("%s:%d → localhost:%d", device, t.RemotePort, t.LocalPort)
	}
	return fmt.Sprintf("%s:%d → localhost:%d (remapped)", device, t.RemotePort, t.LocalPort)
}

var (
	// ErrAlreadyOpen is returned when a tunnel for that remote port exists.
	ErrAlreadyOpen = errors.New("ports: that port is already tunnelled")

	// ErrNoTunnel is returned for an unknown tunnel.
	ErrNoTunnel = errors.New("ports: no such tunnel")
)

// Dialer opens a stream to the peer that will be joined to a remote port.
type Dialer func(ctx context.Context, remotePort int) (link.Stream, error)

// Manager owns this machine's tunnels.
type Manager struct {
	Logger *slog.Logger

	// Dial opens a tunnel stream to the peer.
	Dial Dialer

	// OnChange is called when a tunnel opens or closes, so the window updates
	// without polling.
	OnChange func()

	mu      sync.RWMutex
	tunnels map[int]*Tunnel
}

// NewManager builds a tunnel manager.
func NewManager(dial Dialer, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{Logger: logger, Dial: dial, tunnels: map[int]*Tunnel{}}
}

// Open forwards a remote port to this machine.
//
// It tries the same port number first, because `localhost:3000` is what the
// user already has in their muscle memory and their bookmarks. When that is
// taken it picks another and says so.
func (m *Manager) Open(ctx context.Context, remotePort int, name string) (*Tunnel, error) {
	if remotePort <= 0 || remotePort > 65535 {
		return nil, fmt.Errorf("ports: %d is not a port", remotePort)
	}

	m.mu.Lock()
	if existing, ok := m.tunnels[remotePort]; ok {
		m.mu.Unlock()
		return existing, ErrAlreadyOpen
	}
	m.mu.Unlock()

	listener, localPort, remapped, err := listenPreferring(remotePort)
	if err != nil {
		return nil, err
	}

	tunnelCtx, cancel := context.WithCancel(ctx)
	t := &Tunnel{
		RemotePort: remotePort,
		LocalPort:  localPort,
		Name:       name,
		Remapped:   remapped,
		OpenedAt:   time.Now(),
		listener:   listener,
		cancel:     cancel,
	}

	m.mu.Lock()
	m.tunnels[remotePort] = t
	m.mu.Unlock()

	go m.accept(tunnelCtx, t)

	m.Logger.Info("ports: tunnel open",
		"remote", remotePort, "local", localPort, "remapped", remapped)
	if m.OnChange != nil {
		m.OnChange()
	}
	return t, nil
}

// listenPreferring binds the wanted port, or the next free one.
//
// Loopback only: a tunnel is for this machine, and binding it to every
// interface would quietly republish the far machine's private server to the
// whole network.
func listenPreferring(want int) (net.Listener, int, bool, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", want))
	if err == nil {
		return ln, want, false, nil
	}

	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, false, fmt.Errorf("ports: could not listen locally: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, 0, false, errors.New("ports: unexpected listener address")
	}
	return ln, addr.Port, true, nil
}

// accept forwards each local connection over the link.
func (m *Manager) accept(ctx context.Context, t *Tunnel) {
	defer t.listener.Close()

	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// A closed listener ends the tunnel; anything else is transient.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go m.forward(ctx, t, conn)
	}
}

// forward joins one local connection to a stream on the link.
func (m *Manager) forward(ctx context.Context, t *Tunnel, local net.Conn) {
	defer local.Close()

	m.mu.Lock()
	t.Connections++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		t.Connections--
		m.mu.Unlock()
	}()

	if m.Dial == nil {
		return
	}
	stream, err := m.Dial(ctx, t.RemotePort)
	if err != nil {
		m.Logger.Debug("ports: could not reach the far port",
			"port", t.RemotePort, "err", err)
		return
	}
	defer stream.Close()

	// Copy both ways until either end closes. Two goroutines rather than one
	// loop, because a connection is full duplex and a browser will send while
	// the server is still replying.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stream, local)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, stream)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Close shuts a tunnel.
func (m *Manager) Close(remotePort int) error {
	m.mu.Lock()
	t, ok := m.tunnels[remotePort]
	if !ok {
		m.mu.Unlock()
		return ErrNoTunnel
	}
	delete(m.tunnels, remotePort)
	m.mu.Unlock()

	t.cancel()
	_ = t.listener.Close()
	m.Logger.Info("ports: tunnel closed", "remote", remotePort)
	if m.OnChange != nil {
		m.OnChange()
	}
	return nil
}

// CloseAll shuts every tunnel. Used on unpair, where the PRD requires every
// tunnel to close at once.
func (m *Manager) CloseAll() int {
	m.mu.Lock()
	tunnels := make([]*Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		tunnels = append(tunnels, t)
	}
	m.tunnels = map[int]*Tunnel{}
	m.mu.Unlock()

	for _, t := range tunnels {
		t.cancel()
		_ = t.listener.Close()
	}
	if len(tunnels) > 0 && m.OnChange != nil {
		m.OnChange()
	}
	return len(tunnels)
}

// List returns the open tunnels.
func (m *Manager) List() []Tunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RemotePort < out[j].RemotePort })
	return out
}

// Get returns one tunnel.
func (m *Manager) Get(remotePort int) (*Tunnel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tunnels[remotePort]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// Pin marks a tunnel to be reopened automatically.
//
// This is what turns a demo into a habit: the port you use every day comes
// back by itself after a lid close.
func (m *Manager) Pin(remotePort int, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tunnels[remotePort]
	if !ok {
		return ErrNoTunnel
	}
	t.Pinned = true
	if name != "" {
		t.Name = name
	}
	return nil
}

// Pinned returns the tunnels to reopen on reconnect.
func (m *Manager) Pinned() []Tunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Tunnel
	for _, t := range m.tunnels {
		if t.Pinned {
			out = append(out, *t)
		}
	}
	return out
}

// Reopen restores pinned tunnels after the link comes back.
func (m *Manager) Reopen(ctx context.Context, pinned []Tunnel) {
	for _, p := range pinned {
		if _, ok := m.Get(p.RemotePort); ok {
			continue
		}
		if _, err := m.Open(ctx, p.RemotePort, p.Name); err != nil && !errors.Is(err, ErrAlreadyOpen) {
			m.Logger.Debug("ports: could not reopen a pinned tunnel",
				"port", p.RemotePort, "err", err)
			continue
		}
		_ = m.Pin(p.RemotePort, p.Name)
	}
}

// ServeTunnel is the far side: join an inbound stream to a local port.
//
// Connects to this machine's own loopback, which is how a server bound to
// 127.0.0.1 becomes reachable from the other machine without changing how it
// is bound.
func ServeTunnel(ctx context.Context, stream link.Stream, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("ports: %d is not a port", port)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("ports: nothing is listening on %d here: %w", port, err)
	}
	defer conn.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, stream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(stream, conn)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}
