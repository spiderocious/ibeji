package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Persisting the pinned peer.
//
// The pin is the trust root: it is what makes "subsequent connections verify
// the pinned key and never re-prompt" true, which is the whole point of
// pairing once. Until this file existed, Store.Pin wrote the peer to a struct
// field and nothing else, so the pairing lived exactly as long as the daemon
// process. Every pairing test used one long-lived store and passed; the
// product lost its pairing on the first restart, and the symptom reached a
// user as "I suddenly cannot connect to my other Mac this morning".
//
// It goes in the keychain beside the device seed, not in ~/.ibeji. That
// directory holds the trace database and the API handoff file — cache and
// runtime state, both safe to delete. The pinned key is neither.

// accountPeer is the keychain account holding the pinned peer. A stable
// string: changing it orphans the existing item and silently forces a
// re-pair, which is exactly the failure this file exists to prevent.
const accountPeer = "pinned-peer"

// storedPeer is the on-disk shape. Separate from Peer so the wire format is
// explicit and can gain fields without changing the in-memory type.
//
// The public key is base64 rather than raw bytes because the keychain holds
// a blob that a human may well end up looking at while debugging.
type storedPeer struct {
	PublicKey   string    `json:"publicKey"`
	Name        string    `json:"name"`
	LastAddress string    `json:"lastAddress,omitempty"`
	PairedAt    time.Time `json:"pairedAt"`
}

// savePeer writes the pinned peer, or clears it when p is nil.
func savePeer(ks SecretStore, p *Peer) error {
	if ks == nil {
		return nil // an in-memory store, used by tests that do not need durability
	}
	if p == nil {
		if err := ks.Delete(accountPeer); err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("identity: forget pinned peer: %w", err)
		}
		return nil
	}
	blob, err := json.Marshal(storedPeer{
		PublicKey:   base64.StdEncoding.EncodeToString(p.PublicKey),
		Name:        p.Name,
		LastAddress: p.LastAddress,
		PairedAt:    p.PairedAt,
	})
	if err != nil {
		return fmt.Errorf("identity: encode pinned peer: %w", err)
	}
	if err := ks.Set(accountPeer, blob); err != nil {
		return fmt.Errorf("identity: persist pinned peer: %w", err)
	}
	return nil
}

// loadPeer reads the pinned peer, returning nil when none is stored.
func loadPeer(ks SecretStore) (*Peer, error) {
	if ks == nil {
		return nil, nil
	}
	blob, err := ks.Get(accountPeer)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var sp storedPeer
	if err := json.Unmarshal(blob, &sp); err != nil {
		// A corrupt record must not be papered over by silently starting
		// unpaired: that reads to the user as "it forgot my other Mac", with
		// no way to tell that from a revoked pairing. Say so.
		return nil, fmt.Errorf("identity: stored pairing is unreadable (re-pairing will be required): %w", err)
	}
	pub, err := base64.StdEncoding.DecodeString(sp.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("identity: stored pairing has an unreadable key (re-pairing will be required): %w", err)
	}
	if len(pub) != PublicKeySize {
		return nil, fmt.Errorf("identity: stored pairing has a %d-byte key, want %d (re-pairing will be required)",
			len(pub), PublicKeySize)
	}
	return &Peer{
		PublicKey:   ed25519.PublicKey(pub),
		Name:        sp.Name,
		LastAddress: sp.LastAddress,
		PairedAt:    sp.PairedAt,
	}, nil
}

// LoadStore builds a store and restores the pinned peer from the secret
// store, so a daemon that restarts comes back paired.
//
// This is the constructor the daemon uses. NewStore remains for tests that
// want a store with no durability.
func LoadStore(self *Identity, name string, ks SecretStore) (*Store, error) {
	peer, err := loadPeer(ks)
	if err != nil {
		return nil, err
	}
	return &Store{self: self, name: name, peer: peer, secrets: ks}, nil
}
