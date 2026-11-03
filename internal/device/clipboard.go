package device

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"time"

	"github.com/feranmi/ibeji/internal/transfer"
)

// Clipboard sync.
//
// Off by default and visibly indicated while on, because a shared clipboard
// carries passwords and whatever you last copied out of a password manager.
// Having it on by default would mean every copy on a corporate machine
// crossing to an unmanaged one without anyone deciding.
//
// Contents are never written to any log, trace or history. That is enforced in
// the trace recorder rather than promised here, but it is the reason nothing
// in this file logs what it moved.

// clipboardPoll is how often the clipboard is checked while sync is on.
//
// macOS has no change notification for the pasteboard, so polling is the only
// option. Half a second feels instant to a person and costs one cheap syscall;
// the loop does not run at all while sync is off.
const clipboardPoll = 500 * time.Millisecond

// WatchClipboard sends local clipboard changes to the peer while sync is on.
func (d *Daemon) WatchClipboard(ctx context.Context) {
	if runtime.GOOS != "darwin" {
		return
	}

	ticker := time.NewTicker(clipboardPoll)
	defer ticker.Stop()

	var last string
	primed := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if !d.ClipboardEnabled() || d.Conn() == nil {
			// Forget what we saw: turning sync back on should not
			// immediately push whatever was copied while it was off.
			primed = false
			last = ""
			continue
		}

		text, err := readClipboard()
		if err != nil || text == "" {
			continue
		}
		if !primed {
			// The first read after turning sync on is the existing contents,
			// which the peer did not ask for.
			last = text
			primed = true
			continue
		}
		if text == last {
			continue
		}
		last = text

		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = d.sendTransfer(sendCtx, transfer.Request{
			Op: transfer.OpClipboard, Text: text,
		}, nil, 0)
		cancel()
		if err != nil {
			// Nothing about the contents is logged, ever — not even a length.
			d.Logger.Debug("transfer: clipboard sync failed")
		}
	}
}

// readClipboard reads this machine's clipboard.
func readClipboard() (string, error) {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return "", err
	}
	return string(bytes.TrimRight(out, "\n")), nil
}
