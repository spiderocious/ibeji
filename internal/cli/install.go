package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/feranmi/ibeji/internal/launchagent"
)

// --- install ---

// cmdInstall sets the machine up from a packaged app.
//
// Everything this does was previously only reachable through `make`: a person
// who downloads a DMG has no checkout, no Go toolchain and no Makefile. It
// installs the LaunchAgent so the daemon starts at login, and symlinks the CLI
// somewhere on PATH so `ibeji` works in a terminal.
//
// It is Offline because it runs before any daemon exists — that is the point.
var cmdInstall = &Command{
	Name:    "install",
	Usage:   "ibeji install [--prefix DIR] [--uninstall]",
	Summary: "Install the daemon as a LaunchAgent and put ibeji on PATH",
	Offline: true,
	Run: func(ctx context.Context, env *Env, args []string) error {
		fs := flag.NewFlagSet("install", flag.ContinueOnError)
		fs.SetOutput(env.Err)
		prefix := fs.String("prefix", defaultPrefix(), "directory to link the CLI into")
		uninstall := fs.Bool("uninstall", false, "remove the LaunchAgent and the symlink")
		if err := fs.Parse(args); err != nil {
			return err
		}

		if runtime.GOOS != "darwin" {
			return fmt.Errorf("ibeji install is macOS-only: %w", launchagent.ErrNotDarwin)
		}
		// Installing as root writes a plist into root's LaunchAgents, where the
		// user's login session never sees it. launchagent.Install refuses too,
		// but failing here names the mistake before anything is written.
		if os.Geteuid() == 0 {
			return errors.New(
				"refusing to run as root — Ìbejì runs in your login session, " +
					"not as a system daemon. Run this without sudo")
		}

		daemon, cliPath, err := locateBinaries()
		if err != nil {
			return err
		}

		if *uninstall {
			return doUninstall(env, *prefix)
		}

		if err := launchagent.Install(launchagent.Config{Program: daemon}); err != nil {
			return fmt.Errorf("install the LaunchAgent: %w", err)
		}
		fmt.Fprintf(env.Out, "daemon   installed and started (%s)\n", daemon)

		link := filepath.Join(*prefix, "ibeji")
		switch err := linkCLI(cliPath, link); {
		case err == nil:
			fmt.Fprintf(env.Out, "cli      %s -> %s\n", link, cliPath)
		case errors.Is(err, os.ErrPermission):
			// Not fatal: the daemon is installed and the app works. Only the
			// terminal shortcut is missing, and the person can finish it.
			fmt.Fprintf(env.Out, "cli      could not write %s\n", link)
			fmt.Fprintf(env.Out, "         run: sudo ln -sf %s %s\n", cliPath, link)
		default:
			return fmt.Errorf("link the CLI: %w", err)
		}

		fmt.Fprintf(env.Out, "\nNext: run `ibeji doctor` to check the install, then pair this\n")
		fmt.Fprintf(env.Out, "machine with the other one from the Ìbejì window.\n")
		return nil
	},
}

// defaultPrefix picks a PATH directory that exists.
//
// /usr/local/bin is on the default PATH and is writable without sudo on most
// developer Macs; ~/.local/bin is the fallback for a locked-down machine.
func defaultPrefix() string {
	if st, err := os.Stat("/usr/local/bin"); err == nil && st.IsDir() {
		return "/usr/local/bin"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/usr/local/bin"
	}
	return filepath.Join(home, ".local", "bin")
}

// locateBinaries finds the daemon and CLI next to the running executable.
//
// Inside a packaged app they sit together in Contents/Resources/bin; in a
// checkout they sit together in bin/. Resolving relative to the running
// binary covers both without a special case, and symlinks are resolved so a
// CLI invoked through its own symlink still finds its siblings.
func locateBinaries() (daemon, cli string, err error) {
	self, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("locate this executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	dir := filepath.Dir(self)
	daemon = filepath.Join(dir, "ibeji-daemon")
	if _, err := os.Stat(daemon); err != nil {
		return "", "", fmt.Errorf(
			"ibeji-daemon is not next to %s — reinstall the app from the DMG", self)
	}
	return daemon, self, nil
}

// linkCLI points link at target, replacing an existing link.
func linkCLI(target, link string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	// Remove first: os.Symlink fails on an existing path, and an upgrade must
	// replace the old link rather than leaving it pointing at a stale app.
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(target, link)
}

// doUninstall removes the agent and the symlink, reporting each.
func doUninstall(env *Env, prefix string) error {
	if err := launchagent.Uninstall(launchagent.Config{}); err != nil {
		return fmt.Errorf("remove the LaunchAgent: %w", err)
	}
	fmt.Fprintln(env.Out, "daemon   stopped and removed")

	link := filepath.Join(prefix, "ibeji")
	// Only remove a symlink this install created: a real binary at that path
	// belongs to something else, and deleting it would be destructive.
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(link); err != nil {
			fmt.Fprintf(env.Out, "cli      could not remove %s (%v)\n", link, err)
		} else {
			fmt.Fprintf(env.Out, "cli      removed %s\n", link)
		}
	}
	fmt.Fprintln(env.Out, "\nYour identity and paired device are kept in the keychain.")
	fmt.Fprintln(env.Out, "Unpair from the window first if you want them gone.")
	return nil
}
