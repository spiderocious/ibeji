// Command ibeji is the Ìbejì command-line client.
//
// It is a thin client over the daemon's local HTTP API and holds no logic the
// API does not have: if the CLI can do it, the app and an agent can do it too,
// because there is only one implementation.
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
		fmt.Printf("ibeji %s (%s, built %s)\n", version, commit, buildDate)
		return
	}
	fmt.Fprintln(os.Stderr, "ibeji: not yet wired up")
	os.Exit(1)
}
