package doctor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// The contract the whole package exists for: a check that reports a problem
// must say what to do about it. A message naming only the symptom moves the
// guessing somewhere else, which is the burden doctor is meant to remove.
func TestEveryFailureNamesAFix(t *testing.T) {
	d := New()
	d.Add(1, func(context.Context) Check {
		return Failedf("Thing", "Do the fix.", "it is broken")
	})
	d.Add(2, func(context.Context) Check {
		return Warned("Other", "it is odd", "Consider the fix.")
	})
	d.Add(3, func(context.Context) Check { return Passed("Fine", "all good") })

	for _, c := range d.Run(ctx(t)).Checks {
		if c.Severity == Pass {
			continue
		}
		if c.Fix == "" {
			t.Errorf("check %q reports a problem with no fix", c.Name)
		}
		if c.Detail == "" {
			t.Errorf("check %q reports no detail", c.Name)
		}
	}
}

func TestRunOrdersByDependency(t *testing.T) {
	d := New()
	// Registered out of order; a failure high in the list explains the ones
	// below it, so the report must read in dependency order.
	d.Add(OrderLoginShell, func(context.Context) Check { return Passed("Login shell", "") })
	d.Add(OrderLaunchAgent, func(context.Context) Check { return Passed("LaunchAgent", "") })
	d.Add(OrderKeychain, func(context.Context) Check { return Passed("Keychain", "") })

	got := []string{}
	for _, c := range d.Run(ctx(t)).Checks {
		got = append(got, c.Name)
	}
	want := []string{"LaunchAgent", "Keychain", "Login shell"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// One hung check must not stop the rest of the report: finding out which thing
// is broken is the entire job.
func TestOneSlowCheckDoesNotStopTheReport(t *testing.T) {
	d := New()
	d.Add(1, func(c context.Context) Check {
		<-c.Done() // blocks until the per-check timeout fires
		return Failedf("Hung", "Fix it.", "timed out")
	})
	d.Add(2, func(context.Context) Check { return Passed("After", "ran anyway") })

	done := make(chan Report, 1)
	go func() { done <- d.Run(context.Background()) }()

	select {
	case r := <-done:
		if len(r.Checks) != 2 {
			t.Fatalf("got %d checks, want 2", len(r.Checks))
		}
		if r.Checks[1].Name != "After" || !r.Checks[1].OK {
			t.Fatal("the check after a hung one did not run")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned; a hung check blocked the whole report")
	}
}

func TestReportCountsAndFailed(t *testing.T) {
	r := Report{Checks: []Check{
		Passed("a", ""), Passed("b", ""),
		Warned("c", "", "fix"),
		Failedf("d", "fix", "broken"),
	}}
	pass, warn, fail := r.Counts()
	if pass != 2 || warn != 1 || fail != 1 {
		t.Fatalf("counts = %d/%d/%d, want 2/1/1", pass, warn, fail)
	}
	if !r.Failed() {
		t.Fatal("Failed() = false with a failing check")
	}
	// A warning alone must not fail the run: a sleeping peer and a blocked
	// multicast network are both normal.
	warnOnly := Report{Checks: []Check{Passed("a", ""), Warned("b", "", "fix")}}
	if warnOnly.Failed() {
		t.Fatal("Failed() = true with only warnings; warnings are not failures")
	}
}

// --- the login shell check -------------------------------------------------

// The check that earns its keep. A login shell reads ~/.zprofile and ~/.zshenv
// but never ~/.zshrc, so a PATH set in ~/.zshrc makes remote exec fail with
// "command not found" for a tool that works when typed on that machine.
//
// This test builds exactly that situation with a real shell and a real tool.
func TestLoginShellDetectsPathOnlyInZshrc(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the zprofile/zshrc distinction is the macOS case")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A tool that exists only on the PATH set in .zshrc.
	tool := "ibeji-fake-tool"
	script := filepath.Join(bin, tool)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write tool: %v", err)
	}

	// The mistake: PATH exported from .zshrc, which a login shell never reads.
	if err := os.WriteFile(filepath.Join(home, ".zshrc"),
		[]byte("export PATH=\""+bin+":$PATH\"\n"), 0o644); err != nil {
		t.Fatalf("write .zshrc: %v", err)
	}
	// An empty .zprofile, so the login shell genuinely lacks it.
	if err := os.WriteFile(filepath.Join(home, ".zprofile"), []byte("\n"), 0o644); err != nil {
		t.Fatalf("write .zprofile: %v", err)
	}

	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)

	check := LoginShell([]string{tool})(ctx(t))

	if check.OK {
		t.Fatalf("check passed, but %s is only on the interactive PATH: %+v", tool, check)
	}
	if check.Severity != Fail {
		t.Fatalf("severity = %q, want fail — this breaks every remote command", check.Severity)
	}
	// The fix must name the actual remedy: move it to .zprofile, and say why.
	if !strings.Contains(check.Fix, ".zprofile") {
		t.Fatalf("fix does not mention ~/.zprofile: %q", check.Fix)
	}
	if !strings.Contains(check.Fix, ".zshrc") {
		t.Fatalf("fix does not mention ~/.zshrc, where the line actually is: %q", check.Fix)
	}
	if !strings.Contains(check.Detail, tool) {
		t.Fatalf("detail does not name the missing tool: %q", check.Detail)
	}
}

// A tool that is simply not installed is a warning, not a failure: not every
// machine needs every toolchain, and failing the run would cry wolf.
func TestLoginShellWarnsForGenuinelyAbsentTools(t *testing.T) {
	if _, err := exec.LookPath(loginShell()); err != nil {
		t.Skipf("%s not available", loginShell())
	}
	check := LoginShell([]string{"ibeji-definitely-not-installed-anywhere"})(ctx(t))
	if check.Severity == Fail {
		t.Fatalf("an uninstalled tool failed the check: %+v", check)
	}
	if check.Severity != Warn {
		t.Fatalf("severity = %q, want warn", check.Severity)
	}
}

func TestLoginShellPassesWithTheToolsItFinds(t *testing.T) {
	if _, err := exec.LookPath(loginShell()); err != nil {
		t.Skipf("%s not available", loginShell())
	}
	// `sh` is on every PATH worth having, including a login shell's.
	check := LoginShell([]string{"sh"})(ctx(t))
	if !check.OK {
		t.Fatalf("check failed for a universally present tool: %+v", check)
	}
	if check.Severity != Pass {
		t.Fatalf("severity = %q, want pass", check.Severity)
	}
}

func TestShellQuote(t *testing.T) {
	// The inputs are internal constants today, but the helper will outlive
	// that assumption.
	for in, want := range map[string]string{
		"git":        `'git'`,
		"a b":        `'a b'`,
		"it's":       `'it'\''s'`,
		"; rm -rf /": `'; rm -rf /'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- keychain ---------------------------------------------------------------

type fakeStore struct {
	err     error
	warning string
}

func (f fakeStore) Reachable() error { return f.err }
func (f fakeStore) Warning() string  { return f.warning }

// The signature failure of a daemon installed in the wrong place. The fix must
// name the real cause rather than telling the user to unlock something.
func TestKeychainNamesTheLaunchDaemonMistake(t *testing.T) {
	check := Keychain(fakeStore{
		err: errors.New("security: User interaction is not allowed."),
	})(ctx(t))

	if check.OK {
		t.Fatal("a locked keychain passed")
	}
	if !strings.Contains(check.Fix, "LaunchAgent") {
		t.Fatalf("fix does not name the LaunchAgent/LaunchDaemon mistake: %q", check.Fix)
	}
	if !strings.Contains(strings.ToLower(check.Fix), "launchdaemon") {
		t.Fatalf("fix does not name the root LaunchDaemon as the likely cause: %q", check.Fix)
	}
}

func TestKeychainPassesAndWarns(t *testing.T) {
	if check := Keychain(fakeStore{})(ctx(t)); !check.OK || check.Severity != Pass {
		t.Fatalf("a reachable keychain did not pass: %+v", check)
	}
	// A weaker store on a non-macOS platform is a warning, not a failure.
	check := Keychain(fakeStore{warning: "secrets are in a 0600 file"})(ctx(t))
	if check.Severity != Warn {
		t.Fatalf("severity = %q, want warn", check.Severity)
	}
	if check.Detail == "" {
		t.Fatal("the warning says nothing about what is weaker")
	}
}

func TestKeychainGenericFailure(t *testing.T) {
	check := Keychain(fakeStore{err: errors.New("something else went wrong")})(ctx(t))
	if check.Severity != Fail {
		t.Fatalf("severity = %q, want fail", check.Severity)
	}
	if check.Fix == "" {
		t.Fatal("no fix offered")
	}
}

// --- API token --------------------------------------------------------------

func TestAPITokenAgainstARealServer(t *testing.T) {
	const token = "the-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if check := APIToken(srv.URL, token)(ctx(t)); !check.OK {
		t.Fatalf("a valid token failed: %+v", check)
	}

	// A stale handoff file is a real situation: the daemon restarted and
	// rotated nothing, but the file was not reread.
	check := APIToken(srv.URL, "stale-token")(ctx(t))
	if check.OK {
		t.Fatal("a rejected token passed")
	}
	if !strings.Contains(check.Fix, "api.json") {
		t.Fatalf("fix does not point at the handoff file: %q", check.Fix)
	}
}

func TestAPITokenWhenDaemonIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	check := APIToken(url, "tok")(ctx(t))
	if check.OK {
		t.Fatal("the check passed against a dead daemon")
	}
	if !strings.Contains(strings.ToLower(check.Fix), "daemon") {
		t.Fatalf("fix does not mention starting the daemon: %q", check.Fix)
	}
}

func TestAPITokenWithoutCredentials(t *testing.T) {
	check := APIToken("", "")(ctx(t))
	if check.OK {
		t.Fatal("the check passed with no endpoint or token")
	}
}

// --- mDNS -------------------------------------------------------------------

// Blocked multicast is a warning, never a failure: the product works over a
// manually entered IP, and the networks that block it are the ones this product
// is most useful on.
func TestMDNSFailureIsAWarningWithTheIPAdvice(t *testing.T) {
	check := MDNS("Desktop", func(context.Context) (string, error) {
		return "", errors.New("no such host")
	})(ctx(t))

	if check.Severity == Fail {
		t.Fatal("blocked multicast failed the run; the product works fine over a typed IP")
	}
	if check.Severity != Warn {
		t.Fatalf("severity = %q, want warn", check.Severity)
	}
	if !strings.Contains(check.Fix, "IP address") {
		t.Fatalf("fix does not suggest adding by IP: %q", check.Fix)
	}
}

func TestMDNSPasses(t *testing.T) {
	check := MDNS("Desktop", func(context.Context) (string, error) {
		return "192.168.1.31", nil
	})(ctx(t))
	if !check.OK || check.Severity != Pass {
		t.Fatalf("a resolving peer did not pass: %+v", check)
	}
	if !strings.Contains(check.Detail, "192.168.1.31") {
		t.Fatalf("detail does not carry the address: %q", check.Detail)
	}
}

// --- link -------------------------------------------------------------------

type fakeLink struct {
	paired, up, keyOK bool
	name, addr        string
}

func (f fakeLink) Paired() bool        { return f.paired }
func (f fakeLink) PeerName() string    { return f.name }
func (f fakeLink) PeerAddress() string { return f.addr }
func (f fakeLink) LinkUp() bool        { return f.up }
func (f fakeLink) KeyMatches() bool    { return f.keyOK }

// A sleeping peer is normal on a LAN, so it warns and leads with the ordinary
// explanation rather than alarming.
func TestLinkOfflineIsAWarning(t *testing.T) {
	check := Link(fakeLink{paired: true, keyOK: true, up: false, name: "Desktop"})(ctx(t))
	if check.Severity == Fail {
		t.Fatal("a sleeping peer failed the run; offline is normal on a LAN")
	}
	if !strings.Contains(strings.ToLower(check.Fix), "asleep") {
		t.Fatalf("fix does not lead with the ordinary explanation: %q", check.Fix)
	}
}

// A key mismatch is never transient and never auto-trusted, so it fails and
// the fix must not tell the user to just pair again without thinking.
func TestLinkKeyMismatchFailsAndWarnsAboutTrust(t *testing.T) {
	check := Link(fakeLink{paired: true, keyOK: false, name: "Desktop"})(ctx(t))
	if check.Severity != Fail {
		t.Fatalf("severity = %q, want fail for a key mismatch", check.Severity)
	}
	if !strings.Contains(check.Fix, "reinstalled") {
		t.Fatalf("fix does not offer the benign explanation: %q", check.Fix)
	}
	if !strings.Contains(check.Fix, "do not pair") {
		t.Fatalf("fix does not warn against pairing blindly: %q", check.Fix)
	}
}

func TestLinkUnpairedIsAWarning(t *testing.T) {
	check := Link(fakeLink{paired: false})(ctx(t))
	if check.Severity != Warn {
		t.Fatalf("severity = %q, want warn — not being paired yet is normal on first run", check.Severity)
	}
}

func TestLinkUpPasses(t *testing.T) {
	check := Link(fakeLink{paired: true, keyOK: true, up: true,
		name: "Desktop", addr: "192.168.1.31"})(ctx(t))
	if !check.OK {
		t.Fatalf("a healthy link did not pass: %+v", check)
	}
	if !strings.Contains(check.Detail, "192.168.1.31") {
		t.Fatalf("detail omits the address: %q", check.Detail)
	}
}

// --- the standard six -------------------------------------------------------

func TestStandardRunsSixChecksInOrder(t *testing.T) {
	report := Standard(Config{
		LaunchAgentLabel: "com.ibeji.daemon.test-nonexistent",
		Secrets:          fakeStore{},
		Endpoint:         "",
		Token:            "",
		PeerName:         "Desktop",
		ResolvePeer:      func(context.Context) (string, error) { return "", errors.New("blocked") },
		Link:             fakeLink{paired: false},
		LoginShellTools:  []string{"sh"},
	}).Run(ctx(t))

	if len(report.Checks) != 6 {
		t.Fatalf("got %d checks, want 6", len(report.Checks))
	}
	want := []string{"LaunchAgent", "Keychain", "API token", "mDNS", "Link", "Login shell"}
	for i, w := range want {
		if report.Checks[i].Name != w {
			t.Fatalf("check %d is %q, want %q", i, report.Checks[i].Name, w)
		}
	}
	// Every check records how long it took, so a slow one is visible.
	for _, c := range report.Checks {
		if c.Duration < 0 {
			t.Errorf("check %q has a negative duration", c.Name)
		}
	}
}
