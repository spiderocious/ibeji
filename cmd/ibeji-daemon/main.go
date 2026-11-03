// Command ibeji-daemon is the Ìbejì background daemon.
//
// It holds the device identity, the link to the paired device, the permission
// engine, the activity log and the trace, and it serves the local HTTP API that
// the app, the CLI and the MCP server are all clients of.
//
// On macOS it must run as a LaunchAgent inside the logged-in user's session —
// never a root LaunchDaemon. Remote commands need the user's keychain, GUI
// session and PATH; a system daemon gets "user interaction not allowed" from
// the keychain and cannot open a browser for handoff.
package main

import (
	"fmt"
	"os"
)

// Set by the linker; see the Makefile.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("ibeji-daemon %s (%s, built %s)\n", version, commit, buildDate)
		return
	}
	fmt.Fprintln(os.Stderr, "ibeji-daemon: not yet wired up")
	os.Exit(1)
}
