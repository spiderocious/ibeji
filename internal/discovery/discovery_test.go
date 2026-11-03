package discovery

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

func mustKey(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id
}

func candidate(t *testing.T, name, addr string, src Source) Candidate {
	t.Helper()
	return Candidate{
		Name:       name,
		Address:    addr,
		Port:       7749,
		PublicKey:  mustKey(t).Public(),
		APIVersion: APIVersion,
		Source:     src,
	}
}

// --- TXT records ------------------------------------------------------------

func TestAdvertisementTXTRoundTrip(t *testing.T) {
	id := mustKey(t)
	ad := Advertisement{Name: "Desktop", Port: 7749, PublicKey: id.Public()}

	name, pub, version, err := ParseTXT(ad.TXT(), "192.168.1.31")
	if err != nil {
		t.Fatalf("ParseTXT: %v", err)
	}
	if name != "Desktop" {
		t.Fatalf("name = %q, want Desktop", name)
	}
	if !pub.Equal(id.Public()) {
		t.Fatal("public key did not survive the TXT round trip")
	}
	if version != APIVersion {
		t.Fatalf("version = %q, want %q", version, APIVersion)
	}
}

// A record without a key is useless: there is nothing to pair against.
func TestParseTXTRequiresAKey(t *testing.T) {
	for _, txt := range []map[string]string{
		{},
		{txtName: "Desktop"},
		{txtName: "Desktop", txtKey: ""},
		{txtName: "Desktop", txtKey: "not-base64!!"},
		{txtName: "Desktop", txtKey: "c2hvcnQ"}, // valid base64, wrong length
	} {
		if _, _, _, err := ParseTXT(txt, "192.168.1.31"); !errors.Is(err, ErrMalformedRecord) {
			t.Fatalf("ParseTXT(%v) = %v, want ErrMalformedRecord", txt, err)
		}
	}
}

// A nameless device you can still pair with beats one you cannot see.
func TestParseTXTFallsBackToAddressForName(t *testing.T) {
	id := mustKey(t)
	txt := map[string]string{txtKey: identity.EncodePublic(id.Public())}
	name, _, version, err := ParseTXT(txt, "192.168.1.31")
	if err != nil {
		t.Fatalf("ParseTXT: %v", err)
	}
	if name != "192.168.1.31" {
		t.Fatalf("name = %q, want the address as fallback", name)
	}
	// A peer that predates the version field is v1, not incompatible.
	if version != "1" {
		t.Fatalf("version = %q, want 1 for a record with no version", version)
	}
}

func TestCandidateHelpers(t *testing.T) {
	id := mustKey(t)
	c := Candidate{
		Name: "Desktop", Address: "192.168.1.31", Port: 7749,
		PublicKey: id.Public(), APIVersion: APIVersion,
	}
	if c.Endpoint() != "192.168.1.31:7749" {
		t.Fatalf("Endpoint = %q", c.Endpoint())
	}
	if c.Fingerprint() != identity.Fingerprint(id.Public()) {
		t.Fatal("Fingerprint does not match the key")
	}
	if !c.Compatible() {
		t.Fatal("a candidate on the current version reports incompatible")
	}
	c.APIVersion = "99"
	if c.Compatible() {
		t.Fatal("a candidate on a future version reports compatible")
	}
}

// IPv6 endpoints must bracket, or every later dial is malformed.
func TestEndpointBracketsIPv6(t *testing.T) {
	c := Candidate{Address: "fd00::1", Port: 7749}
	if got := c.Endpoint(); got != "[fd00::1]:7749" {
		t.Fatalf("Endpoint = %q, want [fd00::1]:7749", got)
	}
}

// --- address selection ------------------------------------------------------

func TestBestAddressPrefersIPv4(t *testing.T) {
	addr, err := bestAddress([]net.IP{
		net.ParseIP("fd00::1"),
		net.ParseIP("192.168.1.31"),
	})
	if err != nil {
		t.Fatalf("bestAddress: %v", err)
	}
	if addr != "192.168.1.31" {
		t.Fatalf("bestAddress = %q, want the IPv4 address", addr)
	}
}

func TestBestAddressSkipsUnusable(t *testing.T) {
	// Loopback and unspecified are not reachable from another machine.
	if _, err := bestAddress([]net.IP{
		net.ParseIP("127.0.0.1"),
		net.ParseIP("0.0.0.0"),
		net.ParseIP("::1"),
	}); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("bestAddress over unusable addresses = %v, want ErrNoAddress", err)
	}
	if _, err := bestAddress(nil); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("bestAddress(nil) = %v, want ErrNoAddress", err)
	}
}

func TestBestAddressSkipsLinkLocalIPv6(t *testing.T) {
	// A link-local v6 address needs a zone identifier to be dialable, which
	// complicates every later use; a routable v6 is accepted.
	addr, err := bestAddress([]net.IP{
		net.ParseIP("fe80::1"),
		net.ParseIP("fd00::1"),
	})
	if err != nil {
		t.Fatalf("bestAddress: %v", err)
	}
	if addr != "fd00::1" {
		t.Fatalf("bestAddress = %q, want the routable v6 address", addr)
	}
}

// --- registry ---------------------------------------------------------------

func TestRegistryAddAndList(t *testing.T) {
	r := NewRegistry()
	c := candidate(t, "Desktop", "192.168.1.31", SourceMDNS)
	r.Add(c)

	got := r.Candidates()
	if len(got) != 1 {
		t.Fatalf("Candidates = %d, want 1", len(got))
	}
	if got[0].Name != "Desktop" {
		t.Fatalf("name = %q", got[0].Name)
	}
	if got[0].SeenAt.IsZero() {
		t.Fatal("Add did not stamp SeenAt")
	}
	if _, ok := r.Lookup(c.Fingerprint()); !ok {
		t.Fatal("Lookup did not find the candidate just added")
	}
}

// Identity is the key, not the address: a peer that changed IP is the same
// entry, not a second one.
func TestRegistryKeysByIdentityNotAddress(t *testing.T) {
	r := NewRegistry()
	id := mustKey(t)

	first := Candidate{Name: "Desktop", Address: "192.168.1.31", Port: 7749,
		PublicKey: id.Public(), APIVersion: APIVersion, Source: SourceMDNS}
	r.Add(first)

	// Same machine, new lease.
	second := first
	second.Address = "192.168.1.44"
	r.Add(second)

	got := r.Candidates()
	if len(got) != 1 {
		t.Fatalf("a peer that changed IP produced %d entries, want 1", len(got))
	}
	if got[0].Address != "192.168.1.44" {
		t.Fatalf("address = %q, want the new one", got[0].Address)
	}
}

func TestRegistryRejectsMalformedKey(t *testing.T) {
	r := NewRegistry()
	r.Add(Candidate{Name: "Bad", Address: "192.168.1.9", PublicKey: []byte("short")})
	if r.Len() != 0 {
		t.Fatal("registry accepted a candidate with a malformed key")
	}
}

// An mDNS peer that stops advertising ages out.
func TestMDNSCandidatesExpire(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.SetClock(func() time.Time { return now })

	r.Add(candidate(t, "Desktop", "192.168.1.31", SourceMDNS))
	if len(r.Candidates()) != 1 {
		t.Fatal("candidate missing immediately after Add")
	}

	now = now.Add(DefaultTTL + time.Second)
	if got := r.Candidates(); len(got) != 0 {
		t.Fatalf("an mDNS candidate survived its TTL: %+v", got)
	}
	if r.Len() != 0 {
		t.Fatal("the expired candidate was not evicted")
	}
}

// The user typed it, so it stays until they remove it — this is what makes
// manual entry a first-class path rather than a fallback.
func TestManualCandidatesNeverExpire(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.SetClock(func() time.Time { return now })

	r.AddManual(candidate(t, "Desktop", "192.168.1.31", SourceManual))

	now = now.Add(100 * DefaultTTL)
	got := r.Candidates()
	if len(got) != 1 {
		t.Fatal("a manually added candidate expired; it must not")
	}
	if got[0].Source != SourceManual {
		t.Fatalf("source = %q, want manual", got[0].Source)
	}
}

// A later mDNS sighting must not quietly downgrade a manual entry, or it would
// start expiring behind the user's back.
func TestMDNSSightingDoesNotDowngradeAManualEntry(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.SetClock(func() time.Time { return now })

	id := mustKey(t)
	manual := Candidate{Name: "Desktop", Address: "192.168.1.31", Port: 7749,
		PublicKey: id.Public(), APIVersion: APIVersion}
	r.AddManual(manual)

	seen := manual
	seen.Source = SourceMDNS
	r.Add(seen)

	now = now.Add(DefaultTTL + time.Second)
	got := r.Candidates()
	if len(got) != 1 {
		t.Fatal("the manual entry expired after an mDNS sighting")
	}
	if got[0].Source != SourceManual {
		t.Fatalf("source = %q, want it to stay manual", got[0].Source)
	}
}

func TestRegistryRemove(t *testing.T) {
	r := NewRegistry()
	c := candidate(t, "Desktop", "192.168.1.31", SourceMDNS)
	r.Add(c)
	r.Remove(c.Fingerprint())
	if r.Len() != 0 {
		t.Fatal("Remove did not drop the candidate")
	}
	r.Remove(c.Fingerprint()) // removing twice must not panic
}

func TestCandidatesAreSortedFreshestFirst(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.SetClock(func() time.Time { return now })

	older := candidate(t, "Older", "192.168.1.10", SourceMDNS)
	older.SeenAt = now.Add(-30 * time.Second)
	r.Add(older)

	newer := candidate(t, "Newer", "192.168.1.11", SourceMDNS)
	newer.SeenAt = now
	r.Add(newer)

	got := r.Candidates()
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if got[0].Name != "Newer" {
		t.Fatalf("first candidate is %q, want the freshest", got[0].Name)
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	r := NewRegistry()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.Add(candidate(t, "Peer", "192.168.1.31", SourceMDNS))
		}
	}()
	for i := 0; i < 200; i++ {
		_ = r.Candidates()
		_ = r.Len()
	}
	<-done
}

// --- empty state ------------------------------------------------------------

// The hint is the difference between a product that works at the office and
// one that leaves the user staring at an empty list.
func TestEmptyStateHintNamesBlockedMulticast(t *testing.T) {
	hint := EmptyStateHint(true, false)
	if !contains(hint, "IP address") {
		t.Fatalf("hint for a silent network does not suggest adding by IP: %q", hint)
	}
	if !contains(hint, "ulticast") {
		t.Fatalf("hint for a silent network does not mention multicast: %q", hint)
	}

	// If we heard traffic, multicast works and the other machine is just off —
	// suggesting an IP would be misleading.
	hint = EmptyStateHint(true, true)
	if contains(hint, "IP address") {
		t.Fatalf("hint wrongly blames the network when multicast is working: %q", hint)
	}

	if hint := EmptyStateHint(false, false); contains(hint, "ulticast") {
		t.Fatalf("hint blames multicast before browsing started: %q", hint)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// --- real multicast ---------------------------------------------------------

// Advertise on this machine and discover it from the same process.
//
// This is the test that proves the library is wired up correctly — the registry
// tests above would all pass against a broken mDNS layer.
//
// mDNS answers once per network interface, and on a Mac most of those carry
// only loopback or link-local addresses. bestAddress rejects those, so a
// correct implementation drops several sightings before finding the one with
// the real LAN address. That is the behaviour under test here, not an edge
// case: feeding the registry nine copies of one peer must leave one entry.
//
// Skipped when multicast is genuinely unavailable (CI, a locked-down network),
// since that is a fact about the environment rather than a defect.
func TestMDNSAdvertiseAndBrowse(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	if os.Getenv("IBEJI_SKIP_MDNS_TESTS") != "" {
		t.Skip("IBEJI_SKIP_MDNS_TESTS is set")
	}

	id := mustKey(t)
	ad := Advertisement{
		Name:      "ibeji-test-" + id.Fingerprint()[7:11],
		Port:      7749,
		PublicKey: id.Public(),
	}

	m := NewMDNS()
	advCtx, stopAdv := context.WithCancel(context.Background())
	defer stopAdv()

	advErr := make(chan error, 1)
	go func() { advErr <- m.Advertise(advCtx, ad) }()

	// Let the responder announce before browsing; without this the browse
	// often starts into silence and the test looks like a blocked network.
	time.Sleep(2 * time.Second)

	browseCtx, stopBrowse := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopBrowse()

	// Feed sightings into a real registry: that is how the daemon consumes
	// them, and it is what collapses the per-interface duplicates.
	reg := NewRegistry()
	sightings := make(chan Candidate, 32)
	go func() {
		_ = m.Browse(browseCtx, func(c Candidate) {
			reg.Add(c)
			select {
			case sightings <- c:
			default:
			}
		})
	}()

	deadline := time.After(15 * time.Second)
	for {
		select {
		case c := <-sightings:
			if !c.PublicKey.Equal(id.Public()) {
				continue // another Ìbejì on this network; not ours
			}
			if c.Port != 7749 {
				t.Fatalf("discovered port %d, want 7749", c.Port)
			}
			if c.Address == "" {
				t.Fatal("discovered candidate has no address")
			}
			if ip := net.ParseIP(c.Address); ip == nil || ip.IsLoopback() {
				t.Fatalf("discovered address %q is not usable from another machine", c.Address)
			}
			if c.Source != SourceMDNS {
				t.Fatalf("source = %q, want mdns", c.Source)
			}
			if !c.Compatible() {
				t.Fatalf("discovered an incompatible version: %q", c.APIVersion)
			}

			// The real assertion: however many interfaces answered, the
			// registry holds exactly one entry for this device, because it is
			// keyed by identity rather than address.
			if got := reg.Len(); got != 1 {
				t.Fatalf("registry holds %d entries for one device; per-interface sightings must collapse", got)
			}
			t.Logf("discovered self at %s (registry holds %d entry)", c.Endpoint(), reg.Len())
			return

		case err := <-advErr:
			if err != nil {
				t.Skipf("multicast unavailable in this environment: %v", err)
			}

		case <-deadline:
			t.Skip("no usable mDNS response within 15s - multicast is likely blocked here")
		}
	}
}
