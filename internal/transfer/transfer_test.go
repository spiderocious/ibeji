package transfer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- the inbox ---

func TestReceiveWritesIntoTheInbox(t *testing.T) {
	dir := t.TempDir()
	inbox := &Inbox{Dir: dir}
	content := "the file's contents\n"

	path, err := inbox.Receive("report.pdf", int64(len(content)), strings.NewReader(content))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Fatalf("contents = %q", got)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("landed in %q, want the inbox", filepath.Dir(path))
	}
}

// The name comes from the other machine, so a path like ../../.ssh/authorized_keys
// must not write outside the inbox.
func TestReceiveRefusesToEscapeTheInbox(t *testing.T) {
	dir := t.TempDir()
	inbox := &Inbox{Dir: dir}

	for _, name := range []string{
		"../escaped.txt",
		"../../.ssh/authorized_keys",
		"/etc/passwd",
		"a/b/../../../out.txt",
	} {
		path, err := inbox.Receive(name, 4, strings.NewReader("test"))
		if err != nil {
			continue // refusing outright is also fine
		}
		if filepath.Dir(path) != dir {
			t.Fatalf("a file named %q landed at %q, outside the inbox", name, path)
		}
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":          "report.pdf",
		"../../../etc/passwd": "passwd",
		"/absolute/path.txt":  "path.txt",
		"":                    "received-file",
		"..":                  "received-file",
		"with\x00null.txt":    "withnull.txt",
		"with\nnewline.txt":   "withnewline.txt",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	// And it stays a usable filename length.
	long := strings.Repeat("a", 500) + ".txt"
	if got := safeName(long); len(got) > 210 {
		t.Errorf("safeName left a %d-character name", len(got))
	}
}

// Two drops of report.pdf are two files, not one lost.
func TestReceiveNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	inbox := &Inbox{Dir: dir}

	first, err := inbox.Receive("report.pdf", 5, strings.NewReader("first"))
	if err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	second, err := inbox.Receive("report.pdf", 6, strings.NewReader("second"))
	if err != nil {
		t.Fatalf("second Receive: %v", err)
	}
	if first == second {
		t.Fatal("the second drop overwrote the first")
	}
	got, _ := os.ReadFile(first)
	if string(got) != "first" {
		t.Fatalf("the first file now holds %q", got)
	}
}

// A partial download must never be visible at the real path: someone
// double-clicking a half-arrived file is a confusing failure.
func TestReceiveRejectsAShortTransfer(t *testing.T) {
	dir := t.TempDir()
	inbox := &Inbox{Dir: dir}

	// Claims 100 bytes, supplies 5.
	_, err := inbox.Receive("truncated.bin", 100, strings.NewReader("short"))
	if err == nil {
		t.Fatal("Receive accepted a short transfer")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".ibeji-drop-") {
			t.Errorf("a failed transfer left %q behind", e.Name())
		}
	}
}

func TestReceiveRefusesOversizedFiles(t *testing.T) {
	inbox := &Inbox{Dir: t.TempDir()}
	if _, err := inbox.Receive("huge.bin", MaxFileSize+1, strings.NewReader("")); err == nil {
		t.Fatal("Receive accepted a file over the limit")
	}
}

// --- the scratchpad ---

func TestScratchpadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.md")
	pad := NewScratchpad(path)

	text, at := pad.Get()
	if text != "" || !at.IsZero() {
		t.Fatalf("a new scratchpad is not empty: %q", text)
	}

	now := time.Now()
	if err := pad.Set("## tuesday\n- check the barrier\n", now); err != nil {
		t.Fatalf("Set: %v", err)
	}
	text, at = pad.Get()
	if !strings.Contains(text, "tuesday") {
		t.Fatalf("text = %q", text)
	}
	if at.IsZero() {
		t.Error("the scratchpad records no update time")
	}

	// It survives a restart.
	again := NewScratchpad(path)
	text, _ = again.Get()
	if !strings.Contains(text, "tuesday") {
		t.Fatalf("the note did not survive a restart: %q", text)
	}
}

// Last writer wins per save: an older save arriving late must not undo a
// newer one.
func TestScratchpadIgnoresAnOlderSave(t *testing.T) {
	pad := NewScratchpad(filepath.Join(t.TempDir(), "pad.md"))

	now := time.Now()
	if err := pad.Set("the newer version", now); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := pad.Set("an older version arriving late", now.Add(-time.Minute)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	text, _ := pad.Get()
	if text != "the newer version" {
		t.Fatalf("an older save overwrote a newer one: %q", text)
	}
}

// --- framing ---

// The decoder reads ahead, so content following a request frame has to start
// from what it buffered — the same trap the remote protocol hit, where every
// file write hung forever.
func TestRequestFramingDoesNotSwallowContent(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRequest(&buf, Request{Op: OpFile, Name: "a.txt", Bytes: 11}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	buf.WriteString("hello world")

	req, body, err := ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Op != OpFile || req.Name != "a.txt" {
		t.Fatalf("request = %+v", req)
	}
	content := make([]byte, 11)
	if _, err := body.Read(content); err != nil {
		t.Fatalf("read content: %v", err)
	}
	if string(content) != "hello world" {
		t.Fatalf("content = %q; the decoder swallowed it", content)
	}
}

func TestResponseFraming(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteResponse(&buf, Response{Path: "/inbox/a.txt"}); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	resp, err := ReadResponse(&buf)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.Path != "/inbox/a.txt" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestInboxDefaultsUnderHome(t *testing.T) {
	// The default has to be somewhere a person will look.
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := (&Inbox{}).Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if !strings.HasPrefix(path, home) {
		t.Fatalf("the default inbox is at %q, outside the home directory", path)
	}
	if !strings.Contains(path, "Ibeji") {
		t.Fatalf("the default inbox %q is not obviously Ìbejì's", path)
	}
}
