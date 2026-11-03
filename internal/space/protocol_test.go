package space

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// linked wires a manager to a client over the in-memory fabric, as the daemon
// does over a real link.
func linked(t *testing.T, m *Manager) *Client {
	t.Helper()
	fabric := link.NewMemoryFabric()
	ln, err := fabric.Listen("peer")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		for {
			stream, err := conn.Accept(ctx)
			if err != nil {
				return
			}
			go m.Serve(ctx, stream)
		}
	}()

	conn, err := fabric.Dialer("self").Dial(ctx, "peer")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return &Client{Conn: func() link.Conn { return conn }}
}

func TestProtocolManifestAndFetch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "the contents\n")
	writeFile(t, dir, "src/main.go", "package main\n")

	m := NewManager("Desktop")
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Master, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c := linked(t, m)
	ctx := context.Background()

	names, err := c.Spaces(ctx)
	if err != nil {
		t.Fatalf("Spaces: %v", err)
	}
	if len(names) != 1 || names[0] != "s" {
		t.Fatalf("Spaces = %v", names)
	}

	manifest, err := c.Manifest(ctx, "s")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if _, ok := manifest.Index()["a.txt"]; !ok {
		t.Fatalf("the manifest is missing a.txt: %+v", manifest.Entries)
	}

	body, mode, err := c.Fetch(ctx, "s", "a.txt")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(body) != "the contents\n" {
		t.Fatalf("fetched %q", body)
	}
	if mode == 0 {
		t.Error("the fetch reported no file mode")
	}
}

// The path comes from the peer, so it is checked rather than trusted: a `../`
// would otherwise read any file the user can, through a space.
func TestProtocolRefusesPathsOutsideTheSpace(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "inside.txt", "fine\n")

	// A secret next to the space, which a traversal would reach.
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("not yours\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	defer os.Remove(secret)

	m := NewManager("Desktop")
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Master, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c := linked(t, m)
	ctx := context.Background()

	for _, rel := range []string{
		"../secret.txt",
		"../../etc/hosts",
		"a/../../secret.txt",
		"/etc/hosts",
	} {
		body, _, err := c.Fetch(ctx, "s", rel)
		if err == nil {
			t.Errorf("fetching %q succeeded and returned %d bytes; a space must not read outside itself",
				rel, len(body))
		}
	}

	// And an ordinary path still works.
	if _, _, err := c.Fetch(ctx, "s", "inside.txt"); err != nil {
		t.Fatalf("a legitimate fetch was refused: %v", err)
	}
}

func TestProtocolRefusesDirectoriesAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/main.go", "package main\n")

	m := NewManager("Desktop")
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Master, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c := linked(t, m)
	ctx := context.Background()

	if _, _, err := c.Fetch(ctx, "s", "src"); err == nil {
		t.Error("fetching a directory succeeded")
	}
	if _, _, err := c.Fetch(ctx, "s", "nope.txt"); err == nil {
		t.Error("fetching a missing file succeeded")
	}
}

func TestProtocolUnknownSpace(t *testing.T) {
	m := NewManager("Desktop")
	c := linked(t, m)
	if _, err := c.Manifest(context.Background(), "nope"); err == nil {
		t.Fatal("a manifest for an unknown space succeeded")
	}
}

func TestProtocolState(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("Desktop")
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Master, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c := linked(t, m)
	ctx := context.Background()

	manifest, _ := m.ScanLocal(ctx, "s")
	_ = m.Converged("s", manifest)

	state, err := c.State(ctx, "s")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != InSync {
		t.Fatalf("state = %q, want in-sync", state)
	}
}

// With no link there is nothing to carry a sync. On a LAN that is normal.
func TestProtocolOffline(t *testing.T) {
	c := &Client{Conn: func() link.Conn { return nil }}
	if _, err := c.Manifest(context.Background(), "s"); err == nil {
		t.Fatal("a manifest succeeded with no link")
	}
}

// --- the sync loop, end to end over the link ---

// Two managers, each serving the other, converging through the real protocol
// rather than a test shortcut.
func TestSyncLoopConvergesOverTheLink(t *testing.T) {
	masterDir, replicaDir := t.TempDir(), t.TempDir()

	master := NewManager("Desktop")
	master.PeerName = func() string { return "Laptop" }
	replica := NewManager("Laptop")
	replica.PeerName = func() string { return "Desktop" }

	if _, err := master.Create(Space{Name: "s", Path: masterDir, Role: Master, Mode: TwoWay}); err != nil {
		t.Fatalf("create master: %v", err)
	}
	if _, err := replica.Create(Space{Name: "s", Path: replicaDir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create replica: %v", err)
	}

	writeFile(t, masterDir, "a.txt", "from the master\n")

	// The replica syncs from the master through the protocol.
	clientToMaster := linked(t, master)
	syncer := NewSyncer(replica, clientToMaster, nil)
	syncer.Linked = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	syncer.SyncNow(ctx, "s")

	got, err := os.ReadFile(filepath.Join(replicaDir, "a.txt"))
	if err != nil {
		t.Fatalf("the file did not arrive: %v", err)
	}
	if string(got) != "from the master\n" {
		t.Fatalf("contents = %q", got)
	}

	status, _ := replica.Status("s")
	if status.State != InSync {
		t.Fatalf("state after a clean sync = %q, want in-sync", status.State)
	}
}

// Offline is normal on a LAN — the lid is shut — so the loop reports
// unreachable and waits rather than erroring.
func TestSyncLoopHandlesAnOfflinePeer(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("Laptop")
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}

	syncer := NewSyncer(m, &Client{Conn: func() link.Conn { return nil }}, nil)
	syncer.Linked = func() bool { return false }
	syncer.SyncNow(context.Background(), "s")

	status, _ := m.Status("s")
	if status.State != Offline {
		t.Fatalf("state with a sleeping peer = %q, want offline", status.State)
	}
}

func TestSyncLoopSkipsPausedSpaces(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("Laptop")
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Replica, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Pause("s"); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	syncer := NewSyncer(m, &Client{Conn: func() link.Conn { return nil }}, nil)
	syncer.Linked = func() bool { return true }
	syncer.SyncNow(context.Background(), "s")

	status, _ := m.Status("s")
	if status.State != Paused {
		t.Fatalf("a paused space moved to %q", status.State)
	}
}

// A file larger than the limit is refused with a reason rather than attempted
// and abandoned halfway.
func TestProtocolRefusesOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("Desktop")
	if _, err := m.Create(Space{Name: "s", Path: dir, Role: Master, Mode: TwoWay}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A sparse file over the limit, without writing 2 GiB.
	big := filepath.Join(dir, "huge.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(MaxSyncFile + 1); err != nil {
		f.Close()
		t.Skipf("cannot make a sparse file here: %v", err)
	}
	f.Close()

	c := linked(t, m)
	_, _, err = c.Fetch(context.Background(), "s", "huge.bin")
	if err == nil {
		t.Fatal("an oversized file was transferred")
	}
	if !strings.Contains(err.Error(), "too_large") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}
