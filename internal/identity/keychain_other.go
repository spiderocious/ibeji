//go:build !darwin

package identity

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Keychain on non-Darwin platforms is a 0600 file under the user's config dir.
//
// The PRD is macOS-first and requires platform-specific paths to "degrade
// gracefully rather than crash on other platforms". This is the graceful
// degradation: it works, and it is honestly weaker than a real keychain, which
// is why Warning() exists and why doctor surfaces it. A Linux/Windows keyring
// integration is a v2 concern alongside first-class support for those targets.
type Keychain struct {
	Service string
	// Dir overrides the storage directory; empty means the user config dir.
	Dir string
}

// ErrKeychainLocked is declared for parity with Darwin so callers can branch on
// it without build tags. It is never returned on this platform.
var ErrKeychainLocked = errors.New("identity: keychain refused access")

// ErrNotFound indicates no such item exists yet.
var ErrNotFound = errors.New("identity: no such keychain item")

const defaultService = "com.ibeji.daemon"

// NewKeychain builds a keychain accessor for the default service.
func NewKeychain() *Keychain {
	return &Keychain{Service: defaultService}
}

// Warning describes how this platform's secret storage is weaker than macOS's,
// for doctor to report. Returns "" on platforms with real keychain support.
func (k *Keychain) Warning() string {
	return "secrets are stored in a 0600 file, not an OS keychain — " +
		"macOS is the supported platform for v1"
}

func (k *Keychain) dir() (string, error) {
	if k.Dir != "" {
		return k.Dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("identity: locate config dir: %w", err)
	}
	svc := k.Service
	if svc == "" {
		svc = defaultService
	}
	return filepath.Join(base, svc), nil
}

func (k *Keychain) path(account string) (string, error) {
	d, err := k.dir()
	if err != nil {
		return "", err
	}
	// Account names are internal constants, but refuse separators anyway so a
	// future caller cannot traverse out of the directory.
	if strings.ContainsAny(account, `/\`) || account == "" {
		return "", fmt.Errorf("identity: invalid account name %q", account)
	}
	return filepath.Join(d, account+".secret"), nil
}

// Get reads a secret by account name.
func (k *Keychain) Get(account string) ([]byte, error) {
	p, err := k.path(account)
	if err != nil {
		return nil, err
	}
	enc, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("identity: read secret %q: %w", account, err)
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(enc)))
	if err != nil {
		return nil, fmt.Errorf("identity: secret %q is not valid base64: %w", account, err)
	}
	return raw, nil
}

// Set writes a secret by account name, replacing any existing value.
func (k *Keychain) Set(account string, secret []byte) error {
	d, err := k.dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return fmt.Errorf("identity: create secret dir: %w", err)
	}
	p, err := k.path(account)
	if err != nil {
		return err
	}
	enc := base64.RawStdEncoding.EncodeToString(secret)
	// Write to a temp file then rename, so an interrupted write cannot leave a
	// truncated key behind — the same discipline the sync engine uses.
	tmp, err := os.CreateTemp(d, ".tmp-*")
	if err != nil {
		return fmt.Errorf("identity: create temp secret: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("identity: chmod temp secret: %w", err)
	}
	if _, err := tmp.WriteString(enc); err != nil {
		tmp.Close()
		return fmt.Errorf("identity: write temp secret: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("identity: close temp secret: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("identity: install secret %q: %w", account, err)
	}
	return nil
}

// Delete removes a secret. Deleting a missing item is not an error.
func (k *Keychain) Delete(account string) error {
	p, err := k.path(account)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("identity: delete secret %q: %w", account, err)
	}
	return nil
}

// Reachable reports whether secret storage works, for `ibeji doctor`.
func (k *Keychain) Reachable() error {
	const probe = "doctor-probe"
	want := []byte("ok")
	if err := k.Set(probe, want); err != nil {
		return err
	}
	got, err := k.Get(probe)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("identity: secret probe returned %q, want %q", got, want)
	}
	return k.Delete(probe)
}
