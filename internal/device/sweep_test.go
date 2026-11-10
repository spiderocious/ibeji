package device

import (
	"net"
	"testing"

	"github.com/feranmi/ibeji/internal/identity"
)

func TestHostsOfNarrowsToASlash24(t *testing.T) {
	// A /16 is 65,534 probes: minutes of work, and it looks like a port scan
	// to network gear. The peer is on the same /24, so that is what gets
	// swept however wide the mask actually is.
	_, wide, err := net.ParseCIDR("10.0.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	hosts := hostsOf(net.ParseIP("10.0.5.20").To4(), wide)
	if len(hosts) != 254 {
		t.Fatalf("a /16 should narrow to 254 hosts, got %d", len(hosts))
	}
	// And it must be the /24 around this machine, not the bottom of the /16.
	first, last := hosts[0], hosts[len(hosts)-1]
	if first != "10.0.5.1" {
		t.Errorf("sweep should start at this machine's own /24, got %q", first)
	}
	if last != "10.0.5.254" {
		t.Errorf("sweep should end at this machine's own /24, got %q", last)
	}
}

func TestHostsOfCoversTheSubnetAndSkipsNetworkAndBroadcast(t *testing.T) {
	_, ipnet, err := net.ParseCIDR("192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	hosts := hostsOf(net.ParseIP("192.168.1.35").To4(), ipnet)
	if len(hosts) != 254 {
		t.Fatalf("a /24 has 254 usable hosts, got %d", len(hosts))
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		seen[h] = true
	}
	// The address that actually mattered: the peer had moved to .39 while
	// the address in hand still said .147. Both must be in range.
	for _, want := range []string{"192.168.1.1", "192.168.1.39", "192.168.1.147", "192.168.1.254"} {
		if !seen[want] {
			t.Errorf("sweep should probe %s", want)
		}
	}
	// .0 is the network and .255 the broadcast address; probing either is
	// pointless and the broadcast is actively antisocial.
	for _, skip := range []string{"192.168.1.0", "192.168.1.255"} {
		if seen[skip] {
			t.Errorf("sweep should not probe %s", skip)
		}
	}
}

func TestLessIPOrdersNumerically(t *testing.T) {
	// String ordering puts .10 before .9, which reads as a bug in a list of
	// found machines.
	if !lessIP("192.168.1.9:7749", "192.168.1.10:7749") {
		t.Error(".9 should sort before .10")
	}
	if lessIP("192.168.1.147:7749", "192.168.1.39:7749") {
		t.Error(".39 should sort before .147")
	}
	// Bare addresses, no port, must not panic or misorder.
	if !lessIP("10.0.0.2", "10.0.0.10") {
		t.Error("bare addresses should order numerically too")
	}
}

func TestPrimaryIPv4FindsALocalAddress(t *testing.T) {
	// Runs on a real machine, so this asserts shape rather than a value: a
	// usable IPv4 address that is neither loopback nor link-local, because
	// those are the two that would make the sweep probe nothing.
	ip, ipnet, err := primaryIPv4()
	if err != nil {
		t.Skipf("no local IPv4 on this machine: %v", err)
	}
	if ip.To4() == nil {
		t.Fatalf("expected an IPv4 address, got %v", ip)
	}
	if ip.IsLoopback() {
		t.Error("the sweep must not run against loopback")
	}
	if ip.IsLinkLocalUnicast() {
		t.Error("a link-local address means no DHCP; sweeping it finds nothing")
	}
	if ipnet == nil {
		t.Fatal("expected a network alongside the address")
	}
	if len(hostsOf(ip.To4(), ipnet)) == 0 {
		t.Error("the network around this machine should have addresses to probe")
	}
}

func TestIsSelfAddressNeedsHostAndPort(t *testing.T) {
	// Host alone is not enough. Two daemons on one machine is how the e2e
	// suite works and how anyone tries the product before owning a second
	// Mac, so "my IP on a different port" is a legitimate peer. Comparing
	// only the host refused those and broke pairing outright.
	d := &Daemon{listenAddr: "0.0.0.0:7749"}
	d.store = identity.NewStore(mustIdentity(t), "test")

	if d.isSelfAddress("192.168.1.35:17750") {
		t.Error("a different port is a different daemon, even on this machine")
	}
	// Loopback on our own port is unambiguously us.
	if !d.isSelfAddress("127.0.0.1:7749") {
		t.Error("loopback on our own port is this daemon")
	}
	if d.isSelfAddress("127.0.0.1:17750") {
		t.Error("loopback on another port is another daemon")
	}
	// A bare host cannot be judged, so it is not refused: normalizePairAddress
	// fills the port in before this is ever reached.
	if d.isSelfAddress("192.168.1.35") {
		t.Error("an address with no port should not be treated as our own")
	}
}

func mustIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
