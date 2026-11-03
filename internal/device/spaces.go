package device

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/feranmi/ibeji/internal/api"
	"github.com/feranmi/ibeji/internal/link"
	"github.com/feranmi/ibeji/internal/space"
)

// Spaces, processes and the barrier, as the daemon exposes them.

// Spaces implements api.SpaceState.
func (d *Daemon) Spaces() *space.Manager { return d.spaces }

// SpaceClient returns a client for the peer's spaces.
func (d *Daemon) SpaceClient() *space.Client {
	return &space.Client{Conn: d.Conn}
}

// StartSyncing begins the sync loops, and arranges for a space created later
// to get one too.
//
// The second half is the part that matters: on a fresh install there are no
// spaces at boot, so a one-shot pass over the list would mean nothing ever
// syncs until the daemon is restarted.
func (d *Daemon) StartSyncing(ctx context.Context) {
	if d.syncer == nil {
		d.syncer = space.NewSyncer(d.spaces, d.SpaceClient(), d.Logger)
		d.syncer.Linked = func() bool { return d.Conn() != nil }
	}

	d.mu.Lock()
	d.syncCtx = ctx
	d.mu.Unlock()

	d.spaces.OnCreate = func(name string) {
		d.mu.RLock()
		loopCtx := d.syncCtx
		d.mu.RUnlock()
		if loopCtx == nil {
			return
		}
		if err := d.syncer.Start(loopCtx, name); err != nil {
			d.Logger.Warn("space: could not start syncing", "space", name, "err", err)
			return
		}
		d.Logger.Info("space: syncing started", "space", name)
	}

	for _, s := range d.spaces.List() {
		if err := d.syncer.Start(ctx, s.Name); err != nil {
			d.Logger.Warn("space: could not start syncing", "space", s.Name, "err", err)
		}
	}
}

// Syncer returns the sync loop, so a newly created space can be started.
func (d *Daemon) Syncer() *space.Syncer { return d.syncer }

// WaitForPeerSync implements api.SpaceState: the barrier.
//
// It waits for BOTH devices to report the space converged. One side being
// in-sync is not convergence — the race the barrier exists to prevent is
// exactly the window where this machine has finished writing and the other
// has not finished receiving.
func (d *Daemon) WaitForPeerSync(r *http.Request, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if _, err := d.spaces.Get(name); err != nil {
		return err
	}

	// Push a sync round immediately rather than waiting for the watcher: the
	// caller is blocked, and the change it cares about may have just landed.
	if d.syncer != nil {
		d.syncer.SyncNow(ctx, name)
	}

	// This side first.
	if err := d.spaces.WaitForSync(ctx, name); err != nil {
		return err
	}

	// Then the peer's. Polling rather than a notification because the peer
	// may not be reachable at all, and a poll turns that into a timeout with
	// a reason rather than an indefinite wait.
	client := d.SpaceClient()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		state, err := client.State(ctx, name)
		switch {
		case err == nil && state == space.InSync:
			return nil
		case err == nil && state == space.Conflicted:
			return fmt.Errorf("%w: %s", space.ErrUnresolvedConflict, name)
		case errors.Is(err, link.ErrNotLinked):
			return err
		}

		select {
		case <-ticker.C:
			if d.syncer != nil {
				d.syncer.SyncNow(ctx, name)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// --- processes ---

// RemoteProcesses implements api.ProcessState.
func (d *Daemon) RemoteProcesses() []api.ProcessView {
	client, err := d.Remote()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	procs, err := client.Processes(ctx)
	if err != nil {
		return nil
	}
	out := make([]api.ProcessView, 0, len(procs))
	for _, p := range procs {
		out = append(out, api.ProcessView{
			ID: p.ID, Command: p.Command, Cwd: p.Cwd, PID: p.PID,
			Started: p.Started, Running: p.Running, ExitCode: p.ExitCode,
			Device: d.PeerName(),
		})
	}
	return out
}

// KillRemote implements api.ProcessState.
func (d *Daemon) KillRemote(id string) error {
	client, err := d.Remote()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return client.Kill(ctx, id)
}

// AttachRemote implements api.ProcessState.
func (d *Daemon) AttachRemote(id string) ([]api.OutputLine, error) {
	client, err := d.Remote()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	chunks, err := client.Attach(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]api.OutputLine, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, api.OutputLine{Stream: c.Stream, Text: c.Text})
	}
	return out, nil
}
