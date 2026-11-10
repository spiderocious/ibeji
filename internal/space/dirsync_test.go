package space

import (
	"testing"
	"time"
)

// A space with any subdirectory could not sync at all.
//
// Scan records directories in the manifest (IsDir: true), but Diff compared
// entries without ever asking whether one was a directory. A directory present
// only on the far side became an ordinary Pull, and pull() then asked the peer
// to send its bytes. The peer correctly refused — serveFetch rejects a
// directory with "that is a directory" — and that error failed the whole sync,
// which then retried forever.
//
// The effect: `ibeji spaces` sat at "offline", nothing transferred, and the
// log filled with `space: fetch ".agents": that is a directory`. Every space
// in this session's earlier tests was a flat folder of two files, which is the
// only shape that never hits it. A real repository hits it immediately.

func dirEntry(rel string) Entry {
	return Entry{Rel: rel, IsDir: true, Mode: 0o755, ModTime: time.Unix(1000, 0)}
}

func fileEntry(rel string, size int64) Entry {
	return Entry{Rel: rel, Size: size, Mode: 0o644, ModTime: time.Unix(1000, 0), Hash: rel}
}

func TestDiffNeverAsksToFetchADirectory(t *testing.T) {
	// The far side has a folder with a file in it; this side has neither.
	local := &Manifest{}
	remote := &Manifest{Entries: []Entry{
		dirEntry(".agents"),
		fileEntry(".agents/notes.md", 12),
		dirEntry("rfcs"),
		fileEntry("README.md", 400),
	}}

	plan := Diff(local, remote, nil, Replica, TwoWay, "peer")

	for _, c := range plan.Changes {
		if c.Rel == ".agents" || c.Rel == "rfcs" {
			if c.Kind == Pull {
				t.Errorf("planned to fetch %q as a file, but it is a directory — "+
					"this is what failed every sync of a real folder", c.Rel)
			}
		}
	}

	// The files inside must still come across; skipping directories must not
	// skip their contents.
	var gotFile bool
	for _, c := range plan.Changes {
		if c.Rel == ".agents/notes.md" && c.Kind == Pull {
			gotFile = true
		}
	}
	if !gotFile {
		t.Error("the file inside the directory was not planned for transfer")
	}
}

// The same hazard in the other direction: a directory that exists only here
// must not be queued as an upload of file bytes.
func TestDiffNeverAsksToSendADirectory(t *testing.T) {
	local := &Manifest{Entries: []Entry{
		dirEntry("meetings"),
		fileEntry("meetings/2026-11.md", 50),
	}}
	remote := &Manifest{}

	plan := Diff(local, remote, nil, Master, TwoWay, "peer")

	for _, c := range plan.Changes {
		if c.Rel == "meetings" && c.Kind == Push {
			t.Error("planned to send a directory as file bytes")
		}
	}
}

// An empty folder has no file to bring it into being, so it needs an explicit
// change — otherwise it would simply never appear on the other side, which is
// a quiet wrong answer rather than a loud one.
func TestDiffCreatesAnEmptyDirectory(t *testing.T) {
	local := &Manifest{}
	remote := &Manifest{Entries: []Entry{dirEntry("empty-folder")}}

	plan := Diff(local, remote, nil, Replica, TwoWay, "peer")

	var got bool
	for _, c := range plan.Changes {
		if c.Rel == "empty-folder" {
			if c.Kind != MakeDir {
				t.Errorf("an empty folder should be created, got kind %q", c.Kind)
			}
			got = true
		}
	}
	if !got {
		t.Error("an empty folder on the other machine was ignored entirely")
	}
}

// A folder on both sides is already agreed; planning anything for it is noise
// that shows up in the activity feed on every single sync.
func TestDiffIgnoresADirectoryBothSidesHave(t *testing.T) {
	both := []Entry{dirEntry("rfcs"), fileEntry("rfcs/a.md", 10)}
	plan := Diff(&Manifest{Entries: both}, &Manifest{Entries: both}, nil, Replica, TwoWay, "peer")

	for _, c := range plan.Changes {
		if c.Rel == "rfcs" {
			t.Errorf("a folder both machines have needs no change, got %q", c.Kind)
		}
	}
}
