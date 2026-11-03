package identity

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Keychain account names. Stable strings: changing one orphans the existing
// item and forces a re-pair, so they are constants rather than derived.
const (
	accountSeed     = "device-seed"
	accountAPIToken = "api-token"
)

// SecretStore is the subset of Keychain the loader needs, so tests can supply
// an in-memory implementation without touching the real keychain.
type SecretStore interface {
	Get(account string) ([]byte, error)
	Set(account string, secret []byte) error
	Delete(account string) error
}

// Load returns this device's identity, generating and persisting one on first
// run. The seed lives in the keychain; the keypair is derived each start.
func Load(ks SecretStore) (*Identity, error) {
	seed, err := ks.Get(accountSeed)
	switch {
	case err == nil:
		id, err := FromSeed(seed)
		if err != nil {
			// A corrupt seed is not recoverable by retrying, and silently
			// regenerating would break the pinning on the peer — which would
			// then look like an impersonation. Fail loudly instead.
			return nil, fmt.Errorf("identity: stored seed is unusable (re-pairing will be required): %w", err)
		}
		return id, nil

	case errors.Is(err, ErrNotFound):
		id, err := Generate()
		if err != nil {
			return nil, err
		}
		if err := ks.Set(accountSeed, id.Seed()); err != nil {
			return nil, fmt.Errorf("identity: persist new device seed: %w", err)
		}
		return id, nil

	default:
		return nil, err
	}
}

// APIToken returns the local API token, generating and persisting one on first
// run.
//
// This token is what stops any local process from driving the daemon. Binding
// the API to loopback keeps the network out; the token keeps a browser tab on
// this machine out, which is the likelier attack of the two.
func APIToken(ks SecretStore) (string, error) {
	raw, err := ks.Get(accountAPIToken)
	switch {
	case err == nil:
		if len(raw) < 16 {
			return "", errors.New("identity: stored API token is too short to be trusted")
		}
		return string(raw), nil

	case errors.Is(err, ErrNotFound):
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("identity: generate API token: %w", err)
		}
		tok := base64.RawURLEncoding.EncodeToString(buf)
		if err := ks.Set(accountAPIToken, []byte(tok)); err != nil {
			return "", fmt.Errorf("identity: persist API token: %w", err)
		}
		return tok, nil

	default:
		return "", err
	}
}

// Forget removes this device's identity and API token. Used by an uninstall,
// never by a reconnect — regenerating an identity invalidates the peer's
// pinning and presents as a key mismatch there, which is correct but abrupt.
func Forget(ks SecretStore) error {
	if err := ks.Delete(accountSeed); err != nil {
		return err
	}
	return ks.Delete(accountAPIToken)
}

// MemoryStore is an in-memory SecretStore for tests and for the two-daemon
// harness, where two daemons in one process must not share one keychain.
type MemoryStore struct {
	items map[string][]byte
}

// NewMemoryStore builds an empty in-memory secret store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: map[string][]byte{}}
}

// Get implements SecretStore.
func (m *MemoryStore) Get(account string) ([]byte, error) {
	v, ok := m.items[account]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

// Set implements SecretStore.
func (m *MemoryStore) Set(account string, secret []byte) error {
	if m.items == nil {
		m.items = map[string][]byte{}
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	m.items[account] = cp
	return nil
}

// Delete implements SecretStore.
func (m *MemoryStore) Delete(account string) error {
	delete(m.items, account)
	return nil
}
