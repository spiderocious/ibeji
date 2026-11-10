// Package space keeps a folder identical on both machines.
//
// One master, one replica, master wins collisions, and the replica's version
// is always kept beside it. That rule is not a cop-out: the master is the
// machine that owns the git credentials and the corporate access, so a
// `git pull` there *should* beat an unsynced local edit — and the replica's
// edit is never lost, only moved aside and surfaced.
//
// This is the one module that can lose your work. Everything else in Ìbejì
// fails harmlessly: a declined command or a dropped tunnel costs nothing. A
// sync bug overwrites a file. So: atomic renames everywhere, never delete
// without a conflict copy, and tests that assume it will go wrong.
package space

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// Role is a device's part in one space. Per space, not per device: either
// machine can be master of one and replica of another.
type Role string

const (
	// Master is authoritative. Its version wins a collision.
	Master Role = "master"

	// Replica holds a real, full copy on disk — not a network mount, so the
	// editor, the language server and the file watcher all behave normally
	// and the folder works when the peer is asleep.
	Replica Role = "replica"
)

// State is what a space reports. The exec barrier waits for InSync.
type State string

const (
	// InSync means both participants have converged.
	InSync State = "in-sync"

	// Syncing means transfers are in flight.
	Syncing State = "syncing"

	// Paused means this device stopped participating.
	Paused State = "paused"

	// Offline means a participant is unreachable; it will re-converge.
	Offline State = "offline"

	// Conflicted means the master won a collision and a conflict copy is
	// waiting to be resolved.
	Conflicted State = "conflict"
)

// Mode is how changes flow.
type Mode string

const (
	// TwoWay syncs in both directions, with master-wins on collision.
	TwoWay Mode = "two-way"

	// Mirror flows master → replica only. Replica edits are neither pushed
	// nor conflict-copied. For build output and artefact folders, where the
	// replica's copy is disposable by definition.
	Mirror Mode = "mirror"
)

var (
	// ErrNotFound is returned for an unknown space.
	ErrNotFound = errors.New("space: no such space")

	// ErrExists is returned when creating a space that already exists.
	ErrExists = errors.New("space: a space with that name already exists")

	// ErrNotMaster is returned for an operation only the master may do.
	ErrNotMaster = errors.New("space: only the master can do that")

	// ErrUnresolvedConflict is returned when a barrier cannot proceed because
	// a conflict is waiting.
	ErrUnresolvedConflict = errors.New("space: an unresolved conflict is blocking convergence")
)

// Space is a folder shared with the paired device.
type Space struct {
	Name string `json:"name"`

	// Path is where it lives on THIS machine. The two devices may keep it in
	// different places.
	Path string `json:"path"`

	Role Role `json:"role"`
	Mode Mode `json:"mode"`

	// Ignore patterns, applied before the event queue rather than after: a
	// repo with node_modules unignored is 40,000 files and a watcher storm,
	// so this is load-bearing for performance, not just tidiness.
	Ignore []string `json:"ignore"`

	CreatedAt time.Time `json:"createdAt"`

	// Paused stops this device participating until resumed.
	Paused bool `json:"paused"`
}

// DefaultIgnores are pre-filled on every new space.
//
// `.git/` is the one that matters most. Syncing it corrupts repos through
// index.lock races and half-written objects, and it defeats the product's core
// pattern: sync the working tree, run git on the machine that owns the
// credentials. Removing it raises a warning rather than being quietly allowed.
var DefaultIgnores = []string{
	".git/",
	"node_modules/",
	".DS_Store",
	"dist/",
	"build/",
	".next/",
	"target/",
	".venv/",
	"__pycache__/",
	"*.swp",
	".idea/",
}

// GitIgnorePattern is the entry whose removal warns.
const GitIgnorePattern = ".git/"

// IgnoresGit reports whether this space still ignores .git/.
func (s *Space) IgnoresGit() bool {
	for _, p := range s.Ignore {
		if p == GitIgnorePattern || p == ".git" {
			return true
		}
	}
	return false
}

// GitWarning is what the UI shows when someone removes the .git/ ignore.
const GitWarning = "Syncing .git/ corrupts repositories: the two machines race on index.lock " +
	"and write half of an object each. Sync the working tree and run git on the machine " +
	"that owns the credentials."

// Matcher decides whether a path is ignored.
//
// Patterns are gitignore-shaped but deliberately simpler: a trailing slash
// means a directory, a leading slash anchors to the space root, and `*` does
// not cross a path separator. Negation is not supported — a pattern language
// in a sync engine is somewhere bugs hide, and the defaults plus a few globs
// cover what people actually need.
type Matcher struct {
	patterns []pattern
}

type pattern struct {
	raw      string
	glob     string
	dirOnly  bool
	anchored bool
}

// NewMatcher compiles ignore patterns.
func NewMatcher(patterns []string) *Matcher {
	m := &Matcher{}
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		item := pattern{raw: raw}
		if strings.HasSuffix(p, "/") {
			item.dirOnly = true
			p = strings.TrimSuffix(p, "/")
		}
		if strings.HasPrefix(p, "/") {
			item.anchored = true
			p = strings.TrimPrefix(p, "/")
		}
		item.glob = p
		m.patterns = append(m.patterns, item)
	}
	return m
}

// Match reports whether a path relative to the space root is ignored.
//
// A directory match prunes everything beneath it, which is what makes
// ignoring node_modules cheap rather than merely tidy.
func (m *Matcher) Match(rel string, isDir bool) bool {
	rel = strings.TrimPrefix(path.Clean(rel), "./")
	if rel == "." || rel == "" {
		return false
	}
	segments := strings.Split(rel, "/")

	for _, p := range m.patterns {
		if p.dirOnly {
			// A directory pattern matches the directory and everything in it.
			for i, seg := range segments {
				if p.anchored && i > 0 {
					break
				}
				last := i == len(segments)-1
				if last && !isDir {
					continue
				}
				if ok, _ := path.Match(p.glob, seg); ok {
					return true
				}
			}
			continue
		}
		if p.anchored {
			if ok, _ := path.Match(p.glob, rel); ok {
				return true
			}
			continue
		}
		// Unanchored: match the full path or any single segment, so `*.swp`
		// catches a swap file anywhere.
		if ok, _ := path.Match(p.glob, rel); ok {
			return true
		}
		for _, seg := range segments {
			if ok, _ := path.Match(p.glob, seg); ok {
				return true
			}
		}
	}
	return false
}

// Patterns returns the compiled patterns, for display.
func (m *Matcher) Patterns() []string {
	out := make([]string, len(m.patterns))
	for i, p := range m.patterns {
		out[i] = p.raw
	}
	return out
}

// ImportGitignore reads patterns out of a .gitignore's contents.
//
// Offered at space creation because a repo's .gitignore already says what is
// derived rather than authored, which is very nearly the same question.
// Negations are dropped rather than silently misapplied.
func ImportGitignore(contents string) []string {
	var out []string
	for _, line := range strings.Split(contents, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if strings.HasPrefix(p, "!") {
			// Unsupported, and guessing would be worse than skipping.
			continue
		}
		out = append(out, p)
	}
	return out
}

// Entry is one file in a space's manifest.
type Entry struct {
	// Rel is the path relative to the space root, with forward slashes on
	// every platform so the two machines agree.
	Rel string `json:"rel"`

	Size int64 `json:"size"`

	// ModTime is the file's modification time, truncated to the second:
	// filesystems disagree about sub-second precision, and a spurious
	// difference would mean syncing a file that did not change.
	ModTime time.Time `json:"modTime"`

	// Hash is the content hash. The authority on whether two files differ —
	// size and mtime are the cheap pre-check, never the decision.
	Hash string `json:"hash"`

	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"isDir"`
}

// Same reports whether two entries hold the same content.
func (e Entry) Same(other Entry) bool {
	if e.IsDir != other.IsDir {
		return false
	}
	if e.IsDir {
		return true
	}
	// Hash alone: a file can be rewritten with identical content and a new
	// mtime, and syncing that would be pure waste.
	return e.Hash != "" && e.Hash == other.Hash
}

// Manifest is a snapshot of a space's contents.
type Manifest struct {
	Space   string    `json:"space"`
	TakenAt time.Time `json:"takenAt"`
	Entries []Entry   `json:"entries"`
}

// Index returns the manifest keyed by path, for comparison.
func (m *Manifest) Index() map[string]Entry {
	out := make(map[string]Entry, len(m.Entries))
	for _, e := range m.Entries {
		out[e.Rel] = e
	}
	return out
}

// Sort orders entries so a manifest is stable and diffable.
func (m *Manifest) Sort() {
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Rel < m.Entries[j].Rel })
}

// ChangeKind is what a sync plan does to one path.
type ChangeKind string

const (
	// Push sends this device's version to the peer.
	Push ChangeKind = "push"

	// Pull takes the peer's version.
	Pull ChangeKind = "pull"

	// MakeDir creates a folder that exists only on the other machine.
	//
	// Only needed for a folder with nothing in it: any folder with a file gets
	// created by WriteAtomic's MkdirAll when that file lands. Without this an
	// empty folder would simply never appear, which is a quiet wrong answer
	// rather than a loud one.
	MakeDir ChangeKind = "make-dir"

	// DeleteLocal removes a file the peer deleted.
	DeleteLocal ChangeKind = "delete-local"

	// DeleteRemote removes a file this device deleted.
	DeleteRemote ChangeKind = "delete-remote"

	// Collide means both changed: the master's version is taken and the
	// replica's is preserved beside it.
	//
	// Named Collide rather than Conflict because Conflict is the unresolved
	// collision a user has to decide about — a change kind and a thing
	// waiting on the user's desk are different.
	Collide ChangeKind = "conflict"
)

// Change is one action in a sync plan.
type Change struct {
	Kind ChangeKind `json:"kind"`
	Rel  string     `json:"rel"`

	// ConflictCopy is where the losing version is kept, for a Conflict.
	ConflictCopy string `json:"conflictCopy,omitempty"`

	// Reason explains the change, for the activity feed.
	Reason string `json:"reason,omitempty"`
}

// Plan is what a sync will do.
type Plan struct {
	Changes []Change `json:"changes"`
}

// Conflicts returns just the collisions.
func (p *Plan) Conflicts() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Kind == Collide {
			out = append(out, c)
		}
	}
	return out
}

// Empty reports whether there is nothing to do.
func (p *Plan) Empty() bool { return len(p.Changes) == 0 }

// ConflictPath is where a replica's losing version is kept.
//
// Named so it is obvious what it is and which machine it came from, and so it
// sorts next to the original: `report.md` becomes
// `report.conflict-from-laptop.md`.
func ConflictPath(rel, device string) string {
	dir, file := path.Split(rel)
	ext := path.Ext(file)
	base := strings.TrimSuffix(file, ext)
	slug := slugify(device)
	return dir + base + ".conflict-from-" + slug + ext
}

// IsConflictPath reports whether a path is a conflict copy, so the engine does
// not sync conflict copies back and forth or make conflict copies of them.
func IsConflictPath(rel string) bool {
	_, file := path.Split(rel)
	return strings.Contains(file, ".conflict-from-")
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Diff builds a sync plan from two manifests.
//
// local is this device's view, remote the peer's, and role says whether this
// device is the master — which decides every collision.
//
// Deletes are the ambiguous case: a file missing on one side is either "I
// deleted it" or "it never arrived". This compares against the last agreed
// state (base) to tell them apart; with no base, absence is treated as "not
// yet transferred" rather than as a delete, because inventing a delete is the
// one mistake that loses data.
func Diff(local, remote *Manifest, base map[string]Entry, role Role, mode Mode, peerName string) *Plan {
	plan := &Plan{}
	localIdx := local.Index()
	remoteIdx := remote.Index()

	seen := map[string]bool{}
	for rel := range localIdx {
		seen[rel] = true
	}
	for rel := range remoteIdx {
		seen[rel] = true
	}

	rels := make([]string, 0, len(seen))
	for rel := range seen {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		// Conflict copies are artefacts of a previous collision; syncing them
		// would multiply them.
		if IsConflictPath(rel) {
			continue
		}

		l, hasLocal := localIdx[rel]
		r, hasRemote := remoteIdx[rel]
		b, hadBase := base[rel]

		// A directory is not a thing to transfer, and asking for one broke
		// every space that had a subfolder.
		//
		// Scan records directories (IsDir: true) so the manifest describes the
		// shape of the tree. Diff then compared them like any other entry, so
		// a folder present only on the far side became a Pull, and pull() asked
		// the peer for its bytes. serveFetch correctly refused — "that is a
		// directory" — and that error failed the whole sync, which retried
		// forever: the space sat at "offline", nothing moved, and the log
		// filled with the same line. Only a flat folder of files ever worked.
		//
		// Directories need no transfer: WriteAtomic does MkdirAll on each
		// file's parent, so the tree appears as its contents arrive. The one
		// case that needs doing explicitly is an empty directory, which has no
		// file to bring it into being.
		if (hasLocal && l.IsDir) || (hasRemote && r.IsDir) {
			if hasRemote && r.IsDir && !hasLocal {
				plan.Changes = append(plan.Changes, Change{
					Kind: MakeDir, Rel: rel,
					Reason: "a new folder on the other machine",
				})
			}
			continue
		}

		switch {
		case hasLocal && hasRemote:
			if l.Same(r) {
				continue
			}
			localChanged := !hadBase || !l.Same(b)
			remoteChanged := !hadBase || !r.Same(b)

			switch {
			case localChanged && remoteChanged:
				// Both changed. In mirror mode the replica's copy is
				// disposable by definition, so there is nothing to preserve.
				if mode == Mirror {
					if role == Master {
						plan.Changes = append(plan.Changes, Change{
							Kind: Push, Rel: rel, Reason: "mirror: master's version",
						})
					} else {
						plan.Changes = append(plan.Changes, Change{
							Kind: Pull, Rel: rel, Reason: "mirror: master's version",
						})
					}
					continue
				}
				if role == Master {
					// Our version wins; the peer keeps theirs beside it.
					plan.Changes = append(plan.Changes, Change{
						Kind: Push, Rel: rel,
						Reason: "both changed; master's version wins",
					})
					continue
				}
				// We are the replica: take the master's, keep ours beside it.
				plan.Changes = append(plan.Changes, Change{
					Kind:         Collide,
					Rel:          rel,
					ConflictCopy: ConflictPath(rel, peerName),
					Reason:       "both changed; master's version wins, yours kept beside it",
				})

			case localChanged:
				if mode == Mirror && role == Replica {
					// Replica edits are neither pushed nor conflict-copied.
					continue
				}
				plan.Changes = append(plan.Changes, Change{Kind: Push, Rel: rel})

			case remoteChanged:
				plan.Changes = append(plan.Changes, Change{Kind: Pull, Rel: rel})
			}

		case hasLocal && !hasRemote:
			if hadBase && l.Same(b) {
				// Unchanged here and gone there: the peer deleted it.
				if mode == Mirror && role == Master {
					// A mirror's replica may not delete from the master.
					continue
				}
				plan.Changes = append(plan.Changes, Change{
					Kind: DeleteLocal, Rel: rel, Reason: "deleted on the other machine",
				})
				continue
			}
			// New here, or changed here after the peer deleted it. Send it:
			// a file someone just wrote is not a file to delete.
			if mode == Mirror && role == Replica {
				continue
			}
			plan.Changes = append(plan.Changes, Change{Kind: Push, Rel: rel})

		case !hasLocal && hasRemote:
			if hadBase && r.Same(b) {
				plan.Changes = append(plan.Changes, Change{
					Kind: DeleteRemote, Rel: rel, Reason: "deleted here",
				})
				continue
			}
			plan.Changes = append(plan.Changes, Change{Kind: Pull, Rel: rel})
		}
	}
	return plan
}

// Validate checks a space definition before it is created.
func (s *Space) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("space: a space needs a name")
	}
	if strings.ContainsAny(s.Name, "/\\:") {
		return fmt.Errorf("space: %q cannot be a space name", s.Name)
	}
	if strings.TrimSpace(s.Path) == "" {
		return fmt.Errorf("space: a space needs a folder")
	}
	if s.Role != Master && s.Role != Replica {
		return fmt.Errorf("space: role must be master or replica, got %q", s.Role)
	}
	if s.Mode != TwoWay && s.Mode != Mirror {
		return fmt.Errorf("space: mode must be two-way or mirror, got %q", s.Mode)
	}
	return nil
}

// Mirror reports whether this space is one-way.
func (s *Space) Mirror() bool { return s.Mode == Mirror }
