// Package launchagent installs the daemon so it starts at login and keeps
// running when the window is closed.
//
// On macOS it must be a LaunchAgent in the logged-in user's session, never a
// root LaunchDaemon. This is a product requirement rather than a deployment
// preference: remote commands need the user's keychain, their GUI session and
// their PATH. A system daemon gets "user interaction not allowed" from the
// keychain the first time it tries to read the device key, and cannot open a
// browser for handoff at all — and both failures present as something else
// entirely, which is why the placement is enforced here and checked by doctor.
package launchagent

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Label is the LaunchAgent's reverse-DNS identifier.
const Label = "com.ibeji.daemon"

var (
	// ErrNotDarwin is returned on platforms without launchd.
	ErrNotDarwin = errors.New("launchagent: launchd is macOS-only")

	// ErrRunningAsRoot is returned when installation is attempted as root.
	//
	// Installing as root is the exact mistake this package exists to prevent:
	// it would write a plist the daemon cannot use, and the resulting failures
	// look like a broken keychain rather than a misplaced daemon.
	ErrRunningAsRoot = errors.New(
		"launchagent: refusing to install as root — Ìbejì must run as a LaunchAgent " +
			"in your login session, never a root LaunchDaemon. Run this without sudo")
)

// Config describes the agent to install.
type Config struct {
	// Label identifies the job. Defaults to Label.
	Label string

	// Program is the absolute path to the daemon binary.
	Program string

	// Args are passed after the program path.
	Args []string

	// LogDir is where stdout and stderr go. Defaults to ~/Library/Logs/Ìbejì.
	LogDir string

	// Dir overrides where the plist is written. Defaults to
	// ~/Library/LaunchAgents. Used by tests.
	Dir string
}

func (c Config) label() string {
	if c.Label != "" {
		return c.Label
	}
	return Label
}

// PlistPath is where the agent's plist lives.
func (c Config) PlistPath() (string, error) {
	dir := c.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("launchagent: locate home directory: %w", err)
		}
		dir = filepath.Join(home, "Library", "LaunchAgents")
	}
	return filepath.Join(dir, c.label()+".plist"), nil
}

func (c Config) logDir() (string, error) {
	if c.LogDir != "" {
		return c.LogDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("launchagent: locate home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "Ibeji"), nil
}

// The plist is written as text rather than through encoding/xml.
//
// launchd's parser is stricter than the format looks. It requires the
// self-closing <true/> and rejects <true></true> — which encoding/xml always
// writes — with "Bootstrap failed: 5: Input/output error", naming neither the
// key nor the reason. `plutil -lint` accepts both forms, so linting the file
// does not catch it either.
//
// The format is fixed and tiny, so emitting it directly is both simpler than
// fighting the encoder and the only way to control the exact bytes launchd
// sees. Every value is escaped on the way in.

const plistHeader = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`

const plistFooter = `</dict>
</plist>
`

// escape makes a value safe to embed in the plist. Paths can contain & and <,
// and an unescaped one produces a file launchd rejects opaquely.
func escape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		// EscapeText only fails if the writer fails, and a strings.Builder
		// does not.
		return s
	}
	return b.String()
}

func writeString(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "\t<key>%s</key>\n\t<string>%s</string>\n", escape(key), escape(value))
}

func writeBool(b *strings.Builder, key string, value bool) {
	name := "false"
	if value {
		name = "true"
	}
	// Self-closing, which is the form launchd accepts.
	fmt.Fprintf(b, "\t<key>%s</key>\n\t<%s/>\n", escape(key), name)
}

func writeInt(b *strings.Builder, key string, value int) {
	// ThrottleInterval and friends are integers to launchd; a <string> there
	// is rejected the same opaque way as an expanded boolean.
	fmt.Fprintf(b, "\t<key>%s</key>\n\t<integer>%d</integer>\n", escape(key), value)
}

func writeArray(b *strings.Builder, key string, values []string) {
	fmt.Fprintf(b, "\t<key>%s</key>\n\t<array>\n", escape(key))
	for _, v := range values {
		fmt.Fprintf(b, "\t\t<string>%s</string>\n", escape(v))
	}
	b.WriteString("\t</array>\n")
}

// Render produces the plist contents.
func Render(c Config) ([]byte, error) {
	if c.Program == "" {
		return nil, errors.New("launchagent: no program path")
	}
	if !filepath.IsAbs(c.Program) {
		// launchd will not resolve a relative path, and the failure is silent.
		return nil, fmt.Errorf("launchagent: program path must be absolute, got %q", c.Program)
	}
	logDir, err := c.logDir()
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString(plistHeader)

	writeString(&b, "Label", c.label())
	writeArray(&b, "ProgramArguments", append([]string{c.Program}, c.Args...))
	// Start at login and stay running: closing the window must never drop the
	// link.
	writeBool(&b, "RunAtLoad", true)
	writeBool(&b, "KeepAlive", true)
	writeString(&b, "StandardOutPath", filepath.Join(logDir, "daemon.log"))
	writeString(&b, "StandardErrorPath", filepath.Join(logDir, "daemon.err.log"))
	// Background: the daemon is not a foreground app and must not take focus
	// or appear in the Dock.
	writeString(&b, "ProcessType", "Background")
	// Tie the job to the app bundle, so macOS treats the daemon's privacy
	// requests as the app's own.
	//
	// Without this, launchd runs the daemon as a free-standing executable
	// with no relationship to com.ibeji.app, and macOS has no app to ask
	// about when it wants local network access. The denial is silent: every
	// LAN dial returns "connect: no route to host" — the same error an
	// absent machine gives — while the internet keeps working. Two Macs on
	// one network could not see each other, System Settings listed eleven
	// separate "ibeji-daemon" entries all switched on, and none of them
	// governed the process actually running.
	writeArray(&b, "AssociatedBundleIdentifiers", []string{"com.ibeji.app"})
	// A crash loop must not spin: launchd will not restart more often than
	// this.
	writeInt(&b, "ThrottleInterval", 10)

	b.WriteString(plistFooter)
	return []byte(b.String()), nil
}

// Install writes the plist and loads the job.
//
// Idempotent: installing over an existing agent rewrites the plist and
// reloads, which is what an upgrade does.
func Install(c Config) error {
	if runtime.GOOS != "darwin" {
		return ErrNotDarwin
	}
	if err := refuseRoot(); err != nil {
		return err
	}

	body, err := Render(c)
	if err != nil {
		return err
	}
	path, err := c.PlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("launchagent: create LaunchAgents directory: %w", err)
	}
	logDir, err := c.logDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("launchagent: create log directory: %w", err)
	}

	// Write-then-rename: launchd may read the file at any moment, and a
	// half-written plist is a job that will not load with a cryptic error.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plist-*")
	if err != nil {
		return fmt.Errorf("launchagent: create temp plist: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("launchagent: write plist: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("launchagent: chmod plist: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("launchagent: close plist: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("launchagent: install plist: %w", err)
	}

	return load(c.label(), path)
}

// refuseRoot stops the mistake this package exists to prevent.
func refuseRoot() error {
	if os.Geteuid() == 0 {
		return ErrRunningAsRoot
	}
	// SUDO_USER being set means we are under sudo even if euid was dropped.
	if os.Getenv("SUDO_USER") != "" {
		return ErrRunningAsRoot
	}
	return nil
}

// domain is the launchd domain target for the current user's GUI session.
//
// gui/<uid> rather than user/<uid>: the GUI domain is the one with access to
// the login keychain and the window server, which is exactly what the daemon
// needs and exactly what a root LaunchDaemon lacks.
func domain() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("launchagent: current user: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return "", fmt.Errorf("launchagent: parse uid %q: %w", u.Uid, err)
	}
	return "gui/" + strconv.Itoa(uid), nil
}

func load(label, path string) error {
	dom, err := domain()
	if err != nil {
		return err
	}
	target := dom + "/" + label

	// Unload an existing job first; bootstrap fails if one is already there.
	//
	// bootout returns before launchd has finished removing the job, so a
	// bootstrap issued immediately after can still collide with it and fail
	// as "Input/output error" — which names neither the job nor the cause.
	// That is the upgrade path, so it has to be waited out rather than hoped
	// past.
	_ = exec.Command("launchctl", "bootout", target).Run()
	waitUntilGone(target)

	// Retry briefly: launchd can still be settling even after the job stops
	// being listed, and this is the difference between an upgrade that works
	// and one that leaves the user with no daemon.
	var lastOut []byte
	for attempt := 0; attempt < 5; attempt++ {
		out, err := exec.Command("launchctl", "bootstrap", dom, path).CombinedOutput()
		if err == nil {
			return nil
		}
		lastOut = out
		time.Sleep(200 * time.Millisecond)
		waitUntilGone(target)
	}
	return fmt.Errorf("launchagent: bootstrap %s: %s", label, strings.TrimSpace(string(lastOut)))
}

// waitUntilGone blocks until launchd stops reporting the job, or gives up.
func waitUntilGone(target string) {
	for attempt := 0; attempt < 25; attempt++ {
		if err := exec.Command("launchctl", "print", target).Run(); err != nil {
			return // no longer known to launchd
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Uninstall unloads the job and removes the plist.
func Uninstall(c Config) error {
	if runtime.GOOS != "darwin" {
		return ErrNotDarwin
	}
	path, err := c.PlistPath()
	if err != nil {
		return err
	}
	if dom, err := domain(); err == nil {
		_ = exec.Command("launchctl", "bootout", dom+"/"+c.label()).Run()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("launchagent: remove plist: %w", err)
	}
	return nil
}

// Status reports whether the job is loaded and its pid.
type Status struct {
	Installed bool
	Loaded    bool
	PID       int
	LastExit  int
}

// Query reports the job's current state.
func Query(c Config) (Status, error) {
	var st Status
	path, err := c.PlistPath()
	if err != nil {
		return st, err
	}
	if _, err := os.Stat(path); err == nil {
		st.Installed = true
	}
	if runtime.GOOS != "darwin" {
		return st, nil
	}

	out, err := exec.Command("launchctl", "list").Output()
	if err != nil {
		return st, fmt.Errorf("launchagent: launchctl list: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[2] != c.label() {
			continue
		}
		st.Loaded = true
		if pid, err := strconv.Atoi(fields[0]); err == nil {
			st.PID = pid
		}
		if code, err := strconv.Atoi(fields[1]); err == nil {
			st.LastExit = code
		}
		return st, nil
	}
	return st, nil
}

// Verify checks that an installed plist still matches what Install would write.
//
// Called at app start, per the PRD's "verified at app start, repaired if
// missing". An upgraded binary at a new path leaves a plist pointing at the old
// one, and the symptom is a daemon that silently never starts.
func Verify(c Config) (bool, error) {
	path, err := c.PlistPath()
	if err != nil {
		return false, err
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("launchagent: read plist: %w", err)
	}
	want, err := Render(c)
	if err != nil {
		return false, err
	}
	return string(existing) == string(want), nil
}
