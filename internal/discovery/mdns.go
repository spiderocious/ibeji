package discovery

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/brutella/dnssd"
)

// MDNS implements Browser and Advertiser over multicast DNS.
//
// The dependency is contained to this file: everything above it works against
// the Browser and Advertiser interfaces, so the registry's merging and ageing
// logic is tested without multicast, and swapping the library later touches
// one file.
type MDNS struct {
	// Interfaces limits which network interfaces are used. Empty means all,
	// which is what the daemon wants — a machine on Wi-Fi and Ethernet at once
	// should be findable on both.
	Interfaces []string
}

// NewMDNS builds an mDNS browser/advertiser.
func NewMDNS() *MDNS { return &MDNS{} }

// Advertise publishes this device until the context is cancelled.
func (m *MDNS) Advertise(ctx context.Context, ad Advertisement) error {
	if ad.Port <= 0 {
		return fmt.Errorf("discovery: cannot advertise on port %d", ad.Port)
	}
	if len(ad.PublicKey) == 0 {
		return fmt.Errorf("discovery: cannot advertise without a public key")
	}

	cfg := dnssd.Config{
		Name: ad.Name,
		Type: ServiceType,
		Port: ad.Port,
		Text: ad.TXT(),
	}
	if len(m.Interfaces) > 0 {
		cfg.Ifaces = m.Interfaces
	}

	service, err := dnssd.NewService(cfg)
	if err != nil {
		return fmt.Errorf("discovery: build service: %w", err)
	}

	responder, err := dnssd.NewResponder()
	if err != nil {
		// A responder that will not start is the signature of multicast being
		// unavailable. Say so, because the fix is "add it by IP" rather than
		// anything the user can do about the network.
		return fmt.Errorf("discovery: start mDNS responder (multicast may be unavailable on this network): %w", err)
	}
	if _, err := responder.Add(service); err != nil {
		return fmt.Errorf("discovery: register service: %w", err)
	}

	// Responds until ctx is done, then tears the advertisement down.
	return responder.Respond(ctx)
}

// Browse streams peers until the context is cancelled.
//
// Every candidate it yields is dialable: entries without a usable address or
// without a public key are dropped here rather than surfaced, because a device
// row the user cannot act on is worse than no row.
func (m *MDNS) Browse(ctx context.Context, found func(Candidate)) error {
	service := strings.TrimSuffix(ServiceType, ".") + "." + Domain

	addFn := func(e dnssd.BrowseEntry) {
		c, err := candidateFrom(e)
		if err != nil {
			// A malformed advertisement is someone else's bug or an unrelated
			// service answering; skip it rather than failing the browse.
			return
		}
		found(c)
	}
	// Removals are handled by the registry's TTL rather than here: a peer that
	// stops advertising because its Wi-Fi blipped should linger briefly, not
	// vanish from the list mid-click.
	rmvFn := func(e dnssd.BrowseEntry) {}

	if err := dnssd.LookupType(ctx, service, addFn, rmvFn); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("discovery: browse (multicast may be blocked on this network): %w", err)
	}
	return nil
}

// candidateFrom converts a browse entry into a candidate, choosing the best
// address the peer offered.
func candidateFrom(e dnssd.BrowseEntry) (Candidate, error) {
	addr, err := bestAddress(e.IPs)
	if err != nil {
		return Candidate{}, err
	}
	name, pub, version, err := ParseTXT(e.Text, addr)
	if err != nil {
		return Candidate{}, err
	}
	// Prefer the instance name the peer chose; it is what the user named the
	// machine, and ParseTXT only falls back to the address.
	if n := strings.TrimSpace(e.Name); n != "" {
		name = n
	}
	return Candidate{
		Name:       name,
		Address:    addr,
		Port:       e.Port,
		PublicKey:  pub,
		APIVersion: version,
		Source:     SourceMDNS,
		SeenAt:     time.Now(),
	}, nil
}

// bestAddress picks the address to dial.
//
// IPv4 first: it is what a home LAN actually routes, and a link-local IPv6
// address needs a zone identifier that complicates every later use. Loopback
// is skipped — a peer reachable only on its own loopback is not reachable.
func bestAddress(ips []net.IP) (string, error) {
	var v6 string
	for _, ip := range ips {
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
		if v6 == "" && !ip.IsLinkLocalUnicast() {
			v6 = ip.String()
		}
	}
	if v6 != "" {
		return v6, nil
	}
	return "", ErrNoAddress
}
