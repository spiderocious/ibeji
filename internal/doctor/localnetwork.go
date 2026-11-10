package doctor

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Is macOS blocking this daemon from the local network?
//
// This check exists because its absence cost two days of debugging. macOS
// requires explicit permission to reach devices on the local network, and a
// denial is SILENT: every LAN dial returns "connect: no route to host" —
// the same error a genuinely absent machine produces — while the internet
// keeps working perfectly. Two Macs on one network could not see each other
// and nothing anywhere said why.
//
// The tell is the asymmetry. A real network fault takes the internet with it.
// A permission denial does not. So: reach something off-LAN, then reach the
// default gateway, which is on the LAN and is always there. Internet yes and
// gateway no is a permission problem and nothing else.

// LocalNetwork reports whether this process can reach the LAN at all.
func LocalNetwork() CheckFunc {
	return func(ctx context.Context) Check {
		const name = "Local network"

		gw, err := defaultGateway()
		if err != nil || gw == "" {
			// No gateway is not a verdict about permission; say so rather
			// than inventing one.
			return Warned(name, "no default gateway, so this could not be checked", "")
		}

		lanOK := canReach(ctx, net.JoinHostPort(gw, "80"), 2*time.Second) ||
			canReach(ctx, net.JoinHostPort(gw, "443"), 2*time.Second) ||
			canReach(ctx, net.JoinHostPort(gw, "53"), 2*time.Second)
		if lanOK {
			return Passed(name, fmt.Sprintf("reachable · gateway %s", gw))
		}

		// The LAN is unreachable. Whether that is a permission denial or a
		// real network fault depends on the internet.
		netOK := canReach(ctx, "1.1.1.1:443", 3*time.Second) ||
			canReach(ctx, "8.8.8.8:53", 3*time.Second)

		if netOK {
			return Failedf(name,
				"Open System Settings → Privacy & Security → Local Network and turn on "+
					"Ìbejì. This recurs after every update: macOS ties the permission to "+
					"the app's exact contents, which change with each version, and these "+
					"builds are not signed with a certificate it can follow instead. "+
					"Expect a new entry in that list rather than the old one switching "+
					"itself back on.",
				"macOS is blocking Ìbejì from this network (the internet works, but the "+
					"router at %s does not answer)", gw)
		}

		return Failedf(name,
			"Check Wi-Fi or Ethernet on this machine.",
			"no working network: neither the router at %s nor the internet answered", gw)
	}
}

// canReach reports whether a TCP connection to addr succeeds.
func canReach(ctx context.Context, addr string, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// defaultGateway reads the default route's gateway.
func defaultGateway() (string, error) {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "gateway:")), nil
		}
	}
	return "", fmt.Errorf("doctor: no gateway in the default route")
}

// lanBlocked reports whether this process is cut off from the LAN while the
// internet still works — the signature of a macOS Local Network denial.
//
// Shared with the Link check so the two cannot contradict each other. It
// returns the gateway too, because naming the address that did not answer is
// what makes the verdict checkable rather than an assertion.
func lanBlocked(ctx context.Context) (bool, string) {
	gw, err := defaultGateway()
	if err != nil || gw == "" {
		return false, ""
	}
	lanOK := canReach(ctx, net.JoinHostPort(gw, "80"), 2*time.Second) ||
		canReach(ctx, net.JoinHostPort(gw, "443"), 2*time.Second) ||
		canReach(ctx, net.JoinHostPort(gw, "53"), 2*time.Second)
	if lanOK {
		return false, gw
	}
	netOK := canReach(ctx, "1.1.1.1:443", 3*time.Second) ||
		canReach(ctx, "8.8.8.8:53", 3*time.Second)
	return netOK, gw
}
