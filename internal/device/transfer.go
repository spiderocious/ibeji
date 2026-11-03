package device

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/feranmi/ibeji/internal/api"
	"github.com/feranmi/ibeji/internal/link"
	"github.com/feranmi/ibeji/internal/permission"
	"github.com/feranmi/ibeji/internal/transfer"
)

// File drop, clipboard, handoff and the scratchpad, as the daemon wires them.

// Inbox returns where received files land.
func (d *Daemon) Inbox() *transfer.Inbox {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inbox == nil {
		d.inbox = &transfer.Inbox{}
	}
	return d.inbox
}

// InboxPath implements api.TransferState.
func (d *Daemon) InboxPath() (string, error) { return d.Inbox().Path() }

// Pad returns the shared note.
func (d *Daemon) Pad() *transfer.Scratchpad {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pad == nil {
		d.pad = transfer.NewScratchpad(d.padPath)
	}
	return d.pad
}

// SetPadPath says where the note is stored.
func (d *Daemon) SetPadPath(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.padPath = path
}

// Scratchpad implements api.TransferState.
func (d *Daemon) Scratchpad() (string, time.Time) { return d.Pad().Get() }

// SetScratchpad implements api.TransferState.
func (d *Daemon) SetScratchpad(r *http.Request, text string) error {
	now := time.Now()
	if err := d.Pad().Set(text, now); err != nil {
		return err
	}
	// Best effort to the peer: the note is saved here either way, and a
	// sleeping machine picks it up when it wakes.
	if d.Conn() == nil {
		return nil
	}
	return d.sendTransfer(r.Context(), transfer.Request{
		Op: transfer.OpScratchpad, Text: text, UpdatedAt: now,
	}, nil, 0)
}

// ClipboardEnabled implements api.TransferState.
func (d *Daemon) ClipboardEnabled() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.clipboardOn
}

// SetClipboardEnabled implements api.TransferState.
func (d *Daemon) SetClipboardEnabled(on bool) {
	d.mu.Lock()
	d.clipboardOn = on
	d.mu.Unlock()
	d.Logger.Info("transfer: clipboard sync", "enabled", on)
}

// SendFile implements api.TransferState.
func (d *Daemon) SendFile(r *http.Request, name string, size int64, content io.Reader) error {
	if d.Conn() == nil {
		// No store-and-forward: a send to a sleeping machine fails visibly.
		return link.ErrNotLinked
	}
	return d.sendTransfer(r.Context(), transfer.Request{
		Op: transfer.OpFile, Name: name, Bytes: size,
	}, content, size)
}

// OpenURL implements api.TransferState.
//
// The escape hatch that matters: when an SSO session lapses on the machine
// that owns it, the sign-in URL goes across and is clicked through there.
func (d *Daemon) OpenURL(r *http.Request, url string) error {
	if d.Conn() == nil {
		return link.ErrNotLinked
	}
	return d.sendTransfer(r.Context(), transfer.Request{
		Op: transfer.OpOpenURL, Text: url,
	}, nil, 0)
}

// LaunchApp implements api.TransferState.
func (d *Daemon) LaunchApp(r *http.Request, app, path string) error {
	if d.Conn() == nil {
		return link.ErrNotLinked
	}
	return d.sendTransfer(r.Context(), transfer.Request{
		Op: transfer.OpLaunch, App: app, Path: path,
	}, nil, 0)
}

// sendTransfer opens a transfer stream and sends one request.
func (d *Daemon) sendTransfer(ctx context.Context, req transfer.Request, content io.Reader, size int64) error {
	conn := d.Conn()
	if conn == nil {
		return link.ErrNotLinked
	}
	stream, err := conn.Open(ctx, link.KindTransfer)
	if err != nil {
		return err
	}
	defer stream.Close()

	if err := transfer.WriteRequest(stream, req); err != nil {
		return err
	}
	if content != nil && size > 0 {
		if _, err := io.CopyN(stream, content, size); err != nil {
			return fmt.Errorf("transfer: send: %w", err)
		}
	}
	if err := stream.SetReadDeadline(time.Now().Add(5 * time.Minute)); err != nil {
		return err
	}
	resp, err := transfer.ReadResponse(stream)
	if err != nil {
		return err
	}
	if resp.Code != "" {
		return fmt.Errorf("transfer: %s: %s", resp.Code, resp.Message)
	}
	return nil
}

// ServeTransferStream handles an inbound file drop, handoff or note.
func (d *Daemon) ServeTransferStream(ctx context.Context, stream link.Stream) {
	defer stream.Close()

	if err := stream.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}
	req, body, err := transfer.ReadRequest(stream)
	if err != nil {
		_ = transfer.WriteResponse(stream, transfer.Response{
			Code: "bad_request", Message: err.Error(),
		})
		return
	}
	_ = stream.SetReadDeadline(time.Time{})

	switch req.Op {
	case transfer.OpFile:
		// A file arriving in the inbox is not a side effect that needs
		// approval: the PRD puts it in the "no prompt" column, because a
		// file in a folder does nothing until someone opens it.
		path, err := d.Inbox().Receive(req.Name, req.Bytes, body)
		if err != nil {
			_ = transfer.WriteResponse(stream, transfer.Response{
				Code: "internal", Message: err.Error(),
			})
			return
		}
		d.Logger.Info("transfer: received a file", "path", path)
		if d.events != nil {
			d.events.Publish(api.EventTransferChanged, map[string]string{"received": path})
		}
		_ = transfer.WriteResponse(stream, transfer.Response{Path: path})

	case transfer.OpScratchpad:
		if err := d.Pad().Set(req.Text, req.UpdatedAt); err != nil {
			_ = transfer.WriteResponse(stream, transfer.Response{
				Code: "internal", Message: err.Error(),
			})
			return
		}
		_ = transfer.WriteResponse(stream, transfer.Response{})

	case transfer.OpOpenURL, transfer.OpLaunch:
		// Handoff IS a side effect, so it goes through the permission engine
		// like anything else — and "open a browser on Desktop" is a rule you
		// approve once and never think about again.
		if err := d.guardHandoff(ctx, req); err != nil {
			code := "declined"
			if _, timedOut := err.(interface{ Timeout() bool }); timedOut {
				code = "timed_out"
			}
			_ = transfer.WriteResponse(stream, transfer.Response{
				Code: code, Message: err.Error(),
			})
			return
		}
		if err := d.performHandoff(req); err != nil {
			_ = transfer.WriteResponse(stream, transfer.Response{
				Code: "internal", Message: err.Error(),
			})
			return
		}
		_ = transfer.WriteResponse(stream, transfer.Response{})

	case transfer.OpClipboard:
		// Clipboard contents are never written to any log or trace, so
		// nothing about this is recorded beyond that it happened.
		if !d.ClipboardEnabled() {
			_ = transfer.WriteResponse(stream, transfer.Response{
				Code: "refused", Message: "clipboard sync is off on this machine",
			})
			return
		}
		if err := setClipboard(req.Text); err != nil {
			_ = transfer.WriteResponse(stream, transfer.Response{
				Code: "internal", Message: err.Error(),
			})
			return
		}
		_ = transfer.WriteResponse(stream, transfer.Response{})

	default:
		_ = transfer.WriteResponse(stream, transfer.Response{
			Code: "bad_request", Message: fmt.Sprintf("unknown transfer %q", req.Op),
		})
	}
}

// guardHandoff asks the permission engine before opening anything.
func (d *Daemon) guardHandoff(ctx context.Context, req transfer.Request) error {
	if d.perms == nil {
		return fmt.Errorf("no permission engine")
	}
	a := permission.Action{Device: d.peerOrDefault(), Provenance: "api"}
	if req.Op == transfer.OpOpenURL {
		a.Kind = permission.KindHandoff
		a.Effect = "open " + req.Text + " in the browser"
	} else {
		a.Kind = permission.KindLaunch
		a.Effect = "open " + req.App
		if req.Path != "" {
			a.Effect += " at " + req.Path
		}
	}
	_, err := d.perms.Decide(ctx, a)
	return err
}

// performHandoff opens the URL or application.
func (d *Daemon) performHandoff(req transfer.Request) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("handoff is macOS-only in v1")
	}
	// `open` is the right tool: it uses the user's default browser and their
	// GUI session, which is exactly what a root daemon could not do and the
	// reason this runs as a LaunchAgent.
	var cmd *exec.Cmd
	switch req.Op {
	case transfer.OpOpenURL:
		cmd = exec.Command("open", req.Text)
	case transfer.OpLaunch:
		if req.Path != "" {
			cmd = exec.Command("open", "-a", req.App, filepath.Clean(req.Path))
		} else {
			cmd = exec.Command("open", "-a", req.App)
		}
	default:
		return fmt.Errorf("not a handoff")
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not open: %w", err)
	}
	d.Logger.Info("transfer: handoff", "op", req.Op, "target", req.Text+req.App)
	return nil
}

// setClipboard writes to this machine's clipboard.
func setClipboard(text string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("clipboard sync is macOS-only in v1")
	}
	cmd := exec.Command("pbcopy")
	cmd.Stdin = bytes.NewReader([]byte(text))
	return cmd.Run()
}
