// Command ibeji is the Ìbejì command-line client.
//
// It is a thin client over the daemon's local HTTP API and holds no logic the
// API does not have: if the CLI can do it, the window and an agent can do it
// too, because there is only one implementation.
//
// It exits with the remote command's own exit code, so
// `ibeji run desktop -- npm test && deploy` means what it says.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/feranmi/ibeji/internal/cli"
)

// Set by the linker; see the Makefile.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	args := os.Args[1:]
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	case "-v", "--version", "version":
		fmt.Printf("ibeji %s (%s, built %s)\n", version, commit, buildDate)
		return 0
	}

	command, ok := cli.Lookup(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "ibeji: no command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}

	client, err := cli.Connect()
	if err != nil {
		// The commonest failure by far, so its message leads somewhere
		// rather than dumping a connection error.
		fmt.Fprintf(os.Stderr, "ibeji: %v\n", err)
		if errors.Is(err, cli.ErrDaemonUnreachable) {
			fmt.Fprintln(os.Stderr, "       Start it, or run `ibeji doctor` to find out why it is not running.")
		}
		return cli.ExitCode(err)
	}

	// Ctrl-C cancels the request, which stops a streaming command rather than
	// leaving it running on the other machine with nobody watching.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := &cli.Env{Client: client, Out: os.Stdout, Err: os.Stderr}
	if err := command.Run(ctx, env, args[1:]); err != nil {
		var exitErr *cli.ExitError
		if errors.As(err, &exitErr) {
			// The command ran and said no. Not a failure of Ìbejì, so nothing
			// is printed — only the status is passed along.
			return exitErr.Code
		}
		fmt.Fprintf(os.Stderr, "ibeji: %v\n", err)
		return cli.ExitCode(err)
	}
	return 0
}

func usage(w *os.File) {
	fmt.Fprintln(w, "Ìbejì — work across your two machines.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ibeji <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range cli.Commands() {
		fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Summary)
	}
	tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Examples:")
	fmt.Fprintln(w, "  ibeji devices")
	fmt.Fprintln(w, "  ibeji ls desktop:~/projects")
	fmt.Fprintln(w, "  ibeji grep desktop:~/projects/medcord TODO --glob '*.ts'")
	fmt.Fprintln(w, "  ibeji run desktop --cwd ~/projects/medcord -- git push")
	fmt.Fprintln(w, "  ibeji check desktop https://gitlab.com")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `ibeji <command> -h` for a command's own flags.")
}
