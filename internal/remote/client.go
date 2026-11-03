package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// Client speaks the remote protocol to the paired device.
//
// It is the mirror of Server: the daemon uses it to carry the local API's
// requests across the link, so the window, the CLI and an agent all reach the
// far machine through the same code.
type Client struct {
	// Conn is the live link. Nil when the peer is unreachable, which on a LAN
	// is normal rather than an error.
	Conn func() link.Conn

	// Provenance stamps every request with who asked.
	Provenance string
}

// ErrOffline is returned when there is no link to carry the request.
var ErrOffline = link.ErrNotLinked

// open starts a stream and sends the request frame.
func (c *Client) open(ctx context.Context, req Request) (link.Stream, error) {
	conn := c.Conn()
	if conn == nil {
		return nil, ErrOffline
	}
	if req.Provenance == "" {
		req.Provenance = c.Provenance
	}

	kind := link.KindRPC
	if req.Op == OpExec {
		kind = link.KindExec
	}
	stream, err := conn.Open(ctx, kind)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("remote: encode request: %w", err)
	}
	if _, err := stream.Write(body); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// readResponse reads the reply frame.
func readResponse(stream link.Stream) (Response, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(stream, hdr[:]); err != nil {
		return Response{}, fmt.Errorf("remote: read response: %w", err)
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n < 0 || n > 16<<20 {
		return Response{}, fmt.Errorf("remote: response frame of %d bytes is implausible", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(stream, body); err != nil {
		return Response{}, fmt.Errorf("remote: read response body: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return Response{}, fmt.Errorf("remote: decode response: %w", err)
	}
	return resp, nil
}

// RemoteError is a failure the far machine reported.
//
// It carries the stable code so a caller can branch — declined means stop,
// peer_offline means retry later, timed_out means nobody was there.
type RemoteError struct {
	Code    string
	Message string
	Hint    string
}

func (e *RemoteError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// asError converts a failing response into an error.
func asError(resp Response) error {
	if resp.Code == "" {
		return nil
	}
	return &RemoteError{Code: resp.Code, Message: resp.Message, Hint: resp.Hint}
}

// call performs a request/response operation.
func (c *Client) call(ctx context.Context, req Request, timeout time.Duration) (Response, link.Stream, error) {
	stream, err := c.open(ctx, req)
	if err != nil {
		return Response{}, nil, err
	}
	if timeout > 0 {
		if err := stream.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			stream.Close()
			return Response{}, nil, err
		}
	}
	resp, err := readResponse(stream)
	if err != nil {
		stream.Close()
		return Response{}, nil, err
	}
	if err := asError(resp); err != nil {
		stream.Close()
		return Response{}, nil, err
	}
	return resp, stream, nil
}

// defaultCallTimeout bounds an ordinary request. Generous, because a prompt on
// the far machine can legitimately take up to its own timeout to answer.
const defaultCallTimeout = 2 * time.Minute

// List returns a remote directory's contents.
func (c *Client) List(ctx context.Context, path string) ([]DirEntry, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpList, Path: path}, defaultCallTimeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return resp.Entries, nil
}

// Stat describes a remote path.
func (c *Client) Stat(ctx context.Context, path string) (*FileInfo, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpStat, Path: path}, defaultCallTimeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return resp.Stat, nil
}

// Read streams a remote file into w.
func (c *Client) Read(ctx context.Context, path string, w io.Writer) (int64, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpRead, Path: path}, defaultCallTimeout)
	if err != nil {
		return 0, err
	}
	defer stream.Close()

	if err := stream.SetReadDeadline(time.Now().Add(10 * time.Minute)); err != nil {
		return 0, err
	}
	return io.CopyN(w, stream, resp.Bytes)
}

// Search greps the far machine.
func (c *Client) Search(ctx context.Context, req SearchRequest) ([]Match, bool, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpSearch, Search: &req}, defaultCallTimeout)
	if err != nil {
		return nil, false, err
	}
	defer stream.Close()
	return resp.Matches, resp.Truncated, nil
}

// Write sends a file to the far machine.
func (c *Client) Write(ctx context.Context, path string, req WriteRequest, content io.Reader) error {
	stream, err := c.open(ctx, Request{Op: OpWrite, Path: path, Write: &req})
	if err != nil {
		return err
	}
	defer stream.Close()

	if req.Bytes > 0 && content != nil {
		if _, err := io.CopyN(stream, content, req.Bytes); err != nil {
			return fmt.Errorf("remote: send content: %w", err)
		}
	}
	if err := stream.SetReadDeadline(time.Now().Add(defaultCallTimeout)); err != nil {
		return err
	}
	resp, err := readResponse(stream)
	if err != nil {
		return err
	}
	return asError(resp)
}

// Info describes the far machine.
func (c *Client) Info(ctx context.Context) (*DeviceInfo, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpInfo}, defaultCallTimeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return resp.Info, nil
}

// Check asks the far machine to probe a target.
func (c *Client) Check(ctx context.Context, req CheckRequest) (*CheckResult, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpCheck, Check: &req}, defaultCallTimeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return resp.Check, nil
}

// Processes lists what this device started on the far machine.
func (c *Client) Processes(ctx context.Context) ([]ProcessInfo, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpPS}, defaultCallTimeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return resp.Processes, nil
}

// Kill stops a process on the far machine, and everything it spawned.
func (c *Client) Kill(ctx context.Context, id string) error {
	_, stream, err := c.call(ctx, Request{Op: OpKill, ProcessID: id}, defaultCallTimeout)
	if err != nil {
		return err
	}
	stream.Close()
	return nil
}

// Attach returns a process's remembered output.
func (c *Client) Attach(ctx context.Context, id string) ([]OutputChunkJSON, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpAttach, ProcessID: id}, defaultCallTimeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return resp.Output, nil
}

// Ports lists what is listening on the far machine.
func (c *Client) Ports(ctx context.Context) ([]PortInfo, error) {
	resp, stream, err := c.call(ctx, Request{Op: OpPorts}, defaultCallTimeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return resp.Ports, nil
}

// Exec runs a command on the far machine, streaming output to the sink.
//
// Returns the remote exit status, which the CLI then exits with: `ibeji run …
// && deploy` has to mean what it says.
func (c *Client) Exec(ctx context.Context, req ExecRequest, sink Sink) (*ExitStatus, *ExecStarted, error) {
	stream, err := c.open(ctx, Request{Op: OpExec, Exec: &req})
	if err != nil {
		return nil, nil, err
	}
	defer stream.Close()

	// The acknowledgement may take as long as a prompt on the far machine.
	if err := stream.SetReadDeadline(time.Now().Add(defaultCallTimeout)); err != nil {
		return nil, nil, err
	}
	resp, err := readResponse(stream)
	if err != nil {
		return nil, nil, err
	}
	if err := asError(resp); err != nil {
		return nil, nil, err
	}
	if req.Detach {
		return nil, resp.Exec, nil
	}

	// Output has no overall deadline: a build legitimately runs for minutes,
	// and the far machine's own timeout bounds it.
	if err := stream.SetReadDeadline(time.Time{}); err != nil {
		return nil, nil, err
	}

	for {
		if ctx.Err() != nil {
			return nil, resp.Exec, ctx.Err()
		}
		kind, body, err := readOutputFrame(stream)
		if err != nil {
			return nil, resp.Exec, fmt.Errorf("remote: read output: %w", err)
		}
		switch kind {
		case OutStdout, OutStderr:
			if sink != nil {
				if err := sink.Write(kind, body); err != nil {
					return nil, resp.Exec, err
				}
			}
		case OutExit:
			var exit ExitStatus
			if err := json.Unmarshal(body, &exit); err != nil {
				return nil, resp.Exec, fmt.Errorf("remote: decode exit status: %w", err)
			}
			return &exit, resp.Exec, nil
		}
	}
}

// readOutputFrame reads one chunk of command output.
func readOutputFrame(stream link.Stream) (OutputKind, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(stream, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(hdr[1])<<24 | int(hdr[2])<<16 | int(hdr[3])<<8 | int(hdr[4])
	if n < 0 || n > 16<<20 {
		return 0, nil, fmt.Errorf("remote: output frame of %d bytes is implausible", n)
	}
	body := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(stream, body); err != nil {
			return 0, nil, err
		}
	}
	return OutputKind(hdr[0]), body, nil
}
