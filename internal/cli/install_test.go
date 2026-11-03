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
