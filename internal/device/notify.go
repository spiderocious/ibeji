package device

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/feranmi/ibeji/internal/transfer"
)

// Mirroring Ìbejì's own events to the other machine as notifications.
//
// Only its own events: a sync conflict, a finished command, an incoming
// prompt, an arrival in the inbox. Mirroring OTHER apps' notifications is
// deferred — on macOS it is a permissions tar pit that would dominate the
// module, and the PRD cuts it for that reason.

// Notify sends a toast to the paired device.
func (d *Daemon) Notify(ctx context.Context, title, body string) {
	if d.Conn() == nil {
		return
	}
	if !d.NotificationsEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = d.sendTransfer(ctx, transfer.Request{
		Op: transfer.OpNotify, Title: title, Body: body,
	}, nil, 0)
}

// NotificationsEnabled reports whether mirroring is on. On by default: these
// are the product's own events, and a sync conflict you are not told about is
// a sync conflict you find out about much later.
func (d *Daemon) NotificationsEnabled() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return !d.notificationsOff
}

// SetNotificationsEnabled turns mirroring on or off, per pair.
func (d *Daemon) SetNotificationsEnabled(on bool) {
	d.mu.Lock()
	d.notificationsOff = !on
	d.mu.Unlock()
}

// showNotification displays a toast on this machine.
//
// osascript rather than a notification framework: it needs no bundle identity
// and no entitlement, and it works from a LaunchAgent in the user's session —
// which is exactly the placement the whole product depends on.
func showNotification(title, body string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	script := "display notification " + quoteAppleScript(body) +
		" with title " + quoteAppleScript(title)
	return exec.Command("osascript", "-e", script).Run()
}

// quoteAppleScript escapes a string for AppleScript.
//
// The text comes from the other machine, so a quote in it would otherwise end
// the string and the rest would be executed as script.
func quoteAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	// Newlines end an osascript -e line; AppleScript has no escape for one
	// inside a literal, so they become spaces.
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return `"` + s + `"`
}
