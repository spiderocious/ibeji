package link

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

// Pairing is the one-time ceremony that turns two strangers on a LAN into a
// trusted pair.
//
// The scheme is a short authenticated string. Both devices exchange public
// keys, then each derives a six-digit code from *both* keys. If the codes shown
// on the two screens match, both devices are looking at the same pair of keys —
// so a machine in the middle substituting its own key would produce a different
// number on one of the screens, and the user would see it.
//
// The security of this rests entirely on a human comparing six digits, which is
// why the PRD gives the code its own 52px treatment and why it is the one place
// a record value is set in territory type: it is read aloud across a room, not
// scanned in a log.

var (
	// ErrCodeMismatch is returned when a device confirms a code that does not
	// match what this side derived. It means the key exchange was tampered
	// with, or the user confirmed the wrong session.
	ErrCodeMismatch = errors.New("link: pairing codes do not match")

	// ErrPairingExpired is returned when a session is confirmed after its
	// window closed. A stale pairing confirmation must not succeed.
	ErrPairingExpired = errors.New("link: this pairing attempt has expired")

	// ErrPairingUnknown is returned for a session id that was never issued or
	// has already been consumed.
	ErrPairingUnknown = errors.New("link: no such pairing attempt")
)

// PairingWindow is how long a pairing attempt stays confirmable.
//
// Long enough to walk to the other machine and read a number; short enough that
// an abandoned attempt on an unattended screen stops being confirmable. Not
// user-configurable: a pairing window is a security parameter, not a preference.
const PairingWindow = 3 * time.Minute

// PairingOffer is what a device publishes to begin pairing: who it claims to
// be, and the key the rest of the ceremony binds to.
type PairingOffer struct {
	DeviceName string
	PublicKey  ed25519.PublicKey
	Address    string
}

// PairingSession is one in-flight attempt, held by the device that started it.
type PairingSession struct {
	ID        string
	Code      string
	Peer      PairingOffer
	StartedAt time.Time
	ExpiresAt time.Time
}

// Expired reports whether the confirmation window has closed.
func (s *PairingSession) Expired(now time.Time) bool {
	return now.After(s.ExpiresAt)
}

// Fingerprint renders the peer's key for display beside the code.
func (s *PairingSession) Fingerprint() string {
	return identity.Fingerprint(s.Peer.PublicKey)
}

// DeriveCode produces the six digits both devices must show.
//
// The two public keys are sorted before hashing so that both sides derive the
// same value regardless of which one initiated — without that, the dialer and
// the accepter would show different numbers and the ceremony could never match.
func DeriveCode(a, b ed25519.PublicKey) (string, error) {
	if len(a) != identity.PublicKeySize || len(b) != identity.PublicKeySize {
		return "", fmt.Errorf("link: cannot derive a code from %d- and %d-byte keys", len(a), len(b))
	}
	keys := [][]byte{append([]byte(nil), a...), append([]byte(nil), b...)}
	sort.Slice(keys, func(i, j int) bool {
		return string(keys[i]) < string(keys[j])
	})

	h := sha256.New()
	// A domain separator keeps this hash from colliding with any other use of
	// these keys elsewhere in the protocol.
	h.Write([]byte("ibeji-pairing-code-v1"))
	h.Write(keys[0])
	h.Write(keys[1])
	sum := h.Sum(nil)

	// Six digits from the first 8 bytes. Modulo bias across 10^6 out of 2^64 is
	// negligible, and the threat model here is a human misreading a number, not
	// an adversary with 2^64 attempts at a three-minute window.
	n := binary.BigEndian.Uint64(sum[:8]) % 1_000_000
	return fmt.Sprintf("%06d", n), nil
}

// FormatCode renders a code for display as two groups of three, the way the
// pairing screen shows it: "418 207". Grouping is for reading aloud.
func FormatCode(code string) string {
	if len(code) != 6 {
		return code
	}
	return code[:3] + " " + code[3:]
}

// Pairer runs the ceremony for one device.
//
// It is deliberately ignorant of transport: it takes an offer from the peer and
// produces a session, and the caller moves bytes. That keeps the code-derivation
// logic — the part whose correctness the whole trust model rests on — testable
// without a network.
type Pairer struct {
	self     *identity.Identity
	selfName string
	now      func() time.Time

	sessions map[string]*PairingSession
	nextID   uint64
}

// NewPairer builds a pairer for this device.
func NewPairer(self *identity.Identity, selfName string) *Pairer {
	return &Pairer{
		self:     self,
		selfName: selfName,
		now:      time.Now,
		sessions: map[string]*PairingSession{},
	}
}

// SetClock replaces the time source, for tests that need to cross the
// expiry boundary without sleeping.
func (p *Pairer) SetClock(now func() time.Time) { p.now = now }

// Offer is what this device publishes to a peer.
func (p *Pairer) Offer(address string) PairingOffer {
	return PairingOffer{
		DeviceName: p.selfName,
		PublicKey:  p.self.Public(),
		Address:    address,
	}
}

// Begin starts a session against a peer's offer and returns the code to show.
//
// Both devices call this with the other's offer; both derive the same code.
func (p *Pairer) Begin(peer PairingOffer) (*PairingSession, error) {
	if len(peer.PublicKey) != identity.PublicKeySize {
		return nil, fmt.Errorf("link: peer offered a %d-byte key", len(peer.PublicKey))
	}
	code, err := DeriveCode(p.self.Public(), peer.PublicKey)
	if err != nil {
		return nil, err
	}
	now := p.now()
	p.nextID++
	s := &PairingSession{
		ID:        fmt.Sprintf("pair-%d", p.nextID),
		Code:      code,
		Peer:      peer,
		StartedAt: now,
		ExpiresAt: now.Add(PairingWindow),
	}
	p.sessions[s.ID] = s
	return s, nil
}

// Session returns an in-flight session by id.
func (p *Pairer) Session(id string) (*PairingSession, error) {
	s, ok := p.sessions[id]
	if !ok {
		return nil, ErrPairingUnknown
	}
	return s, nil
}

// Confirm completes a session after the user said the codes match.
//
// It re-derives the code rather than trusting the stored one, so a session
// whose peer key was somehow altered after Begin cannot be confirmed. The
// session is consumed either way: a failed confirmation must not be retryable
// against the same session.
func (p *Pairer) Confirm(id string) (PairingOffer, error) {
	s, ok := p.sessions[id]
	if !ok {
		return PairingOffer{}, ErrPairingUnknown
	}
	delete(p.sessions, id)

	if s.Expired(p.now()) {
		return PairingOffer{}, ErrPairingExpired
	}
	code, err := DeriveCode(p.self.Public(), s.Peer.PublicKey)
	if err != nil {
		return PairingOffer{}, err
	}
	if code != s.Code {
		return PairingOffer{}, ErrCodeMismatch
	}
	return s.Peer, nil
}

// Cancel abandons a session.
func (p *Pairer) Cancel(id string) error {
	if _, ok := p.sessions[id]; !ok {
		return ErrPairingUnknown
	}
	delete(p.sessions, id)
	return nil
}

// Sweep drops sessions whose window has closed, so an abandoned attempt does
// not sit confirmable in memory.
func (p *Pairer) Sweep() int {
	now := p.now()
	n := 0
	for id, s := range p.sessions {
		if s.Expired(now) {
			delete(p.sessions, id)
			n++
		}
	}
	return n
}

// Active reports how many sessions are in flight, for tests and the API.
func (p *Pairer) Active() int { return len(p.sessions) }

// Latest returns the most recently started session, or nil.
//
// v1 pairs one device at a time, so "the ceremony in flight" is unambiguous;
// if several were somehow started, the newest is the one on screen.
func (p *Pairer) Latest() *PairingSession {
	var newest *PairingSession
	for _, s := range p.sessions {
		if newest == nil || s.StartedAt.After(newest.StartedAt) {
			newest = s
		}
	}
	return newest
}

// Clear abandons every in-flight session.
//
// Called on unpair. Without it, a half-finished ceremony left the listener
// willing to accept a stranger, and the ex-peer's supervisor walked straight
// back in within a second — so revoking a device did nothing visible.
func (p *Pairer) Clear() int {
	n := len(p.sessions)
	p.sessions = map[string]*PairingSession{}
	return n
}
