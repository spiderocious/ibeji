package identity

import (
	"crypto/ed25519"
	"crypto/subtle"
	"fmt"
	"sync"
	"time"
)

// Peer is a device this one has paired with: its pinned public key plus the
// mutable facts about where it currently is.
//
// The key is the identity. Name and address are information shown to the user
// and are expected to change — DHCP renews, Wi-Fi flips to Ethernet, the user
// renames a machine — so neither is ever used to decide whether a connection is
// trusted.
type Peer struct {
	// PublicKey is pinned at pairing and never changes for the life of the
	// pairing. A peer presenting a different key is rejected.
	PublicKey ed25519.PublicKey

	// Name is user-chosen on the peer and propagated here.
	Name string

	// LastAddress is where the peer was last reachable, used as a fallback when
	// mDNS cannot resolve it. Informational, never authoritative.
	LastAddress string

	// PairedAt records when the pinning happened, shown in the device view.
	PairedAt time.Time

	// LastSeen is when the link last carried traffic from this peer.
	LastSeen time.Time
}

// Fingerprint renders the peer's pinned key for visual comparison.
func (p *Peer) Fingerprint() string {
	return Fingerprint(p.PublicKey)
}

// Store holds this device's identity and its pinned peer.
//
// v1 pins exactly one peer. The constraint is enforced here rather than left to
// callers so that "exactly two devices" is a property of the type, not a rule
// someone has to remember — the same reasoning the design system uses for its
// required slots.
type Store struct {
	mu   sync.RWMutex
	self *Identity
	name string
	peer *Peer

	// secrets persists the pinned peer across restarts. Nil in tests that
	// build a store with NewStore and do not care about durability; the
	// daemon always uses LoadStore, which sets it.
	secrets SecretStore
}

// NewStore builds a store around this device's identity and name.
func NewStore(self *Identity, name string) *Store {
	return &Store{self: self, name: name}
}

// Self returns this device's identity.
func (s *Store) Self() *Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.self
}

// Name returns this device's user-chosen name.
func (s *Store) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// SetName renames this device. The new name propagates to the peer over the
// link; propagation is the link layer's job, not the store's.
func (s *Store) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

// Peer returns the pinned peer, or nil when unpaired.
func (s *Store) Peer() *Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.peer == nil {
		return nil
	}
	cp := *s.peer
	return &cp
}

// Paired reports whether a peer is pinned.
func (s *Store) Paired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peer != nil
}

// Pin records a peer's public key as trusted. It fails if a peer is already
// pinned: re-pairing a different device requires Unpair first, so that
// replacing a trusted key is always a deliberate, separately-confirmed act.
func (s *Store) Pin(pub ed25519.PublicKey, name, addr string) (*Peer, error) {
	if len(pub) != PublicKeySize {
		return nil, fmt.Errorf("identity: cannot pin a %d-byte key", len(pub))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer != nil {
		// Pinning the same key again is idempotent, which keeps a retried
		// pairing confirmation from failing confusingly.
		if subtle.ConstantTimeCompare(s.peer.PublicKey, pub) == 1 {
			s.peer.Name = name
			if addr != "" {
				s.peer.LastAddress = addr
			}
			if err := savePeer(s.secrets, s.peer); err != nil {
				return nil, err
			}
			cp := *s.peer
			return &cp, nil
		}
		return nil, ErrAlreadyPaired
	}
	stored := make(ed25519.PublicKey, len(pub))
	copy(stored, pub)
	peer := &Peer{
		PublicKey:   stored,
		Name:        name,
		LastAddress: addr,
		PairedAt:    time.Now(),
	}
	// Persist BEFORE accepting it in memory. A pin that reports success and
	// then vanishes on restart is worse than one that fails loudly: the user
	// believes the machines are paired and finds out they are not the next
	// morning, with nothing in any log to say why.
	if err := savePeer(s.secrets, peer); err != nil {
		return nil, err
	}
	s.peer = peer
	cp := *peer
	return &cp, nil
}

// Verify checks a presented public key against the pinned one.
//
// This is the function that makes a key mismatch a hard failure. It returns
// ErrKeyMismatch rather than a bool so a caller cannot accidentally treat
// "false" as "no peer yet" and proceed.
func (s *Store) Verify(presented ed25519.PublicKey) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.peer == nil {
		return ErrNotPaired
	}
	if subtle.ConstantTimeCompare(s.peer.PublicKey, presented) != 1 {
		return ErrKeyMismatch
	}
	return nil
}

// Unpair forgets the pinned peer. Callers are responsible for closing tunnels,
// syncs and sessions before or immediately after — the PRD requires unpairing
// to take effect at once, and this store does not own those.
func (s *Store) Unpair() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer == nil {
		return ErrNotPaired
	}
	// Clear the stored copy first. If this fails the peer stays pinned, which
	// is the safe direction: an unpair that forgets in memory but not on disk
	// would silently come back on the next restart.
	if err := savePeer(s.secrets, nil); err != nil {
		return err
	}
	s.peer = nil
	return nil
}

// Touch records that the peer was just seen, for the device view's status.
func (s *Store) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer != nil {
		s.peer.LastSeen = time.Now()
	}
}

// SetPeerAddress updates where the peer was last reachable.
func (s *Store) SetPeerAddress(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer != nil && addr != "" {
		s.peer.LastAddress = addr
	}
}

// SetPeerName records a name the peer propagated to us.
func (s *Store) SetPeerName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer != nil && name != "" {
		s.peer.Name = name
	}
}
