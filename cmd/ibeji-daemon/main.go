// Command ibeji-daemon is the Ìbejì background daemon.
//
// It holds the device identity, the link to the paired device, the permission
// engine, the activity log and the trace, and it serves the local HTTP API that
// the app, the CLI and the MCP server are all clients of.
//
// On macOS it must run as a LaunchAgent inside the logged-in user's session —
// never a root LaunchDaemon. Remote commands need the user's keychain, GUI
// session and PATH; a system daemon gets "user interaction not allowed" from
// the keychain and cannot open a browser for handoff.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/feranmi/ibeji/internal/api"
	"github.com/feranmi/ibeji/internal/device"
	"github.com/feranmi/ibeji/internal/discovery"
	"github.com/feranmi/ibeji/internal/doctor"
	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
	"github.com/feranmi/ibeji/internal/trace"
)

// Set by the linker; see the Makefile.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		apiAddr     = flag.String("api", "127.0.0.1:0", "local API address (loopback only)")
		peerPort    = flag.Int("port", link.DefaultPort, "TCP port for peer connections")
		logLevel    = flag.String("log", envOr("IBEJI_LOG", "info"), "log level: debug, info, warn, error")
		noMDNS      = flag.Bool("no-mdns", false, "do not advertise or browse over mDNS")
		instance    = flag.String("instance", envOr("IBEJI_INSTANCE", ""),
			"run an isolated second instance under this name (its own keys and state; for testing two daemons on one machine)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("ibeji-daemon %s (%s, built %s)\n", version, commit, buildDate)
		return
	}

	logger := newLogger(*logLevel)
	if err := run(logger, *apiAddr, *peerPort, *noMDNS, *instance); err != nil {
		logger.Error("daemon: exiting", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, apiAddr string, peerPort int, noMDNS bool, instance string) error {
	// Cancel on SIGINT/SIGTERM so launchd and a terminal both stop us cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// An instance name isolates a second daemon's keys and state from the
	// real one, so two can run on one machine. Without it both would share an
	// identity and a handoff file, and the second would look like the first
	// to anything that connects.
	keychain := identity.NewKeychain()
	stateDir := ""
	if instance != "" {
		keychain.Service = "com.ibeji.daemon." + instance
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home directory: %w", err)
		}
		stateDir = filepath.Join(home, ".ibeji", "instances", instance)
		logger = logger.With("instance", instance)
		logger.Info("daemon: running as an isolated instance", "state", stateDir)
	}

	token, err := identity.APIToken(keychain)
	if err != nil {
		return fmt.Errorf("read API token: %w", err)
	}

	server, err := api.New(token, logger)
	if err != nil {
		return err
	}
	server.StateDir = stateDir

	registry := discovery.NewRegistry()
	d, err := device.New(device.Options{
		Version:  version,
		Logger:   logger,
		Secrets:  keychain,
		Events:   server.Events(),
		Registry: registry,
		Name:     instanceName(instance),
	})
	if err != nil {
		return fmt.Errorf("start device: %w", err)
	}

	logger.Info("daemon: identity loaded",
		"name", d.Self().Name,
		"fingerprint", d.Self().Fingerprint,
		"paired", d.Self().Paired)
	// Name the restored peer. "paired=true" alone cannot distinguish a
	// pairing that came back from one that was never there, which is the
	// exact ambiguity that made losing the pin hard to spot in the log.
	if peer := d.Store().Peer(); peer != nil {
		logger.Info("daemon: pairing restored",
			"peer", peer.Name,
			"address", peer.LastAddress,
			"fingerprint", peer.Fingerprint(),
			"pairedAt", peer.PairedAt.Format(time.RFC3339))
	}

	// Listen for the peer before advertising, so a device that answers our
	// advertisement finds us already accepting.
	// The pin is read at every handshake, not captured here: this listener
	// outlives pairing, and a snapshot would keep accepting a device the user
	// had unpaired.
	transport := &link.TCPTransport{
		Identity:       d.Store().Self(),
		Name:           d.Store().Name(),
		CurrentKey:     d.CurrentPeerKey,
		AcceptUnpaired: true,
	}
	peerLn, err := transport.Listen(ctx, net.JoinHostPort("", strconv.Itoa(peerPort)))
	if err != nil {
		return fmt.Errorf("listen for peer: %w", err)
	}
	defer peerLn.Close()
	d.SetListenAddr(peerLn.Addr().String())
	logger.Info("daemon: accepting peer connections", "addr", peerLn.Addr())

	// The trace goes in with the API, not after it: every call is traced from
	// the first call, because retrofitting means missing the ones you forget.
	d.SetPadPath(filepath.Join(stateDirOr(stateDir), "scratchpad.md"))

	tracePath := filepath.Join(stateDirOr(stateDir), "trace.db")
	tracer, err := trace.Open(tracePath)
	if err != nil {
		// A trace that cannot be opened must not stop the daemon: the product
		// matters more than its diagnostics.
		logger.Warn("daemon: could not open the trace", "path", tracePath, "err", err)
	} else {
		d.SetTrace(tracer)
		defer tracer.Close()
		// Attach it to the API's middleware, so every request is traced from
		// the first request rather than only the ones a handler remembers.
		server.Tracer = apiTracer{tracer}
		logger.Info("daemon: tracing", "path", tracePath)
	}

	saved := api.NewSavedCommands()
	server.RegisterDeviceRoutes(d)
	server.RegisterRemoteRoutes(d)
	server.RegisterDiagnosticRoutes(d)
	server.RegisterSpaceRoutes(d)
	server.RegisterProcessRoutes(d, saved)
	server.RegisterPortRoutes(d)
	server.RegisterTransferRoutes(d)
	api.RegisterShellRoutes(server, d)
	api.RegisterHealthRoutes(server, d)
	api.RegisterLogRoutes(server)
	// The barrier: exec with afterSync waits here before anything runs.
	server.Barrier = d.WaitForPeerSync
	if err := server.Start(apiAddr); err != nil {
		return fmt.Errorf("start API: %w", err)
	}
	// Doctor checks the local API the way a client would, so it needs the
	// same endpoint and token a client reads from the handoff file.
	d.SetAPIDetails(server.Endpoint(), token, keychain)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(shutdownCtx)
	}()

	go d.ServeInbound(ctx, peerLn)
	go d.Supervise(ctx)
	d.StartSyncing(ctx)
	go d.WatchClipboard(ctx)

	if !noMDNS {
		go advertise(ctx, logger, d, peerPort)
		go browse(ctx, logger, d, registry, server)
	}

	logger.Info("daemon: ready", "api", server.Endpoint())

	// Check the local network once at startup and say so loudly if it is
	// blocked.
	//
	// macOS denies local network access silently: every LAN dial returns
	// "no route to host" — the same error an absent machine gives — while
	// the internet keeps working. Without this the first sign is a pairing
	// that fails for no stated reason, which is exactly how this cost a user
	// two days. Run in the background so a slow probe cannot delay startup.
	go func() {
		check := doctor.LocalNetwork()(ctx)
		switch {
		case check.Severity == doctor.Fail:
			logger.Error("daemon: "+check.Detail, "fix", check.Fix)
		case check.Severity == doctor.Warn:
			logger.Warn("daemon: local network " + check.Detail)
		default:
			logger.Info("daemon: local network " + check.Detail)
		}
	}()
	<-ctx.Done()
	logger.Info("daemon: shutting down")
	return nil
}

// advertise publishes this device over mDNS, restarting if the responder stops.
//
// A responder can fail when the network changes — a Wi-Fi switch, a VPN
// interface appearing — and a daemon that gave up would be undiscoverable
// until the user restarted it.
func advertise(ctx context.Context, logger *slog.Logger, d *device.Daemon, port int) {
	m := discovery.NewMDNS()
	for ctx.Err() == nil {
		err := m.Advertise(ctx, d.Advertisement(port))
		if ctx.Err() != nil {
			return
		}
		// Multicast being unavailable is normal on corporate Wi-Fi; the
		// manual-IP path covers it, so this is not an error the user must act
		// on.
		logger.Debug("daemon: mDNS advertisement stopped, retrying", "err", err)
		if !sleepCtx(ctx, 10*time.Second) {
			return
		}
	}
}

// browse watches for peers and feeds the registry.
func browse(ctx context.Context, logger *slog.Logger, d *device.Daemon,
	registry *discovery.Registry, server *api.Server) {

	m := discovery.NewMDNS()
	self := d.Self().Fingerprint

	for ctx.Err() == nil {
		err := m.Browse(ctx, func(c discovery.Candidate) {
			// mDNS answers our own advertisement, once per interface.
			if c.Fingerprint() == self {
				return
			}
			before := registry.Len()
			registry.Add(c)
			if registry.Len() != before {
				logger.Info("daemon: discovered a device",
					"name", c.Name, "address", c.Endpoint())
				server.Events().Publish(api.EventDiscovery, nil)
			}
		})
		if ctx.Err() != nil {
			return
		}
		logger.Debug("daemon: mDNS browse stopped, retrying", "err", err)
		if !sleepCtx(ctx, 10*time.Second) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// instanceName gives an isolated instance a distinguishable default name, so
// two daemons on one machine are tellable apart in a device list.
func instanceName(instance string) string {
	if instance == "" {
		return "" // the device package falls back to the hostname
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "Mac"
	}
	return host + " (" + instance + ")"
}

// stateDirOr resolves where per-instance state lives.
func stateDirOr(dir string) string {
	if dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ibeji"
	}
	return filepath.Join(home, ".ibeji")
}

// apiTracer adapts the trace recorder to the API's middleware.
//
// Payloads are redacted on the way in: the two absolute exclusions — clipboard
// contents and file bodies — are enforced where events are written, never left
// to whoever reads them later.
type apiTracer struct{ rec *trace.Recorder }

func (t apiTracer) Record(ev api.TraceEvent) {
	// Name the endpoint rather than logging every call as "api.request":
	// a trace whose every line reads the same explains nothing.
	typ := ev.Type
	if ev.Path != "" {
		typ = "api" + strings.ReplaceAll(strings.TrimPrefix(ev.Path, "/v1"), "/", ".")
	}
	t.rec.Record(trace.Event{
		At:         ev.At,
		Type:       typ,
		Provenance: ev.Provenance,
		Request: trace.Redact(map[string]any{
			"method": ev.Method,
			"path":   ev.Path,
			"status": ev.Status,
		}),
		Response:   trace.Redact(ev.Response),
		Code:       string(ev.Code),
		Error:      ev.Err,
		DurationMS: ev.DurationMS,
	})
}
