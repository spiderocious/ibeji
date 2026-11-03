package space

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watching a space for local changes.
//
// The PRD requires the daemon to be light: "port scanning and file watching
// must not measurably tax either machine". Two things make that true here.
//
// Ignores are applied BEFORE the event queue, not after. A `npm install` or a
// build emits tens of thousands of events under node_modules; filtering them
// after they are queued means the queue still filled, the watcher still woke,
// and the work was still done.
//
// Events are coalesced. An editor saving a file emits several events, and a
// build emits a storm; syncing on each one would mean a transfer per keystroke.

// DebounceWindow is how long to wait for a burst of changes to settle.
//
// Long enough to collapse an editor's save (which is often a write, a rename
// and a chmod) and the tail of a build; short enough that a save feels like it
// synced immediately.
const DebounceWindow = 400 * time.Millisecond

// MaxDebounce bounds how long a continuous stream of changes can postpone a
// sync. A build that writes for a minute should still sync partway through,
// rather than waiting for a quiet moment that never comes.
const MaxDebounce = 5 * time.Second

// Watcher reports when a space's contents change.
type Watcher struct {
	Logger *slog.Logger

	root    string
	matcher *Matcher
	fs      *fsnotify.Watcher

	mu      sync.Mutex
	closed  bool
	watched map[string]bool
}

// NewWatcher starts watching a space.
func NewWatcher(root string, matcher *Matcher, logger *slog.Logger) (*Watcher, error) {
	if logger == nil {
		logger = slog.Default()
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		Logger:  logger,
		root:    filepath.Clean(root),
		matcher: matcher,
		fs:      fsw,
		watched: map[string]bool{},
	}
	if err := w.addTree(w.root); err != nil {
		fsw.Close()
		return nil, err
	}
	return w, nil
}

// addTree watches a directory and its descendants, skipping ignored ones.
//
// fsnotify watches a directory, not a tree, so each one has to be added. The
// ignore check happens here rather than on each event: never watching
// node_modules is the difference between a quiet daemon and one that wakes
// forty thousand times during an install.
func (w *Watcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable; skip rather than fail the whole watch
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(w.root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel != "." && w.matcher != nil && w.matcher.Match(rel, true) {
			return filepath.SkipDir
		}

		w.mu.Lock()
		already := w.watched[path]
		w.mu.Unlock()
		if already {
			return nil
		}
		if err := w.fs.Add(path); err != nil {
			return nil
		}
		w.mu.Lock()
		w.watched[path] = true
		w.mu.Unlock()
		return nil
	})
}

// Changes streams a signal whenever the space settles after a change.
//
// One signal per burst, not per event: the caller's job is to re-scan and
// sync, and doing that per keystroke would be absurd.
func (w *Watcher) Changes(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 1)

	go func() {
		defer close(out)

		var (
			timer   *time.Timer
			timerC  <-chan time.Time
			firstAt time.Time
			pending bool
		)
		stopTimer := func() {
			if timer != nil {
				timer.Stop()
				timer = nil
				timerC = nil
			}
		}
		defer stopTimer()

		emit := func() {
			pending = false
			stopTimer()
			select {
			case out <- struct{}{}:
			default:
				// A signal is already waiting; the caller will re-scan and
				// pick up everything, so another would be redundant.
			}
		}

		for {
			select {
			case <-ctx.Done():
				return

			case ev, ok := <-w.fs.Events:
				if !ok {
					return
				}
				if !w.interesting(ev) {
					continue
				}
				// A new directory needs watching, or changes inside it are
				// invisible — which is how a whole new folder silently fails
				// to sync.
				if ev.Has(fsnotify.Create) {
					if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
						_ = w.addTree(ev.Name)
					}
				}

				if !pending {
					pending = true
					firstAt = time.Now()
				}
				stopTimer()
				wait := DebounceWindow
				// Never postpone past the cap: a build that writes
				// continuously should still sync partway through.
				if elapsed := time.Since(firstAt); elapsed+wait > MaxDebounce {
					wait = MaxDebounce - elapsed
					if wait < 0 {
						wait = 0
					}
				}
				timer = time.NewTimer(wait)
				timerC = timer.C

			case <-timerC:
				emit()

			case err, ok := <-w.fs.Errors:
				if !ok {
					return
				}
				w.Logger.Debug("space: watch error", "root", w.root, "err", err)
			}
		}
	}()

	return out
}

// interesting reports whether an event is worth reacting to.
func (w *Watcher) interesting(ev fsnotify.Event) bool {
	// Chmod alone does not change content, and some editors chmod on every
	// save. Reacting would mean a sync per save with nothing to send.
	if ev.Op == fsnotify.Chmod {
		return false
	}
	rel, err := filepath.Rel(w.root, ev.Name)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	// Our own temp files would otherwise make every write look like a change
	// and sync forever.
	if strings.HasPrefix(filepath.Base(rel), ".ibeji-sync-") {
		return false
	}
	// Conflict copies are local artefacts.
	if IsConflictPath(rel) {
		return false
	}
	if w.matcher != nil {
		info, err := os.Stat(ev.Name)
		isDir := err == nil && info.IsDir()
		if w.matcher.Match(rel, isDir) {
			return false
		}
	}
	return true
}

// Close stops watching.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	return w.fs.Close()
}

// WatchedCount reports how many directories are being watched, for tests and
// for doctor: a count in the tens of thousands means an ignore is missing.
func (w *Watcher) WatchedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.watched)
}
