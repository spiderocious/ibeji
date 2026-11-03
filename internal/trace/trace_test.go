package trace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Recorder {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestRecordAndQuery(t *testing.T) {
	r := open(t)
	r.Record(Event{Type: "exec.start", Device: "Desktop", Provenance: FromMCP,
		Request: json.RawMessage(`{"command":"git push"}`), DurationMS: 42})

	events, err := r.Events(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Type != "exec.start" || e.Device != "Desktop" || e.Provenance != FromMCP {
		t.Fatalf("event = %+v", e)
	}
	if e.DurationMS != 42 {
		t.Fatalf("duration = %d", e.DurationMS)
	}
	if !strings.Contains(string(e.Request), "git push") {
		t.Fatalf("request = %s", e.Request)
	}
	if e.At.IsZero() {
		t.Fatal("event has no timestamp")
	}
}

// Newest first: the recent past is what explains a failure that just happened.
func TestEventsAreNewestFirst(t *testing.T) {
	r := open(t)
	for _, name := range []string{"first", "second", "third"} {
		r.Record(Event{Type: name})
	}
	events, err := r.Events(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 3 || events[0].Type != "third" {
		t.Fatalf("order = %v", []string{events[0].Type, events[1].Type, events[2].Type})
	}
}

// "Show me only what the agent did" is a query you actually want.
func TestFilters(t *testing.T) {
	r := open(t)
	r.Record(Event{Type: "exec.start", Provenance: FromMCP, Device: "Desktop"})
	r.Record(Event{Type: "exec.start", Provenance: FromCLI, Device: "Desktop"})
	r.Record(Event{Type: "fs.read", Provenance: FromMCP, Device: "Laptop"})
	r.Record(Event{Type: "exec.exit", Provenance: FromMCP, Device: "Desktop",
		Code: "declined", Error: "declined by Desktop"})

	cases := []struct {
		name string
		q    Query
		want int
	}{
		{"all", Query{}, 4},
		{"by provenance", Query{Provenance: FromMCP}, 3},
		{"by device", Query{Device: "Laptop"}, 1},
		{"by type prefix", Query{Type: "exec"}, 3},
		{"errors only", Query{ErrorsOnly: true}, 1},
		{"combined", Query{Type: "exec", Provenance: FromMCP}, 2},
	}
	for _, c := range cases {
		got, err := r.Events(context.Background(), c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: got %d events, want %d", c.name, len(got), c.want)
		}
	}
}

// An agent debugging a failure asks for errors first, and needs the hint.
func TestErrorsCarryCodeAndHint(t *testing.T) {
	r := open(t)
	r.Record(Event{
		Type: "perm.expired", Device: "Desktop", Code: "timed_out",
		Error: "timed out awaiting approval on Desktop",
		Hint:  "Desktop's window was closed; the prompt was never seen",
	})
	events, err := r.Events(context.Background(), Query{ErrorsOnly: true})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d errors", len(events))
	}
	if events[0].Code != "timed_out" {
		t.Fatalf("code = %q", events[0].Code)
	}
	if !strings.Contains(events[0].Hint, "window was closed") {
		t.Fatalf("hint = %q; this is the field that saves an agent a round of guessing", events[0].Hint)
	}
}

func TestSinceFilter(t *testing.T) {
	r := open(t)
	now := time.Now()
	r.Record(Event{Type: "old", At: now.Add(-2 * time.Hour)})
	r.Record(Event{Type: "recent", At: now.Add(-1 * time.Minute)})

	events, err := r.Events(context.Background(), Query{Since: now.Add(-10 * time.Minute)})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "recent" {
		t.Fatalf("Since returned %d events: %+v", len(events), events)
	}
}

func TestLimit(t *testing.T) {
	r := open(t)
	for i := 0; i < 50; i++ {
		r.Record(Event{Type: "noise"})
	}
	events, err := r.Events(context.Background(), Query{Limit: 5})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("limit 5 returned %d", len(events))
	}
}

// --- retention ---

func TestTrimByAge(t *testing.T) {
	r := open(t)
	now := time.Now()
	r.SetClock(func() time.Time { return now })
	r.MaxAge = 24 * time.Hour

	r.Record(Event{Type: "old", At: now.Add(-48 * time.Hour)})
	r.Record(Event{Type: "fresh", At: now.Add(-1 * time.Hour)})

	if err := r.Trim(); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	events, err := r.Events(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "fresh" {
		t.Fatalf("after trimming by age: %+v", events)
	}
}

// The cap must actually bound the file, or a long-running daemon fills the
// disk with its own diagnostics.
func TestTrimBySize(t *testing.T) {
	r := open(t)
	r.MaxAge = 0 // age off, so only the size cap applies
	r.MaxBytes = 128 << 10

	big := strings.Repeat("x", 4096)
	for i := 0; i < 400; i++ {
		r.Record(Event{Type: "bulk", Request: json.RawMessage(`{"payload":"` + big + `"}`)})
	}
	if err := r.Trim(); err != nil {
		t.Fatalf("Trim: %v", err)
	}

	stats, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Bytes > r.MaxBytes {
		t.Fatalf("trace is %d bytes, over the %d cap", stats.Bytes, r.MaxBytes)
	}
	// It kept something: trimming to empty would be useless.
	if stats.Events == 0 {
		t.Fatal("trimming by size emptied the trace")
	}
}

// Oldest first, which is what a ring buffer means.
func TestTrimDropsOldestFirst(t *testing.T) {
	r := open(t)
	r.MaxAge = 0
	r.MaxBytes = 64 << 10

	big := strings.Repeat("y", 4096)
	for i := 0; i < 200; i++ {
		typ := "early"
		if i > 150 {
			typ = "late"
		}
		r.Record(Event{Type: typ, Request: json.RawMessage(`{"payload":"` + big + `"}`)})
	}
	if err := r.Trim(); err != nil {
		t.Fatalf("Trim: %v", err)
	}

	events, err := r.Events(context.Background(), Query{Limit: 1000})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("everything was trimmed")
	}
	// The newest survive.
	if events[0].Type != "late" {
		t.Fatalf("the newest event is %q; trimming took the wrong end", events[0].Type)
	}
}

func TestStats(t *testing.T) {
	r := open(t)
	for i := 0; i < 5; i++ {
		r.Record(Event{Type: "x"})
	}
	s, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Events != 5 {
		t.Fatalf("events = %d", s.Events)
	}
	if s.Bytes <= 0 {
		t.Fatal("stats report no size")
	}
	if s.MaxBytes != DefaultMaxBytes || s.MaxAge != DefaultMaxAge {
		t.Fatalf("defaults not reported: %+v", s)
	}
	if s.OldestAt.IsZero() || s.NewestAt.IsZero() {
		t.Fatal("stats report no time range")
	}
}

func TestDefaultsAreSevenDaysAndFiftyMegabytes(t *testing.T) {
	// These are what people live with, so they are asserted rather than left
	// to drift.
	if DefaultMaxAge != 7*24*time.Hour {
		t.Fatalf("default retention is %v, want 7 days", DefaultMaxAge)
	}
	if DefaultMaxBytes != 50<<20 {
		t.Fatalf("default cap is %d, want 50 MB", DefaultMaxBytes)
	}
}

// Tracing must never be able to fail a request: a failed `git push` because
// the diagnostic database was full would be plainly worse than a missing
// trace entry.
func TestRecordOnAClosedTraceDoesNotPanic(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r.Record(Event{Type: "after.close"}) // must be a no-op, not a panic
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.db")
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Record(Event{Type: "before.restart", Device: "Desktop"})
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	events, err := again.Events(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "before.restart" {
		t.Fatalf("the trace did not survive a restart: %+v", events)
	}
}

func TestOpenCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "trace.db")
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestConcurrentRecording(t *testing.T) {
	r := open(t)
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				r.Record(Event{Type: "concurrent", Device: "Desktop"})
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	count, err := r.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 200 {
		t.Fatalf("recorded %d events, want 200", count)
	}
}
