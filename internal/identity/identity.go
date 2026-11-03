// Package identity holds this device's cryptographic identity and the pinned
// identities of its peer.
//
// The trust unit in Ìbejì is the device pair, not an account. A device proves
// who it is with an Ed25519 key generated on first run and never transmitted;
// pairing exchanges public keys and pins them. Every later connection verifies
// the pinned key, and a mismatch is a hard failure — never an auto-trust — per
// the PRD: a reinstalled machine and an impersonation are indistinguishable
// from the outside, so the product must not guess.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize constants re-exported so callers need not import crypto/ed25519.
const (
	PublicKeySize  = ed25519.PublicKeySize
	PrivateKeySize = ed25519.PrivateKeySize
)

var (
	// ErrKeyMismatch is returned when a peer presents a public key that is not
	// the one pinned for it. The caller must surface a re-pair prompt and must
	// not connect.
	ErrKeyMismatch = errors.New("identity: peer key does not match the pinned key")

	// ErrNotPaired is returned when an operation needs a pinned peer and none
	// has been pinned.
	ErrNotPaired = errors.New("identity: no peer is paired")

	// ErrAlreadyPaired is returned when pinning a second peer. v1 supports
	// exactly one pair; a mesh is deferred to v2.
	ErrAlreadyPaired = errors.New("identity: a peer is already paired (v1 supports exactly two devices)")
)

// Identity is this device's own keypair.
type Identity struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// Generate creates a new device identity from the system CSPRNG.
func Generate() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate: %w", err)
	}
	return &Identity{priv: priv, pub: pub}, nil
}

// FromSeed reconstructs an identity from a stored 32-byte seed. The seed is
// what the keychain holds; the private key is derived rather than stored so a
// keychain item stays a fixed, small size.
func FromSeed(seed []byte) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity: seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("identity: derived public key has unexpected type")
	}
	return &Identity{priv: priv, pub: pub}, nil
}

// Seed returns the 32-byte seed for storage in the keychain. Callers must treat
// the result as secret and must not log it.
func (id *Identity) Seed() []byte {
	return id.priv.Seed()
}

// Public returns this device's public key.
func (id *Identity) Public() ed25519.PublicKey {
	out := make(ed25519.PublicKey, len(id.pub))
	copy(out, id.pub)
	return out
}

// Private returns this device's private key, for use by the TLS layer. Callers
// must not log or transmit it.
func (id *Identity) Private() ed25519.PrivateKey {
	return id.priv
}

// Fingerprint is the human-comparable form of this device's public key, shown
// side by side with a peer's during a key-mismatch confirmation.
func (id *Identity) Fingerprint() string {
	return Fingerprint(id.pub)
}

// Sign signs a message with this device's private key.
func (id *Identity) Sign(msg []byte) []byte {
	return ed25519.Sign(id.priv, msg)
}

// Verify checks a signature against a public key.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// Fingerprint renders a public key as "SHA256:xxxx xxxx … xxxx" — the form the
// pairing screen and the key-mismatch dialog show. Grouped in fours because it
// is read aloud and compared by eye, not parsed.
func Fingerprint(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return "SHA256:<none>"
	}
	sum := sha256.Sum256(pub)
	h := hex.EncodeToString(sum[:])
	// 8 groups of 4 = the first 16 bytes, which is ample for visual comparison.
	var b strings.Builder
	b.WriteString("SHA256:")
	for i := 0; i < 32; i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(h[i : i+4])
	}
	return b.String()
}

// EncodePublic renders a public key for storage or transport over mDNS.
func EncodePublic(pub ed25519.PublicKey) string {
	return base64.RawStdEncoding.EncodeToString(pub)
}

// DecodePublic parses a public key produced by EncodePublic.
func DecodePublic(s string) (ed25519.PublicKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("identity: decode public key: %w", err)
	}
	if len(raw) != PublicKeySize {
		return nil, fmt.Errorf("identity: public key must be %d bytes, got %d", PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
