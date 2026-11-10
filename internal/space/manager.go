package space

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// The manager owns every space on this device: its state, its last agreed
// manifest, and the activity feed.
//
// The state machine is what the exec barrier waits on, so it has to be honest
// rather than optimistic. A space that reports in-sync while a transfer is
// still in flight would make the barrier worse than useless: it would look
// like it was working while the race it exists to prevent still happened.

// Activity is one entry in a space's feed.
type Activity struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`

	// Paths are the files involved. Required on a conflict: an entry that
	// names no file is useless, and the PRD forbids a silent conflict.
	Paths []string `json:"paths,omitempty"`

	// From names the machine the change came from.
	From string `json:"from,omitempty"`

	Detail string `json:"detail,omitempty"`
}

// Activity kinds.
const (
	ActivitySynced   = "synced"
	ActivityConflict = "conflict"
	ActivityResolved = "resolved"
	ActivityDeleted  = "deleted"
	ActivityPaused   = "paused"
	ActivityResumed  = "resumed"
	ActivityCreated  = "created"
	ActivityError    = "error"
)

// Status is a space's full state, as the API reports it.
type Status struct {
	Space     `json:",inline"`
	State     State      `json:"state"`
	Files     int        `json:"files"`
	Bytes     int64      `json:"bytes"`
	LastSync  time.Time  `json:"lastSync,omitempty"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
	Activity  []Activity `json:"activity,omitempty"`
}

// Conflict is an unresolved collision waiting for a decision.
type Conflict struct {
	Rel string `json:"rel"`

	// Copy is where the losing version was kept.
	Copy string `json:"copy"`

	At time.Time `json:"at"`

	// From names the machine whose version was moved aside.
	From string `json:"from"`
}

// Manager holds this device's spaces.
type Manager struct {
	mu     sync.RWMutex
	spaces map[string]*managed

	// DeviceName is this machine's name, used in conflict copy names.
	DeviceName string

	// PeerName names the other machine.
	PeerName func() string

	// OnCreate is called when a space is registered, so its sync loop starts
	// immediately. Without it a space created after the daemon booted would
	// sit there never syncing — which is every space, since none exist at
	// boot on a fresh install.
	OnCreate func(name string)
}

type managed struct {
	space Space
	state State

	// base is the last manifest both devices agreed on. It is what makes a
	// delete distinguishable from a file that never arrived.
	base map[string]Entry

	// last is this device's most recent scan, so the next one can reuse its
	// hashes.
	last map[string]Entry

	conflicts []Conflict
	activity  []Activity
	lastSync  time.Time

	// waiters are callers blocked on convergence — the exec barrier.
	waiters []chan struct{}
}

// NewManager builds an empty manager.
func NewManager(deviceName string) *Manager {
	return &Manager{spaces: map[string]*managed{}, DeviceName: deviceName}
}

// maxActivity bounds a space's feed, so a busy day does not grow without end.
const maxActivity = 500

// Create registers a new space.
func (m *Manager) Create(s Space) (*Space, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	expanded, err := expand(s.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(expanded)
	if err != nil {
		return nil, fmt.Errorf("space: %q: %w", s.Path, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("space: %q is not a directory", s.Path)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.spaces[s.Name]; exists {
		return nil, ErrExists
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	if len(s.Ignore) == 0 {
		s.Ignore = append([]string(nil), DefaultIgnores...)
	}
	if s.Mode == "" {
		s.Mode = TwoWay
	}
	s.Path = expanded

	mg := &managed{
		space: s,
		// Offline until the first sync: claiming in-sync before anything has
		// been compared would let a barrier through on a space that has never
		// converged.
		state: Offline,
		base:  map[string]Entry{},
		last:  map[string]Entry{},
	}
	mg.note(Activity{
		At: time.Now(), Kind: ActivityCreated,
		Detail: fmt.Sprintf("%s of %s", s.Role, s.Path),
	})
	m.spaces[s.Name] = mg
	onCreate := m.OnCreate

	cp := s
	// Outside the lock: the callback starts a loop that reads this manager.
	if onCreate != nil {
		go onCreate(s.Name)
	}
	return &cp, nil
}

// Get returns a space by name.
func (m *Manager) Get(name string) (*Space, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mg, ok := m.spaces[name]
	if !ok {
		return nil, ErrNotFound
	}
	cp := mg.space
	return &cp, nil
}

// List returns every space, by name.
func (m *Manager) List() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Status, 0, len(m.spaces))
	for _, mg := range m.spaces {
		out = append(out, mg.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Status returns one space's full state.
func (m *Manager) Status(name string) (Status, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mg, ok := m.spaces[name]
	if !ok {
		return Status{}, ErrNotFound
	}
	return mg.status(), nil
}

func (mg *managed) status() Status {
	st := Status{
		Space:     mg.space,
		State:     mg.state,
		LastSync:  mg.lastSync,
		Conflicts: append([]Conflict(nil), mg.conflicts...),
	}
	for _, e := range mg.last {
		if !e.IsDir {
			st.Files++
			st.Bytes += e.Size
		}
	}
	// Newest first: the feed is read from the top.
	n := len(mg.activity)
	st.Activity = make([]Activity, 0, n)
	for i := n - 1; i >= 0; i-- {
		st.Activity = append(st.Activity, mg.activity[i])
	}
	return st
}

func (mg *managed) note(a Activity) {
	if a.At.IsZero() {
		a.At = time.Now()
	}
	mg.activity = append(mg.activity, a)
	if len(mg.activity) > maxActivity {
		mg.activity = mg.activity[len(mg.activity)-maxActivity:]
	}
}

// setState moves a space's state and releases anyone waiting on convergence.
func (mg *managed) setState(s State) {
	mg.state = s
	if s != InSync {
		return
	}
	for _, w := range mg.waiters {
		close(w)
	}
	mg.waiters = nil
}

// SetState records a state change from outside — the link dropping, say.
func (m *Manager) SetState(name string, s State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg, ok := m.spaces[name]
	if !ok {
		return ErrNotFound
	}
	mg.setState(s)
	return nil
}

// Pause stops this device participating until resumed.
func (m *Manager) Pause(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg, ok := m.spaces[name]
	if !ok {
		return ErrNotFound
	}
	mg.space.Paused = true
	mg.setState(Paused)
	mg.note(Activity{Kind: ActivityPaused})
	return nil
}

// Resume restarts participation. It behaves like a reconnect: the next sync
// compares from scratch rather than assuming nothing moved while paused.
func (m *Manager) Resume(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg, ok := m.spaces[name]
	if !ok {
		return ErrNotFound
	}
	mg.space.Paused = false
	mg.setState(Offline)
	mg.note(Activity{Kind: ActivityResumed})
	return nil
}

// Leave stops syncing and keeps the local files.
func (m *Manager) Leave(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.spaces[name]; !ok {
		return ErrNotFound
	}
	delete(m.spaces, name)
	return nil
}

// Delete removes a space. Master only, per the PRD — and it never touches the
// files, because "delete the space" and "delete my work" are different things
// and only one of them was asked for.
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg, ok := m.spaces[name]
	if !ok {
		return ErrNotFound
	}
	if mg.space.Role != Master {
		return ErrNotMaster
	}
	delete(m.spaces, name)
	return nil
}

// WaitForSync blocks until a space reports in-sync.
//
// This is what the exec barrier is built on. It returns a distinguishable
// error when convergence is impossible — an unresolved conflict, a sleeping
// peer — because a barrier that hung until its timeout would tell the caller
// nothing about why.
func (m *Manager) WaitForSync(ctx context.Context, name string) error {
	m.mu.Lock()
	mg, ok := m.spaces[name]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	switch mg.state {
	case InSync:
		m.mu.Unlock()
		return nil
	case Conflicted:
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUnresolvedConflict, name)
	}
	ch := make(chan struct{})
	mg.waiters = append(mg.waiters, ch)
	m.mu.Unlock()

	select {
	case <-ch:
		// Re-check: the state may have moved past in-sync already.
		m.mu.RLock()
		state := mg.state
		m.mu.RUnlock()
		if state == Conflicted {
			return fmt.Errorf("%w: %s", ErrUnresolvedConflict, name)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ScanLocal scans a space and remembers the result.
func (m *Manager) ScanLocal(ctx context.Context, name string) (*Manifest, error) {
	m.mu.RLock()
	mg, ok := m.spaces[name]
	if !ok {
		m.mu.RUnlock()
		return nil, ErrNotFound
	}
	s := mg.space
	prev := mg.last
	m.mu.RUnlock()

	manifest, err := Scan(ctx, s.Path, NewMatcher(s.Ignore), prev)
	if err != nil {
		return nil, err
	}
	manifest.Space = name

	m.mu.Lock()
	mg.last = manifest.Index()
	m.mu.Unlock()
	return manifest, nil
}

// Plan compares this device against the peer's manifest.
func (m *Manager) Plan(ctx context.Context, name string, remote *Manifest) (*Plan, error) {
	local, err := m.ScanLocal(ctx, name)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	mg := m.spaces[name]
	base := mg.base
	role := mg.space.Role
	mode := mg.space.Mode
	m.mu.RUnlock()

	peer := "the other machine"
	if m.PeerName != nil {
		if n := m.PeerName(); n != "" {
			peer = n
		}
	}
	return Diff(local, remote, base, role, mode, peer), nil
}

// Transfer fetches one file's contents from the peer.
type Transfer func(ctx context.Context, rel string) (content []byte, mode uint32, err error)

// Sender writes a file to, or deletes one from, the peer.
//
// Needed because "push" cannot mean "wait for them to pull": each side syncs
// on its own schedule, so a replica's change would sit unsent until the master
// happened to look. Without this, sync is one-way in practice however two-way
// it is on paper.
type Sender interface {
	Send(ctx context.Context, rel string, content []byte, mode uint32) error
	Remove(ctx context.Context, rel string) error
}

// Apply executes a plan on this device.
//
// Conflicts move the local version aside before the peer's is written, so the
// losing version exists on disk before anything can overwrite it. The order
// matters: writing first and preserving second would lose the file if the
// process died between the two.
func (m *Manager) Apply(ctx context.Context, name string, plan *Plan, fetch Transfer) error {
	return m.ApplyWith(ctx, name, plan, fetch, nil)
}

// ApplyWith executes a plan, sending pushes and deletes through the sender.
//
// A nil sender means this side only receives — which is what the tests that
// drive both managers by hand want, and what a mirror's replica does.
func (m *Manager) ApplyWith(ctx context.Context, name string, plan *Plan, fetch Transfer, send Sender) error {
	m.mu.RLock()
	mg, ok := m.spaces[name]
	if !ok {
		m.mu.RUnlock()
		return ErrNotFound
	}
	root := mg.space.Path
	m.mu.RUnlock()

	m.mu.Lock()
	mg.setState(Syncing)
	m.mu.Unlock()

	var (
		synced    []string
		deleted   []string
		conflicts []Conflict
		firstErr  error
	)

	for _, change := range plan.Changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch change.Kind {
		case Pull:
			if err := m.pull(ctx, root, change.Rel, fetch); err != nil {
				firstErr = cmpErr(firstErr, err)
				continue
			}
			synced = append(synced, change.Rel)

		case MakeDir:
			// Only empty folders reach here: one with a file in it is created
			// by WriteAtomic's MkdirAll when that file lands. Creating it is
			// idempotent, so a race with an arriving file is harmless.
			dest := filepath.Join(root, filepath.FromSlash(change.Rel))
			if !within(root, dest) {
				firstErr = cmpErr(firstErr, fmt.Errorf("space: %q is outside the space", change.Rel))
				continue
			}
			if err := os.MkdirAll(dest, 0o755); err != nil {
				firstErr = cmpErr(firstErr, fmt.Errorf("space: create %q: %w", change.Rel, err))
				continue
			}
			synced = append(synced, change.Rel)

		case Collide:
			// Preserve first. If this fails, nothing is overwritten.
			if err := MoveAside(root, change.Rel, change.ConflictCopy); err != nil {
				firstErr = cmpErr(firstErr, err)
				continue
			}
			if err := m.pull(ctx, root, change.Rel, fetch); err != nil {
				firstErr = cmpErr(firstErr, err)
				continue
			}
			conflicts = append(conflicts, Conflict{
				Rel: change.Rel, Copy: change.ConflictCopy,
				At: time.Now(), From: m.DeviceName,
			})

		case DeleteLocal:
			if err := RemoveFile(root, change.Rel); err != nil {
				firstErr = cmpErr(firstErr, err)
				continue
			}
			deleted = append(deleted, change.Rel)

		case Push:
			if send == nil {
				continue // the caller is driving both sides itself
			}
			if err := m.push(ctx, root, change.Rel, send); err != nil {
				firstErr = cmpErr(firstErr, err)
				continue
			}
			synced = append(synced, change.Rel)

		case DeleteRemote:
			if send == nil {
				continue
			}
			if err := send.Remove(ctx, change.Rel); err != nil {
				firstErr = cmpErr(firstErr, err)
				continue
			}
			deleted = append(deleted, change.Rel)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(synced) > 0 {
		mg.note(Activity{Kind: ActivitySynced, Paths: synced, From: peerOf(m)})
	}
	if len(deleted) > 0 {
		mg.note(Activity{Kind: ActivityDeleted, Paths: deleted, From: peerOf(m)})
	}
	for _, c := range conflicts {
		mg.conflicts = append(mg.conflicts, c)
		// A conflict entry always names its paths: both the file and where
		// the losing version went.
		mg.note(Activity{
			Kind:   ActivityConflict,
			Paths:  []string{c.Rel, c.Copy},
			From:   peerOf(m),
			Detail: "the master's version was taken; yours is kept beside it",
		})
	}
	if firstErr != nil {
		mg.note(Activity{Kind: ActivityError, Detail: firstErr.Error()})
		mg.setState(Offline)
		return firstErr
	}

	mg.lastSync = time.Now()
	if len(mg.conflicts) > 0 {
		mg.setState(Conflicted)
	} else {
		mg.setState(InSync)
	}
	return nil
}

// push sends one local file to the peer.
func (m *Manager) push(ctx context.Context, root, rel string, send Sender) error {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil {
		// Vanished between the scan and the send. Not an error: the next
		// round will see it is gone and plan a delete instead.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return nil
	}
	content, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("space: read %q to send: %w", rel, err)
	}
	return send.Send(ctx, rel, content, uint32(info.Mode().Perm()))
}

func (m *Manager) pull(ctx context.Context, root, rel string, fetch Transfer) error {
	if fetch == nil {
		return fmt.Errorf("space: no way to fetch %q", rel)
	}
	content, mode, err := fetch(ctx, rel)
	if err != nil {
		return fmt.Errorf("space: fetch %q: %w", rel, err)
	}
	dest := filepath.Join(root, filepath.FromSlash(rel))
	_, err = WriteAtomic(dest, os.FileMode(mode), bytes.NewReader(content))
	return err
}

// Converged records that both devices agree, and remembers the agreed state
// as the base for telling future deletes from files that never arrived.
func (m *Manager) Converged(name string, agreed *Manifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg, ok := m.spaces[name]
	if !ok {
		return ErrNotFound
	}
	mg.base = agreed.Index()
	mg.lastSync = time.Now()
	if len(mg.conflicts) > 0 {
		mg.setState(Conflicted)
		return nil
	}
	mg.setState(InSync)
	return nil
}

// Conflicts returns a space's unresolved collisions.
func (m *Manager) Conflicts(name string) ([]Conflict, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mg, ok := m.spaces[name]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]Conflict(nil), mg.conflicts...), nil
}

// Promote replaces the file with its conflict copy.
//
// Without this a conflict copy is a dead end that has to be hand-carried
// between machines — which is the exact chore this product exists to remove.
// It is a write into the space, so it syncs onward like any other change.
func (m *Manager) Promote(name, rel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg, ok := m.spaces[name]
	if !ok {
		return ErrNotFound
	}

	idx := -1
	for i, c := range mg.conflicts {
		if c.Rel == rel {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("space: no unresolved conflict for %q", rel)
	}
	c := mg.conflicts[idx]

	src := filepath.Join(mg.space.Path, filepath.FromSlash(c.Copy))
	dst := filepath.Join(mg.space.Path, filepath.FromSlash(c.Rel))
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("space: promote %q: %w", rel, err)
	}

	mg.conflicts = append(mg.conflicts[:idx], mg.conflicts[idx+1:]...)
	mg.note(Activity{
		Kind: ActivityResolved, Paths: []string{c.Rel, c.Copy},
		Detail: "your version was promoted",
	})
	if len(mg.conflicts) == 0 {
		mg.setState(Syncing) // the promotion still has to go out
	}
	return nil
}

// Discard throws the conflict copy away, keeping the master's version.
func (m *Manager) Discard(name, rel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg, ok := m.spaces[name]
	if !ok {
		return ErrNotFound
	}

	idx := -1
	for i, c := range mg.conflicts {
		if c.Rel == rel {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("space: no unresolved conflict for %q", rel)
	}
	c := mg.conflicts[idx]

	if err := os.Remove(filepath.Join(mg.space.Path, filepath.FromSlash(c.Copy))); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("space: discard %q: %w", rel, err)
	}
	mg.conflicts = append(mg.conflicts[:idx], mg.conflicts[idx+1:]...)
	mg.note(Activity{
		Kind: ActivityResolved, Paths: []string{c.Rel, c.Copy},
		Detail: "your version was discarded",
	})
	if len(mg.conflicts) == 0 {
		mg.setState(InSync)
	}
	return nil
}

// SetIgnores replaces a space's ignore patterns.
func (m *Manager) SetIgnores(name string, patterns []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mg, ok := m.spaces[name]
	if !ok {
		return ErrNotFound
	}
	mg.space.Ignore = append([]string(nil), patterns...)
	// The next scan compares from scratch: a pattern change means files may
	// have entered or left the space without being touched.
	mg.base = map[string]Entry{}
	mg.last = map[string]Entry{}
	return nil
}

// --- helpers ---

func peerOf(m *Manager) string {
	if m.PeerName == nil {
		return ""
	}
	return m.PeerName()
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

func expand(p string) (string, error) {
	if p == "" {
		return "", errors.New("space: empty path")
	}
	if p[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		if len(p) > 1 && p[1] == '/' {
			return filepath.Join(home, p[2:]), nil
		}
	}
	return filepath.Abs(p)
}
