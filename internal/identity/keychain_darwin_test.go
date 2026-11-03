//go:build darwin

package identity

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// These tests touch the real login keychain, which is the point: the keychain
// is the integration most likely to fail in a way a mock would hide (locked
// keychain, prompt-on-access, a daemon in the wrong session). They use a
// throwaway service name and clean up after themselves.
//
// Skipped when IBEJI_SKIP_KEYCHAIN_TESTS is set, for CI without a login
// keychain.
func testKeychain(t *testing.T) *Keychain {
	t.Helper()
	if os.Getenv("IBEJI_SKIP_KEYCHAIN_TESTS") != "" {
		t.Skip("IBEJI_SKIP_KEYCHAIN_TESTS is set")
	}
	svc := fmt.Sprintf("com.ibeji.test.%d", time.Now().UnixNano())
	k := &Keychain{Service: svc}
	t.Cleanup(func() {
		// Best-effort: remove anything these tests created.
		for _, acct := range []string{"device-seed", "api-token", "doctor-probe", "roundtrip"} {
			_ = k.Delete(acct)
		}
	})
	return k
}

func TestKeychainRoundTrip(t *testing.T) {
	k := testKeychain(t)

	// A binary value with a NUL and a newline: the `security` CLI is a text
	// interface, so this is the case base64 encoding exists to survive.
	secret := []byte{0x00, 0x01, 'a', '\n', 0xff, 0x7f}
	if err := k.Set("roundtrip", secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := k.Get("roundtrip")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("round trip corrupted the secret: got %x, want %x", got, secret)
	}
}

func TestKeychainGetMissingIsErrNotFound(t *testing.T) {
	k := testKeychain(t)
	if _, err := k.Get("never-written"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a missing item = %v, want ErrNotFound", err)
	}
}

func TestKeychainSetIsIdempotent(t *testing.T) {
	k := testKeychain(t)

	if err := k.Set("roundtrip", []byte("first")); err != nil {
		t.Fatalf("Set (first): %v", err)
	}
	// -U must update in place rather than fail or duplicate the item.
	if err := k.Set("roundtrip", []byte("second")); err != nil {
		t.Fatalf("Set (overwrite): %v", err)
	}
	got, err := k.Get("roundtrip")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("overwrite did not take: got %q, want %q", got, "second")
	}
}

func TestKeychainDeleteMissingIsNotAnError(t *testing.T) {
	k := testKeychain(t)
	// Uninstall paths call Delete unconditionally.
	if err := k.Delete("never-written"); err != nil {
		t.Fatalf("Delete of a missing item returned %v; it must be a no-op", err)
	}
}

func TestKeychainReachable(t *testing.T) {
	k := testKeychain(t)
	if err := k.Reachable(); err != nil {
		t.Fatalf("Reachable: %v", err)
	}
	// The probe must clean up after itself, or doctor would litter the keychain
	// on every run.
	if _, err := k.Get("doctor-probe"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reachable left its probe behind (err=%v)", err)
	}
}

func TestKeychainNoWarningOnDarwin(t *testing.T) {
	k := testKeychain(t)
	if w := k.Warning(); w != "" {
		t.Fatalf("Warning on macOS = %q, want empty (the real keychain is in use)", w)
	}
}

// The full first-run path against the real keychain: generate an identity and
// an API token, then reload both and confirm they are the same.
func TestLoadAndAPITokenAgainstRealKeychain(t *testing.T) {
	k := testKeychain(t)

	id, err := Load(k)
	if err != nil {
		t.Fatalf("Load (first run): %v", err)
	}
	tok, err := APIToken(k)
	if err != nil {
		t.Fatalf("APIToken (first run): %v", err)
	}

	id2, err := Load(k)
	if err != nil {
		t.Fatalf("Load (restart): %v", err)
	}
	tok2, err := APIToken(k)
	if err != nil {
		t.Fatalf("APIToken (restart): %v", err)
	}

	if !id.Public().Equal(id2.Public()) {
		t.Fatal("identity changed across a simulated daemon restart; the peer would see a key mismatch")
	}
	if tok != tok2 {
		t.Fatal("API token changed across a simulated daemon restart; every client would break")
	}

	if err := Forget(k); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := k.Get(accountSeed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seed survived Forget (err=%v)", err)
	}
}
