package remote

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree builds a small directory for the filesystem tests.
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("README.md", "# project\na line with TODO in it\n")
	write("src/main.go", "package main\n// TODO: handle the error\nfunc main() {}\n")
	write("src/util.ts", "export const x = 1;\n// todo: lowercase\n")
	write("node_modules/dep/index.js", "// TODO: this must never be searched\n")
	write(".git/config", "# TODO: nor this\n")
	return root
}

func TestListSortsDirectoriesFirst(t *testing.T) {
	root := tree(t)
	entries, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	seenFile := false
	for _, e := range entries {
		if !e.IsDir {
			seenFile = true
		} else if seenFile {
			t.Fatal("a directory sorted after a file; listings should lead with directories")
		}
	}
}

func TestListReportsSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	entries, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range entries {
		if e.Name == "link.txt" {
			if e.Symlink == "" {
				t.Fatal("a symlink was listed without its target; 'where does this point' needs another round trip")
			}
			return
		}
	}
	t.Fatal("the symlink was not listed")
}

func TestListMissingDirectory(t *testing.T) {
	if _, err := List("/no/such/place"); err == nil {
		t.Fatal("List succeeded on a missing directory")
	}
}

func TestStat(t *testing.T) {
	root := tree(t)
	info, err := Stat(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.IsDir || info.Size == 0 {
		t.Fatalf("stat = %+v", info)
	}
	if info.ModTime.IsZero() {
		t.Fatal("stat has no modification time")
	}
}

func TestOpenReadsContents(t *testing.T) {
	root := tree(t)
	rc, size, err := Open(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if int64(len(body)) != size {
		t.Fatalf("read %d bytes, size said %d", len(body), size)
	}
	if !strings.Contains(string(body), "# project") {
		t.Fatalf("contents = %q", body)
	}
}

func TestOpenRefusesADirectory(t *testing.T) {
	root := tree(t)
	if _, _, err := Open(root); err == nil {
		t.Fatal("Open succeeded on a directory")
	}
}

// Write-to-temp then rename: an interrupted write must never leave a truncated
// file at the real path, because a half-written file that looks whole is worse
// than a failed write.
func TestWriteIsAtomic(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "nested", "out.txt")
	content := "the full contents\n"

	err := Write(target, WriteRequest{Bytes: int64(len(content))}, strings.NewReader(content))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Fatalf("wrote %q, want %q", got, content)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ibeji-") {
			t.Fatalf("a temp file survived the write: %s", e.Name())
		}
	}
}

// A short write must fail rather than silently truncating the destination.
func TestWriteRejectsTruncatedContent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "out.txt")

	// Claims 100 bytes, supplies 5.
	err := Write(target, WriteRequest{Bytes: 100}, strings.NewReader("short"))
	if err == nil {
		t.Fatal("Write accepted content shorter than its declared length")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("a failed write left a file at the destination")
	}
}

func TestWriteMkdir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")
	if err := Write(target, WriteRequest{Mkdir: true}, nil); err != nil {
		t.Fatalf("Write mkdir: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("mkdir did not create a directory: %v", err)
	}
}

// --- search: the call that makes the API usable for an agent ---

func TestSearchFindsContentMatches(t *testing.T) {
	root := tree(t)
	matches, truncated, err := Search(context.Background(), SearchRequest{
		Root: root, Query: "TODO",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if truncated {
		t.Fatal("a three-file search reported truncation")
	}
	if len(matches) < 2 {
		t.Fatalf("found %d matches, want at least 2: %+v", len(matches), matches)
	}
	for _, m := range matches {
		if m.Line == 0 {
			t.Fatalf("a content match has no line number: %+v", m)
		}
		if m.Text == "" {
			t.Fatalf("a content match has no text: %+v", m)
		}
	}
}

// A search that walks node_modules spends its whole budget on files nobody
// meant to search and returns matches nobody wanted.
func TestSearchSkipsNoiseDirectories(t *testing.T) {
	root := tree(t)
	matches, _, err := Search(context.Background(), SearchRequest{Root: root, Query: "TODO"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, m := range matches {
		if strings.Contains(m.Path, "node_modules") {
			t.Fatalf("search descended into node_modules: %s", m.Path)
		}
		if strings.Contains(m.Path, "/.git/") {
			t.Fatalf("search descended into .git: %s", m.Path)
		}
	}
}

func TestSearchGlobFiltersByName(t *testing.T) {
	root := tree(t)
	matches, _, err := Search(context.Background(), SearchRequest{
		Root: root, Query: "TODO", Glob: "*.go",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no matches for *.go")
	}
	for _, m := range matches {
		if filepath.Ext(m.Path) != ".go" {
			t.Fatalf("glob *.go matched %s", m.Path)
		}
	}
}

// A glob with no query is a filename search — "where is the file called this"
// is a real question on its own.
func TestSearchGlobWithoutQueryFindsFilenames(t *testing.T) {
	root := tree(t)
	matches, _, err := Search(context.Background(), SearchRequest{Root: root, Glob: "*.ts"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d files matching *.ts, want 1: %+v", len(matches), matches)
	}
	if matches[0].Line != 0 {
		t.Fatal("a filename-only match carries a line number")
	}
}

func TestSearchIgnoreCase(t *testing.T) {
	root := tree(t)
	sensitive, _, _ := Search(context.Background(), SearchRequest{Root: root, Query: "todo"})
	insensitive, _, _ := Search(context.Background(), SearchRequest{
		Root: root, Query: "todo", IgnoreCase: true,
	})
	if len(insensitive) <= len(sensitive) {
		t.Fatalf("case-insensitive search found %d, case-sensitive %d; it should find more",
			len(insensitive), len(sensitive))
	}
}

func TestSearchRegex(t *testing.T) {
	root := tree(t)
	matches, _, err := Search(context.Background(), SearchRequest{
		Root: root, Query: `TODO:\s+handle`, Regex: true,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("regex found %d matches, want 1: %+v", len(matches), matches)
	}
}

func TestSearchRejectsBadRegex(t *testing.T) {
	root := tree(t)
	if _, _, err := Search(context.Background(), SearchRequest{
		Root: root, Query: "([unclosed", Regex: true,
	}); err == nil {
		t.Fatal("Search accepted an invalid regular expression")
	}
}

// A careless query over a home directory must not stream for minutes.
func TestSearchTruncatesAtItsLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		name := filepath.Join(root, "file"+string(rune('a'+i%26))+string(rune('0'+i/26))+".txt")
		if err := os.WriteFile(name, []byte("needle\nneedle\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	matches, truncated, err := Search(context.Background(), SearchRequest{
		Root: root, Query: "needle", MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) > 10 {
		t.Fatalf("returned %d matches over a limit of 10", len(matches))
	}
	if !truncated {
		t.Fatal("a capped search did not report truncation; the caller would think it saw everything")
	}
}

// Searching a compiled binary wastes the budget and returns unreadable noise.
func TestSearchSkipsBinaryFiles(t *testing.T) {
	root := t.TempDir()
	binary := append([]byte("needle"), 0x00, 0x01, 0x02)
	binary = append(binary, bytes.Repeat([]byte{0xff}, 100)...)
	if err := os.WriteFile(filepath.Join(root, "a.bin"), binary, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	matches, _, err := Search(context.Background(), SearchRequest{Root: root, Query: "needle"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, m := range matches {
		if strings.HasSuffix(m.Path, ".bin") {
			t.Fatal("search returned a match from a binary file")
		}
	}
	if len(matches) != 1 {
		t.Fatalf("found %d matches, want just the text file", len(matches))
	}
}

// One minified bundle should not return a megabyte on a single line.
func TestSearchTruncatesLongLines(t *testing.T) {
	root := t.TempDir()
	long := "needle" + strings.Repeat("x", 5000)
	if err := os.WriteFile(filepath.Join(root, "min.js"), []byte(long), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	matches, _, err := Search(context.Background(), SearchRequest{Root: root, Query: "needle"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d matches", len(matches))
	}
	if len(matches[0].Text) > maxLineLength+10 {
		t.Fatalf("a match line is %d characters; long lines must be truncated", len(matches[0].Text))
	}
}

func TestSearchRejectsAFileAsRoot(t *testing.T) {
	root := tree(t)
	if _, _, err := Search(context.Background(), SearchRequest{
		Root: filepath.Join(root, "README.md"), Query: "TODO",
	}); err == nil {
		t.Fatal("Search accepted a file as its root")
	}
}

func TestSearchRespectsContextCancellation(t *testing.T) {
	root := tree(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Search(cancelled, SearchRequest{Root: root, Query: "TODO"}); err == nil {
		t.Fatal("Search ignored a cancelled context")
	}
}

// --- device info ---

func TestInfoDescribesThisMachine(t *testing.T) {
	info := Info(context.Background())
	if info.OS == "" || info.Arch == "" {
		t.Fatalf("info = %+v", info)
	}
	if info.Shell == "" {
		t.Fatal("info does not report the login shell commands will run in")
	}
	if info.Home == "" {
		t.Fatal("info does not report the home directory")
	}
	// Whatever is installed, the versions reported must be of tools actually
	// on the login shell's PATH — which is what a remote command will see.
	if len(info.Toolchain) == 0 {
		t.Log("no toolchain detected; unusual but not a failure")
	}
	for tool, version := range info.Toolchain {
		if version == "" {
			t.Errorf("%s reported an empty version", tool)
		}
		if strings.Contains(version, "\n") {
			t.Errorf("%s version spans lines: %q", tool, version)
		}
	}
}
