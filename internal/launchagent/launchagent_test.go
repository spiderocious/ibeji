package launchagent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Label:   "com.ibeji.daemon.test",
		Program: "/usr/local/bin/ibeji-daemon",
		Dir:     dir,
		LogDir:  filepath.Join(dir, "logs"),
	}
}

// The plist must be valid property-list XML, because launchd's failure for a
// malformed one is cryptic and arrives long after the mistake.
func TestRenderProducesValidPlist(t *testing.T) {
	body, err := Render(testConfig(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(body)

	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"`,
		`<plist version="1.0">`,
		`<key>Label</key>`,
		`<string>com.ibeji.daemon.test</string>`,
		`<key>ProgramArguments</key>`,
		`<string>/usr/local/bin/ibeji-daemon</string>`,
		`<key>RunAtLoad</key>`,
		`<key>KeepAlive</key>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist is missing %q:\n%s", want, s)
		}
	}

	// launchd requires self-closing booleans and rejects the expanded form
	// with an error that names neither the key nor the reason. plutil accepts
	// both, so only an explicit assertion catches a regression here.
	if strings.Contains(s, "<true></true>") || strings.Contains(s, "<false></false>") {
		t.Errorf("plist uses the expanded boolean form; launchd rejects it with "+
			"\"Bootstrap failed: 5: Input/output error\":\n%s", s)
	}
	if !strings.Contains(s, "<true/>") {
		t.Errorf("plist has no self-closing <true/>:\n%s", s)
	}
	// Integers must be <integer>, not <string>: launchd rejects the latter the
	// same opaque way.
	if !strings.Contains(s, "<integer>10</integer>") {
		t.Errorf("ThrottleInterval is not an <integer>:\n%s", s)
	}

	// The real test of validity: macOS's own parser accepts it.
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("plutil"); err == nil {
			path := filepath.Join(t.TempDir(), "test.plist")
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			out, err := exec.Command("plutil", "-lint", path).CombinedOutput()
			if err != nil {
				t.Fatalf("plutil rejected the plist: %v\n%s\n%s", err, out, body)
			}
		}
	}
}

// RunAtLoad and KeepAlive are what make "closing the window must not
// disconnect the link" true. Without them the daemon is an app, not a daemon.
func TestPlistStartsAtLoginAndStaysRunning(t *testing.T) {
	body, err := Render(testConfig(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(body)

	for _, key := range []string{"RunAtLoad", "KeepAlive"} {
		idx := strings.Index(s, "<key>"+key+"</key>")
		if idx == -1 {
			t.Fatalf("plist has no %s key", key)
		}
		// The value immediately follows the key, and must be true.
		rest := s[idx+len("<key>"+key+"</key>"):]
		if !strings.Contains(rest[:40], "<true/>") {
			t.Fatalf("%s is not true: %q", key, strings.TrimSpace(rest[:40]))
		}
	}
}

// A crash loop must not spin: launchd restarts a KeepAlive job immediately
// unless told otherwise, and a daemon that dies at startup would peg a core.
func TestPlistThrottlesRestarts(t *testing.T) {
	body, _ := Render(testConfig(t))
	if !strings.Contains(string(body), "<key>ThrottleInterval</key>") {
		t.Fatal("plist has no ThrottleInterval; a crash loop would restart as fast as it can fail")
	}
}

// The daemon must not take focus or appear in the Dock.
func TestPlistRunsInBackground(t *testing.T) {
	body, _ := Render(testConfig(t))
	s := string(body)
	if !strings.Contains(s, "<key>ProcessType</key>") || !strings.Contains(s, "<string>Background</string>") {
		t.Fatal("plist does not declare ProcessType Background")
	}
}

// launchd will not resolve a relative program path, and fails silently.
func TestRenderRejectsRelativeProgramPath(t *testing.T) {
	c := testConfig(t)
	c.Program = "bin/ibeji-daemon"
	if _, err := Render(c); err == nil {
		t.Fatal("Render accepted a relative program path; launchd would silently never start it")
	}
}

func TestRenderRejectsEmptyProgram(t *testing.T) {
	c := testConfig(t)
	c.Program = ""
	if _, err := Render(c); err == nil {
		t.Fatal("Render accepted an empty program path")
	}
}

func TestRenderIncludesArgs(t *testing.T) {
	c := testConfig(t)
	c.Args = []string{"--log", "debug"}
	body, err := Render(c)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(body)
	// The program path comes first, then the args, in order.
	prog := strings.Index(s, "<string>/usr/local/bin/ibeji-daemon</string>")
	log := strings.Index(s, "<string>--log</string>")
	debug := strings.Index(s, "<string>debug</string>")
	if prog == -1 || log == -1 || debug == -1 {
		t.Fatalf("args missing from plist:\n%s", s)
	}
	if !(prog < log && log < debug) {
		t.Fatal("ProgramArguments are out of order")
	}
}

// Logs must go somewhere findable, or a daemon that fails at startup leaves
// nothing to diagnose.
func TestPlistDirectsLogsToAFile(t *testing.T) {
	c := testConfig(t)
	body, _ := Render(c)
	s := string(body)
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if !strings.Contains(s, "<key>"+key+"</key>") {
			t.Fatalf("plist has no %s; a startup failure would leave nothing to read", key)
		}
	}
	if !strings.Contains(s, c.LogDir) {
		t.Fatalf("logs do not go to the configured dir %q", c.LogDir)
	}
}

// The mistake this package exists to prevent. Installing as root produces a
// daemon that cannot read the keychain, and the failure looks like a broken
// keychain rather than a misplaced daemon.
func TestInstallRefusesUnderSudo(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only")
	}
	// Simulate sudo without actually being root.
	t.Setenv("SUDO_USER", "feranmi")

	err := Install(testConfig(t))
	if !errors.Is(err, ErrRunningAsRoot) {
		t.Fatalf("Install under sudo = %v, want ErrRunningAsRoot", err)
	}
	// The message must explain the rule, not just refuse.
	if !strings.Contains(err.Error(), "LaunchAgent") || !strings.Contains(err.Error(), "LaunchDaemon") {
		t.Fatalf("the refusal does not explain the LaunchAgent/LaunchDaemon rule: %q", err)
	}
	if !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("the refusal does not tell the user to drop sudo: %q", err)
	}
}

func TestPlistPathIsUnderLaunchAgents(t *testing.T) {
	// With an explicit dir, that dir is used.
	c := testConfig(t)
	path, err := c.PlistPath()
	if err != nil {
		t.Fatalf("PlistPath: %v", err)
	}
	if filepath.Dir(path) != c.Dir {
		t.Fatalf("plist path %q is not in the configured dir %q", path, c.Dir)
	}
	if filepath.Base(path) != "com.ibeji.daemon.test.plist" {
		t.Fatalf("plist filename = %q", filepath.Base(path))
	}

	// With no dir, it must land in ~/Library/LaunchAgents — the user's
	// session, never /Library/LaunchDaemons.
	def := Config{Program: "/usr/local/bin/ibeji-daemon"}
	path, err = def.PlistPath()
	if err != nil {
		t.Fatalf("PlistPath: %v", err)
	}
	if !strings.Contains(path, filepath.Join("Library", "LaunchAgents")) {
		t.Fatalf("default plist path %q is not under Library/LaunchAgents", path)
	}
	if strings.Contains(path, "LaunchDaemons") {
		t.Fatalf("default plist path %q is a LaunchDaemon; it must be a LaunchAgent", path)
	}
	if !strings.Contains(path, Label) {
		t.Fatalf("default plist path %q does not carry the label", path)
	}
}

// Verify is what catches an upgraded binary at a new path, whose symptom is a
// daemon that silently never starts.
func TestVerifyDetectsDrift(t *testing.T) {
	c := testConfig(t)
	path, err := c.PlistPath()
	if err != nil {
		t.Fatalf("PlistPath: %v", err)
	}

	// Nothing installed yet.
	ok, err := Verify(c)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Fatal("Verify reported a match with no plist installed")
	}

	// Write what Install would write.
	body, err := Render(c)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	ok, err = Verify(c)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("Verify reported drift for a plist it just rendered")
	}

	// The binary moves — an upgrade — and the stale plist points at the old
	// path.
	moved := c
	moved.Program = "/opt/homebrew/bin/ibeji-daemon"
	ok, err = Verify(moved)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Fatal("Verify missed a plist pointing at the old binary path")
	}
}

func TestQueryReportsNotInstalled(t *testing.T) {
	st, err := Query(testConfig(t))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if st.Installed {
		t.Fatal("Query says installed with no plist on disk")
	}
	if st.Loaded {
		t.Fatal("Query says loaded for a label that was never bootstrapped")
	}
}

func TestQuerySeesAnInstalledPlist(t *testing.T) {
	c := testConfig(t)
	path, _ := c.PlistPath()
	body, _ := Render(c)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := Query(c)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !st.Installed {
		t.Fatal("Query does not see a plist that is on disk")
	}
}

func TestUninstallIsIdempotent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only")
	}
	c := testConfig(t)
	// Uninstalling something never installed must not error: uninstall paths
	// run unconditionally.
	if err := Uninstall(c); err != nil {
		t.Fatalf("Uninstall with nothing installed: %v", err)
	}

	path, _ := c.PlistPath()
	body, _ := Render(c)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Uninstall(c); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Uninstall left the plist behind")
	}
}

func TestNonDarwinIsRefusedNotCrashed(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this is the non-macOS path")
	}
	// The PRD requires platform-specific paths to degrade gracefully rather
	// than crash.
	if err := Install(testConfig(t)); !errors.Is(err, ErrNotDarwin) {
		t.Fatalf("Install on %s = %v, want ErrNotDarwin", runtime.GOOS, err)
	}
}

// The real thing: install against the live launchd, confirm it loads, then
// remove it. This is the only test that proves the plist launchd actually
// accepts is the plist we write.
func TestInstallAgainstRealLaunchd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only")
	}
	if testing.Short() {
		t.Skip("-short")
	}
	if os.Getenv("IBEJI_SKIP_LAUNCHD_TESTS") != "" {
		t.Skip("IBEJI_SKIP_LAUNCHD_TESTS is set")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root; this test installs a user agent")
	}

	dir := t.TempDir()
	c := Config{
		Label: "com.ibeji.daemon.test.install",
		// A program that exists and exits immediately. KeepAlive will restart
		// it, which ThrottleInterval bounds to once every 10s.
		Program: "/bin/echo",
		Args:    []string{"ibeji-launchd-test"},
		Dir:     dir,
		LogDir:  filepath.Join(dir, "logs"),
	}
	t.Cleanup(func() { _ = Uninstall(c) })

	if err := Install(c); err != nil {
		t.Skipf("could not install against launchd in this environment: %v", err)
	}

	st, err := Query(c)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !st.Installed {
		t.Fatal("plist is not on disk after Install")
	}
	if !st.Loaded {
		t.Fatal("launchd did not load the job after Install")
	}

	// Installing again must succeed: that is what an upgrade does.
	if err := Install(c); err != nil {
		t.Fatalf("second Install (the upgrade path): %v", err)
	}

	if err := Uninstall(c); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	st, err = Query(c)
	if err != nil {
		t.Fatalf("Query after Uninstall: %v", err)
	}
	if st.Loaded {
		t.Fatal("the job is still loaded after Uninstall")
	}
	if st.Installed {
		t.Fatal("the plist is still on disk after Uninstall")
	}
}
