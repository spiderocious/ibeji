package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Go's flag package stops parsing at the first non-flag argument. Any command
// that calls fs.Parse(args) directly therefore throws away every flag typed
// after a positional — silently, with no error, falling back to the default.
//
// For `spaces create` that default was `master`. The command printed
// "created NAME · master", then printed instructions telling you to run
// `--role replica` on the other machine, in the exact position that does not
// work. Anyone following its own output ended up with two masters of one
// space: a split brain, in the subsystem whose whole premise is that exactly
// one side wins a collision. Nothing reported it.

func TestSpacesCreateHonoursRoleAfterPositionals(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		args []string
	}{
		// The form the command's own suggestion line tells you to type.
		{"flag after positionals", []string{"create", "s1", dir, "--role", "replica"}},
		// The form that always worked, which must keep working.
		{"flag before positionals", []string{"create", "--role", "replica", "s1", dir}},
		// Mixed, because people type what they remember.
		{"flag between positionals", []string{"create", "s1", "--role", "replica", dir}},
		{"equals form, trailing", []string{"create", "s1", dir, "--role=replica"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotRole string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct{ Role string }
				_ = json.Unmarshal(body, &req)
				gotRole = req.Role
				_ = json.NewEncoder(w).Encode(map[string]any{
					"space": map[string]any{"name": "s1", "role": req.Role, "path": dir, "mode": "two-way"},
				})
			}))
			defer srv.Close()

			var out, errOut bytes.Buffer
			env := &Env{
				Client: &Client{Endpoint: srv.URL, Token: "t", HTTP: srv.Client()},
				Out:    &out, Err: &errOut,
			}
			if _, err := spaceSub(context.Background(), env, tc.args); err != nil {
				t.Fatalf("spaces create failed: %v", err)
			}
			if gotRole != "replica" {
				t.Errorf("the daemon was asked for role %q, not replica: the flag was dropped", gotRole)
			}
			// And the printed result must not claim master, because that is
			// what sent somebody into a split brain believing it had worked.
			if strings.Contains(out.String(), "· master ·") {
				t.Errorf("reported master for a replica: %q", out.String())
			}
		})
	}
}

// The suggestion line is an instruction, so it has to be one that works. It
// told you to put --role last, which was precisely the broken position.
func TestSpacesCreateSuggestsTheOppositeRole(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"space":  map[string]any{"name": "s1", "role": "master", "path": dir, "mode": "two-way"},
			"ignore": []string{".git/"},
		})
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	env := &Env{
		Client: &Client{Endpoint: srv.URL, Token: "t", HTTP: srv.Client()},
		Out:    &out, Err: &errOut,
	}
	if _, err := spaceSub(context.Background(), env, []string{"create", "s1", dir, "--role", "master"}); err != nil {
		t.Fatalf("spaces create failed: %v", err)
	}
	if !strings.Contains(out.String(), "--role replica") {
		t.Errorf("a master should tell you to make a replica, got %q", out.String())
	}
}

// `ibeji pad` takes plain words and has no flags at all, which made a mistyped
// flag indistinguishable from note text: `ibeji pad --set "hi"` stored the
// literal "--set hi" and reported success. The note was wrong and silent.
func TestPadRefusesAFlagInsteadOfStoringIt(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	env := &Env{
		Client: &Client{Endpoint: srv.URL, Token: "t", HTTP: srv.Client()},
		Out:    &out, Err: &errOut,
	}
	err := cmdPad.Run(context.Background(), env, []string{"--set", "hello"})
	if err == nil {
		t.Fatal("pad accepted a flag it does not have, storing it as note text")
	}
	if posted {
		t.Error("pad wrote to the daemon despite the bad arguments")
	}
	// The error has to show the form that works, or it just says no.
	if !strings.Contains(err.Error(), "ibeji pad") {
		t.Errorf("the refusal should show the right command, got %q", err.Error())
	}

	// A plain note still goes through untouched, including a leading dash
	// inside the text rather than as its own word.
	posted = false
	if err := cmdPad.Run(context.Background(), env, []string{"a", "note", "-", "with", "a", "dash"}); err != nil {
		t.Fatalf("a plain note was refused: %v", err)
	}
	if !posted {
		t.Error("a plain note never reached the daemon")
	}
	_ = os.Stdout
}
