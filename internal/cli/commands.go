package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/feranmi/ibeji/internal/remote"
)

// Command is one `ibeji` subcommand.
type Command struct {
	Name    string
	Usage   string
	Summary string

	// Run does the work. It returns an error; main maps it to an exit code.
	Run func(ctx context.Context, env *Env, args []string) error
}

// Env is what a command needs: a connection and somewhere to write.
//
// Passed rather than global so the commands are testable without a daemon and
// without capturing os.Stdout.
type Env struct {
	Client *Client
	Out    io.Writer
	Err    io.Writer
}

// Commands returns every subcommand, in the order help lists them.
func Commands() []*Command {
	return []*Command{
		cmdDevices, cmdInfo, cmdLs, cmdCat, cmdGrep, cmdRun, cmdCheck, cmdDoctor, cmdTrace,
	}
}

// Lookup finds a command by name.
func Lookup(name string) (*Command, bool) {
	for _, c := range Commands() {
		if c.Name == name {
			return c, true
		}
	}
	return nil, false
}

// --- devices ---

var cmdDevices = &Command{
	Name:    "devices",
	Usage:   "ibeji devices",
	Summary: "List the paired device: name, address, status and mode",
	Run: func(ctx context.Context, env *Env, args []string) error {
		var out struct {
			Devices []struct {
				Name        string `json:"name"`
				Address     string `json:"address"`
				Status      string `json:"status"`
				Mode        string `json:"mode"`
				Fingerprint string `json:"fingerprint"`
			} `json:"devices"`
		}
		if err := env.Client.GetJSON(ctx, "/v1/devices", &out); err != nil {
			return err
		}
		if len(out.Devices) == 0 {
			fmt.Fprintln(env.Out, "No device paired. Open Ìbejì to pair one.")
			return nil
		}
		w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tADDRESS\tSTATUS\tMODE")
		for _, d := range out.Devices {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Name, d.Address, d.Status, d.Mode)
		}
		return w.Flush()
	},
}

// --- device info ---

var cmdInfo = &Command{
	Name:    "info",
	Usage:   "ibeji info <device>",
	Summary: "The other machine's OS, arch and toolchain versions",
	Run: func(ctx context.Context, env *Env, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("usage: ibeji info <device>")
		}
		var info remote.DeviceInfo
		if err := env.Client.GetJSON(ctx, "/v1/device/info", &info); err != nil {
			return err
		}
		w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "host\t%s\n", info.Hostname)
		fmt.Fprintf(w, "os\t%s/%s\n", info.OS, info.Arch)
		fmt.Fprintf(w, "user\t%s\n", info.User)
		fmt.Fprintf(w, "shell\t%s\n", info.Shell)
		tools := make([]string, 0, len(info.Toolchain))
		for name := range info.Toolchain {
			tools = append(tools, name)
		}
		sort.Strings(tools)
		for _, name := range tools {
			fmt.Fprintf(w, "%s\t%s\n", name, info.Toolchain[name])
		}
		return w.Flush()
	},
}

// --- ls ---

var cmdLs = &Command{
	Name:    "ls",
	Usage:   "ibeji ls <device>:<path>",
	Summary: "List a directory on the other machine",
	Run: func(ctx context.Context, env *Env, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("usage: ibeji ls <device>:<path>")
		}
		target, err := ParseTarget(args[0])
		if err != nil {
			return err
		}
		var out struct {
			Entries []remote.DirEntry `json:"entries"`
		}
		path := target.PathOr("~")
		if err := env.Client.GetJSON(ctx, "/v1/fs/list?path="+url.QueryEscape(path), &out); err != nil {
			return err
		}
		w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		for _, e := range out.Entries {
			name := e.Name
			if e.IsDir {
				name += "/"
			}
			if e.Symlink != "" {
				name += " -> " + e.Symlink
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", e.Mode, formatBytes(e.Size), name)
		}
		return w.Flush()
	},
}

// --- cat ---

var cmdCat = &Command{
	Name:    "cat",
	Usage:   "ibeji cat <device>:<path>",
	Summary: "Print a file from the other machine",
	Run: func(ctx context.Context, env *Env, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("usage: ibeji cat <device>:<path>")
		}
		target, err := ParseTarget(args[0])
		if err != nil {
			return err
		}
		if target.Path == "" {
			return fmt.Errorf("which file? try %s:~/notes.md", target.Device)
		}
		resp, err := env.Client.Do(ctx, http.MethodGet,
			"/v1/fs/read?path="+url.QueryEscape(target.Path), nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(env.Out, resp.Body)
		return err
	},
}

// --- grep ---

var cmdGrep = &Command{
	Name:    "grep",
	Usage:   "ibeji grep <device>:<path> <query> [--glob '*.ts'] [-i] [-e]",
	Summary: "Search file contents and names on the other machine",
	Run: func(ctx context.Context, env *Env, args []string) error {
		fs := flag.NewFlagSet("grep", flag.ContinueOnError)
		fs.SetOutput(env.Err)
		glob := fs.String("glob", "", "only files whose name matches this pattern")
		ignoreCase := fs.Bool("i", false, "ignore case")
		isRegex := fs.Bool("e", false, "treat the query as a regular expression")
		limit := fs.Int("n", 0, "stop after this many matches")

		positional, err := splitFlags(fs, args, 2)
		if err != nil {
			return err
		}
		if len(positional) < 1 {
			return fmt.Errorf("usage: ibeji grep <device>:<path> <query> [--glob '*.ts'] [-i] [-e]")
		}
		target, err := ParseTarget(positional[0])
		if err != nil {
			return err
		}
		query := ""
		if len(positional) > 1 {
			query = positional[1]
		}
		if query == "" && *glob == "" {
			return fmt.Errorf("give a query, a --glob, or both")
		}

		req := remote.SearchRequest{
			Root:       target.PathOr("~"),
			Query:      query,
			Glob:       *glob,
			Regex:      *isRegex,
			IgnoreCase: *ignoreCase,
			MaxResults: *limit,
		}
		var out struct {
			Matches   []remote.Match `json:"matches"`
			Truncated bool           `json:"truncated"`
		}
		if err := env.Client.PostJSON(ctx, "/v1/fs/search", req, &out); err != nil {
			return err
		}
		for _, m := range out.Matches {
			if m.Line == 0 {
				fmt.Fprintln(env.Out, m.Path)
				continue
			}
			fmt.Fprintf(env.Out, "%s:%d:%s\n", m.Path, m.Line, m.Text)
		}
		// A capped answer must never be mistaken for a complete one.
		if out.Truncated {
			fmt.Fprintf(env.Err, "… more matches than shown; raise -n to see them\n")
		}
		return nil
	},
}

// --- run ---

var cmdRun = &Command{
	Name:    "run",
	Usage:   "ibeji run <device> [--cwd DIR] [--timeout 5m] [--detach] -- <command>",
	Summary: "Run a command on the other machine",
	Run: func(ctx context.Context, env *Env, args []string) error {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.SetOutput(env.Err)
		cwd := fs.String("cwd", "", "working directory on the other machine")
		timeout := fs.Duration("timeout", 0, "give up after this long")
		detach := fs.Bool("detach", false, "run in the background and return a process id")
		afterSync := fs.String("after-sync", "", "wait for this space to be in sync first")

		// Everything after `--` is the command, untouched: flags meant for it
		// must not be eaten by ours.
		var command []string
		if i := indexOf(args, "--"); i >= 0 {
			command = args[i+1:]
			args = args[:i]
		}
		positional, err := splitFlags(fs, args, 1)
		if err != nil {
			return err
		}
		if len(positional) < 1 {
			return fmt.Errorf("usage: ibeji run <device> [--cwd DIR] [--timeout 5m] [--detach] -- <command>")
		}
		if len(command) == 0 {
			return fmt.Errorf("no command. Put it after `--`, like: ibeji run <device> [--cwd DIR] [--timeout 5m] [--detach] -- <command>")
		}
		if _, err := ParseTarget(positional[0]); err != nil {
			return err
		}

		req := remote.ExecRequest{
			Command:        joinCommand(command),
			Cwd:            *cwd,
			TimeoutSeconds: int(timeout.Seconds()),
			Detach:         *detach,
			AfterSync:      *afterSync,
		}

		resp, err := env.Client.Do(ctx, http.MethodPost, "/v1/exec", req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if *detach {
			var out struct {
				Process *remote.ExecStarted `json:"process"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return err
			}
			if out.Process != nil {
				fmt.Fprintf(env.Out, "%s  pid %d  in %s\n",
					out.Process.ProcessID, out.Process.PID, out.Process.Cwd)
			}
			return nil
		}

		return streamExec(resp.Body, env)
	},
}

// streamExec prints a command's output and returns its exit status.
//
// The remote exit code becomes this process's exit code, which is what makes
// `ibeji run … && deploy` mean what it says.
func streamExec(body io.Reader, env *Env) error {
	scanner := bufio.NewScanner(body)
	// Generous line buffer: a build can emit very long lines.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		var frame struct {
			Stream string             `json:"stream"`
			Text   string             `json:"text"`
			Exit   *remote.ExitStatus `json:"exit"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			continue
		}
		switch frame.Stream {
		case "stdout":
			fmt.Fprint(env.Out, frame.Text)
		case "stderr":
			// To stderr, so piping stdout gets only the command's output.
			fmt.Fprint(env.Err, frame.Text)
		case "error":
			return fmt.Errorf("%s", frame.Text)
		case "exit":
			if frame.Exit == nil {
				return nil
			}
			if frame.Exit.TimedOut {
				return &APIError{
					Code:    "exec_timeout",
					Message: "the command ran past its timeout and was stopped",
				}
			}
			if frame.Exit.Code != 0 {
				return &ExitError{Code: frame.Exit.Code}
			}
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading output: %w", err)
	}
	return nil
}

// ExitError carries a remote command's non-zero exit code.
//
// Not a failure of Ìbejì — the command ran and said no — so it prints nothing
// and only sets the exit status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("remote command exited %d", e.Code) }

// --- check ---

var cmdCheck = &Command{
	Name:    "check",
	Usage:   "ibeji check <device> <url-or-host>",
	Summary: "Ask the other machine whether it can reach something",
	Run: func(ctx context.Context, env *Env, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("usage: ibeji check <device> <url-or-host>")
		}
		if _, err := ParseTarget(args[0]); err != nil {
			return err
		}
		var res remote.CheckResult
		if err := env.Client.PostJSON(ctx, "/v1/check",
			remote.CheckRequest{Target: args[1]}, &res); err != nil {
			return err
		}

		if !res.Reachable {
			fmt.Fprintf(env.Out, "unreachable  %s\n", res.Target)
			if res.Error != "" {
				fmt.Fprintf(env.Out, "             %s\n", res.Error)
			}
			return &ExitError{Code: 1}
		}
		line := fmt.Sprintf("reachable    %s", res.Target)
		if res.StatusCode > 0 {
			line += fmt.Sprintf("  HTTP %d", res.StatusCode)
		}
		if res.ResolvedIP != "" {
			line += "  " + res.ResolvedIP
		}
		fmt.Fprintf(env.Out, "%s  %dms\n", line, res.LatencyMS)
		return nil
	},
}

// --- doctor ---

var cmdDoctor = &Command{
	Name:    "doctor",
	Usage:   "ibeji doctor",
	Summary: "Diagnose a broken install or link, and say how to fix it",
	Run: func(ctx context.Context, env *Env, args []string) error {
		var out struct {
			Checks []struct {
				Name     string `json:"name"`
				OK       bool   `json:"ok"`
				Severity string `json:"severity"`
				Detail   string `json:"detail"`
				Fix      string `json:"fix"`
			} `json:"checks"`
		}
		if err := env.Client.GetJSON(ctx, "/v1/doctor", &out); err != nil {
			return err
		}

		failed := 0
		for _, c := range out.Checks {
			mark := "✓"
			switch c.Severity {
			case "warn":
				mark = "!"
			case "fail":
				mark = "✗"
				failed++
			}
			fmt.Fprintf(env.Out, "%s %-14s %s\n", mark, c.Name, c.Detail)
			if c.Fix != "" {
				for _, line := range wrap(c.Fix, 68) {
					fmt.Fprintf(env.Out, "  %-14s %s\n", "", line)
				}
			}
		}
		if failed > 0 {
			return &ExitError{Code: 1}
		}
		return nil
	},
}

// --- trace ---

var cmdTrace = &Command{
	Name:    "trace",
	Usage:   "ibeji trace [--since 10m] [--errors] [--provenance mcp] [-n 50]",
	Summary: "What the daemon did, with payloads — for debugging a failure",
	Run: func(ctx context.Context, env *Env, args []string) error {
		fs := flag.NewFlagSet("trace", flag.ContinueOnError)
		fs.SetOutput(env.Err)
		since := fs.Duration("since", time.Hour, "how far back to look")
		errorsOnly := fs.Bool("errors", false, "only failures")
		provenance := fs.String("provenance", "", "app, cli, mcp or api")
		kind := fs.String("type", "", "event type prefix, e.g. exec or perm")
		limit := fs.Int("n", 50, "how many to show")
		if err := fs.Parse(args); err != nil {
			return err
		}

		q := url.Values{}
		q.Set("since", since.String())
		q.Set("limit", fmt.Sprint(*limit))
		if *errorsOnly {
			q.Set("errors", "true")
		}
		if *provenance != "" {
			q.Set("provenance", *provenance)
		}
		if *kind != "" {
			q.Set("type", *kind)
		}

		var out struct {
			Events []struct {
				At         time.Time       `json:"at"`
				Type       string          `json:"type"`
				Device     string          `json:"device"`
				Provenance string          `json:"provenance"`
				Code       string          `json:"code"`
				Error      string          `json:"error"`
				Hint       string          `json:"hint"`
				DurationMS int64           `json:"durationMs"`
				Request    json.RawMessage `json:"request"`
			} `json:"events"`
		}
		if err := env.Client.GetJSON(ctx, "/v1/trace?"+q.Encode(), &out); err != nil {
			return err
		}

		// Oldest first when printing: a trace reads like a story.
		for i := len(out.Events) - 1; i >= 0; i-- {
			e := out.Events[i]
			line := fmt.Sprintf("%s  %-18s", e.At.Format("15:04:05.000"), e.Type)
			if e.Device != "" {
				line += "  " + e.Device
			}
			if e.Provenance != "" {
				line += "  " + e.Provenance
			}
			if e.DurationMS > 0 {
				line += fmt.Sprintf("  %dms", e.DurationMS)
			}
			fmt.Fprintln(env.Out, line)
			if e.Code != "" {
				fmt.Fprintf(env.Out, "  error  %s: %s\n", e.Code, e.Error)
			}
			// The hint is the part that saves a round of guessing.
			if e.Hint != "" {
				fmt.Fprintf(env.Out, "  hint   %s\n", e.Hint)
			}
		}
		return nil
	},
}

// --- helpers ---

// splitFlags parses flags that may appear before or after positional
// arguments, so `ibeji grep desktop:~/p TODO --glob '*.ts'` works as typed.
func splitFlags(fs *flag.FlagSet, args []string, wantPositional int) ([]string, error) {
	var positional []string
	rest := args
	for len(positional) < wantPositional {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return nil, err
	}
	return append(positional, fs.Args()...), nil
}

// joinCommand rebuilds a command line from the arguments after `--`.
//
// The shell already split and unquoted them, so joining with spaces loses the
// boundaries: `sh -c "exit 42"` would become `sh -c exit 42`, which runs
// `exit` with no argument and succeeds. Anything that is not plainly safe is
// re-quoted, so the far shell sees the same words the near one did.
func joinCommand(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps a word in single quotes unless it plainly needs none.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`!*?[]{}()<>|&;#~=") {
		return s
	}
	// Single quotes protect everything except a single quote itself, which is
	// closed, escaped and reopened.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// formatBytes renders a size the way a person reads it.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// wrap breaks text at a width, for the fix lines doctor prints.
func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	return append(lines, line)
}
