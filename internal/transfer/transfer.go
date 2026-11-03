// Package transfer moves files and clipboard content between the two
// machines, and hands a URL or an app across.
//
// Deliberately simple. File drop lands in a known inbox and notifies; there is
// no chat, no history, no previews. Clipboard sync is off by default and its
// contents are never written anywhere. Handoff relaunches an app pointed at
// the same context rather than migrating a window.
package transfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// Op identifies a transfer message.
type Op string

const (
	// OpFile sends one file into the peer's inbox.
	OpFile Op = "file"

	// OpClipboard offers clipboard content.
	OpClipboard Op = "clipboard"

	// OpOpenURL asks the peer to open a URL in its browser.
	//
	// The escape hatch that matters: when an SSO session lapses on the
	// machine that owns it, the sign-in URL goes across and is clicked
	// through there, where the session and the posture are already trusted.
	OpOpenURL Op = "open-url"

	// OpLaunch asks the peer to open an application, optionally with a path.
	OpLaunch Op = "launch"

	// OpScratchpad carries the shared note.
	OpScratchpad Op = "scratchpad"

	// OpNotify mirrors one of Ìbejì's own events as a toast.
	OpNotify Op = "notify"
)

// Request is the first frame on a transfer stream.
type Request struct {
	Op Op `json:"op"`

	// Name is the file's name, for a file drop.
	Name string `json:"name,omitempty"`

	// Bytes is how many bytes follow the request frame.
	Bytes int64 `json:"bytes,omitempty"`

	// Text carries a URL, clipboard text, or the scratchpad.
	Text string `json:"text,omitempty"`

	// App names an application to launch.
	App string `json:"app,omitempty"`

	// Path is an argument for the application.
	Path string `json:"path,omitempty"`

	// Title and Body carry a mirrored notification.
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`

	// UpdatedAt orders scratchpad saves: last writer wins per save.
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// Response is the reply.
type Response struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`

	// Path is where a received file landed.
	Path string `json:"path,omitempty"`

	// Text returns the scratchpad.
	Text      string    `json:"text,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// DefaultInboxName is the folder received files land in.
const DefaultInboxName = "Ibeji/Inbox"

// MaxFileSize bounds one drop.
const MaxFileSize = 10 << 30 // 10 GiB

var (
	// ErrTooLarge is returned for a file over the limit.
	ErrTooLarge = errors.New("transfer: file is too large")

	// ErrOffline is returned when the peer is unreachable.
	//
	// There is no store-and-forward: a send to a sleeping machine fails
	// visibly. A queue that silently held the file until tomorrow is worse
	// than an error today, because you act on the belief it arrived.
	ErrOffline = link.ErrNotLinked
)

// Inbox is where received files land.
type Inbox struct {
	// Dir is the folder. Defaults to ~/Ibeji/Inbox.
	Dir string
}

// Path resolves the inbox folder, creating it.
func (i *Inbox) Path() (string, error) {
	dir := i.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("transfer: locate home directory: %w", err)
		}
		dir = filepath.Join(home, filepath.FromSlash(DefaultInboxName))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("transfer: create inbox: %w", err)
	}
	return dir, nil
}

// Receive writes an incoming file into the inbox.
//
// The name comes from the other machine, so it is reduced to a base name: a
// path like ../../.ssh/authorized_keys would otherwise write outside the
// inbox entirely.
func (i *Inbox) Receive(name string, size int64, content io.Reader) (string, error) {
	if size > MaxFileSize {
		return "", fmt.Errorf("%w: %d bytes", ErrTooLarge, size)
	}
	dir, err := i.Path()
	if err != nil {
		return "", err
	}

	safe := safeName(name)
	dest := filepath.Join(dir, safe)
	// Never overwrite: two drops of report.pdf are two files, not one lost.
	dest = uniquePath(dest)

	tmp, err := os.CreateTemp(dir, ".ibeji-drop-*")
	if err != nil {
		return "", fmt.Errorf("transfer: temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	written, err := io.Copy(tmp, io.LimitReader(content, size))
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("transfer: receive %q: %w", safe, err)
	}
	if written != size {
		tmp.Close()
		return "", fmt.Errorf("transfer: %q arrived as %d of %d bytes", safe, written, size)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// Atomic rename, so a partial download is never visible at the real path
	// — someone double-clicking a half-arrived file is a confusing failure.
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", fmt.Errorf("transfer: install %q: %w", safe, err)
	}
	return dest, nil
}

// safeName reduces a peer-supplied name to something safe to write.
func safeName(name string) string {
	name = filepath.Base(filepath.FromSlash(name))
	name = strings.TrimSpace(name)
	// Control characters would corrupt a terminal listing the inbox.
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)
	switch name {
	case "", ".", "..":
		return "received-file"
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		name = name[:200-len(ext)] + ext
	}
	return name
}

// uniquePath appends a counter rather than overwriting.
func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return base + "-" + time.Now().Format("20060102-150405") + ext
}

// Scratchpad is the one shared note.
//
// A single markdown note, last-writer-wins per save. It stays one note on
// purpose: the moment it grows titles and search it is a notes app, and
// someone else already built that.
type Scratchpad struct {
	mu        sync.RWMutex
	text      string
	updatedAt time.Time
	path      string
}

// NewScratchpad opens the note, loading whatever was there.
func NewScratchpad(path string) *Scratchpad {
	s := &Scratchpad{path: path}
	if body, err := os.ReadFile(path); err == nil {
		s.text = string(body)
		if info, err := os.Stat(path); err == nil {
			s.updatedAt = info.ModTime()
		}
	}
	return s
}

// Get returns the note.
func (s *Scratchpad) Get() (string, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.text, s.updatedAt
}

// Set replaces the note and persists it.
func (s *Scratchpad) Set(text string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	s.mu.Lock()
	// Last writer wins per save: an older save arriving late must not undo a
	// newer one. Simple, and right for a note two people rarely edit at once.
	if at.Before(s.updatedAt) {
		s.mu.Unlock()
		return nil
	}
	s.text = text
	s.updatedAt = at
	path := s.path
	s.mu.Unlock()

	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- framing ---

// WriteRequest sends a request frame.
//
// Bare JSON, no length prefix: the reader decodes a single value and hands
// back whatever the decoder buffered past it, which is how a file's content
// can follow immediately on the same stream. A length prefix here would have
// to agree with that read, and the two drifting apart is exactly the bug that
// made every remote write hang.
func WriteRequest(w io.Writer, req Request) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadRequest reads a request frame, returning any content buffered past it.
func ReadRequest(r io.Reader) (Request, io.Reader, error) {
	dec := json.NewDecoder(io.LimitReader(r, 4<<20))
	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, nil, err
	}
	// The decoder reads ahead, so content following the frame has to start
	// from what it buffered — the same trap the remote protocol hit.
	return req, io.MultiReader(dec.Buffered(), r), nil
}

// WriteResponse sends a reply frame.
func WriteResponse(w io.Writer, resp Response) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return writeFrame(w, body)
}

// ReadResponse reads a reply frame.
func ReadResponse(r io.Reader) (Response, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Response{}, err
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n < 0 || n > 16<<20 {
		return Response{}, fmt.Errorf("transfer: response frame of %d bytes is implausible", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

func writeFrame(w io.Writer, body []byte) error {
	var hdr [4]byte
	hdr[0] = byte(len(body) >> 24)
	hdr[1] = byte(len(body) >> 16)
	hdr[2] = byte(len(body) >> 8)
	hdr[3] = byte(len(body))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}
