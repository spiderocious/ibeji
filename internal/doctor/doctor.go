// Package doctor diagnoses a broken Ìbejì install.
//
// Half the support burden of a LAN product is "it is not working" with no way
// to tell which of six things failed. Each check here answers one of those
// questions, and every failure names the fix rather than the symptom — a
// message that says what is wrong without saying what to do about it just moves
// the guessing somewhere else.
//
// It runs in the first milestone on purpose, while the install path is still
// new and most likely to be wrong.
package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Severity is how badly a check failed.
type Severity string

const (
	// Pass means the check succeeded.
	Pass Severity = "pass"

	// Warn means something is off but the product still works — a pending
	// conflict, a weaker secret store on a non-macOS platform.
	Warn Severity = "warn"

	// Fail means something the product needs is broken.
	Fail Severity = "fail"
)

// Check is one diagnosis.
type Check struct {
	Name     string   `json:"name"`
	OK       bool     `json:"ok"`
	Severity Severity `json:"severity"`

	// Detail states the machine-level fact: what was found.
	Detail string `json:"detail"`

	// Fix states what to do about it. Present whenever OK is false — a check
	// that reports a problem without a fix has done half its job.
	Fix string `json:"fix,omitempty"`

	// Duration is how long the check took, so a slow one is visible.
	Duration time.Duration `json:"-"`
}

// Report is a full run.
type Report struct {
	Checks []Check `json:"checks"`
}

// Failed reports whether anything failed outright.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Severity == Fail {
			return true
		}
	}
	return false
}

// Counts summarises the run.
func (r Report) Counts() (pass, warn, fail int) {
	for _, c := range r.Checks {
		switch c.Severity {
		case Pass:
			pass++
		case Warn:
			warn++
		case Fail:
			fail++
		}
	}
	return
}

// CheckFunc runs one diagnosis.
type CheckFunc func(ctx context.Context) Check

// Doctor holds the checks to run.
type Doctor struct {
	checks []namedCheck
}

type namedCheck struct {
	order int
	fn    CheckFunc
}

// New builds an empty doctor.
func New() *Doctor { return &Doctor{} }

// Add registers a check. Order is the display order, so the report reads in
// dependency order — a failed LaunchAgent explains a dead link below it.
func (d *Doctor) Add(order int, fn CheckFunc) {
	d.checks = append(d.checks, namedCheck{order, fn})
}

// Run executes every check.
//
// Checks run sequentially and each gets its own timeout: a hung keychain must
// not stop the rest of the report from being produced, because the whole point
// is to find out which thing is broken.
func (d *Doctor) Run(ctx context.Context) Report {
	sorted := make([]namedCheck, len(d.checks))
	copy(sorted, d.checks)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].order < sorted[j].order })

	report := Report{Checks: make([]Check, 0, len(sorted))}
	for _, c := range sorted {
		start := time.Now()
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result := c.fn(checkCtx)
		cancel()
		result.Duration = time.Since(start)
		report.Checks = append(report.Checks, result)
	}
	return report
}

// --- helpers for building results ---

// Passed builds a passing check.
func Passed(name, detail string) Check {
	return Check{Name: name, OK: true, Severity: Pass, Detail: detail}
}

// Warned builds a warning.
func Warned(name, detail, fix string) Check {
	return Check{Name: name, OK: true, Severity: Warn, Detail: detail, Fix: fix}
}

// Failedf builds a failure. The fix is required by the signature, because a
// failure without one is the thing this package exists to avoid.
func Failedf(name, fix, format string, args ...any) Check {
	return Check{
		Name:     name,
		OK:       false,
		Severity: Fail,
		Detail:   fmt.Sprintf(format, args...),
		Fix:      fix,
	}
}

// --- the login shell check ---

// LoginShellTools are the tools whose absence from a login shell's PATH breaks
// remote exec in ways that look like something else entirely.
//
// This is the check that earns its keep. Remote commands run in `zsh -lc`, and
// a login shell reads ~/.zprofile and ~/.zshenv but NOT ~/.zshrc — where most
// people put their PATH. The result is `git push` failing with "command not
// found: pnpm" on the far machine while the same command works fine when typed
// there, which is a genuinely baffling hour until someone explains it.
var LoginShellTools = []string{"git", "node", "npm", "pnpm", "go", "cargo", "python3"}

// LoginShell returns a check that the user's login shell provides a usable PATH.
func LoginShell(tools []string) CheckFunc {
	if len(tools) == 0 {
		tools = LoginShellTools
	}
	return func(ctx context.Context) Check {
		const name = "Login shell"

		shell := loginShell()
		path, err := loginShellPath(ctx, shell)
		if err != nil {
			return Failedf(name,
				fmt.Sprintf("Check that %s starts cleanly: run `%s -lc 'echo ok'` in a terminal.", shell, shell),
				"could not read the PATH from %s: %v", shell, err)
		}
		if strings.TrimSpace(path) == "" {
			return Failedf(name,
				"Add your PATH to ~/.zprofile (not ~/.zshrc — a login shell does not read it).",
				"%s -lc returned an empty PATH", shell)
		}

		missing := missingFrom(ctx, shell, tools)
		if len(missing) == 0 {
			return Passed(name, fmt.Sprintf("%s provides %d of the %d common tools",
				shell, len(tools), len(tools)))
		}

		// Present in an interactive shell but missing from a login shell is the
		// diagnostic signature of PATH being set in ~/.zshrc. Say so exactly,
		// because the fix is one line in a different file.
		interactiveOnly := presentInteractivelyOnly(ctx, shell, missing)
		if len(interactiveOnly) > 0 {
			return Failedf(name,
				"Move those PATH lines from ~/.zshrc to ~/.zprofile. A login shell "+
					"(which is what remote commands use) reads ~/.zprofile and ~/.zshenv, never ~/.zshrc.",
				"%s are on your PATH in an interactive shell but not a login shell",
				strings.Join(interactiveOnly, ", "))
		}

		// Genuinely not installed is a warning, not a failure: not every
		// machine needs every toolchain.
		return Warned(name,
			fmt.Sprintf("not on the login shell's PATH: %s", strings.Join(missing, ", ")),
			"Install them, or ignore this if that machine does not need them.")
	}
}

// loginShell reports the shell remote commands will use.
func loginShell() string {
	if runtime.GOOS == "darwin" {
		return "zsh"
	}
	return "sh"
}

// loginShellPath asks a login shell what its PATH is.
func loginShellPath(ctx context.Context, shell string) (string, error) {
	out, err := exec.CommandContext(ctx, shell, "-lc", "printf %s \"$PATH\"").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// missingFrom reports which tools a login shell cannot find.
func missingFrom(ctx context.Context, shell string, tools []string) []string {
	var missing []string
	for _, tool := range tools {
		// `command -v` is the portable test and does not depend on the tool
		// running successfully.
		cmd := exec.CommandContext(ctx, shell, "-lc", "command -v "+shellQuote(tool))
		if err := cmd.Run(); err != nil {
			missing = append(missing, tool)
		}
	}
	return missing
}

// presentInteractivelyOnly reports which of the missing tools ARE found by an
// interactive shell — the signature of PATH set in ~/.zshrc.
func presentInteractivelyOnly(ctx context.Context, shell string, missing []string) []string {
	var found []string
	for _, tool := range missing {
		cmd := exec.CommandContext(ctx, shell, "-ic", "command -v "+shellQuote(tool))
		if err := cmd.Run(); err == nil {
			found = append(found, tool)
		}
	}
	return found
}

// shellQuote makes a string safe to embed in a shell command.
//
// The tool names here are internal constants, but quoting is applied anyway:
// this helper will outlive the assumption that its input is trusted.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
