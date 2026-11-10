package device

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/feranmi/ibeji/internal/link"
)

// Finding the other Mac when it has moved.
//
// mDNS is the intended way to find a peer, and when it works none of this is
// needed. But plenty of routers block multicast, guest networks isolate
// clients, and some Wi-Fi drivers simply drop it — so the product has always
// had "pair by IP address" as the fallback. That fallback has a flaw nobody
// noticed until it bit: an IP address is written down by a human, and a
// router hands out a different one overnight.
//
// The failure that cost a day looked like this. Two Macs paired fine on
// 192.168.0.x. The network moved to 192.168.1.x. The other Mac came back as
// 192.168.1.39; the address in hand still said 192.168.1.147. Every attempt
// timed out, the window said "the paired device is not reachable", and that
// sentence sent the user hunting a macOS permission problem that did not
// exist. A sweep of the 254 addresses on the subnet found the machine in under
// two seconds.
//
// So the daemon does the sweep itself. It is a small, bounded, local-only
// probe: open a TCP connection to one port on the addresses of the subnet this
// machine is already on, and report which ones answer. Nothing is sent, no
// identity is offered, and nothing is trusted as a result — a machine that
// answers on 7749 is a candidate to be paired with, and the six-digit ceremony
// still gates it.

// SweepResult is one address that answered.
type SweepResult struct {
	Address string `json:"address"`
	// Name is what the machine called itself, when it would say. A sweep that
	// returns three bare IPs makes a person guess; one that names them does
	// not.
	Name string `json:"name,omitempty"`
	// Self marks this machine, which always answers its own port and would
	// otherwise look like a candidate.
	Self bool `json:"self,omitempty"`
	// Ibeji reports whether the thing that answered spoke Ìbejì's handshake.
	// Something else may hold the port; saying which is which saves a
	// pointless pairing attempt.
	Ibeji bool `json:"ibeji"`
}

// SweepView is the result of a sweep, as the API serves it.
type SweepView struct {
	Subnet  string        `json:"subnet"`
	Port    int           `json:"port"`
	Scanned int           `json:"scanned"`
	Took    string        `json:"took"`
	Found   []SweepResult `json:"found"`
	// Hint is what to do with what came back, in a sentence, since the right
	// next move differs between "found exactly one other machine" and "found
	// nothing at all".
	Hint string `json:"hint"`
}

// sweepTimeout bounds one probe.
//
// On a LAN a host that is up answers in single-digit milliseconds, and one
// that is absent never answers at all; the only thing this value trades is how
// long the absent ones take to give up. 400ms is comfortably above any real
// LAN round trip and keeps a full /24 sweep inside two seconds at the
// concurrency below.
const sweepTimeout = 400 * time.Millisecond

// sweepConcurrency bounds how many probes are in flight.
//
// 64 keeps a /24 to four rounds of 400ms. Higher starts to look like a port
// scan to network gear and risks exhausting file descriptors for nothing.
const sweepConcurrency = 64

// Sweep probes this machine's subnet for daemons.
//
// Deliberately limited to the /24 around this machine's own address. A general
// network scanner is not what this is: it answers one question — "the Mac I
// paired with is on this network somewhere, where?" — and the answer is
// always on the same subnet, because that is the only place the rest of the
// product can reach it anyway.
func (d *Daemon) Sweep(ctx context.Context) (*SweepView, error) {
	started := time.Now()
	port := d.peerPort()
	if port == 0 {
		port = link.DefaultPort
	}

	self, ipnet, err := primaryIPv4()
	if err != nil {
		return nil, err
	}
	hosts := hostsOf(self, ipnet)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("device: this Mac's network has no other addresses to look at")
	}

	d.Logger.Info("sweep: starting", "subnet", ipnet.String(), "port", port, "hosts", len(hosts))

	var (
		mu    sync.Mutex
		found []SweepResult
		wg    sync.WaitGroup
	)
	gate := make(chan struct{}, sweepConcurrency)

	for _, h := range hosts {
		select {
		case <-ctx.Done():
			// A cancelled sweep returns what it has. The window may have
			// closed the modal, and a partial answer beats an error.
		default:
		}
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()

			addr := net.JoinHostPort(ip, fmt.Sprint(port))
			dialer := net.Dialer{Timeout: sweepTimeout}
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				return
			}
			_ = conn.Close()

			r := SweepResult{Address: addr, Self: ip == self.String()}
			// Never shake hands with ourselves.
			//
			// probeName dials in pairing mode, and the far end cannot tell a
			// probe from an intent to pair: when the sweep reached this
			// machine's own listener, the accept loop logged "pairing started
			// by the other machine" and raised a ceremony against itself. A
			// closed TCP connection is all the evidence needed that this
			// machine is up, and we already know its name.
			if r.Self {
				r.Name = d.store.Name()
				r.Ibeji = true
				mu.Lock()
				found = append(found, r)
				mu.Unlock()
				return
			}
			// Ask it who it is. Probing a port tells you something is there;
			// the name is what lets a person recognise their own machine.
			if name, ok := d.probeName(ctx, addr); ok {
				r.Name = name
				r.Ibeji = true
			}
			mu.Lock()
			found = append(found, r)
			mu.Unlock()
		}(h)
	}
	wg.Wait()

	sort.Slice(found, func(i, j int) bool {
		// This machine last: it is never the answer being looked for.
		if found[i].Self != found[j].Self {
			return found[j].Self
		}
		return lessIP(found[i].Address, found[j].Address)
	})

	v := &SweepView{
		Subnet:  ipnet.String(),
		Port:    port,
		Scanned: len(hosts),
		Took:    time.Since(started).Round(time.Millisecond).String(),
		Found:   found,
	}

	others := 0
	for _, f := range found {
		if !f.Self {
			others++
		}
	}
	switch {
	case others == 0:
		v.Hint = fmt.Sprintf("Nothing else on %s is listening on port %d. "+
			"Check that Ìbejì is open on the other Mac, and that both machines are on this same network — "+
			"a phone hotspot or a guest network puts them on different ones.", ipnet.String(), port)
	case others == 1:
		for _, f := range found {
			if !f.Self {
				who := f.Name
				if who == "" {
					who = "A machine"
				}
				v.Hint = fmt.Sprintf("%s is listening at %s. That is almost certainly your other Mac — pair with it.", who, f.Address)
			}
		}
	default:
		v.Hint = fmt.Sprintf("%d machines are listening on port %d. Pair with the one whose name you recognise.", others, port)
	}

	d.Logger.Info("sweep: done", "found", len(found), "others", others, "took", v.Took)
	return v, nil
}

// probeName asks an address what it calls itself.
//
// Uses the same pairing-mode dial as the ceremony — no key pinned, nothing
// trusted — purely to read the name out of the hello. Failure is ordinary and
// not worth reporting: the thing on that port may not be Ìbejì at all, which
// is itself the useful answer.
func (d *Daemon) probeName(ctx context.Context, addr string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	transport := &link.TCPTransport{Identity: d.store.Self(), Name: d.store.Name()}
	conn, err := transport.Dial(ctx, addr)
	if err != nil {
		return "", false
	}
	defer conn.Close()

	for attempt := 0; attempt < 3; attempt++ {
		name, _, err := exchangeHello(ctx, conn, d.store.Name(), false, d.peerPort())
		if err == nil {
			return name, true
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	// It completed a TLS handshake, so it is an Ìbejì daemon even though it
	// would not say its name in time.
	return "", true
}

// primaryIPv4 finds this machine's own LAN address and the network around it.
func primaryIPv4() (net.IP, *net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Point-to-point interfaces are VPNs and tunnels. Sweeping those is
		// both slow and pointless: the peer is on the LAN.
		if iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4, ipnet, nil
		}
	}
	return nil, nil, fmt.Errorf("device: this Mac has no IPv4 address on a local network")
}

// hostsOf lists the addresses to probe.
//
// Capped at a /24's worth. A /16 would be 65,534 probes, which is a port scan,
// takes minutes and is not what anybody wants — so a wider mask is narrowed to
// the 254 addresses around this machine, which is where a peer on the same
// network actually is.
func hostsOf(self net.IP, ipnet *net.IPNet) []string {
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil
	}
	if ones < 24 {
		ones = 24
	}
	mask := net.CIDRMask(ones, 32)
	base := self.Mask(mask)

	size := 1 << uint(32-ones)
	out := make([]string, 0, size)
	for i := 1; i < size-1; i++ {
		ip := make(net.IP, 4)
		copy(ip, base)
		v := uint32(i)
		ip[3] |= byte(v)
		ip[2] |= byte(v >> 8)
		ip[1] |= byte(v >> 16)
		ip[0] |= byte(v >> 24)
		out = append(out, ip.String())
	}
	return out
}

// lessIP orders addresses numerically, so .9 comes before .10.
func lessIP(a, b string) bool {
	ha, _, err := net.SplitHostPort(a)
	if err != nil {
		ha = a
	}
	hb, _, err := net.SplitHostPort(b)
	if err != nil {
		hb = b
	}
	ia, ib := net.ParseIP(ha).To4(), net.ParseIP(hb).To4()
	if ia == nil || ib == nil {
		return ha < hb
	}
	for i := 0; i < 4; i++ {
		if ia[i] != ib[i] {
			return ia[i] < ib[i]
		}
	}
	return false
}
