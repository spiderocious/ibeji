package device

import (
	"strings"
	"testing"
)

// The text comes from the other machine, so a quote in it would otherwise end
// the AppleScript string and the rest would be executed as script.
func TestAppleScriptQuotingIsSafe(t *testing.T) {
	cases := []struct {
		in      string
		mustNot string
	}{
		{`say "hello"`, `" & do shell script`},
		{`"); do shell script "rm -rf ~"; display notification ("`, `do shell script "rm`},
		{"back\\slash", ""},
	}
	for _, c := range cases {
		got := quoteAppleScript(c.in)
		// The result is one quoted literal: it opens and closes with a quote,
		// and every quote inside is escaped.
		if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
			t.Errorf("quoteAppleScript(%q) = %q, not a quoted literal", c.in, got)
			continue
		}
		inner := got[1 : len(got)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == '"' {
				// Must be escaped: count the backslashes before it.
				slashes := 0
				for j := i - 1; j >= 0 && inner[j] == '\\'; j-- {
					slashes++
				}
				if slashes%2 == 0 {
					t.Errorf("quoteAppleScript(%q) = %q leaves an unescaped quote", c.in, got)
					break
				}
			}
		}
	}
}

// A newline ends an osascript -e line, and AppleScript has no escape for one
// inside a literal.
func TestAppleScriptQuotingRemovesNewlines(t *testing.T) {
	got := quoteAppleScript("line one\nline two\rline three")
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("quoteAppleScript left a newline in %q", got)
	}
}

// A notification is a nudge, not a document: an unbounded body would be
// rejected by the OS or truncated unpredictably.
func TestAppleScriptQuotingBoundsLength(t *testing.T) {
	got := quoteAppleScript(strings.Repeat("a", 5000))
	if len(got) > 600 {
		t.Fatalf("quoteAppleScript returned %d characters", len(got))
	}
}

// Off by default: a shared clipboard carries whatever you last copied out of
// a password manager, and having it on without anyone deciding would mean
// every copy on a corporate machine crossing to an unmanaged one.
func TestClipboardSyncIsOffByDefault(t *testing.T) {
	d := newDaemon(t, "Laptop")
	if d.ClipboardEnabled() {
		t.Fatal("clipboard sync is on by default")
	}
	d.SetClipboardEnabled(true)
	if !d.ClipboardEnabled() {
		t.Fatal("SetClipboardEnabled(true) did not take")
	}
	d.SetClipboardEnabled(false)
	if d.ClipboardEnabled() {
		t.Fatal("SetClipboardEnabled(false) did not take")
	}
}

// Notifications are on by default, because they carry the product's own
// events: a sync conflict you are not told about is one you find out about
// much later.
func TestNotificationMirroringIsOnByDefault(t *testing.T) {
	d := newDaemon(t, "Laptop")
	if !d.NotificationsEnabled() {
		t.Fatal("notification mirroring is off by default")
	}
	d.SetNotificationsEnabled(false)
	if d.NotificationsEnabled() {
		t.Fatal("SetNotificationsEnabled(false) did not take")
	}
}
