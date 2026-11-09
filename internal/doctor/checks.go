package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// The six checks, in dependency order. A failure high in the list explains
// failures below it, which is why Run sorts rather than racing: a report that
// says "link down" without saying "because the daemon is not running" sends the
// reader looking in the wrong place.
const (
	OrderLaunchAgent = 10
	OrderKeychain    = 20
	OrderAPIToken    = 30
	// Local network comes BEFORE mDNS and the link, because when macOS has
	// denied it both of those fail as a consequence and their messages send
	// you looking in the wrong place.
	OrderLocalNet   = 35
	OrderMDNS       = 40
	OrderLink       = 50
	OrderLoginShell = 60
)

// SecretStore is the subset of the keychain doctor needs.
type SecretStore interface {
	Reachable() error
	Warning() string
}

// LinkState is what the daemon knows about pairing and the link.
type LinkState interface {
	Paired() bool
	PeerName() string
	PeerAddress() string
	LinkUp() bool
	// KeyMatches reports whether the last handshake matched the pinned key.
	// A mismatch is the one failure here that is never transient.
	KeyMatches() bool
}

// LaunchAgent returns a check that the daemon is installed as a LaunchAgent in
// the user's session — never a root LaunchDaemon.
//
// The placement is a requirement, not an implementation detail: a system daemon
// gets "user interaction not allowed" from the keychain and cannot open a
// browser for handoff. When it is wrong, every other check fails in a way that
// does not name the cause, so this one runs first.
func LaunchAgent(label string) CheckFunc {
	return func(ctx context.Context) Check {
		const name = "LaunchAgent"

		if runtime.GOOS != "darwin" {
			return Warned(name,
				fmt.Sprintf("not applicable on %s", runtime.GOOS),
				"macOS is the supported platform for v1; start the daemon however this system does it.")
		}

		out, err := exec.CommandContext(ctx, "launchctl", "list").Output()
		if err != nil {
			return Failedf(name,
				"Run `launchctl list` in a terminal to see what it reports.",
				"could not query launchctl: %v", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, label) {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			pid := fields[0]
			if pid == "-" {
				return Failedf(name,
					fmt.Sprintf("Start it: `launchctl kickstart -k gui/$(id -u)/%s`", label),
					"registered but not running (last exit status %s)", fields[1])
			}
			return Passed(name, fmt.Sprintf("loaded, pid %s", pid))
		}
		return Failedf(name,
			fmt.Sprintf("Install it: `ibeji install`, which writes ~/Library/LaunchAgents/%s.plist "+
				"and loads it. It must be a LaunchAgent in your session, never a root LaunchDaemon.", label),
			"%s is not loaded", label)
	}
}

// Keychain returns a check that secret storage answers.
func Keychain(store SecretStore) CheckFunc {
	return func(ctx context.Context) Check {
		const name = "Keychain"

		if store == nil {
			return Failedf(name, "This is a bug; please report it.", "no secret store configured")
		}
		if err := store.Reachable(); err != nil {
			// The signature failure of a daemon in the wrong session.
			if strings.Contains(err.Error(), "user interaction") ||
				strings.Contains(strings.ToLower(err.Error()), "interaction is not allowed") {
				return Failedf(name,
					"The daemon is running outside your login session — almost always because it was "+
						"installed as a root LaunchDaemon. Reinstall it as a LaunchAgent: `ibeji install`.",
					"the keychain refused access: %v", err)
			}
			return Failedf(name,
				"Unlock your login keychain in Keychain Access, then try again.",
				"keychain unreachable: %v", err)
		}
		if w := store.Warning(); w != "" {
			return Warned(name, w, "macOS is the supported platform for v1.")
		}
		return Passed(name, "reachable")
	}
}

// APIToken returns a check that the local API answers on its endpoint with the
// token from the handoff file.
//
// This is what proves the whole local surface works: the window, the CLI and
// the MCP server all reach the daemon exactly this way.
func APIToken(endpoint, token string) CheckFunc {
	return func(ctx context.Context) Check {
		const name = "API token"

		if endpoint == "" || token == "" {
			return Failedf(name,
				"Start the daemon. If it is running, check ~/.ibeji/api.json exists and is readable.",
				"no endpoint or token available")
		}

		req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/v1/self", nil)
		if err != nil {
			return Failedf(name, "This is a bug; please report it.", "malformed endpoint %q: %v", endpoint, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return Failedf(name,
				"The daemon is not answering. Start it, or run `ibeji doctor` again after it starts.",
				"could not reach %s: %v", endpoint, err)
		}
		defer res.Body.Close()

		switch res.StatusCode {
		case http.StatusOK:
			return Passed(name, fmt.Sprintf("valid · %s", endpoint))
		case http.StatusUnauthorized:
			return Failedf(name,
				"The token in ~/.ibeji/api.json does not match the running daemon. "+
					"Restart the daemon so it rewrites the file.",
				"the daemon rejected the token in the handoff file")
		default:
			return Failedf(name,
				"Check the daemon's logs.",
				"the daemon answered %s", res.Status)
		}
	}
}

// MDNS returns a check that multicast works on this network.
//
// A failure here is a warning rather than a failure: the product works fine
// over a manually entered IP, and the networks that block multicast are exactly
// the ones this product is most useful on. Reporting it as broken would be
// wrong.
func MDNS(peerName string, resolve func(ctx context.Context) (string, error)) CheckFunc {
	return func(ctx context.Context) Check {
		const name = "mDNS"

		if resolve == nil {
			return Warned(name, "not checked", "")
		}
		addr, err := resolve(ctx)
		if err != nil || addr == "" {
			target := peerName
			if target == "" {
				target = "the other device"
			}
			return Warned(name,
				fmt.Sprintf("could not resolve %s over multicast", target),
				"Multicast is often blocked on corporate and guest Wi-Fi. "+
					"Add the other machine by IP address instead — it works identically.")
		}
		return Passed(name, fmt.Sprintf("%s → %s", peerName, addr))
	}
}

// Link returns a check on pairing and the connection to the peer.
func Link(state LinkState) CheckFunc {
	return func(ctx context.Context) Check {
		const name = "Link"

		if state == nil || !state.Paired() {
			return Warned(name, "no device paired",
				"Pair with your other machine in the Ìbejì window, or run `ibeji devices` to see what is on the network.")
		}
		if !state.KeyMatches() {
			// Never transient, and never auto-trusted.
			return Failedf(name,
				"The other machine presented a different key than the one pinned. If you reinstalled "+
					"Ìbejì there, unpair and pair again, comparing the fingerprints. If you did not, "+
					"do not pair until you know why.",
				"the peer's key does not match the pinned key")
		}
		if !state.LinkUp() {
			// Offline is normal on a LAN, so this is a warning with the
			// ordinary explanation first.
			return Warned(name,
				fmt.Sprintf("%s is not reachable right now", state.PeerName()),
				"Usually this just means the other machine is asleep or on a different network. "+
					"Check it is awake and on the same Wi-Fi.")
		}
		return Passed(name, fmt.Sprintf("up · %s at %s", state.PeerName(), state.PeerAddress()))
	}
}

// Standard builds the full six-check doctor.
func Standard(cfg Config) *Doctor {
	d := New()
	d.Add(OrderLaunchAgent, LaunchAgent(cfg.LaunchAgentLabel))
	d.Add(OrderKeychain, Keychain(cfg.Secrets))
	d.Add(OrderAPIToken, APIToken(cfg.Endpoint, cfg.Token))
	d.Add(OrderLocalNet, LocalNetwork())
	d.Add(OrderMDNS, MDNS(cfg.PeerName, cfg.ResolvePeer))
	d.Add(OrderLink, Link(cfg.Link))
	d.Add(OrderLoginShell, LoginShell(cfg.LoginShellTools))
	return d
}

// Config is what Standard needs to build its checks.
type Config struct {
	LaunchAgentLabel string
	Secrets          SecretStore
	Endpoint         string
	Token            string
	PeerName         string
	ResolvePeer      func(ctx context.Context) (string, error)
	Link             LinkState
	LoginShellTools  []string
}

// ResolveViaDNS builds a resolver that looks a peer up by mDNS hostname.
func ResolveViaDNS(host string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if host == "" {
			return "", errors.New("doctor: no host to resolve")
		}
		if !strings.HasSuffix(host, ".local") && !strings.HasSuffix(host, ".local.") {
			host += ".local"
		}
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			return "", err
		}
		if len(addrs) == 0 {
			return "", fmt.Errorf("doctor: %s resolved to nothing", host)
		}
		return addrs[0], nil
	}
}
