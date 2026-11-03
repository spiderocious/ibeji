package trace

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// These are the tests that matter most in this package. A trace that quietly
// accumulated the contents of everything you ever synced would be the worst
// artefact this product could leave on disk, and unlike most bugs it would not
// announce itself.

// The PRD is absolute about this one: clipboard contents are never traced. Not
// in any mode, not with verbose on, not as a hash. A shared clipboard carries
// passwords and whatever you last copied out of a password manager.
func TestClipboardContentsNeverReachTheDatabase(t *testing.T) {
	secret := "hunter2-my-actual-password"

	payloads := []any{
		map[string]any{"clipboard": secret},
		map[string]any{"Clipboard": secret},
		map[string]any{"clipboardText": secret},
		map[string]any{"event": map[string]any{"clipboard": secret}},
		map[string]any{"items": []any{map[string]any{"clipboard": secret}}},
	}

	r, err := Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	for _, p := range payloads {
		redacted := Redact(p)
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("clipboard contents survived redaction: %s", redacted)
		}
		r.Record(Event{Type: "clipboard.sync", Request: redacted})
	}

	// And nothing in the stored trace contains it, however it was nested.
	events, err := r.Events(context.Background(), Query{Limit: 100})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if strings.Contains(string(e.Request), secret) {
			t.Fatalf("clipboard contents reached the database: %s", e.Request)
		}
	}
}

// A file write is traced as path plus byte count, never the body. "Wrote 4.2 KB
// to src/main.go" explains what happened as well as the bytes would.
func TestFileBodiesAreTracedAsSizeNotContent(t *testing.T) {
	body := "package main\n// the actual source of a private file\n"

	redacted := Redact(map[string]any{
		"path":  "~/projects/secret/main.go",
		"body":  body,
		"bytes": len(body),
	})
	s := string(redacted)

	if strings.Contains(s, "the actual source") {
		t.Fatalf("a file body was traced: %s", s)
	}
	// The useful parts survive: which file, and how much.
	if !strings.Contains(s, "main.go") {
		t.Fatalf("the path was redacted too; the trace must still say which file: %s", s)
	}
	if !strings.Contains(s, "bytes") {
		t.Fatalf("the byte count was lost: %s", s)
	}
}

// A token nested three objects deep is exactly as sensitive as one at the
// root, and these payload shapes will change as the product grows.
func TestSecretsAreRedactedAtAnyDepth(t *testing.T) {
	secret := "sk-live-abcdef0123456789"

	cases := []any{
		map[string]any{"token": secret},
		map[string]any{"apiKey": secret},
		map[string]any{"api_key": secret},
		map[string]any{"password": secret},
		map[string]any{"passphrase": secret},
		map[string]any{"privateKey": secret},
		map[string]any{"authorization": "Bearer " + secret},
		map[string]any{"cookie": secret},
		map[string]any{"seed": secret},
		map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"secret": secret}}}},
		map[string]any{"list": []any{map[string]any{"credential": secret}}},
	}
	for _, c := range cases {
		got := string(Redact(c))
		if strings.Contains(got, secret) {
			t.Errorf("a secret survived redaction: %s", got)
		}
	}
}

// Redaction must not eat the trace. An event with nothing useful left in it
// explains nothing, which defeats the point.
func TestOrdinaryFieldsSurviveRedaction(t *testing.T) {
	got := string(Redact(map[string]any{
		"command":  "git push origin main",
		"cwd":      "~/projects/medcord",
		"device":   "Desktop",
		"exitCode": 0,
		"nested":   map[string]any{"afterSync": "medcord"},
	}))

	for _, want := range []string{"git push origin main", "medcord", "Desktop", "afterSync"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction removed a useful field %q: %s", want, got)
		}
	}
}

// A byte count is the useful part of a write, and must not be mistaken for
// content and stripped.
func TestNumericByteCountsSurvive(t *testing.T) {
	got := string(Redact(map[string]any{"path": "a.txt", "bytes": 4096}))
	if !strings.Contains(got, "4096") {
		t.Fatalf("a numeric byte count was redacted: %s", got)
	}
}

func TestRedactHandlesAwkwardInput(t *testing.T) {
	if Redact(nil) != nil {
		t.Fatal("Redact(nil) returned something")
	}
	// A value that cannot be marshalled must not panic.
	if got := Redact(map[string]any{"ch": make(chan int)}); got != nil {
		t.Fatalf("Redact of an unmarshallable value = %s", got)
	}
	// Plain values pass through.
	if got := string(Redact("just a string")); got != `"just a string"` {
		t.Fatalf("Redact of a string = %s", got)
	}
}

func TestRedactedOutputIsValidJSON(t *testing.T) {
	raw := Redact(map[string]any{
		"clipboard": "secret",
		"body":      "content",
		"command":   "ls",
	})
	var into map[string]any
	if err := json.Unmarshal(raw, &into); err != nil {
		t.Fatalf("redacted output is not valid JSON: %v (%s)", err, raw)
	}
	if into["command"] != "ls" {
		t.Fatalf("command field = %v", into["command"])
	}
}

// The redaction marker should say why, not just that something is missing: a
// reader of the trace who sees an empty field will wonder whether the field
// was empty or the trace is lying.
func TestRedactionMarkersExplainThemselves(t *testing.T) {
	got := string(Redact(map[string]any{"clipboard": "x"}))
	if !strings.Contains(got, "never traced") {
		t.Fatalf("the clipboard marker does not explain itself: %s", got)
	}
	got = string(Redact(map[string]any{"body": "x"}))
	if !strings.Contains(got, "not traced") {
		t.Fatalf("the body marker does not explain itself: %s", got)
	}
}
