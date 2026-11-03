package space

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The torture tests.
//
// This is the one module that can lose your work: everything else in Ìbejì
// fails harmlessly, but a sync bug overwrites a file. So these assume it will
// go wrong and check the cases where it would — both sides edit, one deletes,
// one renames, the peer was offline for a day, a transfer dies mid-file.
//
// Each one states what would be lost if it failed, because a sync test that
// only says "want X got Y" tells a future reader nothing about the stakes.

// twoMachines builds a master and a replica with their own folders, and a
// transfer that moves bytes between them.
type twoMachines struct {
	t          *testing.T
	masterDir  string
	replicaDir string
	master     *Manager
	replica    *Manager
	name       string
}

func newTwoMachines(t *testing.T) *twoMachines {
	t.Helper()
	tm := &twoMachines{
		t:          t,
		masterDir:  t.TempDir(),
		replicaDir: t.TempDir(),
		name:       "medcord",
	}
	tm.master = NewManager("Desktop")
	tm.master.PeerName = func() string { return "Laptop" }
	tm.replica = NewManager("Laptop")
	tm.replica.PeerName = func() string { return "Desktop" }

	if _, err := tm.master.Create(Space{
		Name: tm.name, Path: tm.masterDir, Role: Master, Mode: TwoWay,
	}); err != nil {
		t.Fatalf("create master space: %v", err)
	}
	if _, err := tm.replica.Create(Space{
		Name: tm.name, Path: tm.replicaDir, Role: Replica, Mode: TwoWay,
	}); err != nil {
		t.Fatalf("create replica space: %v", err)
	}
	return tm
}

// fetchFrom reads a file out of the other machine's folder.
func fetchFrom(dir string) Transfer {
	return func(ctx context.Context, rel string) ([]byte, uint32, error) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, 0, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, 0, err
		}
		return b, uint32(info.Mode().Perm()), nil
	}
}

// sync runs one full round: both sides scan, the replica applies its plan, and
// the master applies the mirror of it.
func (tm *twoMachines) sync() {
	tm.t.Helper()
	ctx := context.Background()

	masterManifest, err := tm.master.ScanLocal(ctx, tm.name)
	if err != nil {
		tm.t.Fatalf("scan master: %v", err)
	}
	replicaPlan, err := tm.replica.Plan(ctx, tm.name, masterManifest)
	if err != nil {
		tm.t.Fatalf("plan replica: %v", err)
	}
	if err := tm.replica.Apply(ctx, tm.name, replicaPlan, fetchFrom(tm.masterDir)); err != nil {
		tm.t.Fatalf("apply on replica: %v", err)
	}

	// The master takes what the replica pushed.
	replicaManifest, err := tm.replica.ScanLocal(ctx, tm.name)
	if err != nil {
		tm.t.Fatalf("scan replica: %v", err)
	}
	masterPlan, err := tm.master.Plan(ctx, tm.name, replicaManifest)
	if err != nil {
		tm.t.Fatalf("plan master: %v", err)
	}
	// From the master's side, the replica's pushes read as pulls.
	mirrored := &Plan{}
	for _, c := range masterPlan.Changes {
		switch c.Kind {
		case Pull:
			mirrored.Changes = append(mirrored.Changes, c)
		case DeleteLocal:
			mirrored.Changes = append(mirrored.Changes, c)
		}
	}
	if err := tm.master.Apply(ctx, tm.name, mirrored, fetchFrom(tm.replicaDir)); err != nil {
		tm.t.Fatalf("apply on master: %v", err)
	}

	// Both agree on the master's view afterwards.
	agreed, err := tm.master.ScanLocal(ctx, tm.name)
	if err != nil {
		tm.t.Fatalf("rescan master: %v", err)
	}
	_ = tm.master.Converged(tm.name, agreed)
	_ = tm.replica.Converged(tm.name, agreed)
}

func (tm *twoMachines) writeMaster(rel, content string) {
	tm.t.Helper()
	writeFile(tm.t, tm.masterDir, rel, content)
}

func (tm *twoMachines) writeReplica(rel, content string) {
	tm.t.Helper()
	writeFile(tm.t, tm.replicaDir, rel, content)
}

func (tm *twoMachines) readMaster(rel string) string {
	tm.t.Helper()
	b, err := os.ReadFile(filepath.Join(tm.masterDir, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(b)
}

func (tm *twoMachines) readReplica(rel string) string {
	tm.t.Helper()
	b, err := os.ReadFile(filepath.Join(tm.replicaDir, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(b)
}

// --- the basic shape ---

func TestFirstSyncCopiesBothWays(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("from-master.txt", "master content\n")
	tm.writeReplica("from-replica.txt", "replica content\n")

	tm.sync()

	if got := tm.readReplica("from-master.txt"); got != "master content\n" {
		t.Errorf("the master's file did not reach the replica: %q", got)
	}
	if got := tm.readMaster("from-replica.txt"); got != "replica content\n" {
		t.Errorf("the replica's file did not reach the master: %q", got)
	}
}

func TestEditPropagates(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("a.txt", "v1\n")
	tm.sync()

	tm.writeMaster("a.txt", "v2\n")
	tm.sync()

	if got := tm.readReplica("a.txt"); got != "v2\n" {
		t.Fatalf("the edit did not propagate: %q", got)
	}
}

// --- the case that loses work if it is wrong ---

// Both machines edit the same file. The master's version wins and the
// replica's is preserved beside it — never deleted, never silently replaced.
func TestBothEditTheSameFile(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("report.md", "the original\n")
	tm.sync()

	// Both edit, neither having seen the other's change.
	tm.writeMaster("report.md", "the master's version\n")
	tm.writeReplica("report.md", "the replica's version\n")
	tm.sync()

	// The master's version is what is at the real path on both machines.
	if got := tm.readReplica("report.md"); got != "the master's version\n" {
		t.Errorf("the replica does not hold the master's version: %q", got)
	}

	// And the replica's version still exists, which is the whole point.
	conflicts, err := tm.replica.Conflicts(tm.name)
	if err != nil {
		t.Fatalf("Conflicts: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(conflicts))
	}
	preserved := tm.readReplica(conflicts[0].Copy)
	if preserved != "the replica's version\n" {
		t.Fatalf("the replica's work was lost: the conflict copy holds %q", preserved)
	}
}

// A conflict that nobody is told about is the same as losing the file.
func TestConflictIsSurfacedWithItsPaths(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("report.md", "original\n")
	tm.sync()
	tm.writeMaster("report.md", "theirs\n")
	tm.writeReplica("report.md", "mine\n")
	tm.sync()

	status, err := tm.replica.Status(tm.name)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != Conflicted {
		t.Errorf("state = %q, want conflict", status.State)
	}

	var entry *Activity
	for i := range status.Activity {
		if status.Activity[i].Kind == ActivityConflict {
			entry = &status.Activity[i]
			break
		}
	}
	if entry == nil {
		t.Fatal("the conflict is not in the activity feed")
	}
	// A conflict entry that names no file path is useless.
	if len(entry.Paths) < 2 {
		t.Fatalf("the conflict names %v; it must name the file and the copy", entry.Paths)
	}
	if entry.Detail == "" {
		t.Error("the conflict does not explain what happened")
	}
}

// Without promotion a conflict copy is a dead end you hand-carry between
// machines — the exact chore this product exists to remove.
func TestPromoteAConflictCopy(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("report.md", "original\n")
	tm.sync()
	tm.writeMaster("report.md", "the master's\n")
	tm.writeReplica("report.md", "the replica's\n")
	tm.sync()

	if err := tm.replica.Promote(tm.name, "report.md"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := tm.readReplica("report.md"); got != "the replica's\n" {
		t.Fatalf("promotion did not restore the replica's version: %q", got)
	}

	// And it goes out to the master on the next sync.
	tm.sync()
	if got := tm.readMaster("report.md"); got != "the replica's\n" {
		t.Fatalf("the promoted version did not reach the master: %q", got)
	}

	conflicts, _ := tm.replica.Conflicts(tm.name)
	if len(conflicts) != 0 {
		t.Errorf("%d conflicts remain after promotion", len(conflicts))
	}
}

func TestDiscardAConflictCopy(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("report.md", "original\n")
	tm.sync()
	tm.writeMaster("report.md", "the master's\n")
	tm.writeReplica("report.md", "the replica's\n")
	tm.sync()

	conflicts, _ := tm.replica.Conflicts(tm.name)
	copyPath := conflicts[0].Copy

	if err := tm.replica.Discard(tm.name, "report.md"); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got := tm.readReplica("report.md"); got != "the master's\n" {
		t.Fatalf("discarding changed the file: %q", got)
	}
	if _, err := os.Stat(filepath.Join(tm.replicaDir, filepath.FromSlash(copyPath))); !os.IsNotExist(err) {
		t.Error("the conflict copy survived being discarded")
	}
	if status, _ := tm.replica.Status(tm.name); status.State == Conflicted {
		t.Error("the space is still conflicted after the last conflict was resolved")
	}
}

// --- deletes: the ambiguous case ---

func TestDeletePropagates(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("gone.txt", "content\n")
	tm.writeMaster("kept.txt", "content\n")
	tm.sync()

	if err := os.Remove(filepath.Join(tm.masterDir, "gone.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	tm.sync()

	if tm.readReplica("gone.txt") != "" {
		t.Error("a deleted file is still on the replica")
	}
	if tm.readReplica("kept.txt") == "" {
		t.Error("an untouched file was deleted too")
	}
}

// A file someone just wrote is not a file to delete. If this is wrong, work
// disappears because the other machine happened to delete the same path.
func TestAnEditBeatsADelete(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("a.txt", "original\n")
	tm.sync()

	// The master deletes; the replica edits.
	if err := os.Remove(filepath.Join(tm.masterDir, "a.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	tm.writeReplica("a.txt", "the replica kept working on this\n")
	tm.sync()

	if got := tm.readReplica("a.txt"); got != "the replica kept working on this\n" {
		t.Fatalf("the replica's edit was deleted: %q", got)
	}
	if got := tm.readMaster("a.txt"); got != "the replica kept working on this\n" {
		t.Fatalf("the edit did not reach the master: %q", got)
	}
}

// A rename is a delete plus a create. Both halves must land, or the file is
// either duplicated or lost.
func TestRenameSyncsAsDeleteAndCreate(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("old-name.md", "the contents\n")
	tm.sync()

	if err := os.Rename(
		filepath.Join(tm.masterDir, "old-name.md"),
		filepath.Join(tm.masterDir, "new-name.md")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	tm.sync()

	if got := tm.readReplica("new-name.md"); got != "the contents\n" {
		t.Fatalf("the renamed file did not arrive: %q", got)
	}
	if tm.readReplica("old-name.md") != "" {
		t.Error("the old name survived the rename; the file is now duplicated")
	}
}

// A replica that was asleep for a day must converge on reconnect, applying
// the same rule to everything that moved.
func TestOfflineReplicaConvergesOnReconnect(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("a.txt", "v1\n")
	tm.sync()

	// A day passes with no syncing. The master moves on; the replica also
	// works on something.
	tm.writeMaster("a.txt", "v2\n")
	tm.writeMaster("new-on-master.txt", "added while away\n")
	tm.writeReplica("new-on-replica.txt", "added while away\n")
	if err := os.Remove(filepath.Join(tm.masterDir, "a.txt")); err == nil {
		tm.writeMaster("a.txt", "v3\n") // and edited again
	}

	tm.sync()

	if got := tm.readReplica("a.txt"); got != "v3\n" {
		t.Errorf("the replica did not catch up: %q", got)
	}
	if got := tm.readReplica("new-on-master.txt"); got == "" {
		t.Error("a file added while the replica was away never arrived")
	}
	if got := tm.readMaster("new-on-replica.txt"); got == "" {
		t.Error("the replica's own work never reached the master")
	}
}

// --- mirror mode ---

func TestMirrorIgnoresReplicaEdits(t *testing.T) {
	masterDir, replicaDir := t.TempDir(), t.TempDir()
	master := NewManager("Desktop")
	master.PeerName = func() string { return "Laptop" }
	replica := NewManager("Laptop")
	replica.PeerName = func() string { return "Desktop" }

	if _, err := master.Create(Space{Name: "out", Path: masterDir, Role: Master, Mode: Mirror}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := replica.Create(Space{Name: "out", Path: replicaDir, Role: Replica, Mode: Mirror}); err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx := context.Background()
	writeFile(t, masterDir, "bundle.js", "built by the master\n")
	manifest, _ := master.ScanLocal(ctx, "out")
	plan, _ := replica.Plan(ctx, "out", manifest)
	if err := replica.Apply(ctx, "out", plan, fetchFrom(masterDir)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	agreed, _ := master.ScanLocal(ctx, "out")
	_ = master.Converged("out", agreed)
	_ = replica.Converged("out", agreed)

	// The replica edits its copy. In a mirror that is disposable by
	// definition: no push, no conflict copy.
	writeFile(t, replicaDir, "bundle.js", "edited on the replica\n")
	manifest, _ = master.ScanLocal(ctx, "out")
	plan, _ = replica.Plan(ctx, "out", manifest)
	for _, c := range plan.Changes {
		if c.Kind == Push {
			t.Errorf("a mirror's replica pushed: %+v", c)
		}
		if c.Kind == Collide {
			t.Errorf("a mirror made a conflict copy: %+v", c)
		}
	}
}

// --- the state machine the barrier rests on ---

// A space that reported in-sync while a transfer was still in flight would
// make the barrier worse than useless: it would look like it was working
// while the race it exists to prevent still happened.
func TestWaitForSyncBlocksUntilConverged(t *testing.T) {
	m := NewManager("Laptop")
	dir := t.TempDir()
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}

	released := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		released <- m.WaitForSync(ctx, "s")
	}()

	// Still offline: the waiter must not be released.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-released:
		t.Fatalf("WaitForSync returned before convergence: %v", err)
	default:
	}

	manifest, _ := m.ScanLocal(context.Background(), "s")
	if err := m.Converged("s", manifest); err != nil {
		t.Fatalf("Converged: %v", err)
	}

	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("WaitForSync: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitForSync was not released by convergence")
	}
}

func TestWaitForSyncReturnsImmediatelyWhenAlreadyInSync(t *testing.T) {
	m := NewManager("Laptop")
	dir := t.TempDir()
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	manifest, _ := m.ScanLocal(context.Background(), "s")
	_ = m.Converged("s", manifest)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.WaitForSync(ctx, "s"); err != nil {
		t.Fatalf("WaitForSync on an already-converged space: %v", err)
	}
}

// A barrier that hung until its timeout would tell the caller nothing about
// why. An unresolved conflict means convergence is impossible until someone
// decides, so it says so.
func TestWaitForSyncFailsOnAnUnresolvedConflict(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("report.md", "original\n")
	tm.sync()
	tm.writeMaster("report.md", "theirs\n")
	tm.writeReplica("report.md", "mine\n")
	tm.sync()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := tm.replica.WaitForSync(ctx, tm.name)
	if err == nil {
		t.Fatal("WaitForSync succeeded with an unresolved conflict")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("the error does not say why: %v", err)
	}
}

func TestWaitForSyncRespectsItsContext(t *testing.T) {
	m := NewManager("Laptop")
	dir := t.TempDir()
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := m.WaitForSync(ctx, "s"); err == nil {
		t.Fatal("WaitForSync ignored its deadline")
	}
}

// --- interruption ---

// An interrupted transfer must not leave a corrupt partial at the real path.
// A half-written file that looks complete is worse than a failed sync.
func TestInterruptedTransferLeavesTheOriginalIntact(t *testing.T) {
	tm := newTwoMachines(t)
	tm.writeMaster("important.txt", "v1\n")
	tm.sync()

	tm.writeMaster("important.txt", "v2 which will not arrive\n")

	ctx := context.Background()
	manifest, _ := tm.master.ScanLocal(ctx, tm.name)
	plan, _ := tm.replica.Plan(ctx, tm.name, manifest)

	failing := func(ctx context.Context, rel string) ([]byte, uint32, error) {
		return nil, 0, fmt.Errorf("the link dropped mid-file")
	}
	if err := tm.replica.Apply(ctx, tm.name, plan, failing); err == nil {
		t.Fatal("Apply reported success with a failing transfer")
	}

	// The previous version is still there, whole.
	if got := tm.readReplica("important.txt"); got != "v1\n" {
		t.Fatalf("a failed transfer damaged the existing file: %q", got)
	}
	// And no debris the user would see in their editor.
	entries, _ := os.ReadDir(tm.replicaDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ibeji-sync-") {
			t.Errorf("a temp file survived a failed transfer: %s", e.Name())
		}
	}
	// The space reports itself not converged, so a barrier will not let a
	// command through on a half-synced tree.
	if status, _ := tm.replica.Status(tm.name); status.State == InSync {
		t.Error("a space reported in-sync after a failed transfer")
	}
}

// --- large content ---

func TestLargeFileSurvives(t *testing.T) {
	tm := newTwoMachines(t)

	// 4 MiB with a recognisable pattern, so corruption anywhere shows up.
	var sb strings.Builder
	for i := 0; i < 4*1024*1024/16; i++ {
		fmt.Fprintf(&sb, "%015d\n", i)
	}
	content := sb.String()
	tm.writeMaster("big.bin", content)

	tm.sync()

	got := tm.readReplica("big.bin")
	if len(got) != len(content) {
		t.Fatalf("transferred %d bytes, want %d", len(got), len(content))
	}
	if got != content {
		t.Fatal("a large file was corrupted in transfer")
	}
}

// --- concurrency ---

// The manager is read by the API while the sync engine writes to it.
func TestManagerIsConcurrencySafe(t *testing.T) {
	m := NewManager("Laptop")
	dir := t.TempDir()
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = m.Status("s")
				_ = m.List()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			_ = m.SetState("s", Syncing)
			_ = m.SetState("s", InSync)
		}
	}()
	wg.Wait()
}

// --- lifecycle ---

func TestPauseAndResume(t *testing.T) {
	m := NewManager("Laptop")
	dir := t.TempDir()
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := m.Pause("s"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	status, _ := m.Status("s")
	if status.State != Paused || !status.Paused {
		t.Fatalf("after Pause: %+v", status.State)
	}

	if err := m.Resume("s"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	status, _ = m.Status("s")
	// Resume behaves like a reconnect rather than claiming in-sync: things
	// may have moved while paused.
	if status.State == InSync {
		t.Error("Resume claimed in-sync without comparing anything")
	}
	if status.Paused {
		t.Error("the space is still paused after Resume")
	}
}

// Deleting a space is master-only, and it never touches the files: "delete
// the space" and "delete my work" are different things.
func TestDeleteIsMasterOnlyAndKeepsTheFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "work.txt", "do not delete me\n")

	replica := NewManager("Laptop")
	if _, err := replica.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := replica.Delete("s"); err == nil {
		t.Fatal("a replica deleted a space")
	}

	master := NewManager("Desktop")
	if _, err := master.Create(Space{Name: "s", Path: dir, Role: Master, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := master.Delete("s"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "work.txt")); err != nil {
		t.Fatal("deleting a space deleted the files in it")
	}
}

func TestLeaveKeepsTheFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "work.txt", "mine\n")
	m := NewManager("Laptop")
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Leave("s"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "work.txt")); err != nil {
		t.Fatal("leaving a space deleted the files")
	}
	if _, err := m.Get("s"); err == nil {
		t.Error("the space is still registered after Leave")
	}
}

func TestCreateRejectsDuplicatesAndMissingFolders(t *testing.T) {
	m := NewManager("Laptop")
	dir := t.TempDir()
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err == nil {
		t.Error("a duplicate space name was accepted")
	}
	if _, err := m.Create(Space{
		Name: "missing", Path: filepath.Join(dir, "nope"), Role: Replica, Mode: TwoWay,
	}); err == nil {
		t.Error("a space over a missing folder was accepted")
	}
}

func TestNewSpaceGetsTheDefaultIgnores(t *testing.T) {
	m := NewManager("Laptop")
	dir := t.TempDir()
	s, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !s.IgnoresGit() {
		t.Fatal("a new space does not ignore .git/")
	}
	if len(s.Ignore) < 5 {
		t.Fatalf("a new space has only %d ignores", len(s.Ignore))
	}
}
