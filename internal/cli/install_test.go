package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// An upgrade must replace the old symlink. Leaving it would point `ibeji` at
// the previous app bundle, so the CLI and the running daemon would disagree
// about versions — the exact drift one repo, one version exists to prevent.
func TestLinkCLIReplacesAnExistingLink(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old-ibeji")
	newer := filepath.Join(dir, "new-ibeji")
	for _, p := range []string{old, newer} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "bin", "ibeji")

	if err := linkCLI(old, link); err != nil {
		t.Fatalf("first link: %v", err)
	}
	if err := linkCLI(newer, link); err != nil {
		t.Fatalf("relink: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != newer {
		t.Fatalf("link points at %s, want %s", got, newer)
	}
}

// Uninstall must never delete a real file at that path. If someone has their
// own `ibeji` binary in /usr/local/bin, removing it would be destroying work
// this program did not create.
func TestUninstallLeavesARealBinaryAlone(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "ibeji")
	if err := os.WriteFile(victim, []byte("not ours"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := &Env{Out: os.Stderr, Err: os.Stderr}
	// Uninstall also touches launchd, which fails harmlessly under test; the
	// symlink rule is what this asserts.
	_ = doUninstall(env, dir)

	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("uninstall deleted a real binary it did not create: %v", err)
	}
	if string(body) != "not ours" {
		t.Fatalf("uninstall modified a file it did not create")
	}
}

// The install path must be derived from the running binary, so a packaged app
// and a checkout both work with no special case.
func TestLocateBinariesRequiresTheDaemonAlongside(t *testing.T) {
	if _, _, err := locateBinaries(); err == nil {
		// Running under `go test`, the test binary has no ibeji-daemon
		// sibling, so this must fail — and say what to do about it.
		t.Skip("test binary happens to sit beside an ibeji-daemon")
	} else if !containsAll(err.Error(), "ibeji-daemon", "reinstall") {
		t.Fatalf("error does not name the binary and the fix: %v", err)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// The default prefix must never be a directory we cannot write. Defaulting to
// root-owned /usr/local/bin turned the common install into a sudo prompt for a
// symlink, which is not a reasonable thing to ask of someone opening an app.
func TestDefaultPrefixIsWritable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PATH", "/usr/bin:"+filepath.Join(dir, ".local", "bin"))

	// With ~/.local/bin on PATH but not yet created, the fallback still names
	// it: ensureInstalled and `ibeji install` both create it.
	if got := defaultPrefix(); got != filepath.Join(dir, ".local", "bin") {
		t.Fatalf("defaultPrefix() = %s, want the home-local bin", got)
	}

	// Once it exists and is writable, it must be chosen over /usr/local/bin.
	local := filepath.Join(dir, ".local", "bin")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/local/bin:"+local)
	if got := defaultPrefix(); got != local {
		t.Fatalf("defaultPrefix() = %s, want %s (writable beats root-owned)", got, local)
	}
}

// writable must answer by trying, not by reading mode bits: a directory can be
// mode 0755 and still refuse a write because of an ACL or a read-only mount.
func TestWritableRejectsAReadOnlyDirectory(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	if writable(ro) {
		t.Fatal("writable() said yes for a directory with no write permission")
	}
	if !writable(dir) {
		t.Fatal("writable() said no for a directory we own")
	}
}
