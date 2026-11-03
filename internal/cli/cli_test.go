package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// --- target parsing ---

func TestParseTarget(t *testing.T) {
	cases := map[string]Target{
		"desktop":            {Device: "desktop"},
		"desktop:":           {Device: "desktop"},
		"desktop:~/projects": {Device: "desktop", Path: "~/projects"},
		"desktop:/etc/hosts": {Device: "desktop", Path: "/etc/hosts"},
		"desktop:a:b":        {Device: "desktop", Path: "a:b"},
	}
	for in, want := range cases {
		got, err := ParseTarget(in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// `ibeji cat /etc/hosts` is a natural thing to type, and must fail with
// advice rather than being read as a device called "/etc/hosts".
func TestParseTargetRejectsBareLocalPaths(t *testing.T) {
	for _, in := range []string{"/etc/hosts", "~/projects", "./file", ""} {
		_, err := ParseTarget(in)
		if err == nil {
			t.Errorf("ParseTarget(%q) succeeded; a local path is not a device", in)
			continue
		}
		if in != "" && !strings.Contains(err.Error(), "desktop:") {
			t.Errorf("ParseTarget(%q) does not show the right shape: %v", in, err)
		}
	}
}

func TestTargetHelpers(t *testing.T) {
	bare := Target{Device: "desktop"}
	if bare.PathOr("~") != "~" {
		t.Error("PathOr did not fall back")
	}
	if bare.String() != "desktop" {
		t.Errorf("String() = %q", bare.String())
	}
	full := Target{Device: "desktop", Path: "~/p"}
	if full.String() != "desktop:~/p" {
		t.Errorf("String() = %q", full.String())
	}
}

// --- command quoting ---

// The shell already split and unquoted the arguments, so joining them with
// spaces loses the boundaries: `sh -c "exit 42"` would become `sh -c exit 42`,
// which runs `exit` with no argument and succeeds. That is a silent wrong
// answer, which is the worst kind.
func TestJoinCommandPreservesArgumentBoundaries(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"git", "push"}, "git push"},
		{[]string{"sh", "-c", "exit 42"}, "sh -c 'exit 42'"},
		{[]string{"echo", "hello world"}, "echo 'hello world'"},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"echo", ""}, "echo ''"},
		{[]string{"npm", "run", "build:prod"}, "npm run build:prod"},
		{[]string{"sh", "-c", "echo a; echo b"}, "sh -c 'echo a; echo b'"},
	}
	for _, c := range cases {
		if got := joinCommand(c.args); got != c.want {
			t.Errorf("joinCommand(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestShellQuoteLeavesSafeWordsAlone(t *testing.T) {
	// Quoting everything would make traces and prompts unreadable.
	for _, safe := range []string{"git", "push", "--force", "main", "a/b/c.go", "v1.2.3"} {
		if got := shellQuote(safe); got != safe {
			t.Errorf("shellQuote(%q) = %q; safe words should pass through", safe, got)
		}
	}
	for _, unsafe := range []string{"a b", "a;b", "a|b", "a$b", "a`b", "a'b", `a"b`} {
		if got := shellQuote(unsafe); got == unsafe {
			t.Errorf("shellQuote(%q) left it unquoted", unsafe)
		}
	}
}

// --- exit codes ---

// Distinct codes rather than a blanket 1, so a script can tell a decline from
// a sleeping peer from a daemon that is not running.
func TestExitCodesAreDistinct(t *testing.T) {
	cases := map[string]int{
		"declined":      126,
		"timed_out":     124,
		"exec_timeout":  124,
		"peer_offline":  125,
		"not_paired":    127,
		"sync_conflict": 75,
	}
	for code, want := range cases {
		if got := ExitCode(&APIError{Code: code}); got != want {
			t.Errorf("ExitCode(%s) = %d, want %d", code, got, want)
		}
	}

	// Conditions a script would act on differently must not collapse onto one
	// status. The two timeouts share 124 on purpose — both mean "it did not
	// finish" — so they are compared as a pair rather than individually.
	distinct := map[int][]string{}
	for code, status := range cases {
		distinct[status] = append(distinct[status], code)
	}
	for status, codes := range distinct {
		if len(codes) == 1 {
			continue
		}
		sort.Strings(codes)
		if status == 124 && len(codes) == 2 &&
			codes[0] == "exec_timeout" && codes[1] == "timed_out" {
			continue // both mean "it did not finish"
		}
		t.Errorf("%v share exit code %d but mean different things", codes, status)
	}

	if ExitCode(nil) != 0 {
		t.Error("ExitCode(nil) is not 0")
	}
	if ExitCode(ErrDaemonUnreachable) != 69 {
		t.Errorf("ExitCode(daemon unreachable) = %d", ExitCode(ErrDaemonUnreachable))
	}
	// Exit codes stay clear of what a remote command might itself return.
	for _, code := range cases {
		if code > 0 && code < 64 && code != 75 {
			t.Errorf("exit code %d could be confused with a command's own", code)
		}
	}
}

// A non-zero exit from the remote command is not a failure of Ìbejì: the
// command ran and said no, so nothing is printed and only the status passes
// through.
func TestExitErrorIsJustAStatus(t *testing.T) {
	err := &ExitError{Code: 42}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("ExitError does not name the code: %v", err)
	}
	var target *ExitError
	if !errors.As(error(err), &target) || target.Code != 42 {
		t.Error("ExitError does not unwrap")
	}
}

// --- the client ---

func TestAPIErrorsCarryCodeAndHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "timed_out",
			"message": "timed out awaiting approval on Desktop",
			"hint":    "Desktop's window was closed",
		})
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, Token: "t", HTTP: srv.Client()}
	err := c.GetJSON(context.Background(), "/v1/devices", nil)
	if err == nil {
		t.Fatal("a 504 was not reported as an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an APIError: %v", err)
	}
	if apiErr.Code != "timed_out" {
		t.Errorf("code = %q", apiErr.Code)
	}
	// The hint is what saves an agent a round of guessing, so it must survive
	// all the way to the message a person reads.
	if !strings.Contains(apiErr.Error(), "window was closed") {
		t.Errorf("the hint was lost: %v", apiErr)
	}
	if ExitCode(err) != 124 {
		t.Errorf("exit code = %d, want 124", ExitCode(err))
	}
}

// Every call says it came from the CLI, so a command you ran and a command an
// agent ran are never confused in the log.
func TestRequestsCarryProvenance(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Ibeji-Provenance")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, Token: "t", HTTP: srv.Client()}
	if err := c.GetJSON(context.Background(), "/v1/devices", nil); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if got != "cli" {
		t.Errorf("provenance = %q, want cli", got)
	}
}

func TestClientSendsTheToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, Token: "the-token", HTTP: srv.Client()}
	if err := c.GetJSON(context.Background(), "/v1/devices", nil); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if auth != "Bearer the-token" {
		t.Errorf("Authorization = %q", auth)
	}
}

// A daemon that is not running is the commonest failure by far, so it has to
// be nameable rather than a connection-refused dump.
func TestUnreachableDaemonIsNameable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := &Client{Endpoint: url, Token: "t", HTTP: http.DefaultClient}
	err := c.GetJSON(context.Background(), "/v1/devices", nil)
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("error = %v, want ErrDaemonUnreachable", err)
	}
}

// --- exec streaming ---

func TestStreamExecPassesThroughTheRemoteExitCode(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"stream":"stdout","text":"building\n"}`,
		`{"stream":"stderr","text":"a warning\n"}`,
		`{"stream":"exit","exit":{"code":7,"durationMs":120}}`,
	}, "\n")

	var out, errOut strings.Builder
	env := &Env{Out: &out, Err: &errOut}
	err := streamExec(strings.NewReader(ndjson), env)

	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v, want an ExitError", err)
	}
	if exitErr.Code != 7 {
		t.Fatalf("exit code = %d, want 7", exitErr.Code)
	}
	if out.String() != "building\n" {
		t.Errorf("stdout = %q", out.String())
	}
	// stderr goes to stderr, so piping stdout gets only the command's output.
	if errOut.String() != "a warning\n" {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestStreamExecSuccessIsSilent(t *testing.T) {
	ndjson := `{"stream":"exit","exit":{"code":0,"durationMs":5}}`
	var out, errOut strings.Builder
	if err := streamExec(strings.NewReader(ndjson), &Env{Out: &out, Err: &errOut}); err != nil {
		t.Fatalf("a successful command returned %v", err)
	}
	if out.String() != "" || errOut.String() != "" {
		t.Errorf("a successful command printed something: %q / %q", out.String(), errOut.String())
	}
}

// A command stopped by its timeout must say so rather than looking like a
// crash: "it overran" and "it went wrong" lead somewhere different.
func TestStreamExecReportsATimeout(t *testing.T) {
	ndjson := `{"stream":"exit","exit":{"code":-1,"timedOut":true}}`
	var out, errOut strings.Builder
	err := streamExec(strings.NewReader(ndjson), &Env{Out: &out, Err: &errOut})

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "exec_timeout" {
		t.Fatalf("error = %v, want an exec_timeout APIError", err)
	}
}

// --- helpers ---

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1024:       "1.0 KB",
		1536:       "1.5 KB",
		1048576:    "1.0 MB",
		1073741824: "1.0 GB",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestWrap(t *testing.T) {
	lines := wrap("the quick brown fox jumps over the lazy dog", 15)
	if len(lines) < 2 {
		t.Fatalf("wrap produced %d lines", len(lines))
	}
	for _, l := range lines {
		if len(l) > 15 && !strings.Contains(l, " ") {
			continue // one long word cannot be broken
		}
		if len(l) > 15 {
			t.Errorf("line over the width: %q", l)
		}
	}
	if wrap("", 10) != nil {
		t.Error("wrap of an empty string returned lines")
	}
}

// Every command has a usage line and a summary: `ibeji help` is the first
// thing someone reads.
func TestEveryCommandIsDocumented(t *testing.T) {
	for _, c := range Commands() {
		if c.Name == "" {
			t.Error("a command has no name")
		}
		if c.Usage == "" {
			t.Errorf("%s has no usage line", c.Name)
		}
		if c.Summary == "" {
			t.Errorf("%s has no summary", c.Name)
		}
		if !strings.HasPrefix(c.Usage, "ibeji "+c.Name) {
			t.Errorf("%s's usage does not start with its own name: %q", c.Name, c.Usage)
		}
		if c.Run == nil {
			t.Errorf("%s does nothing", c.Name)
		}
	}
	// The commands milestone 2 promises.
	for _, name := range []string{"devices", "ls", "cat", "grep", "run", "check", "doctor", "trace"} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("`ibeji %s` is missing", name)
		}
	}
}
