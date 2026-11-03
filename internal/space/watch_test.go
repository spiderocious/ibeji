package space

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func watch(t *testing.T, root string, patterns []string) (*Watcher, <-chan struct{}, context.CancelFunc) {
	t.Helper()
	w, err := NewWatcher(root, NewMatcher(patterns), nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); w.Close() })
	return w, w.Changes(ctx), cancel
}

// waitForChange reports whether a signal arrived within the window.
func waitForChange(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

func TestWatcherNoticesAWrite(t *testing.T) {
	root := t.TempDir()
	_, changes, _ := watch(t, root, DefaultIgnores)

	writeFile(t, root, "a.txt", "hello")
	if !waitForChange(changes, 3*time.Second) {
		t.Fatal("a write produced no change signal")
	}
}

// An editor saving a file emits several events — often a write, a rename and
// a chmod. Syncing on each would mean a transfer per keystroke.
func TestWatcherCoalescesABurst(t *testing.T) {
	root := t.TempDir()
	_, changes, _ := watch(t, root, DefaultIgnores)

	for i := 0; i < 20; i++ {
		writeFile(t, root, "a.txt", "write number "+string(rune('a'+i)))
		time.Sleep(5 * time.Millisecond)
	}
	if !waitForChange(changes, 3*time.Second) {
		t.Fatal("a burst produced no signal at all")
	}
	// After the burst settles, there should not be a queue of further
	// signals waiting.
	extra := 0
	deadline := time.After(600 * time.Millisecond)
	for {
		select {
		case <-changes:
			extra++
			if extra > 2 {
				t.Fatalf("20 rapid writes produced %d+ separate signals; they are not being coalesced", extra+1)
			}
		case <-deadline:
			return
		}
	}
}

// Never watching node_modules is the difference between a quiet daemon and
// one that wakes forty thousand times during an install.
func TestWatcherIgnoresTheNoisyDirectories(t *testing.T) {
	root := t.TempDir()
	// Pre-create the directories so they exist when the watcher starts.
	for _, dir := range []string{"node_modules/dep", ".git/objects", "src"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	w, changes, _ := watch(t, root, DefaultIgnores)

	// An install's worth of writes under node_modules.
	for i := 0; i < 50; i++ {
		writeFile(t, root, "node_modules/dep/f"+string(rune('a'+i%26))+".js", "x")
	}
	writeFile(t, root, ".git/objects/abc", "x")

	if waitForChange(changes, 1200*time.Millisecond) {
		t.Fatal("writes under node_modules and .git woke the watcher")
	}

	// The watch count is the tell: if node_modules were watched, this would
	// climb with every directory in it.
	if n := w.WatchedCount(); n > 3 {
		t.Errorf("watching %d directories; ignored trees are being walked", n)
	}

	// And a real change still gets through.
	writeFile(t, root, "src/main.go", "package main")
	if !waitForChange(changes, 3*time.Second) {
		t.Fatal("a real change did not wake the watcher")
	}
}

// A new directory has to be watched, or changes inside it are invisible —
// which is how a whole new folder silently fails to sync.
func TestWatcherPicksUpNewDirectories(t *testing.T) {
	root := t.TempDir()
	_, changes, _ := watch(t, root, DefaultIgnores)

	if err := os.MkdirAll(filepath.Join(root, "new", "deeper"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	waitForChange(changes, 2*time.Second) // the mkdir itself

	writeFile(t, root, "new/deeper/file.txt", "content")
	if !waitForChange(changes, 3*time.Second) {
		t.Fatal("a write inside a newly created directory was not noticed")
	}
}

// Our own temp files would otherwise make every write look like a change and
// sync forever.
func TestWatcherIgnoresItsOwnTempFiles(t *testing.T) {
	root := t.TempDir()
	_, changes, _ := watch(t, root, DefaultIgnores)

	f, err := os.CreateTemp(root, ".ibeji-sync-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	f.WriteString("in-flight content")
	f.Close()

	if waitForChange(changes, 1200*time.Millisecond) {
		t.Fatal("a sync temp file woke the watcher; syncing would never settle")
	}
}

// Conflict copies are local artefacts, and reacting to one would start a sync
// that produces another.
func TestWatcherIgnoresConflictCopies(t *testing.T) {
	root := t.TempDir()
	_, changes, _ := watch(t, root, DefaultIgnores)

	writeFile(t, root, "report.conflict-from-laptop.md", "the preserved version")
	if waitForChange(changes, 1200*time.Millisecond) {
		t.Fatal("a conflict copy woke the watcher")
	}
}

// Some editors chmod on every save, and a chmod changes no content.
func TestWatcherIgnoresChmodAlone(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "content")
	time.Sleep(200 * time.Millisecond)

	_, changes, _ := watch(t, root, DefaultIgnores)
	if err := os.Chmod(filepath.Join(root, "a.txt"), 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if waitForChange(changes, 1200*time.Millisecond) {
		t.Fatal("a chmod woke the watcher; every editor save would sync with nothing to send")
	}
}

// A build that writes continuously should still sync partway through, rather
// than waiting for a quiet moment that never comes.
func TestWatcherDoesNotPostponeForever(t *testing.T) {
	root := t.TempDir()
	_, changes, _ := watch(t, root, DefaultIgnores)

	stop := make(chan struct{})
	go func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				writeFile(t, root, "build.log", "line "+string(rune('a'+i%26)))
				i++
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	defer close(stop)

	// Writes never stop, so only the cap can release a signal.
	if !waitForChange(changes, MaxDebounce+3*time.Second) {
		t.Fatalf("continuous writes postponed the sync past %v", MaxDebounce)
	}
}

func TestWatcherCloseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	w, err := NewWatcher(root, NewMatcher(DefaultIgnores), nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestWatcherStopsWithItsContext(t *testing.T) {
	root := t.TempDir()
	w, err := NewWatcher(root, NewMatcher(DefaultIgnores), nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	changes := w.Changes(ctx)
	cancel()

	select {
	case _, open := <-changes:
		if open {
			// A signal that was already queued is fine; the channel closing
			// after it is what matters.
			select {
			case _, open := <-changes:
				if open {
					t.Fatal("the change channel kept producing after cancellation")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the change channel did not close after cancellation")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the change channel did not close after cancellation")
	}
}
