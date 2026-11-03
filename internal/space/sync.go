package space

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// The sync loop: watch, scan, plan, apply, repeat.
//
// One loop per space, driven by the watcher and by a slow periodic sweep. The
// sweep exists because a watcher can miss things — a file changed while the
// daemon was restarting, a filesystem that does not report every event — and a
// space that silently stopped converging is worse than one that syncs a little
// more often than it strictly needs to.

// SweepInterval is how often a space is compared even with no local changes.
//
// Slow enough to be invisible, frequent enough that a missed event costs
// minutes rather than hours.
const SweepInterval = 2 * time.Minute

// Syncer keeps one space converged with the peer.
type Syncer struct {
	Manager *Manager
	Client  *Client
	Logger  *slog.Logger

	// Linked reports whether the peer is reachable. Offline is normal on a
	// LAN — the lid is shut — so the loop waits rather than erroring.
	Linked func() bool

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// NewSyncer builds a syncer.
func NewSyncer(m *Manager, c *Client, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{Manager: m, Client: c, Logger: logger, running: map[string]context.CancelFunc{}}
}

// Start begins syncing a space until the context is cancelled.
func (s *Syncer) Start(ctx context.Context, name string) error {
	sp, err := s.Manager.Get(name)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if cancel, already := s.running[name]; already {
		cancel()
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s.running[name] = cancel
	s.mu.Unlock()

	go s.loop(loopCtx, *sp)
	return nil
}

// Stop ends a space's loop.
func (s *Syncer) Stop(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.running[name]; ok {
		cancel()
		delete(s.running, name)
	}
}

// StopAll ends every loop.
func (s *Syncer) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, cancel := range s.running {
		cancel()
		delete(s.running, name)
	}
}

func (s *Syncer) loop(ctx context.Context, sp Space) {
	watcher, err := NewWatcher(sp.Path, NewMatcher(sp.Ignore), s.Logger)
	if err != nil {
		s.Logger.Warn("space: cannot watch", "space", sp.Name, "err", err)
		// Without a watcher the sweep still converges, just more slowly.
		// Better than refusing to sync the space at all.
	}
	var changes <-chan struct{}
	if watcher != nil {
		defer watcher.Close()
		changes = watcher.Changes(ctx)
		s.Logger.Info("space: watching", "space", sp.Name,
			"path", sp.Path, "directories", watcher.WatchedCount())
	}

	sweep := time.NewTicker(SweepInterval)
	defer sweep.Stop()

	// Sync once at startup: whatever moved while the daemon was down has to
	// be picked up without waiting for a change or a sweep.
	s.syncOnce(ctx, sp.Name)

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			s.syncOnce(ctx, sp.Name)
		case <-sweep.C:
			s.syncOnce(ctx, sp.Name)
		}
	}
}

// syncOnce runs one round for a space.
func (s *Syncer) syncOnce(ctx context.Context, name string) {
	sp, err := s.Manager.Get(name)
	if err != nil {
		return
	}
	if sp.Paused {
		return
	}
	if s.Linked != nil && !s.Linked() {
		// Offline is normal on a LAN, not an error. The space reports
		// unreachable and converges when the peer comes back.
		_ = s.Manager.SetState(name, Offline)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	remote, err := s.Client.Manifest(ctx, name)
	if err != nil {
		s.Logger.Debug("space: could not read the peer's manifest", "space", name, "err", err)
		_ = s.Manager.SetState(name, Offline)
		return
	}

	plan, err := s.Manager.Plan(ctx, name, remote)
	if err != nil {
		s.Logger.Warn("space: could not plan", "space", name, "err", err)
		return
	}
	if plan.Empty() {
		_ = s.Manager.Converged(name, remote)
		return
	}

	s.Logger.Info("space: syncing", "space", name, "changes", len(plan.Changes))
	if err := s.Manager.ApplyWith(ctx, name, plan,
		s.Client.Transfer(name), s.Client.Sender(name)); err != nil {
		s.Logger.Warn("space: sync failed", "space", name, "err", err)
		return
	}

	// Re-read the peer's view after applying, so the agreed base reflects
	// what both machines now hold rather than what one of them held before.
	agreed, err := s.Client.Manifest(ctx, name)
	if err != nil {
		return
	}
	_ = s.Manager.Converged(name, agreed)
}

// SyncNow runs one round immediately, for a caller that cannot wait for the
// watcher — the barrier's first attempt, say.
func (s *Syncer) SyncNow(ctx context.Context, name string) {
	s.syncOnce(ctx, name)
}
