package space

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// The sync protocol: what crosses the link between two spaces.
//
// Deliberately small. A manifest each way, then file contents for whatever
// differs — because the interesting decisions (who wins, what is preserved)
// are made locally from the two manifests, not negotiated over the wire. A
// protocol that negotiated them would put the rule that can lose work in the
// hardest place to test.

// SyncOp identifies a message on a sync stream.
type SyncOp string

const (
	// OpManifest asks for, or carries, a space's manifest.
	OpManifest SyncOp = "manifest"

	// OpFetch asks for one file's contents.
	OpFetch SyncOp = "fetch"

	// OpState asks for a space's current state, for the barrier.
	OpState SyncOp = "state"

	// OpListSpaces asks which spaces the peer has.
	OpListSpaces SyncOp = "spaces"

	// OpPush sends a file TO the peer.
	//
	// Needed because "push" cannot mean "wait for them to pull": each side
	// syncs on its own schedule, so a replica's change would sit unsent until
	// the master happened to look. Without this, sync is one-way in practice
	// however two-way it is on paper.
	OpPush SyncOp = "push"

	// OpDelete removes a file on the peer.
	OpDelete SyncOp = "delete"
)

// SyncRequest is the first frame on a sync stream.
type SyncRequest struct {
	Op    SyncOp `json:"op"`
	Space string `json:"space,omitempty"`
	Rel   string `json:"rel,omitempty"`

	// Bytes and Mode carry a pushed file's size and permissions.
	Bytes int64  `json:"bytes,omitempty"`
	Mode  uint32 `json:"mode,omitempty"`
}

// SyncResponse is the reply.
type SyncResponse struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`

	Manifest *Manifest `json:"manifest,omitempty"`
	State    State     `json:"state,omitempty"`
	Spaces   []string  `json:"spaces,omitempty"`

	// Bytes is how many bytes of content follow, for a fetch.
	Bytes int64  `json:"bytes,omitempty"`
	Mode  uint32 `json:"mode,omitempty"`
}

// MaxSyncFile bounds a single file transfer.
//
// Large enough for anything in a working tree worth syncing, small enough that
// a stray disk image does not occupy the link for an hour. Bigger files are
// refused with a reason rather than attempted and abandoned.
const MaxSyncFile = 2 << 30 // 2 GiB

// Serve answers a peer's sync request on this machine.
func (m *Manager) Serve(ctx context.Context, stream link.Stream) {
	defer stream.Close()

	if err := stream.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}
	var req SyncRequest
	dec := json.NewDecoder(io.LimitReader(stream, 1<<20))
	if err := dec.Decode(&req); err != nil {
		_ = writeSyncResponse(stream, SyncResponse{Code: "bad_request", Message: err.Error()})
		return
	}
	// The decoder reads ahead, so a pushed file's first bytes may already be
	// in its buffer.
	body := io.MultiReader(dec.Buffered(), stream)
	_ = stream.SetReadDeadline(time.Time{})

	switch req.Op {
	case OpListSpaces:
		var names []string
		for _, s := range m.List() {
			names = append(names, s.Name)
		}
		_ = writeSyncResponse(stream, SyncResponse{Spaces: names})

	case OpManifest:
		manifest, err := m.ScanLocal(ctx, req.Space)
		if err != nil {
			_ = writeSyncResponse(stream, SyncResponse{Code: "not_found", Message: err.Error()})
			return
		}
		_ = writeSyncResponse(stream, SyncResponse{Manifest: manifest})

	case OpState:
		status, err := m.Status(req.Space)
		if err != nil {
			_ = writeSyncResponse(stream, SyncResponse{Code: "not_found", Message: err.Error()})
			return
		}
		_ = writeSyncResponse(stream, SyncResponse{State: status.State})

	case OpFetch:
		m.serveFetch(stream, req)

	case OpPush:
		m.servePush(stream, req, body)

	case OpDelete:
		m.serveDelete(stream, req)

	default:
		_ = writeSyncResponse(stream, SyncResponse{
			Code:    "bad_request",
			Message: fmt.Sprintf("unknown sync operation %q", req.Op),
		})
	}
}

func (m *Manager) serveFetch(stream link.Stream, req SyncRequest) {
	s, err := m.Get(req.Space)
	if err != nil {
		_ = writeSyncResponse(stream, SyncResponse{Code: "not_found", Message: err.Error()})
		return
	}

	// The path comes from the peer, so it is checked rather than trusted: a
	// `../` would otherwise read any file on this machine through a space.
	clean := filepath.Clean(filepath.FromSlash(req.Rel))
	full := filepath.Join(s.Path, clean)
	if !within(s.Path, full) {
		_ = writeSyncResponse(stream, SyncResponse{
			Code: "bad_request", Message: "that path is outside the space",
		})
		return
	}

	info, err := os.Stat(full)
	if err != nil {
		_ = writeSyncResponse(stream, SyncResponse{Code: "not_found", Message: err.Error()})
		return
	}
	if info.IsDir() {
		_ = writeSyncResponse(stream, SyncResponse{Code: "bad_request", Message: "that is a directory"})
		return
	}
	if info.Size() > MaxSyncFile {
		_ = writeSyncResponse(stream, SyncResponse{
			Code: "too_large",
			Message: fmt.Sprintf("%s is %d bytes, over the %d limit",
				req.Rel, info.Size(), int64(MaxSyncFile)),
		})
		return
	}

	f, err := os.Open(full)
	if err != nil {
		_ = writeSyncResponse(stream, SyncResponse{Code: "internal", Message: err.Error()})
		return
	}
	defer f.Close()

	if err := writeSyncResponse(stream, SyncResponse{
		Bytes: info.Size(), Mode: uint32(info.Mode().Perm()),
	}); err != nil {
		return
	}
	_, _ = io.Copy(stream, f)
}

// servePush writes a file the peer sent.
func (m *Manager) servePush(stream link.Stream, req SyncRequest, body io.Reader) {
	sp, err := m.Get(req.Space)
	if err != nil {
		_ = writeSyncResponse(stream, SyncResponse{Code: "not_found", Message: err.Error()})
		return
	}
	// A mirror's replica may not write to the master: its copy is disposable
	// by definition, and letting it push would make mirror mode two-way.
	if sp.Mode == Mirror && sp.Role == Master {
		_ = writeSyncResponse(stream, SyncResponse{
			Code: "refused", Message: "this is a one-way mirror; the replica cannot push",
		})
		return
	}

	clean := filepath.Clean(filepath.FromSlash(req.Rel))
	dest := filepath.Join(sp.Path, clean)
	if !within(sp.Path, dest) {
		_ = writeSyncResponse(stream, SyncResponse{
			Code: "bad_request", Message: "that path is outside the space",
		})
		return
	}
	if req.Bytes > MaxSyncFile {
		_ = writeSyncResponse(stream, SyncResponse{
			Code: "too_large", Message: "that file is over the transfer limit",
		})
		return
	}

	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if _, err := WriteAtomic(dest, mode, io.LimitReader(body, req.Bytes)); err != nil {
		_ = writeSyncResponse(stream, SyncResponse{Code: "internal", Message: err.Error()})
		return
	}
	_ = writeSyncResponse(stream, SyncResponse{})
}

// serveDelete removes a file the peer deleted.
func (m *Manager) serveDelete(stream link.Stream, req SyncRequest) {
	sp, err := m.Get(req.Space)
	if err != nil {
		_ = writeSyncResponse(stream, SyncResponse{Code: "not_found", Message: err.Error()})
		return
	}
	if sp.Mode == Mirror && sp.Role == Master {
		_ = writeSyncResponse(stream, SyncResponse{
			Code: "refused", Message: "this is a one-way mirror",
		})
		return
	}
	clean := filepath.Clean(filepath.FromSlash(req.Rel))
	if !within(sp.Path, filepath.Join(sp.Path, clean)) {
		_ = writeSyncResponse(stream, SyncResponse{
			Code: "bad_request", Message: "that path is outside the space",
		})
		return
	}
	if err := RemoveFile(sp.Path, filepath.ToSlash(clean)); err != nil {
		_ = writeSyncResponse(stream, SyncResponse{Code: "internal", Message: err.Error()})
		return
	}
	_ = writeSyncResponse(stream, SyncResponse{})
}

// within reports whether path is inside root, after resolving symlinks.
//
// A space whose folder contains a symlink out of the tree would otherwise let
// a crafted path read anywhere the user can.
func within(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' &&
		(rel[2] == filepath.Separator || rel[2] == '/')
}

// Client asks the peer about its spaces.
type Client struct {
	Conn func() link.Conn
}

// ErrOffline is returned when there is no link to carry a sync.
var ErrOffline = link.ErrNotLinked

func (c *Client) open(ctx context.Context, req SyncRequest) (link.Stream, SyncResponse, error) {
	conn := c.Conn()
	if conn == nil {
		return nil, SyncResponse{}, ErrOffline
	}
	stream, err := conn.Open(ctx, link.KindSync)
	if err != nil {
		return nil, SyncResponse{}, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		stream.Close()
		return nil, SyncResponse{}, err
	}
	if _, err := stream.Write(body); err != nil {
		stream.Close()
		return nil, SyncResponse{}, err
	}
	if err := stream.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		stream.Close()
		return nil, SyncResponse{}, err
	}

	resp, err := readSyncResponse(stream)
	if err != nil {
		stream.Close()
		return nil, SyncResponse{}, err
	}
	if resp.Code != "" {
		stream.Close()
		return nil, resp, fmt.Errorf("space: %s: %s", resp.Code, resp.Message)
	}
	return stream, resp, nil
}

// Spaces lists the peer's spaces.
func (c *Client) Spaces(ctx context.Context) ([]string, error) {
	stream, resp, err := c.open(ctx, SyncRequest{Op: OpListSpaces})
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return resp.Spaces, nil
}

// Manifest fetches the peer's view of a space.
func (c *Client) Manifest(ctx context.Context, space string) (*Manifest, error) {
	stream, resp, err := c.open(ctx, SyncRequest{Op: OpManifest, Space: space})
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	if resp.Manifest == nil {
		return nil, errors.New("space: the peer sent no manifest")
	}
	return resp.Manifest, nil
}

// State asks the peer what state a space is in.
func (c *Client) State(ctx context.Context, space string) (State, error) {
	stream, resp, err := c.open(ctx, SyncRequest{Op: OpState, Space: space})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	return resp.State, nil
}

// Fetch returns a file's contents from the peer.
func (c *Client) Fetch(ctx context.Context, space, rel string) ([]byte, uint32, error) {
	stream, resp, err := c.open(ctx, SyncRequest{Op: OpFetch, Space: space, Rel: rel})
	if err != nil {
		return nil, 0, err
	}
	defer stream.Close()

	// A generous deadline: a large file over a slow link is slow, not stuck.
	if err := stream.SetReadDeadline(time.Now().Add(30 * time.Minute)); err != nil {
		return nil, 0, err
	}
	body := make([]byte, resp.Bytes)
	if _, err := io.ReadFull(stream, body); err != nil {
		return nil, 0, fmt.Errorf("space: fetch %q: %w", rel, err)
	}
	return body, resp.Mode, nil
}

// Push sends a file to the peer.
func (c *Client) Push(ctx context.Context, space, rel string, content []byte, mode uint32) error {
	conn := c.Conn()
	if conn == nil {
		return ErrOffline
	}
	stream, err := conn.Open(ctx, link.KindSync)
	if err != nil {
		return err
	}
	defer stream.Close()

	req := SyncRequest{
		Op: OpPush, Space: space, Rel: rel,
		Bytes: int64(len(content)), Mode: mode,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := stream.Write(body); err != nil {
		return err
	}
	if len(content) > 0 {
		if _, err := stream.Write(content); err != nil {
			return err
		}
	}
	if err := stream.SetReadDeadline(time.Now().Add(10 * time.Minute)); err != nil {
		return err
	}
	resp, err := readSyncResponse(stream)
	if err != nil {
		return err
	}
	if resp.Code != "" {
		return fmt.Errorf("space: push %q: %s: %s", rel, resp.Code, resp.Message)
	}
	return nil
}

// Delete removes a file on the peer.
func (c *Client) Delete(ctx context.Context, space, rel string) error {
	stream, _, err := c.open(ctx, SyncRequest{Op: OpDelete, Space: space, Rel: rel})
	if err != nil {
		return err
	}
	stream.Close()
	return nil
}

// Sender returns a Sender for a space, for Manager.ApplyWith.
func (c *Client) Sender(space string) Sender { return &spaceSender{client: c, space: space} }

type spaceSender struct {
	client *Client
	space  string
}

func (s *spaceSender) Send(ctx context.Context, rel string, content []byte, mode uint32) error {
	return s.client.Push(ctx, s.space, rel, content, mode)
}

func (s *spaceSender) Remove(ctx context.Context, rel string) error {
	return s.client.Delete(ctx, s.space, rel)
}

// Transfer returns a Transfer for a space, for Manager.Apply.
func (c *Client) Transfer(space string) Transfer {
	return func(ctx context.Context, rel string) ([]byte, uint32, error) {
		return c.Fetch(ctx, space, rel)
	}
}

// --- framing ---

func writeSyncResponse(w io.Writer, resp SyncResponse) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	var hdr [4]byte
	hdr[0] = byte(len(body) >> 24)
	hdr[1] = byte(len(body) >> 16)
	hdr[2] = byte(len(body) >> 8)
	hdr[3] = byte(len(body))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readSyncResponse(r io.Reader) (SyncResponse, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return SyncResponse{}, err
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n < 0 || n > 64<<20 {
		return SyncResponse{}, fmt.Errorf("space: response frame of %d bytes is implausible", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return SyncResponse{}, err
	}
	var resp SyncResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return SyncResponse{}, err
	}
	return resp, nil
}
