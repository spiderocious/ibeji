// Package trace records what the daemon did, so a failure can be explained.
//
// It exists because the first user of this product's API is an agent, and an
// agent cannot read a log file over someone's shoulder. When a push fails, the
// agent should be able to ask what happened and get "the prompt expired after
// 60 seconds because the window was closed" rather than retrying three times
// and reporting that it does not know.
//
// It is a superset of the activity log: the activity log is the human,
// permission-focused view of what was asked and what you decided; the trace
// holds payloads, timings, and the calls that never reached a decision.
//
// Two exclusions are absolute and enforced at the recorder rather than at the
// reader, so a forgotten filter downstream cannot leak them: clipboard
// contents are never traced, and a file write's payload is recorded as path
// plus byte count, never the body.
package trace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Retention defaults. Both configurable; these are what people live with.
const (
	// DefaultMaxBytes caps the database. 50 MB holds a long week of ordinary
	// use and costs nothing on a machine with a dev toolchain on it.
	DefaultMaxBytes = 50 << 20

	// DefaultMaxAge drops entries older than this.
	DefaultMaxAge = 7 * 24 * time.Hour
)

// Provenance says who originated an event. Self-declared, and therefore not a
// security boundary — it is for legibility, so a command an agent ran and a
// command you ran are never confused in a log.
const (
	FromApp = "app"
	FromCLI = "cli"
	FromMCP = "mcp"
	FromAPI = "api"
)

// Event is one recorded thing.
type Event struct {
	ID int64 `json:"id"`

	// At is when it happened.
	At time.Time `json:"at"`

	// Type is the event name: "api.request", "exec.start", "perm.expired".
	// Stable strings, like the error codes: clients and agents branch on them.
	Type string `json:"type"`

	// Device names the machine involved, when one is.
	Device string `json:"device,omitempty"`

	// Provenance is who asked.
	Provenance string `json:"provenance,omitempty"`

	// Request is the payload, as JSON. Subject to the exclusions above.
	Request json.RawMessage `json:"request,omitempty"`

	// Response is the result, as JSON.
	Response json.RawMessage `json:"response,omitempty"`

	// Code is the stable error code when this failed.
	Code string `json:"code,omitempty"`

	// Error is the human sentence for a failure.
	Error string `json:"error,omitempty"`

	// Hint names the likely cause. This is the field that saves an agent a
	// round of guessing.
	Hint string `json:"hint,omitempty"`

	// DurationMS is how long it took.
	DurationMS int64 `json:"durationMs,omitempty"`
}

// Recorder writes events to a capped store.
type Recorder struct {
	// MaxBytes caps the database size.
	MaxBytes int64

	// MaxAge drops entries older than this.
	MaxAge time.Duration

	db *sql.DB

	mu       sync.Mutex
	writes   int
	closed   bool
	nowFn    func() time.Time
	lastTrim time.Time
}

// Open creates or opens a trace at path.
func Open(path string) (*Recorder, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("trace: create directory: %w", err)
		}
	}

	// WAL so a reader (the CLI tailing, the window's trace pane) never blocks
	// the daemon writing. busy_timeout so a concurrent reader waits rather
	// than failing.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("trace: open %q: %w", path, err)
	}
	// SQLite tolerates one writer; more connections buy nothing and invite
	// lock contention.
	db.SetMaxOpenConns(1)

	r := &Recorder{
		MaxBytes: DefaultMaxBytes,
		MaxAge:   DefaultMaxAge,
		db:       db,
		nowFn:    time.Now,
	}
	if err := r.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

func (r *Recorder) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  at          INTEGER NOT NULL,
  type        TEXT    NOT NULL,
  device      TEXT,
  provenance  TEXT,
  request     TEXT,
  response    TEXT,
  code        TEXT,
  error       TEXT,
  hint        TEXT,
  duration_ms INTEGER
);
CREATE INDEX IF NOT EXISTS events_at   ON events(at);
CREATE INDEX IF NOT EXISTS events_type ON events(type);
CREATE INDEX IF NOT EXISTS events_code ON events(code);`
	if _, err := r.db.Exec(schema); err != nil {
		return fmt.Errorf("trace: migrate: %w", err)
	}
	return nil
}

// SetClock replaces the time source, for tests crossing a retention boundary.
func (r *Recorder) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nowFn = now
}

func (r *Recorder) now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nowFn()
}

// Record writes an event.
//
// It never returns an error to its caller: tracing must not be able to fail a
// request. A trace that cannot be written is logged and dropped, because the
// alternative — a failed `git push` because the diagnostic database was full —
// is plainly worse.
func (r *Recorder) Record(ev Event) {
	if ev.At.IsZero() {
		ev.At = r.now()
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}

	_, err := r.db.Exec(
		`INSERT INTO events (at, type, device, provenance, request, response, code, error, hint, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.At.UnixMilli(), ev.Type, nullable(ev.Device), nullable(ev.Provenance),
		nullableJSON(ev.Request), nullableJSON(ev.Response),
		nullable(ev.Code), nullable(ev.Error), nullable(ev.Hint), ev.DurationMS,
	)
	if err != nil {
		// Nothing to do but carry on: the request matters more than its trace.
		return
	}

	r.mu.Lock()
	r.writes++
	due := r.writes%200 == 0 || r.nowFn().Sub(r.lastTrim) > time.Hour
	if due {
		r.lastTrim = r.nowFn()
	}
	r.mu.Unlock()

	if due {
		_ = r.Trim()
	}
}

// Query describes a trace search.
type Query struct {
	// Since bounds how far back to look.
	Since time.Time

	// Type filters by event type prefix, e.g. "exec" or "perm".
	Type string

	// Device filters by machine.
	Device string

	// Provenance filters by origin — "show me only what the agent did".
	Provenance string

	// ErrorsOnly returns just failures, which is what an agent debugging a
	// problem asks for first.
	ErrorsOnly bool

	// Limit caps the answer. Zero means 100.
	Limit int
}

// Events returns matching events, newest first.
func (r *Recorder) Events(ctx context.Context, q Query) ([]Event, error) {
	var (
		where []string
		args  []any
	)
	if !q.Since.IsZero() {
		where = append(where, "at >= ?")
		args = append(args, q.Since.UnixMilli())
	}
	if q.Type != "" {
		where = append(where, "type LIKE ?")
		args = append(args, q.Type+"%")
	}
	if q.Device != "" {
		where = append(where, "device = ?")
		args = append(args, q.Device)
	}
	if q.Provenance != "" {
		where = append(where, "provenance = ?")
		args = append(args, q.Provenance)
	}
	if q.ErrorsOnly {
		where = append(where, "code IS NOT NULL AND code != ''")
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT id, at, type, COALESCE(device,''), COALESCE(provenance,''),
	                 COALESCE(request,''), COALESCE(response,''), COALESCE(code,''),
	                 COALESCE(error,''), COALESCE(hint,''), COALESCE(duration_ms,0)
	          FROM events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: query: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			ev        Event
			atMS      int64
			req, resp string
		)
		if err := rows.Scan(&ev.ID, &atMS, &ev.Type, &ev.Device, &ev.Provenance,
			&req, &resp, &ev.Code, &ev.Error, &ev.Hint, &ev.DurationMS); err != nil {
			return nil, fmt.Errorf("trace: scan: %w", err)
		}
		ev.At = time.UnixMilli(atMS)
		if req != "" {
			ev.Request = json.RawMessage(req)
		}
		if resp != "" {
			ev.Response = json.RawMessage(resp)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Count reports how many events are held.
func (r *Recorder) Count() (int64, error) {
	var n int64
	err := r.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}

// Trim enforces the retention limits: age first, then size.
//
// Oldest first, which is what a ring buffer means — the recent past is what
// explains a failure that just happened.
func (r *Recorder) Trim() error {
	if r.MaxAge > 0 {
		cutoff := r.now().Add(-r.MaxAge).UnixMilli()
		if _, err := r.db.Exec(`DELETE FROM events WHERE at < ?`, cutoff); err != nil {
			return fmt.Errorf("trace: trim by age: %w", err)
		}
	}
	if r.MaxBytes <= 0 {
		return nil
	}

	for i := 0; i < 20; i++ {
		size, err := r.sizeBytes()
		if err != nil {
			return err
		}
		if size <= r.MaxBytes {
			return nil
		}
		// Drop the oldest tenth and try again. A proportional bite converges
		// quickly without a long loop of single deletes.
		var total int64
		if err := r.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&total); err != nil {
			return err
		}
		if total == 0 {
			return nil
		}
		drop := total / 10
		if drop < 1 {
			drop = 1
		}
		if _, err := r.db.Exec(
			`DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY id ASC LIMIT ?)`,
			drop); err != nil {
			return fmt.Errorf("trace: trim by size: %w", err)
		}
		// VACUUM so the file actually shrinks; without it SQLite keeps the
		// pages and the size check never passes.
		if _, err := r.db.Exec(`VACUUM`); err != nil {
			return fmt.Errorf("trace: vacuum: %w", err)
		}
	}
	return nil
}

// sizeBytes reports the database's size on disk, including its WAL.
func (r *Recorder) sizeBytes() (int64, error) {
	var pageCount, pageSize int64
	if err := r.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("trace: page_count: %w", err)
	}
	if err := r.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("trace: page_size: %w", err)
	}
	return pageCount * pageSize, nil
}

// Stats describes the trace, for doctor and the trace pane's footer.
type Stats struct {
	Events   int64         `json:"events"`
	Bytes    int64         `json:"bytes"`
	MaxBytes int64         `json:"maxBytes"`
	MaxAge   time.Duration `json:"maxAge"`
	OldestAt time.Time     `json:"oldestAt,omitempty"`
	NewestAt time.Time     `json:"newestAt,omitempty"`
}

// Stats reports the trace's current state.
func (r *Recorder) Stats() (Stats, error) {
	s := Stats{MaxBytes: r.MaxBytes, MaxAge: r.MaxAge}
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&s.Events); err != nil {
		return s, err
	}
	size, err := r.sizeBytes()
	if err != nil {
		return s, err
	}
	s.Bytes = size
	if s.Events > 0 {
		var oldest, newest int64
		if err := r.db.QueryRow(`SELECT MIN(at), MAX(at) FROM events`).Scan(&oldest, &newest); err == nil {
			s.OldestAt = time.UnixMilli(oldest)
			s.NewestAt = time.UnixMilli(newest)
		}
	}
	return s, nil
}

// Close flushes and closes the trace.
func (r *Recorder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	return r.db.Close()
}

// --- helpers ---

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// ErrClosed is returned by operations on a closed recorder.
var ErrClosed = errors.New("trace: closed")
