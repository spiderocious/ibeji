// Command ibeji-mcp is the Ìbejì MCP server.
//
// It speaks MCP over stdio and maps each tool onto a call to the daemon's local
// HTTP API, stamping every call with provenance "mcp" so an agent's actions are
// distinguishable from the user's own in the activity log and the trace.
//
// Its tool descriptions are generated from the same surface definition as
// `ibeji help --json`, so the two cannot drift.
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
		fmt.Printf("ibeji-mcp %s (%s, built %s)\n", version, commit, buildDate)
		return
	}
	fmt.Fprintln(os.Stderr, "ibeji-mcp: not yet wired up")
	os.Exit(1)
}
