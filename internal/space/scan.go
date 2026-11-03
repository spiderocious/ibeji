package space

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Scanning a space into a manifest.
//
// The expensive part is hashing, so it is avoided wherever the answer is
// already known: an entry whose size and mtime match the previous scan keeps
// its hash. On a repo with thousands of files that is the difference between a
// scan you notice and one you do not — and the PRD requires the daemon to be
// invisible.

// hashReuseGrace is how recently a file must have been modified for its
// cached hash to be distrusted.
//
// mtime resolution is one second on many filesystems; two gives margin for a
// scan that straddles a tick.
const hashReuseGrace = 2 * time.Second

// Scan walks a space and builds its manifest.
//
// prev, when given, supplies hashes for files that have not changed. Ignored
// paths are pruned during the walk rather than filtered afterwards: descending
// into node_modules to then discard it is most of the cost of not ignoring it
// at all.
func Scan(ctx context.Context, root string, matcher *Matcher, prev map[string]Entry) (*Manifest, error) {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("space: scan %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("space: %q is not a directory", root)
	}

	scanStart := time.Now()
	m := &Manifest{TakenAt: scanStart}

	err = filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped rather than failing the
			// scan: a partial manifest is wrong in a way that could delete
			// files, so this returns the error only for the root itself.
			if abs == root {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if abs == root {
			return nil
		}

		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if matcher != nil && matcher.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Conflict copies are local artefacts: syncing them would multiply
		// them across both machines.
		if IsConflictPath(rel) {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil // vanished between the walk and the stat
		}

		if d.IsDir() {
			m.Entries = append(m.Entries, Entry{
				Rel: rel, IsDir: true, Mode: uint32(fi.Mode().Perm()), ModTime: fi.ModTime().Truncate(time.Second),
			})
			return nil
		}
		// Symlinks are not followed and not synced: following them can walk
		// out of the space entirely, and copying the target silently turns a
		// link into a file.
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return nil
		}

		entry := Entry{
			Rel:     rel,
			Size:    fi.Size(),
			ModTime: fi.ModTime().Truncate(time.Second),
			Mode:    uint32(fi.Mode().Perm()),
		}

		// Reuse the previous hash only when the file is old enough that it
		// cannot have changed since the last scan without the mtime moving.
		//
		// mtime has one-second resolution on many filesystems, so a file
		// edited within the same second as the previous scan keeps its size
		// and its timestamp — and reusing the hash then makes the edit
		// invisible, so it never syncs. Silent divergence, which is the worst
		// kind of bug this module can have.
		//
		// Anything modified in the last two seconds is re-hashed. That is a
		// handful of files on any realistic scan, and it buys certainty.
		fresh := scanStart.Sub(entry.ModTime) < hashReuseGrace
		if old, ok := prev[rel]; ok && !fresh &&
			old.Size == entry.Size && old.ModTime.Equal(entry.ModTime) && old.Hash != "" {
			entry.Hash = old.Hash
		} else {
			h, err := HashFile(abs)
			if err != nil {
				return nil // unreadable; leave it out rather than guess
			}
			entry.Hash = h
		}

		m.Entries = append(m.Entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}

	m.Sort()
	return m, nil
}

// HashFile returns a file's content hash.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteAtomic writes content to a path without ever leaving a partial file
// visible there.
//
// Write to a temp file beside the destination, fsync it, then rename. A rename
// within a directory is atomic, so a reader sees either the old file or the
// new one and never half of either. An interrupted sync that left a truncated
// file looking complete is the failure this exists to prevent.
func WriteAtomic(dest string, mode os.FileMode, content io.Reader) (int64, error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("space: create %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".ibeji-sync-*")
	if err != nil {
		return 0, fmt.Errorf("space: temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Remove the temp file on every failure path, so an interrupted sync does
	// not litter the space with debris the user then sees in their editor.
	defer os.Remove(tmpName)

	if mode == 0 {
		mode = 0o644
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return 0, fmt.Errorf("space: chmod temp: %w", err)
	}

	written, err := io.Copy(tmp, content)
	if err != nil {
		tmp.Close()
		return written, fmt.Errorf("space: write temp: %w", err)
	}
	// fsync before the rename: without it a crash can leave the rename
	// durable and the contents not, which is a file that exists and is empty.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return written, fmt.Errorf("space: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return written, fmt.Errorf("space: close temp: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return written, fmt.Errorf("space: install %q: %w", dest, err)
	}
	return written, nil
}

// MoveAside renames a file to its conflict path, preserving the loser.
//
// Never a delete. The PRD forbids a silent conflict, and a replica's edit that
// vanished because the master also touched the file is the single worst thing
// this product could do.
func MoveAside(root, rel, conflictRel string) error {
	src := filepath.Join(root, filepath.FromSlash(rel))
	dst := filepath.Join(root, filepath.FromSlash(conflictRel))

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("space: prepare conflict copy: %w", err)
	}
	// If a conflict copy is already there from a previous collision, number
	// this one rather than overwriting: losing the first conflict copy to the
	// second would be the same bug one level up.
	if _, err := os.Stat(dst); err == nil {
		dst = nextFreeName(dst)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("space: keep conflict copy: %w", err)
	}
	return nil
}

// nextFreeName appends a counter until the path is free.
func nextFreeName(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return base + "-conflict" + ext
}

// RemoveFile deletes a file from a space, and prunes the directory it leaves
// empty — a tree of empty folders after a delete is noise in an editor.
func RemoveFile(root, rel string) error {
	target := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("space: remove %q: %w", rel, err)
	}
	// Walk up while directories are empty, stopping at the space root.
	dir := filepath.Dir(target)
	rootClean := filepath.Clean(root)
	for dir != rootClean && strings.HasPrefix(dir, rootClean) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return nil
		}
		if err := os.Remove(dir); err != nil {
			return nil
		}
		dir = filepath.Dir(dir)
	}
	return nil
}
