// Package discovery finds the other device on the LAN.
//
// Two paths, and both are first class. mDNS is the one that feels like magic —
// install on both machines and they see each other. Manual entry by IP is the
// one that works on the networks where mDNS does not, which is more of them
// than you would expect: corporate and guest Wi-Fi routinely disable multicast
// between clients. A product that treats manual entry as a degraded fallback
// strands the user on exactly the network the product was built for.
//
// Nothing here decides trust. Discovery reports what it saw; pairing decides
// what to believe, and the pinned key decides what to connect to.
package discovery

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

// ServiceType is the mDNS service Ìbejì advertises.
const ServiceType = "_ibeji._tcp"

// Domain is the mDNS domain. Always .local for link-local discovery.
const Domain = "local."

// APIVersion is the device-to-device protocol version advertised in TXT.
// A peer advertising a version this build does not understand is shown but
// marked incompatible, rather than hidden — "it is there but we cannot talk to
// it" is a far more useful thing to tell someone than silence.
const APIVersion = "1"

// TXT record keys. Short because a TXT record is size-constrained, and stable
// because changing one makes existing peers undiscoverable.
const (
	txtName    = "n" // device name
	txtKey     = "k" // base64 public key
	txtVersion = "v" // API version
)

var (
	// ErrNoAddress is returned when a peer advertises without a usable address.
	ErrNoAddress = errors.New("discovery: peer advertised no usable address")

	// ErrMalformedRecord is returned when a TXT record is missing what a peer
	// must publish to be connectable.
	ErrMalformedRecord = errors.New("discovery: peer's record is missing required fields")
)

// Candidate is a device seen on the network and not yet paired with.
type Candidate struct {
	Name       string
	Address    string
	Port       int
	PublicKey  ed25519.PublicKey
	APIVersion string

	// Source records how this candidate was found, so the UI can say
	// "discovered" or "added by hand" — they behave identically from here on.
	Source Source

	// SeenAt is when this candidate was last observed. A candidate from mDNS
	// goes stale when the peer stops advertising; a manual one never does.
	SeenAt time.Time
}

// Fingerprint renders the candidate's key for display before pairing, so the
// user can compare it with what the other machine shows.
func (c Candidate) Fingerprint() string {
	return identity.Fingerprint(c.PublicKey)
}

// Compatible reports whether this build can talk to the candidate.
func (c Candidate) Compatible() bool {
	return c.APIVersion == APIVersion
}

// Endpoint is the dialable address.
func (c Candidate) Endpoint() string {
	return net.JoinHostPort(c.Address, strconv.Itoa(c.Port))
}

// Source is how a candidate was found.
type Source string

const (
	// SourceMDNS means the peer advertised itself and we heard it.
	SourceMDNS Source = "mdns"

	// SourceManual means the user typed an address. Equal in standing to mDNS:
	// the PRD makes this a first-class path, not a fallback.
	SourceManual Source = "manual"
)

// Advertisement is what this device publishes about itself.
type Advertisement struct {
	Name      string
	Port      int
	PublicKey ed25519.PublicKey
}

// TXT renders the advertisement's TXT record.
func (a Advertisement) TXT() map[string]string {
	return map[string]string{
		txtName:    a.Name,
		txtKey:     identity.EncodePublic(a.PublicKey),
		txtVersion: APIVersion,
	}
}

// ParseTXT reads a peer's TXT record into the fields a candidate needs.
//
// A record missing the key is useless — without it there is nothing to pair
// against — so that is an error rather than a partial result. A missing name
// is tolerated and filled in from the address, because a nameless device you
// can still pair with is better than one you cannot see.
func ParseTXT(txt map[string]string, addr string) (name string, pub ed25519.PublicKey, version string, err error) {
	raw, ok := txt[txtKey]
	if !ok || raw == "" {
		return "", nil, "", fmt.Errorf("%w: no public key", ErrMalformedRecord)
	}
	pub, err = identity.DecodePublic(raw)
	if err != nil {
		return "", nil, "", fmt.Errorf("%w: %v", ErrMalformedRecord, err)
	}
	name = strings.TrimSpace(txt[txtName])
	if name == "" {
		name = addr
	}
	version = txt[txtVersion]
	if version == "" {
		// A peer that publishes no version predates the field; treat it as v1
		// rather than as incompatible.
		version = "1"
	}
	return name, pub, version, nil
}

// Browser watches for peers. Implemented by the mDNS browser and by fakes in
// tests, so the registry's logic is testable without multicast.
type Browser interface {
	// Browse streams candidates until the context is cancelled. It must not
	// return a candidate this device cannot dial.
	Browse(ctx context.Context, found func(Candidate)) error
}

// Advertiser publishes this device. Implemented by the mDNS advertiser.
type Advertiser interface {
	Advertise(ctx context.Context, ad Advertisement) error
}

// Registry is the daemon's live view of who is out there.
//
// It merges what mDNS heard with what the user typed, ages out peers that
// stopped advertising, and never lets a manual entry expire.
type Registry struct {
	mu         sync.RWMutex
	candidates map[string]Candidate // keyed by fingerprint, not address

	// TTL is how long an mDNS candidate survives without being seen again.
	TTL time.Duration

	now func() time.Time
}

// DefaultTTL is how long a peer lingers after it stops advertising.
//
// Long enough to ride out a sleeping Wi-Fi radio or a missed announcement;
// short enough that a machine genuinely gone stops being offered. A stale
// candidate still renders — stale renders, stale does not act — so erring long
// here is cheap.
const DefaultTTL = 90 * time.Second

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		candidates: map[string]Candidate{},
		TTL:        DefaultTTL,
		now:        time.Now,
	}
}

// SetClock replaces the time source, for tests crossing the TTL boundary.
func (r *Registry) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
}

// Add records a candidate, replacing any earlier sighting of the same device.
//
// Keyed by fingerprint rather than address, so a peer that changed IP is the
// same entry rather than a duplicate — identity is the key, the address is
// just where it happens to be. A manual entry is never downgraded to mDNS by a
// later sighting: the user typed it, so it stays until they remove it.
func (r *Registry) Add(c Candidate) {
	if len(c.PublicKey) != identity.PublicKeySize {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if c.SeenAt.IsZero() {
		c.SeenAt = r.now()
	}
	fp := identity.Fingerprint(c.PublicKey)
	if prev, ok := r.candidates[fp]; ok && prev.Source == SourceManual {
		// Refresh where it is and what it calls itself, but keep it manual.
		c.Source = SourceManual
	}
	r.candidates[fp] = c
}

// AddManual records a peer the user typed in.
//
// The address is all the user has; the key arrives when the daemon connects and
// the peer identifies itself. Until then the entry is a placeholder the UI can
// show as "contacting…", keyed by address so a second attempt replaces it.
func (r *Registry) AddManual(c Candidate) {
	c.Source = SourceManual
	r.Add(c)
}

// Remove drops a candidate by fingerprint.
func (r *Registry) Remove(fingerprint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.candidates, fingerprint)
}

// Candidates returns what is currently visible, freshest first.
//
// mDNS entries past their TTL are dropped; manual entries never expire.
func (r *Registry) Candidates() []Candidate {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	out := make([]Candidate, 0, len(r.candidates))
	for fp, c := range r.candidates {
		if c.Source == SourceMDNS && now.Sub(c.SeenAt) > r.TTL {
			delete(r.candidates, fp)
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SeenAt.Equal(out[j].SeenAt) {
			return out[i].SeenAt.After(out[j].SeenAt)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Lookup finds a candidate by fingerprint.
func (r *Registry) Lookup(fingerprint string) (Candidate, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.candidates[fingerprint]
	return c, ok
}

// Len reports how many candidates are held, without ageing any out.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.candidates)
}

// EmptyStateHint is what the UI shows when nothing has been discovered.
//
// It lives here rather than in the renderer because the reason matters and the
// daemon is what knows it: an empty list on a network with multicast blocked
// is not the same as an empty list on a network where the other machine is
// simply off, and telling the user "add it by IP" is the difference between a
// product that works at the office and one that does not.
func EmptyStateHint(browsing bool, sawAnyTraffic bool) string {
	switch {
	case !browsing:
		return "Not looking for devices yet."
	case sawAnyTraffic:
		return "No other Ìbejì devices on this network yet. " +
			"Make sure Ìbejì is running on the other machine."
	default:
		return "Nothing found. Multicast may be blocked on this network — " +
			"common on corporate and guest Wi-Fi. Add the other machine by IP address."
	}
}
