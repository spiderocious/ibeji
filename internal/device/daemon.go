// Package device is the daemon's core: the thing that owns the identity, the
// link to the peer, and the state the API reports.
//
// Everything else in the daemon is a client of this. The API's handlers call
// into it, the discovery loop feeds it, and the link supervisor keeps its
// connection alive — so there is one place that knows whether this machine is
// paired, who with, and whether the other end is reachable right now.
package device

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/feranmi/ibeji/internal/api"
	"github.com/feranmi/ibeji/internal/discovery"
	"github.com/feranmi/ibeji/internal/doctor"
	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/launchagent"
	"github.com/feranmi/ibeji/internal/link"
	"github.com/feranmi/ibeji/internal/permission"
	"github.com/feranmi/ibeji/internal/ports"
	"github.com/feranmi/ibeji/internal/remote"
	"github.com/feranmi/ibeji/internal/space"
	"github.com/feranmi/ibeji/internal/trace"
	"github.com/feranmi/ibeji/internal/transfer"
)

// Daemon holds the device's state and its connection to the peer.
type Daemon struct {
	Version string
	Logger  *slog.Logger

	store    *identity.Store
	secrets  identity.SecretStore
	pairer   *link.Pairer
	registry *discovery.Registry

	// events lets the daemon tell the window what changed, so it does not
	// poll. Nil in tests that are not about events.
	events EventPublisher

	mu sync.RWMutex
	// conn is the live link, or nil when the peer is unreachable.
	conn link.Conn
	// mode is the permission mode this device applies to the peer's requests.
	// Per direction: what the peer applies to ours is the peer's business.
	mode api.Mode
	// keyMismatch records that the last handshake failed on a pinned-key
	// mismatch. Sticky until the user re-pairs, because it is never transient
	// and must not be cleared by a retry succeeding against a different peer.
	keyMismatch bool
	// listenAddr is where this daemon accepts peer connections.
	listenAddr string

	// perms decides every incoming side effect. One engine per device.
	perms *permission.Engine

	// tracer records what happened, for `ibeji trace`.
	tracer *trace.Recorder

	// remoteServer answers the peer's requests. Built lazily and kept, so the
	// process table survives across streams.
	remoteServer *remote.Server

	// spaces holds the shared folders; syncer runs their loops.
	spaces *space.Manager
	syncer *space.Syncer

	// syncCtx is the daemon's lifetime, used to start a loop for a space
	// created after boot, and to outlive the request that opened a tunnel.
	syncCtx context.Context

	// scanner surveys this machine's ports; tunnels forwards the peer's.
	scanner *ports.Scanner
	tunnels *ports.Manager

	// inbox receives dropped files; pad is the shared note.
	inbox   *transfer.Inbox
	pad     *transfer.Scratchpad
	padPath string

	// clipboardOn is off by default: a shared clipboard carries whatever you
	// last copied out of a password manager.
	clipboardOn bool

	// notificationsOff inverts the default, which is on: these are the
	// product's own events, and a sync conflict you are not told about is one
	// you find out about much later.
	notificationsOff bool

	// apiEndpoint, apiToken and doctorSecrets let doctor check the local API
	// and the keychain the way a client would.
	apiEndpoint   string
	apiToken      string
	doctorSecrets doctor.SecretStore
}

// EventPublisher is how the daemon tells clients what changed.
type EventPublisher interface {
	Publish(typ string, data any)
}

// Options configure a daemon.
type Options struct {
	Version  string
	Logger   *slog.Logger
	Secrets  identity.SecretStore
	Name     string
	Events   EventPublisher
	Registry *discovery.Registry
}

// New builds a daemon, loading or generating this device's identity.
func New(opts Options) (*Daemon, error) {
	if opts.Secrets == nil {
		return nil, errors.New("device: no secret store")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	id, err := identity.Load(opts.Secrets)
	if err != nil {
		return nil, err
	}
	name := opts.Name
	if name == "" {
		name = defaultDeviceName()
	}
	registry := opts.Registry
	if registry == nil {
		registry = discovery.NewRegistry()
	}

	spaces := space.NewManager(name)

	// LoadStore, not NewStore: it restores the pinned peer from the keychain
	// so a daemon that restarts comes back paired. NewStore keeps no record,
	// which is why pairing used to vanish on every restart.
	store, err := identity.LoadStore(id, name, opts.Secrets)
	if err != nil {
		return nil, err
	}

	daemon := &Daemon{
		Version:  opts.Version,
		Logger:   logger,
		store:    store,
		secrets:  opts.Secrets,
		pairer:   link.NewPairer(id, name),
		registry: registry,
		events:   opts.Events,
		// Prompt is the default: a machine that asks before running things is
		// the safe starting point, and Bypass is a deliberate, guarded choice.
		mode:   api.ModePrompt,
		perms:  permission.New(nil),
		spaces: spaces,
	}
	// Conflict copies are named after the machine whose version was moved
	// aside, so the manager needs to know who the peer is.
	spaces.PeerName = daemon.PeerName
	return daemon, nil
}

// Store exposes the identity store, for the link supervisor.
func (d *Daemon) Store() *identity.Store { return d.store }

// Pairer exposes the pairing state machine.
func (d *Daemon) Pairer() *link.Pairer { return d.pairer }

// Registry exposes the discovery registry.
func (d *Daemon) Registry() *discovery.Registry { return d.registry }

// SetListenAddr records where this daemon accepts peer connections, so the
// mDNS advertisement and the pairing offer agree with reality.
func (d *Daemon) SetListenAddr(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listenAddr = addr
}

// SetConn records the live link, or nil when it drops.
func (d *Daemon) SetConn(c link.Conn) {
	d.mu.Lock()
	was := d.conn != nil
	d.conn = c
	if c != nil {
		d.keyMismatch = false
	}
	d.mu.Unlock()

	if c != nil {
		d.store.Touch()
	}
	switch {
	case c != nil && !was:
		d.publish(api.EventDeviceOnline, d.pairedDevice())
	case c == nil && was:
		d.publish(api.EventDeviceOffline, d.pairedDevice())
	}
}

// Conn returns the live link, or nil.
func (d *Daemon) Conn() link.Conn {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.conn
}

// NoteKeyMismatch records that the peer presented the wrong key.
//
// Sticky until a successful pairing clears it: a mismatch is never transient,
// and a daemon that forgot it on the next retry would hide the one failure the
// user must be told about.
func (d *Daemon) NoteKeyMismatch() {
	d.mu.Lock()
	already := d.keyMismatch
	d.keyMismatch = true
	d.mu.Unlock()
	if !already {
		d.Logger.Error("device: the paired device presented a different key than the one pinned")
		d.publish(api.EventKeyMismatch, d.pairedDevice())
	}
}

// KeyMismatch reports whether the pinned key check is failing.
func (d *Daemon) KeyMismatch() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.keyMismatch
}

func (d *Daemon) publish(typ string, data any) {
	if d.events != nil {
		d.events.Publish(typ, data)
	}
}

// --- api.DeviceState ---

// Self implements api.DeviceState.
func (d *Daemon) Self() api.SelfInfo {
	return api.SelfInfoFrom(d.store, d.Version, d.peerPort())
}

// peerPort is the port the peer listener is bound to, or zero if it is not
// listening yet. Reported so the window can show a complete address for
// someone to type on the other Mac.
func (d *Daemon) peerPort() int {
	d.mu.Lock()
	addr := d.listenAddr
	d.mu.Unlock()
	if addr == "" {
		return 0
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

// Paired implements api.DeviceState.
func (d *Daemon) Paired() (*api.Device, bool) {
	dev := d.pairedDevice()
	if dev == nil {
		return nil, false
	}
	return dev, true
}

func (d *Daemon) pairedDevice() *api.Device {
	peer := d.store.Peer()
	if peer == nil {
		return nil
	}
	d.mu.RLock()
	online := d.conn != nil
	mode := d.mode
	d.mu.RUnlock()
	return api.DeviceFrom(peer, mode, online)
}

// Candidates implements api.DeviceState.
func (d *Daemon) Candidates() []api.Candidate {
	found := d.registry.Candidates()
	out := make([]api.Candidate, 0, len(found))
	self := d.store.Self().Fingerprint()
	for _, c := range found {
		// Never offer to pair with ourselves: mDNS answers our own
		// advertisement, once per interface.
		if c.Fingerprint() == self {
			continue
		}
		out = append(out, api.CandidateFrom(c))
	}
	return out
}

// Rename implements api.DeviceState.
func (d *Daemon) Rename(name string) error {
	d.store.SetName(name)
	d.Logger.Info("device: renamed", "name", name)
	return nil
}

// SetMode implements api.DeviceState.
func (d *Daemon) SetMode(deviceID string, mode api.Mode) error {
	peer := d.store.Peer()
	if peer == nil {
		return identity.ErrNotPaired
	}
	if deviceID != "" && deviceID != identity.Fingerprint(peer.PublicKey) {
		return api.Errf(api.CodeNotFound, "no device with id %q", deviceID)
	}
	d.mu.Lock()
	d.mode = mode
	engine := d.perms
	d.mu.Unlock()

	// The engine is what actually decides; the stored value is only what the
	// UI reports. Letting them drift would mean a mode the user set having no
	// effect on what runs.
	if engine != nil {
		pm := permission.ModePrompt
		if mode == api.ModeBypass {
			pm = permission.ModeBypass
		}
		if err := engine.SetMode(pm); err != nil {
			return err
		}
	}
	d.Logger.Info("device: permission mode changed", "mode", mode)
	return nil
}

// Mode reports the permission mode applied to the peer's requests.
func (d *Daemon) Mode() api.Mode {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.mode
}

// Unpair implements api.DeviceState.
//
// The PRD requires this to take effect immediately: the pinned key is revoked
// and every tunnel, sync and session closes. Closing the link is how the
// second half happens — everything rides on it, so there is nothing left to
// tear down separately.
func (d *Daemon) Unpair(deviceID string) error {
	peer := d.store.Peer()
	if peer == nil {
		return identity.ErrNotPaired
	}
	if deviceID != "" && deviceID != identity.Fingerprint(peer.PublicKey) {
		return api.Errf(api.CodeNotFound, "no device with id %q", deviceID)
	}

	if err := d.store.Unpair(); err != nil {
		return err
	}
	// Abandon any half-finished ceremony. A live session leaves the listener
	// willing to accept a stranger, and the ex-peer reconnects immediately —
	// which made unpairing look like it had done nothing.
	if n := d.pairer.Clear(); n > 0 {
		d.Logger.Info("device: abandoned pairing attempts on unpair", "count", n)
	}
	// A revoked device's remembered approvals must not survive to greet it if
	// it is ever paired again.
	if d.perms != nil {
		if n := d.perms.Rules().RevokeAll(); n > 0 {
			d.Logger.Info("device: revoked remembered rules on unpair", "count", n)
		}
	}
	// The PRD requires unpairing to close every tunnel at once.
	if d.tunnels != nil {
		if n := d.tunnels.CloseAll(); n > 0 {
			d.Logger.Info("device: closed tunnels on unpair", "count", n)
		}
	}
	d.mu.Lock()
	conn := d.conn
	d.conn = nil
	d.keyMismatch = false
	d.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	// Close every shell: they are reachable only through the pair, and a
	// revoked device must not leave a live shell running on this machine.
	if d.remoteServer != nil && d.remoteServer.Shells != nil {
		d.remoteServer.Shells.CloseAll()
	}
	d.Logger.Info("device: unpaired", "peer", peer.Name)
	d.publish(api.EventDeviceChanged, nil)
	return nil
}

// BeginPairing implements api.DeviceState.
//
// It connects to the address in pairing mode — no key pinned yet — reads the
// key the peer presents, and derives the six digits from both. The connection
// is closed afterwards: pairing establishes trust, and the ordinary link
// supervisor makes the trusted connection.
func (d *Daemon) BeginPairing(address string) (*api.PairingView, error) {
	if d.store.Paired() {
		return nil, identity.ErrAlreadyPaired
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	transport := &link.TCPTransport{
		Identity: d.store.Self(),
		Name:     d.store.Name(),
		// No Expect: this is the one moment the product talks to a key it has
		// not seen, and the six digits are what gate it.
	}
	conn, err := transport.Dial(ctx, address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// The key the peer presented is what the six digits are derived from.
	keyed, ok := conn.(link.KeyedConn)
	if !ok {
		return nil, fmt.Errorf("device: this connection cannot report its peer's key")
	}
	pub, err := keyed.PeerKey()
	if err != nil {
		return nil, err
	}

	// Ask the peer its name, retrying briefly.
	//
	// The connection arrives before the peer's accept loop has necessarily
	// picked it up, so a single attempt races and loses about half the time —
	// which showed up as a ceremony displaying a bare IP instead of "Desktop".
	// A few quick retries close that window without delaying the common case.
	name := address
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		got, _, err := exchangeHello(ctx, conn, d.store.Name(), false, d.peerPort())
		if err == nil {
			name = got
			lastErr = nil
			break
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		// A peer that will not say its name is still pairable; the address
		// stands in, and the fingerprint beside the code is what matters.
		d.Logger.Warn("device: name exchange failed during pairing", "err", lastErr)
	}

	session, err := d.pairer.Begin(link.PairingOffer{
		DeviceName: name,
		PublicKey:  pub,
		Address:    address,
	})
	if err != nil {
		return nil, err
	}
	d.Logger.Info("device: pairing started", "peer", name, "address", address)
	return api.PairingViewFrom(session), nil
}

// PendingPairing implements api.DeviceState.
//
// Pairing state belongs to the daemon, not the window: the ceremony can be
// started from the CLI, and a window opened or reopened while it is in flight
// must show the code rather than an empty device list.
func (d *Daemon) PendingPairing() *api.PairingView {
	d.pairer.Sweep()
	s := d.pairer.Latest()
	if s == nil {
		return nil
	}
	return api.PairingViewFrom(s)
}

// ConfirmPairing implements api.DeviceState.
func (d *Daemon) ConfirmPairing(sessionID string) (*api.Device, error) {
	// v1 is exactly two devices. Confirming a stale session after a pair has
	// been established must not quietly replace it.
	if d.store.Paired() {
		return nil, identity.ErrAlreadyPaired
	}
	offer, err := d.pairer.Confirm(sessionID)
	if err != nil {
		return nil, err
	}
	peer, err := d.store.Pin(offer.PublicKey, offer.DeviceName, offer.Address)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.keyMismatch = false
	d.mu.Unlock()

	d.Logger.Info("device: paired",
		"peer", peer.Name,
		"address", peer.LastAddress,
		"fingerprint", identity.Fingerprint(peer.PublicKey))
	return api.DeviceFrom(peer, d.Mode(), false), nil
}

// CancelPairing implements api.DeviceState.
func (d *Daemon) CancelPairing(sessionID string) error {
	return d.pairer.Cancel(sessionID)
}

// CurrentPeerKey reports the key this device will accept right now, or nil
// when unpaired.
//
// Passed to the listener so the pin is consulted at every handshake. Unpairing
// then takes effect immediately, which is what the PRD requires and what a
// snapshot taken at startup silently failed to do.
func (d *Daemon) CurrentPeerKey() ed25519.PublicKey {
	peer := d.store.Peer()
	if peer == nil {
		return nil
	}
	return peer.PublicKey
}

// PairingOpen reports whether a pairing ceremony is in flight here.
//
// The listener consults it while unpaired: accepting any stranger at all times
// would let an ex-peer reconnect the moment it was unpaired, which made
// revoking a device do nothing visible.
func (d *Daemon) PairingOpen() bool {
	return d.pairer.Active() > 0
}

// --- doctor.LinkState ---

// PeerName reports the paired device's name, for doctor.
func (d *Daemon) PeerName() string {
	if p := d.store.Peer(); p != nil {
		return p.Name
	}
	return ""
}

// PeerAddress reports where the peer was last reachable, for doctor.
func (d *Daemon) PeerAddress() string {
	if p := d.store.Peer(); p != nil {
		return p.LastAddress
	}
	return ""
}

// LinkUp reports whether the link is live, for doctor.
func (d *Daemon) LinkUp() bool { return d.Conn() != nil }

// KeyMatches reports whether the pinned key check is passing, for doctor.
func (d *Daemon) KeyMatches() bool { return !d.KeyMismatch() }

// Advertisement describes this device for mDNS.
func (d *Daemon) Advertisement(port int) discovery.Advertisement {
	return discovery.Advertisement{
		Name:      d.store.Name(),
		Port:      port,
		PublicKey: d.store.Self().Public(),
	}
}

// --- remote, permissions, trace ---

// Remote implements api.RemoteState: a client for the paired device.
func (d *Daemon) Remote() (*remote.Client, error) {
	if !d.store.Paired() {
		return nil, identity.ErrNotPaired
	}
	if d.Conn() == nil {
		// Unreachable is normal on a LAN — the other machine is asleep — so
		// this is a status the UI renders, not an error it alarms about.
		return nil, link.ErrNotLinked
	}
	return &remote.Client{Conn: d.Conn, Provenance: "api"}, nil
}

// RemoteServer answers what the peer asks of this machine.
//
// Built once and reused: it owns the process table, so a dev server started
// through one stream must still be listable and killable through another.
func (d *Daemon) RemoteServer() *remote.Server {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.remoteServer == nil {
		s := remote.NewServer(nil, d.perms, d.Logger)
		s.PeerName = d.PeerName
		s.Ports = d.LocalPorts
		// The shells the other machine's Shell tab types into. One manager
		// for the daemon's life, so a window that closes and reopens finds
		// its tabs still running.
		s.Shells = remote.NewShellManager()
		s.Shells.Logger = d.Logger
		s.Trace = func(ev remote.TraceEvent) {
			d.RecordTrace(trace.Event{
				Type:       ev.Type,
				Device:     ev.Device,
				Provenance: ev.Provenance,
				Request:    trace.Redact(ev.Request),
				Response:   trace.Redact(ev.Response),
				Code:       ev.Code,
				Error:      ev.Error,
				Hint:       ev.Hint,
				DurationMS: ev.DurationMS,
			})
		}
		d.remoteServer = s
	}
	return d.remoteServer
}

// Permissions implements api.DiagnosticState.
func (d *Daemon) Permissions() *permission.Engine { return d.perms }

// Trace implements api.DiagnosticState.
func (d *Daemon) Trace() *trace.Recorder { return d.tracer }

// SetTrace attaches a trace recorder.
func (d *Daemon) SetTrace(r *trace.Recorder) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tracer = r
}

// RecordTrace writes an event, if tracing is on.
//
// Payloads are redacted here rather than at each call site: a filter applied
// where events are written is one place to get right, and the two absolute
// exclusions — clipboard contents, file bodies — must not depend on every
// future caller remembering them.
func (d *Daemon) RecordTrace(ev trace.Event) {
	d.mu.RLock()
	rec := d.tracer
	d.mu.RUnlock()
	if rec == nil {
		return
	}
	rec.Record(ev)
}

// Doctor implements api.DiagnosticState.
func (d *Daemon) Doctor() doctor.Report {
	cfg := doctor.Config{
		LaunchAgentLabel: launchagent.Label,
		Secrets:          d.doctorSecrets,
		Endpoint:         d.apiEndpoint,
		Token:            d.apiToken,
		PeerName:         d.PeerName(),
		Link:             linkState{d},
	}
	if host := d.PeerName(); host != "" {
		cfg.ResolvePeer = doctor.ResolveViaDNS(host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return doctor.Standard(cfg).Run(ctx)
}

// linkState adapts the daemon to doctor.LinkState.
//
// An adapter rather than a method rename: Paired() means "is a device paired"
// to doctor and "which device is paired" to the API, and both readings are
// right in their own context. Contorting either interface to avoid the clash
// would make one of them worse for no gain.
type linkState struct{ d *Daemon }

func (l linkState) Paired() bool        { return l.d.Store().Paired() }
func (l linkState) PeerName() string    { return l.d.PeerName() }
func (l linkState) PeerAddress() string { return l.d.PeerAddress() }
func (l linkState) LinkUp() bool        { return l.d.LinkUp() }
func (l linkState) KeyMatches() bool    { return l.d.KeyMatches() }

// SetAPIDetails records where the local API is, for doctor's own check of it.
func (d *Daemon) SetAPIDetails(endpoint, token string, secrets doctor.SecretStore) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.apiEndpoint = endpoint
	d.apiToken = token
	d.doctorSecrets = secrets
}
