package space

import (
	"context"
	"testing"
)

// Regression: the scan reused a cached hash whenever size and truncated mtime
// matched. A file edited within the same second as the previous scan keeps
// both — so the edit was invisible and never synced. Silent divergence, which
// is the worst thing this module can do, and it took a same-length rewrite in
// the same second to show it.
func TestEditWithinTheSameSecondIsNotInvisible(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "v1")
	first, _ := Scan(context.Background(), root, nil, nil)
	e1 := first.Index()["a.txt"]
	t.Logf("v1: size=%d mtime=%v hash=%s", e1.Size, e1.ModTime, e1.Hash[:8])

	writeFile(t, root, "a.txt", "v2") // same length!
	second, _ := Scan(context.Background(), root, nil, first.Index())
	e2 := second.Index()["a.txt"]
	t.Logf("v2: size=%d mtime=%v hash=%s", e2.Size, e2.ModTime, e2.Hash[:8])

	if e1.Hash == e2.Hash {
		t.Fatal("a same-length edit in the same second kept its old hash; " +
			"the change would be invisible and never sync")
	}
}
