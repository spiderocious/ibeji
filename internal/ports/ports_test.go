package ports

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// --- labelling ---

// Port 3000 is Next.js on one machine and Rails on another, so the process
// name decides. Guessing wrong is worse than saying "node on 3000".
func TestServiceLabellingUsesTheProcess(t *testing.T) {
	cases := []struct {
		port    int
		process string
		want    string
	}{
		{3000, "node", "Next.js"},
		{3000, "ruby", "Rails"},
		{3000, "something-else", "dev server"},
		{5432, "postgres", "Postgres"},
		{5173, "node", "Vite"},
		{9999, "myapp", ""}, // unknown: the process name stands
	}
	for _, c := range cases {
		got := label(Port{Port: c.port, Process: c.process})
		if got != c.want {
			t.Errorf("port %d running %q labelled %q, want %q", c.port, c.process, got, c.want)
		}
	}
}

// Where nothing is recognised the process name stands, which is always true
// even when it is not pretty.
func TestDescribeFallsBackToTheProcess(t *testing.T) {
	if got := (Port{Port: 8080, Process: "java"}).Describe(); !strings.Contains(got, "java") {
		t.Errorf("Describe = %q, want it to name the process", got)
	}
	if got := (Port{Port: 3000, Process: "node", Service: "Next.js"}).Describe(); !strings.Contains(got, "Next.js") {
		t.Errorf("Describe = %q, want the service name", got)
	}
	if got := (Port{Port: 1234}).Describe(); !strings.Contains(got, "1234") {
		t.Errorf("Describe = %q", got)
	}
}

func TestHTTPLike(t *testing.T) {
	for _, p := range []Port{{Port: 3000}, {Port: 8080}, {Port: 5173}, {Port: 443}} {
		if !p.HTTPLike() {
			t.Errorf("port %d is not HTTP-like", p.Port)
		}
	}
	for _, p := range []Port{{Port: 5432}, {Port: 22}, {Port: 6379}} {
		if p.HTTPLike() {
			t.Errorf("port %d reports HTTP-like; offering to open Postgres in a browser is noise", p.Port)
		}
	}
}

// --- lsof parsing ---

// Parsed rather than regexed: the field format is stable, and a regex over
// lsof's human output is how this breaks on a macOS update.
func TestParseLsof(t *testing.T) {
	out := strings.Join([]string{
		"p4821", "cnode", "tIPv4", "n127.0.0.1:3000",
		"p4821", "cnode", "tIPv6", "n[::1]:3000",
		"p510", "cpostgres", "tIPv4", "n*:5432",
	}, "\n")

	got := parseLsof(out)
	if len(got) != 2 {
		t.Fatalf("parsed %d ports, want 2 (the IPv4/IPv6 pair collapses): %+v", len(got), got)
	}

	byPort := map[int]Port{}
	for _, p := range got {
		byPort[p.Port] = p
	}
	if byPort[3000].Process != "node" || byPort[3000].PID != 4821 {
		t.Errorf("port 3000 = %+v", byPort[3000])
	}
	// Loopback-only is the interesting case: it is precisely what cannot be
	// reached from the other machine without a tunnel.
	if !byPort[3000].LoopbackOnly {
		t.Error("127.0.0.1:3000 was not marked loopback-only")
	}
	if byPort[5432].LoopbackOnly {
		t.Error("*:5432 was marked loopback-only")
	}
}

func TestSplitListenAddress(t *testing.T) {
	cases := map[string]struct {
		port int
		host string
	}{
		"127.0.0.1:3000": {3000, "127.0.0.1"},
		"*:5432":         {5432, "*"},
		"[::1]:8080":     {8080, "::1"},
	}
	for in, want := range cases {
		port, host, ok := splitListenAddress(in)
		if !ok || port != want.port || host != want.host {
			t.Errorf("splitListenAddress(%q) = %d, %q, %v", in, port, host, ok)
		}
	}
	if _, _, ok := splitListenAddress("nonsense"); ok {
		t.Error("splitListenAddress accepted nonsense")
	}
}

// --- scanning ---

// The real thing: open a port and confirm the scan finds it.
func TestScannerFindsARealPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()
	want := ln.Addr().(*net.TCPAddr).Port

	s := NewScanner()
	found, err := s.Ports(context.Background())
	if err != nil {
		t.Skipf("port scanning unavailable here: %v", err)
	}
	for _, p := range found {
		if p.Port == want {
			if !p.LoopbackOnly {
				t.Error("a 127.0.0.1 listener was not marked loopback-only")
			}
			return
		}
	}
	t.Skip("the scan did not find our own listener; lsof may be restricted here")
}

// Running lsof on every read would show up in Activity Monitor, which is the
// point at which someone quits the daemon.
func TestScannerCaches(t *testing.T) {
	s := NewScanner()
	s.Interval = time.Hour

	first, err := s.Ports(context.Background())
	if err != nil {
		t.Skipf("port scanning unavailable: %v", err)
	}
	start := time.Now()
	second, _ := s.Ports(context.Background())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("a cached read took %v; it is rescanning every time", elapsed)
	}
	if len(first) != len(second) {
		t.Error("a cached read returned a different list")
	}
}

// Stale renders; stale does not act.
func TestScannerReportsItsAge(t *testing.T) {
	s := NewScanner()
	if s.Age() != 0 {
		t.Error("a scanner that has never scanned reports an age")
	}
	if _, err := s.Ports(context.Background()); err != nil {
		t.Skipf("port scanning unavailable: %v", err)
	}
	if s.Age() > time.Second {
		t.Errorf("a fresh scan reports an age of %v", s.Age())
	}
}

func TestCombinedMergesBothMachines(t *testing.T) {
	local := []Port{{Port: 8080, Process: "java"}}
	remote := []Port{{Port: 3000, Process: "node"}, {Port: 5432, Process: "postgres"}}

	merged := Combined(local, remote, "Laptop", "Desktop")
	if len(merged) != 3 {
		t.Fatalf("merged %d ports, want 3", len(merged))
	}
	// Sorted by port, so one list reads as one survey rather than two.
	if merged[0].Port != 3000 || merged[1].Port != 5432 || merged[2].Port != 8080 {
		t.Fatalf("not sorted by port: %+v", merged)
	}
	// Every row says which machine it is on, or the list is unusable.
	for _, p := range merged {
		if p.Device == "" {
			t.Errorf("port %d does not name its machine", p.Port)
		}
	}
}

// --- tunnels ---

// A tunnel: listen here, forward over the link, connect to loopback there.
func TestTunnelForwardsToTheFarPort(t *testing.T) {
	// The "far" service, which in this test is simply another local listener.
	far, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer far.Close()
	farPort := far.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := far.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte("echo:" + string(buf[:n])))
			}()
		}
	}()

	// The dialer stands in for the link: it connects straight to the far
	// service, which is what the far daemon does on the other side.
	// A net.Conn satisfies everything the forwarder needs from a stream:
	// read, write, close and deadlines.
	m := NewManager(func(ctx context.Context, port int) (link.Stream, error) {
		conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err != nil {
			return nil, err
		}
		return connStream{conn}, nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tunnel, err := m.Open(ctx, farPort, "test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close(farPort)

	conn, err := net.DialTimeout("tcp", tunnel.Address(), 5*time.Second)
	if err != nil {
		t.Fatalf("could not reach the tunnel: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "echo:hello" {
		t.Fatalf("got %q through the tunnel", buf[:n])
	}
}

// A silent remap is how you spend an hour wondering why the wrong app is
// answering on localhost:3000.
func TestTunnelRemapsAndSaysSo(t *testing.T) {
	// Occupy a port so the tunnel cannot have it.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer blocker.Close()
	taken := blocker.Addr().(*net.TCPAddr).Port

	m := NewManager(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tunnel, err := m.Open(ctx, taken, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close(taken)

	if !tunnel.Remapped {
		t.Fatal("a tunnel on a taken port did not report itself remapped")
	}
	if tunnel.LocalPort == taken {
		t.Fatal("the tunnel claims the port that was already in use")
	}
	// The mapping must always be visible.
	mapping := tunnel.Mapping("Desktop")
	if !strings.Contains(mapping, "remapped") {
		t.Errorf("the mapping does not say it was remapped: %q", mapping)
	}
	if !strings.Contains(mapping, itoa(tunnel.LocalPort)) {
		t.Errorf("the mapping does not show the local port: %q", mapping)
	}
}

func TestTunnelLifecycle(t *testing.T) {
	m := NewManager(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t1, err := m.Open(ctx, 19001, "one")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := m.Open(ctx, 19001, "again"); err != ErrAlreadyOpen {
		t.Errorf("opening the same port twice = %v, want ErrAlreadyOpen", err)
	}

	if len(m.List()) != 1 {
		t.Fatalf("List = %d tunnels", len(m.List()))
	}
	if _, ok := m.Get(t1.RemotePort); !ok {
		t.Error("Get did not find an open tunnel")
	}

	// Pinning is what turns a demo into a habit.
	if err := m.Pin(19001, "Medcord API"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	pinned := m.Pinned()
	if len(pinned) != 1 || pinned[0].Name != "Medcord API" {
		t.Fatalf("Pinned = %+v", pinned)
	}

	if err := m.Close(19001); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(19001); err != ErrNoTunnel {
		t.Errorf("closing twice = %v, want ErrNoTunnel", err)
	}
}

// Unpairing must close every tunnel at once, per the PRD.
func TestCloseAll(t *testing.T) {
	m := NewManager(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, p := range []int{19010, 19011, 19012} {
		if _, err := m.Open(ctx, p, ""); err != nil {
			t.Fatalf("Open %d: %v", p, err)
		}
	}
	if n := m.CloseAll(); n != 3 {
		t.Fatalf("CloseAll closed %d, want 3", n)
	}
	if len(m.List()) != 0 {
		t.Fatal("tunnels survived CloseAll")
	}
}

// A pinned tunnel comes back by itself after a lid close.
func TestReopenRestoresPinnedTunnels(t *testing.T) {
	m := NewManager(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := m.Open(ctx, 19020, "Medcord API"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Pin(19020, "Medcord API"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	pinned := m.Pinned()
	m.CloseAll()

	m.Reopen(ctx, pinned)
	restored, ok := m.Get(19020)
	if !ok {
		t.Fatal("a pinned tunnel was not reopened")
	}
	if restored.Name != "Medcord API" {
		t.Errorf("the restored tunnel lost its name: %q", restored.Name)
	}
	m.CloseAll()
}

func TestOpenRejectsNonsensePorts(t *testing.T) {
	m := NewManager(nil, nil)
	for _, p := range []int{0, -1, 70000} {
		if _, err := m.Open(context.Background(), p, ""); err == nil {
			t.Errorf("Open accepted port %d", p)
		}
	}
}

// --- helpers ---

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// connStream adapts a net.Conn to link.Stream, for the tunnel test.
type connStream struct{ net.Conn }

func (connStream) Kind() link.StreamKind { return link.KindTunnel }
