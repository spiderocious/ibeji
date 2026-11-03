package space

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- ignore patterns ---

// Ignores are load-bearing for performance, not just tidiness: a repo with
// node_modules unignored is 40,000 files and a watcher storm.
func TestMatcher(t *testing.T) {
	m := NewMatcher(DefaultIgnores)

	ignored := []struct {
		rel   string
		isDir bool
	}{
		{".git", true},
		{".git/config", false},
		{"node_modules", true},
		{"node_modules/react/index.js", false},
		{"src/node_modules/dep/a.js", false},
		{".DS_Store", false},
		{"src/.DS_Store", false},
		{"dist", true},
		{"dist/bundle.js", false},
		{"a/b/__pycache__/x.pyc", false},
		{"notes.swp", false},
	}
	for _, c := range ignored {
		if !m.Match(c.rel, c.isDir) {
			t.Errorf("%q should be ignored", c.rel)
		}
	}

	kept := []struct {
		rel   string
		isDir bool
	}{
		{"src", true},
		{"src/main.go", false},
		{"README.md", false},
		{"gitignore.md", false},          // not .git
		{"distribution/notes.md", false}, // not dist
		{"src/components/App.tsx", false},
	}
	for _, c := range kept {
		if m.Match(c.rel, c.isDir) {
			t.Errorf("%q should not be ignored", c.rel)
		}
	}
}

func TestMatcherAnchoredAndGlob(t *testing.T) {
	m := NewMatcher([]string{"/build/", "*.log", "tmp/"})

	if !m.Match("build", true) {
		t.Error("/build/ should match the root build directory")
	}
	if m.Match("src/build", true) {
		t.Error("/build/ is anchored and should not match a nested build")
	}
	if !m.Match("a/b/debug.log", false) {
		t.Error("*.log should match at any depth")
	}
	if !m.Match("a/tmp/x", false) {
		t.Error("tmp/ should match a nested tmp directory's contents")
	}
}

// .git/ is the ignore that matters most: syncing it corrupts repos through
// index.lock races, and defeats the product's core pattern.
func TestGitIsIgnoredByDefaultAndItsRemovalIsDetectable(t *testing.T) {
	s := &Space{Name: "medcord", Path: "~/p", Role: Master, Mode: TwoWay, Ignore: DefaultIgnores}
	if !s.IgnoresGit() {
		t.Fatal("a new space does not ignore .git/")
	}

	var without []string
	for _, p := range DefaultIgnores {
		if p != GitIgnorePattern {
			without = append(without, p)
		}
	}
	s.Ignore = without
	if s.IgnoresGit() {
		t.Fatal("IgnoresGit did not notice .git/ being removed")
	}
	// The warning has to say why, not just that it is unwise.
	if !strings.Contains(GitWarning, "index.lock") {
		t.Errorf("the warning does not explain the corruption: %q", GitWarning)
	}
	if !strings.Contains(GitWarning, "credentials") {
		t.Errorf("the warning does not explain the pattern it defeats: %q", GitWarning)
	}
}

func TestImportGitignore(t *testing.T) {
	got := ImportGitignore(`
# comment
node_modules/

dist
!dist/keep.txt
*.log
`)
	want := []string{"node_modules/", "dist", "*.log"}
	if len(got) != len(want) {
		t.Fatalf("imported %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("imported[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Negations are dropped rather than misapplied: guessing at one would be
	// worse than not supporting it.
	for _, p := range got {
		if strings.HasPrefix(p, "!") {
			t.Errorf("a negation survived import: %q", p)
		}
	}
}

// --- conflict naming ---

func TestConflictPath(t *testing.T) {
	cases := map[string]string{
		"report.md":     "report.conflict-from-laptop.md",
		"src/main.go":   "src/main.conflict-from-laptop.go",
		"Makefile":      "Makefile.conflict-from-laptop",
		"a/b/c.test.ts": "a/b/c.test.conflict-from-laptop.ts",
	}
	for in, want := range cases {
		if got := ConflictPath(in, "Laptop"); got != want {
			t.Errorf("ConflictPath(%q) = %q, want %q", in, got, want)
		}
	}
	// A device name with spaces or punctuation still produces a usable
	// filename.
	got := ConflictPath("a.md", "Feranmi's MacBook Pro (work)")
	if strings.ContainsAny(got, " '()") {
		t.Errorf("conflict path is not filename-safe: %q", got)
	}
}

// Conflict copies must never be synced back, or they multiply across both
// machines until someone notices.
func TestConflictCopiesAreRecognised(t *testing.T) {
	if !IsConflictPath("report.conflict-from-laptop.md") {
		t.Error("a conflict copy was not recognised")
	}
	if IsConflictPath("report.md") {
		t.Error("an ordinary file was taken for a conflict copy")
	}
}

// --- the diff: where work gets lost if this is wrong ---

func mf(entries ...Entry) *Manifest {
	m := &Manifest{Entries: entries}
	m.Sort()
	return m
}

func file(rel, hash string) Entry {
	return Entry{Rel: rel, Hash: hash, Size: int64(len(hash)), ModTime: time.Unix(1700000000, 0)}
}

func TestDiffNothingToDo(t *testing.T) {
	a := mf(file("a.txt", "h1"), file("b.txt", "h2"))
	base := a.Index()
	plan := Diff(a, mf(file("a.txt", "h1"), file("b.txt", "h2")), base, Master, TwoWay, "Laptop")
	if !plan.Empty() {
		t.Fatalf("identical manifests produced a plan: %+v", plan.Changes)
	}
}

func TestDiffPushAndPull(t *testing.T) {
	base := mf(file("a.txt", "old"), file("b.txt", "old")).Index()

	local := mf(file("a.txt", "new-local"), file("b.txt", "old"))
	remote := mf(file("a.txt", "old"), file("b.txt", "new-remote"))

	plan := Diff(local, remote, base, Master, TwoWay, "Laptop")
	kinds := map[string]ChangeKind{}
	for _, c := range plan.Changes {
		kinds[c.Rel] = c.Kind
	}
	if kinds["a.txt"] != Push {
		t.Errorf("a.txt (changed here) = %q, want push", kinds["a.txt"])
	}
	if kinds["b.txt"] != Pull {
		t.Errorf("b.txt (changed there) = %q, want pull", kinds["b.txt"])
	}
}

// Master wins, and the replica's version is preserved beside it. This is the
// rule the whole module turns on.
func TestDiffBothChangedMasterWins(t *testing.T) {
	base := mf(file("report.md", "original")).Index()
	local := mf(file("report.md", "mine"))
	remote := mf(file("report.md", "theirs"))

	// As the replica: take the master's, keep ours.
	plan := Diff(local, remote, base, Replica, TwoWay, "Laptop")
	if len(plan.Changes) != 1 {
		t.Fatalf("plan = %+v", plan.Changes)
	}
	c := plan.Changes[0]
	if c.Kind != Collide {
		t.Fatalf("kind = %q, want a collision", c.Kind)
	}
	// A conflict entry that names no file path is useless, and the PRD forbids
	// a silent conflict.
	if c.ConflictCopy == "" {
		t.Fatal("the conflict names no copy; the replica's version would be lost")
	}
	if !strings.Contains(c.ConflictCopy, "conflict-from") {
		t.Errorf("conflict copy = %q", c.ConflictCopy)
	}
	if c.Reason == "" {
		t.Error("the conflict has no reason for the activity feed")
	}

	// As the master: ours wins and goes out; nothing of ours is moved aside.
	plan = Diff(local, remote, base, Master, TwoWay, "Laptop")
	if len(plan.Changes) != 1 || plan.Changes[0].Kind != Push {
		t.Fatalf("as master, plan = %+v", plan.Changes)
	}
}

// Deletes are the ambiguous case: a file missing on one side is either "I
// deleted it" or "it never arrived". Inventing a delete is the one mistake
// that loses data.
func TestDiffDeletes(t *testing.T) {
	base := mf(file("gone.txt", "h"), file("kept.txt", "h2")).Index()

	// They deleted it; we still have it unchanged.
	plan := Diff(mf(file("gone.txt", "h"), file("kept.txt", "h2")),
		mf(file("kept.txt", "h2")), base, Master, TwoWay, "Laptop")
	if len(plan.Changes) != 1 || plan.Changes[0].Kind != DeleteLocal {
		t.Fatalf("peer delete = %+v", plan.Changes)
	}

	// We deleted it; they still have it unchanged.
	plan = Diff(mf(file("kept.txt", "h2")),
		mf(file("gone.txt", "h"), file("kept.txt", "h2")), base, Master, TwoWay, "Laptop")
	if len(plan.Changes) != 1 || plan.Changes[0].Kind != DeleteRemote {
		t.Fatalf("local delete = %+v", plan.Changes)
	}
}

// A file someone just wrote is not a file to delete.
func TestDiffDeleteLosesToAnEdit(t *testing.T) {
	base := mf(file("a.txt", "original")).Index()

	// They deleted it, we changed it. Ours survives.
	plan := Diff(mf(file("a.txt", "edited")), mf(), base, Replica, TwoWay, "Laptop")
	if len(plan.Changes) != 1 || plan.Changes[0].Kind != Push {
		t.Fatalf("an edit lost to a delete: %+v", plan.Changes)
	}
}

// With no record of a previous agreement, absence means "not yet transferred",
// never "deleted" — because inventing a delete loses data and inventing a
// transfer only costs bandwidth.
func TestDiffWithoutABaseNeverDeletes(t *testing.T) {
	local := mf(file("a.txt", "h1"))
	remote := mf(file("b.txt", "h2"))

	plan := Diff(local, remote, nil, Master, TwoWay, "Laptop")
	for _, c := range plan.Changes {
		if c.Kind == DeleteLocal || c.Kind == DeleteRemote {
			t.Fatalf("a first sync planned a delete: %+v", c)
		}
	}
	if len(plan.Changes) != 2 {
		t.Fatalf("first sync should transfer both ways: %+v", plan.Changes)
	}
}

// Mirror: changes flow master → replica only. The replica's copy is
// disposable by definition, so nothing of it is preserved or pushed.
func TestDiffMirrorIsOneWay(t *testing.T) {
	base := mf(file("out.js", "old")).Index()

	// Replica edited it. In a mirror that edit is simply overwritten.
	plan := Diff(mf(file("out.js", "replica-edit")), mf(file("out.js", "old")),
		base, Replica, Mirror, "Desktop")
	for _, c := range plan.Changes {
		if c.Kind == Push {
			t.Fatalf("a mirror's replica pushed an edit: %+v", c)
		}
		if c.Kind == Collide {
			t.Fatalf("a mirror made a conflict copy: %+v", c)
		}
	}

	// Both changed, as the replica: take the master's, no conflict copy.
	plan = Diff(mf(file("out.js", "replica-edit")), mf(file("out.js", "master-edit")),
		base, Replica, Mirror, "Desktop")
	if len(plan.Changes) != 1 || plan.Changes[0].Kind != Pull {
		t.Fatalf("mirror collision = %+v", plan.Changes)
	}
}

// Conflict copies are artefacts of a previous collision; syncing them would
// multiply them across both machines.
func TestDiffSkipsConflictCopies(t *testing.T) {
	local := mf(file("a.txt", "h"), file("a.conflict-from-laptop.txt", "h2"))
	remote := mf(file("a.txt", "h"))

	plan := Diff(local, remote, local.Index(), Master, TwoWay, "Laptop")
	for _, c := range plan.Changes {
		if IsConflictPath(c.Rel) {
			t.Fatalf("a conflict copy was planned for sync: %+v", c)
		}
	}
}

// Content, not timestamps, decides. A file rewritten with identical content
// has a new mtime and nothing to sync.
func TestSameComparesContent(t *testing.T) {
	a := Entry{Rel: "a", Hash: "h", ModTime: time.Unix(1, 0), Size: 10}
	b := Entry{Rel: "a", Hash: "h", ModTime: time.Unix(999, 0), Size: 10}
	if !a.Same(b) {
		t.Error("identical content with different mtimes was treated as a change")
	}
	c := Entry{Rel: "a", Hash: "other", ModTime: time.Unix(1, 0), Size: 10}
	if a.Same(c) {
		t.Error("different content was treated as the same")
	}
	// A missing hash is never "the same": guessing would skip a real change.
	if (Entry{Rel: "a"}).Same(Entry{Rel: "a"}) {
		t.Error("two entries with no hash compared equal")
	}
}

// --- scanning ---

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "# hi\n")
	writeFile(t, root, "src/main.go", "package main\n")
	writeFile(t, root, "node_modules/dep/index.js", "// never scanned\n")
	writeFile(t, root, ".git/config", "# never scanned\n")

	m, err := Scan(context.Background(), root, NewMatcher(DefaultIgnores), nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	idx := m.Index()
	if _, ok := idx["README.md"]; !ok {
		t.Error("README.md missing from the manifest")
	}
	if _, ok := idx["src/main.go"]; !ok {
		t.Error("src/main.go missing")
	}
	for rel := range idx {
		if strings.Contains(rel, "node_modules") || strings.Contains(rel, ".git/") {
			t.Errorf("an ignored path was scanned: %s", rel)
		}
	}
	// Every file carries a hash: that is what Same() compares.
	for _, e := range m.Entries {
		if !e.IsDir && e.Hash == "" {
			t.Errorf("%s has no hash", e.Rel)
		}
	}
}

// Hashing is the expensive part, so an unchanged file must keep its hash.
// On a large repo this is the difference between a scan you notice and one
// you do not.
func TestScanReusesHashesForUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "content")

	// Wait past the re-hash grace window. A file modified within the last
	// couple of seconds is always re-hashed, because mtime resolution cannot
	// prove it did not change — so a cache test has to use a file that is
	// genuinely settled.
	time.Sleep(hashReuseGrace + 200*time.Millisecond)

	first, err := Scan(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	prev := first.Index()

	// Poison the remembered hash. If the scan re-hashes, it will differ; if
	// it reuses, the poison comes back — which is how we can tell.
	poisoned := prev["a.txt"]
	poisoned.Hash = "reused-marker"
	prev["a.txt"] = poisoned

	second, err := Scan(context.Background(), root, nil, prev)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if second.Index()["a.txt"].Hash != "reused-marker" {
		t.Error("an unchanged file was re-hashed; a large repo would be scanned in full every time")
	}

	// But a changed file is re-hashed.
	time.Sleep(1100 * time.Millisecond) // mtime has second resolution
	writeFile(t, root, "a.txt", "different content")
	third, err := Scan(context.Background(), root, nil, prev)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if third.Index()["a.txt"].Hash == "reused-marker" {
		t.Error("a changed file kept its old hash; the change would never sync")
	}
}

// Following a symlink can walk out of the space entirely, and copying the
// target silently turns a link into a file.
func TestScanSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "real.txt", "content")
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	m, err := Scan(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, ok := m.Index()["link.txt"]; ok {
		t.Error("a symlink was scanned into the manifest")
	}
}

func TestScanRejectsAFileAsRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "x")
	if _, err := Scan(context.Background(), filepath.Join(root, "a.txt"), nil, nil); err == nil {
		t.Fatal("Scan accepted a file as its root")
	}
}

// --- atomic writes: the discipline that stops a half-written file looking
// complete ---

func TestWriteAtomic(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "nested", "deep", "out.txt")
	content := "the whole contents\n"

	n, err := WriteAtomic(dest, 0o644, strings.NewReader(content))
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	if n != int64(len(content)) {
		t.Fatalf("wrote %d bytes, want %d", n, len(content))
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Fatalf("contents = %q", got)
	}

	// No debris: an interrupted sync must not leave temp files the user sees
	// in their editor.
	entries, _ := os.ReadDir(filepath.Dir(dest))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ibeji-sync-") {
			t.Errorf("a temp file survived: %s", e.Name())
		}
	}
}

// A failed write must leave the previous version intact, not a truncated one.
func TestWriteAtomicLeavesTheOldFileOnFailure(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "important.txt")
	if err := os.WriteFile(dest, []byte("the original\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := WriteAtomic(dest, 0o644, &failingReader{afterBytes: 4})
	if err == nil {
		t.Fatal("WriteAtomic succeeded with a failing reader")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the original is gone: %v", err)
	}
	if string(got) != "the original\n" {
		t.Fatalf("a failed write damaged the original: %q", got)
	}
	// And no debris.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ibeji-sync-") {
			t.Errorf("a temp file survived a failed write: %s", e.Name())
		}
	}
}

type failingReader struct {
	afterBytes int
	read       int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.read >= r.afterBytes {
		return 0, os.ErrClosed
	}
	n := len(p)
	if n > r.afterBytes-r.read {
		n = r.afterBytes - r.read
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.read += n
	return n, nil
}

// A replica's edit that vanished because the master also touched the file is
// the single worst thing this product could do.
func TestMoveAsidePreservesTheLoser(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "report.md", "the replica's version\n")

	conflict := ConflictPath("report.md", "Laptop")
	if err := MoveAside(root, "report.md", conflict); err != nil {
		t.Fatalf("MoveAside: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(conflict)))
	if err != nil {
		t.Fatalf("the conflict copy is missing: %v", err)
	}
	if string(got) != "the replica's version\n" {
		t.Fatalf("the conflict copy holds %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "report.md")); !os.IsNotExist(err) {
		t.Error("the original is still in place; the master's version cannot be written")
	}
}

// Losing the first conflict copy to the second would be the same bug one
// level up.
func TestMoveAsideDoesNotOverwriteAnEarlierConflictCopy(t *testing.T) {
	root := t.TempDir()
	conflict := ConflictPath("report.md", "Laptop")

	writeFile(t, root, "report.md", "first conflict\n")
	if err := MoveAside(root, "report.md", conflict); err != nil {
		t.Fatalf("first MoveAside: %v", err)
	}
	writeFile(t, root, "report.md", "second conflict\n")
	if err := MoveAside(root, "report.md", conflict); err != nil {
		t.Fatalf("second MoveAside: %v", err)
	}

	first, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(conflict)))
	if err != nil {
		t.Fatalf("the first conflict copy is gone: %v", err)
	}
	if string(first) != "first conflict\n" {
		t.Fatalf("the first conflict copy was overwritten: %q", first)
	}

	// The second is kept under a numbered name.
	entries, _ := os.ReadDir(root)
	found := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), "conflict-from") {
			found++
		}
	}
	if found < 2 {
		t.Fatalf("only %d conflict copies survived; one was lost", found)
	}
}

func TestRemoveFilePrunesEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a/b/c/file.txt", "x")

	if err := RemoveFile(root, "a/b/c/file.txt"); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Error("empty directories were left behind after a delete")
	}
	// But the space root itself survives.
	if _, err := os.Stat(root); err != nil {
		t.Fatal("RemoveFile deleted the space root")
	}
}

func TestRemoveFileKeepsNonEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a/one.txt", "x")
	writeFile(t, root, "a/two.txt", "y")

	if err := RemoveFile(root, "a/one.txt"); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "two.txt")); err != nil {
		t.Fatal("a sibling file was removed")
	}
}

// --- validation ---

func TestSpaceValidate(t *testing.T) {
	good := &Space{Name: "medcord", Path: "~/projects/medcord", Role: Master, Mode: TwoWay}
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid space was rejected: %v", err)
	}

	bad := map[string]*Space{
		"no name":  {Path: "~/p", Role: Master, Mode: TwoWay},
		"no path":  {Name: "a", Role: Master, Mode: TwoWay},
		"bad role": {Name: "a", Path: "~/p", Role: "boss", Mode: TwoWay},
		"bad mode": {Name: "a", Path: "~/p", Role: Master, Mode: "sometimes"},
		"slash":    {Name: "a/b", Path: "~/p", Role: Master, Mode: TwoWay},
	}
	for name, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}
