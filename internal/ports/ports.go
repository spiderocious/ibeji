// Package ports surveys what is listening on each machine, and forwards a
// remote port to this one.
//
// This is the differentiator's component. A dev server bound to
// 127.0.0.1:3000 on the other machine is, by design, unreachable from here —
// that is what loopback means. Port Lens makes it reachable without changing
// the bind address, touching the firewall, or typing an IP: this daemon
// listens on localhost:3000 and forwards over the link to the other daemon,
// which connects to its own loopback internally.
package ports

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Port is one listening socket.
type Port struct {
	Port    int    `json:"port"`
	Process string `json:"process"`
	PID     int    `json:"pid"`

	// Service is the recognised tool, where one is recognised. Rendered as
	// text — no vendor logos appear anywhere in this product.
	Service string `json:"service,omitempty"`

	// LoopbackOnly marks a socket bound to 127.0.0.1, which is exactly what
	// cannot be reached from the other machine without a tunnel.
	LoopbackOnly bool `json:"loopbackOnly"`

	IPv4 bool `json:"-"`

	// Device names the machine it is listening on.
	Device string `json:"device,omitempty"`
}

// HTTPLike reports whether this port probably speaks HTTP, so the UI can offer
// to open it in a browser.
func (p Port) HTTPLike() bool {
	switch p.Port {
	case 80, 443, 3000, 3001, 4000, 5000, 5173, 8000, 8080, 8081, 8443, 9000:
		return true
	}
	switch strings.ToLower(p.Service) {
	case "vite", "next.js", "webpack", "rails", "django", "flask", "nginx", "caddy":
		return true
	}
	return false
}

// services recognises well-known development ports.
//
// By port number and process name together, because port 3000 is Next.js on
// one machine and Rails on another, and guessing wrong is worse than saying
// "node on 3000". Where nothing is recognised the process name stands, which
// is always true even when it is not pretty.
var services = map[int]map[string]string{
	3000:  {"node": "Next.js", "ruby": "Rails", "": "dev server"},
	3001:  {"node": "Next.js", "": "dev server"},
	4000:  {"node": "dev server", "ruby": "Phoenix", "": "dev server"},
	5000:  {"python3": "Flask", "python": "Flask", "": "dev server"},
	5173:  {"node": "Vite", "": "Vite"},
	5432:  {"postgres": "Postgres", "": "Postgres"},
	3306:  {"mysqld": "MySQL", "": "MySQL"},
	6379:  {"redis-server": "Redis", "": "Redis"},
	8000:  {"python3": "Django", "python": "Django", "": "dev server"},
	8080:  {"java": "Java", "": "HTTP"},
	9200:  {"java": "Elasticsearch", "": "Elasticsearch"},
	27017: {"mongod": "MongoDB", "": "MongoDB"},
	5672:  {"beam.smp": "RabbitMQ", "": "RabbitMQ"},
	11211: {"memcached": "Memcached", "": "Memcached"},
}

// label names a port's service, falling back to the process name.
func label(p Port) string {
	if byProcess, ok := services[p.Port]; ok {
		if name, ok := byProcess[p.Process]; ok {
			return name
		}
		if name, ok := byProcess[""]; ok {
			return name
		}
	}
	return ""
}

// Scanner surveys this machine's ports on an interval.
//
// Cached rather than scanned per request: the port view is polled by the
// window and read by an agent, and running lsof on every read would show up
// in Activity Monitor — which is the point at which someone quits the daemon.
type Scanner struct {
	// Interval is how often the machine is rescanned.
	Interval time.Duration

	mu       sync.RWMutex
	cached   []Port
	scanned  time.Time
	scanning bool
}

// DefaultInterval is how often ports are rescanned.
//
// Ports change when someone starts or stops a server, which is rare on the
// scale of seconds. Three seconds keeps the view feeling live without the
// daemon showing up in Activity Monitor.
const DefaultInterval = 3 * time.Second

// NewScanner builds a scanner.
func NewScanner() *Scanner {
	return &Scanner{Interval: DefaultInterval}
}

// Ports returns this machine's listening ports, rescanning if the cache is
// stale.
func (s *Scanner) Ports(ctx context.Context) ([]Port, error) {
	s.mu.RLock()
	fresh := time.Since(s.scanned) < s.interval()
	cached := s.cached
	s.mu.RUnlock()
	if fresh {
		return cached, nil
	}

	found, err := scan(ctx)
	if err != nil {
		// Return the stale list rather than nothing: a port view that empties
		// because one scan failed looks like every server stopped.
		return cached, err
	}
	for i := range found {
		found[i].Service = label(found[i])
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Port < found[j].Port })

	s.mu.Lock()
	s.cached = found
	s.scanned = time.Now()
	s.mu.Unlock()
	return found, nil
}

// Age reports how stale the cached list is, so the UI can say so.
//
// Stale renders; stale does not act — a cached list is readable, but a tunnel
// button on a port that may no longer exist is disabled with a reason.
func (s *Scanner) Age() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.scanned.IsZero() {
		return 0
	}
	return time.Since(s.scanned)
}

func (s *Scanner) interval() time.Duration {
	if s.Interval <= 0 {
		return DefaultInterval
	}
	return s.Interval
}

// Combined merges both machines' ports into one list.
//
// One list of every listening port across every machine you own, each a click
// from being local — which is the thing people describe when they describe
// this product.
func Combined(local, remote []Port, localName, remoteName string) []Port {
	out := make([]Port, 0, len(local)+len(remote))
	for _, p := range local {
		p.Device = localName
		out = append(out, p)
	}
	for _, p := range remote {
		p.Device = remoteName
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Device < out[j].Device
	})
	return out
}

// Describe renders a port for a log or a CLI row.
func (p Port) Describe() string {
	name := p.Service
	if name == "" {
		name = p.Process
	}
	if name == "" {
		return fmt.Sprintf("port %d", p.Port)
	}
	return fmt.Sprintf("%s on %d", name, p.Port)
}
