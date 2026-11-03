package remote

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Filesystem operations on this machine.
//
// Reads are unscoped in v1, by decision rather than oversight: the trust unit
// is the device pair, both machines belong to the same person, and prompting
// on every read would make the API unusable for an agent. The consequence is
// stated plainly in the PRD — in v1, Prompt mode means "cannot run anything,
// can read everything" — and path scoping is the v2 answer.

// MaxReadBytes bounds a single file read.
//
// Large enough for any source file or log worth reading over a link, small
// enough that a careless read of a disk image does not stream for minutes.
const MaxReadBytes = 64 << 20 // 64 MiB

// List returns a directory's contents, directories first then names.
func List(path string) ([]DirEntry, error) {
	dir, err := ExpandPath(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("remote: list %q: %w", path, err)
	}

	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			// A file that vanished between the listing and the stat is not a
			// reason to fail the whole listing.
			continue
		}
		item := DirEntry{
			Name:    e.Name(),
			Size:    info.Size(),
			Mode:    info.Mode().String(),
			IsDir:   e.IsDir(),
			ModTime: info.ModTime(),
		}
		// Report a symlink's target: "where does this point" is otherwise an
		// extra round trip, and on a dev machine it is often the question.
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
				item.Symlink = target
			}
		}
		out = append(out, item)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Stat describes one path.
func Stat(path string) (*FileInfo, error) {
	p, err := ExpandPath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("remote: stat %q: %w", path, err)
	}
	return &FileInfo{
		Path:    p,
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		IsDir:   info.IsDir(),
		ModTime: info.ModTime(),
	}, nil
}

// Open returns a file's contents for streaming, with its size.
func Open(path string) (io.ReadCloser, int64, error) {
	p, err := ExpandPath(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, 0, fmt.Errorf("remote: read %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, 0, fmt.Errorf("remote: %q is a directory", path)
	}
	if info.Size() > MaxReadBytes {
		return nil, 0, fmt.Errorf("remote: %q is %d bytes, over the %d limit",
			path, info.Size(), int64(MaxReadBytes))
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, fmt.Errorf("remote: read %q: %w", path, err)
	}
	return f, info.Size(), nil
}

// Write writes a file, or creates a directory.
//
// Write-to-temp then atomic rename, so an interrupted write never leaves a
// truncated file at the real path. The same discipline the sync engine uses,
// and for the same reason: a half-written file that looks whole is worse than
// a failed write.
func Write(path string, req WriteRequest, content io.Reader) error {
	p, err := ExpandPath(path)
	if err != nil {
		return err
	}

	if req.Mkdir {
		mode := fs.FileMode(req.Mode)
		if mode == 0 {
			mode = 0o755
		}
		if err := os.MkdirAll(p, mode); err != nil {
			return fmt.Errorf("remote: mkdir %q: %w", path, err)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("remote: create parent of %q: %w", path, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), ".ibeji-*")
	if err != nil {
		return fmt.Errorf("remote: create temp for %q: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	mode := fs.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("remote: chmod temp for %q: %w", path, err)
	}
	written, err := io.Copy(tmp, io.LimitReader(content, req.Bytes))
	if err != nil {
		tmp.Close()
		return fmt.Errorf("remote: write %q: %w", path, err)
	}
	if written != req.Bytes {
		tmp.Close()
		return fmt.Errorf("remote: write %q: got %d bytes, expected %d", path, written, req.Bytes)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("remote: sync %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("remote: close temp for %q: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("remote: install %q: %w", path, err)
	}
	return nil
}

// skipDirs are never descended into during a search.
//
// Not a privacy measure — reads are unscoped in v1 — but a performance one: a
// search that walks node_modules or .git spends its whole budget on files
// nobody meant to search, and returns matches nobody wanted.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	".svn":         true,
	".hg":          true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".next":        true,
	".venv":        true,
	"__pycache__":  true,
	".DS_Store":    true,
}

// Search greps file contents and globs file names under a directory.
//
// This is the call that makes the API usable for an agent. Exploring a repo
// one read at a time is unusable; one search replaces fifty reads, which is
// the difference between an agent that can work the far machine and one that
// gives up.
func Search(ctx context.Context, req SearchRequest) ([]Match, bool, error) {
	root, err := ExpandPath(req.Root)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, false, fmt.Errorf("remote: search %q: %w", req.Root, err)
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("remote: search root %q is not a directory", req.Root)
	}

	limit := req.MaxResults
	if limit <= 0 {
		limit = DefaultSearchResults
	}

	var matcher func(string) bool
	if req.Query != "" {
		if req.Regex {
			expr := req.Query
			if req.IgnoreCase {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, false, fmt.Errorf("%w: bad regular expression %q: %v",
					ErrMalformed, req.Query, err)
			}
			matcher = re.MatchString
		} else if req.IgnoreCase {
			needle := strings.ToLower(req.Query)
			matcher = func(line string) bool {
				return strings.Contains(strings.ToLower(line), needle)
			}
		} else {
			matcher = func(line string) bool { return strings.Contains(line, req.Query) }
		}
	}

	var (
		matches   []Match
		truncated bool
	)

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped rather than failing the
			// search: a home directory has plenty, and a partial answer beats
			// no answer.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(matches) >= limit {
			truncated = true
			return filepath.SkipAll
		}

		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		name := d.Name()
		if req.Glob != "" {
			ok, err := filepath.Match(req.Glob, name)
			if err != nil {
				return fmt.Errorf("%w: bad glob %q: %v", ErrMalformed, req.Glob, err)
			}
			if !ok {
				return nil
			}
			// A glob with no query is a filename search, which is a real
			// question on its own: "where is the config file called this".
			if matcher == nil {
				matches = append(matches, Match{Path: path})
				return nil
			}
		}
		if matcher == nil {
			return nil
		}

		found, err := grepFile(path, matcher, limit-len(matches))
		if err != nil {
			return nil // unreadable or binary; skip quietly
		}
		matches = append(matches, found...)
		return nil
	})

	if walkErr != nil && ctx.Err() == nil && !strings.Contains(walkErr.Error(), "SkipAll") {
		// A malformed glob or regex is the caller's error and must surface.
		if strings.Contains(walkErr.Error(), ErrMalformed.Error()) {
			return nil, false, walkErr
		}
	}
	if len(matches) > limit {
		matches = matches[:limit]
		truncated = true
	}
	return matches, truncated, ctx.Err()
}

// maxLineLength bounds a reported match, so one minified bundle does not
// return a megabyte on a single line.
const maxLineLength = 400

// grepFile returns matching lines from one file.
func grepFile(path string, matcher func(string) bool, budget int) ([]Match, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// A quick binary sniff: searching a compiled binary wastes the budget and
	// returns noise nobody can read.
	head := make([]byte, 512)
	n, _ := f.Read(head)
	if isBinary(head[:n]) {
		return nil, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var out []Match
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if !matcher(text) {
			continue
		}
		if len(text) > maxLineLength {
			text = text[:maxLineLength] + "…"
		}
		out = append(out, Match{Path: path, Line: line, Text: strings.TrimRight(text, "\r")})
		if len(out) >= budget {
			break
		}
	}
	return out, scanner.Err()
}

// isBinary reports whether a prefix looks like a binary file.
func isBinary(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

// Info describes this machine, for an agent sizing it up before acting.
func Info(ctx context.Context) *DeviceInfo {
	info := &DeviceInfo{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Shell:     LoginShell(),
		Toolchain: map[string]string{},
	}
	if h, err := os.Hostname(); err == nil {
		info.Hostname = h
	}
	if u, err := user.Current(); err == nil {
		info.User = u.Username
		info.Home = u.HomeDir
	}

	// Versions of what is actually on the login shell's PATH — which is what
	// a remote command will see, and therefore the only honest answer.
	for tool, arg := range map[string]string{
		"git":     "--version",
		"node":    "--version",
		"npm":     "--version",
		"pnpm":    "--version",
		"go":      "version",
		"python3": "--version",
		"cargo":   "--version",
		"docker":  "--version",
	} {
		if v := toolVersion(ctx, tool, arg); v != "" {
			info.Toolchain[tool] = v
		}
	}
	return info
}

// toolVersion asks a tool its version through the login shell.
func toolVersion(ctx context.Context, tool, arg string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	r := NewRunner()
	var sb strings.Builder
	sink := SinkFunc(func(kind OutputKind, b []byte) error {
		if kind == OutStdout {
			sb.Write(b)
		}
		return nil
	})
	res, err := r.Run(ctx, ExecRequest{
		Command:        "command -v " + tool + " >/dev/null 2>&1 && " + tool + " " + arg,
		TimeoutSeconds: 3,
	}, sink)
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(sb.String(), "\n", 2)[0])
}
