//go:build darwin

package identity

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Keychain stores the device seed and the local API token in the macOS login
// keychain via the `security` tool.
//
// Shelling out to `security` rather than binding the Security framework is a
// deliberate trade: it keeps the daemon pure Go (no cgo, so cross-compilation
// for the Windows/Linux degradation stays trivial), and the operations here are
// rare — once at startup, once at pairing. The cost is parsing CLI output,
// which is contained to this file.
//
// This is also where the PRD's LaunchAgent requirement shows its teeth: a root
// LaunchDaemon gets "user interaction not allowed" from every call below,
// because it has no login keychain to read. ErrKeychainLocked exists so that
// failure reports itself as a placement problem rather than as a mysterious
// permission error.
type Keychain struct {
	// Service namespaces our items in the keychain. Overridable so tests can
	// use a throwaway namespace.
	Service string
}

const defaultService = "com.ibeji.daemon"

// ErrKeychainLocked indicates the keychain refused a non-interactive request.
// On a correctly installed LaunchAgent this should not happen; it is the
// signature failure of a daemon running outside the user's GUI session.
var ErrKeychainLocked = errors.New(
	"identity: keychain refused access (user interaction not allowed) — " +
		"the daemon must run as a LaunchAgent in the logged-in user's session, never a root LaunchDaemon")

// ErrNotFound indicates no such item exists yet, which on first run is normal.
var ErrNotFound = errors.New("identity: no such keychain item")

// NewKeychain builds a keychain accessor for the default service.
func NewKeychain() *Keychain {
	return &Keychain{Service: defaultService}
}

func (k *Keychain) service() string {
	if k.Service == "" {
		return defaultService
	}
	return k.Service
}

// Get reads a secret by account name.
func (k *Keychain) Get(account string) ([]byte, error) {
	cmd := exec.Command("security", "find-generic-password",
		"-s", k.service(), "-a", account, "-w")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		stderr := errb.String()
		switch {
		case strings.Contains(stderr, "could not be found"):
			return nil, ErrNotFound
		case strings.Contains(stderr, "User interaction is not allowed"):
			return nil, ErrKeychainLocked
		default:
			return nil, fmt.Errorf("identity: keychain read %q: %w: %s", account, err, strings.TrimSpace(stderr))
		}
	}
	// `-w` prints the password raw with a trailing newline. We store base64 so
	// that binary seeds survive the round trip through a text interface.
	enc := strings.TrimSpace(out.String())
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("identity: keychain item %q is not valid base64: %w", account, err)
	}
	return raw, nil
}

// Set writes a secret by account name, replacing any existing value.
func (k *Keychain) Set(account string, secret []byte) error {
	enc := base64.RawStdEncoding.EncodeToString(secret)
	// -U updates in place when the item already exists, which makes this
	// idempotent and avoids a delete/create race.
	cmd := exec.Command("security", "add-generic-password",
		"-s", k.service(), "-a", account, "-w", enc, "-U",
		"-D", "Ìbejì device secret",
		"-j", "Created by Ìbejì. Deleting this requires re-pairing.")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		stderr := errb.String()
		if strings.Contains(stderr, "User interaction is not allowed") {
			return ErrKeychainLocked
		}
		return fmt.Errorf("identity: keychain write %q: %w: %s", account, err, strings.TrimSpace(stderr))
	}
	return nil
}

// Delete removes a secret. Deleting something that does not exist is not an
// error, so uninstall paths can call it unconditionally.
func (k *Keychain) Delete(account string) error {
	cmd := exec.Command("security", "delete-generic-password",
		"-s", k.service(), "-a", account)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if strings.Contains(errb.String(), "could not be found") {
			return nil
		}
		return fmt.Errorf("identity: keychain delete %q: %w: %s", account, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// Reachable reports whether the keychain answers at all, for `ibeji doctor`.
// It writes and reads back a probe value rather than only reading, because a
// read-only check passes on a locked keychain that cannot be written to.
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
		return fmt.Errorf("identity: keychain probe returned %q, want %q", got, want)
	}
	return k.Delete(probe)
}

// Warning describes any weakness in this platform's secret storage, for doctor
// to report. macOS uses the real login keychain, so there is nothing to warn
// about; the method exists so callers need not branch on GOOS.
func (k *Keychain) Warning() string { return "" }
