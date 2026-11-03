//go:build darwin

package ports

import (
	"bufio"
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// Enumerating this machine's listening TCP ports.
//
// The PRD requires the daemon to be light: "port scanning and file watching
// must not measurably tax either machine". One `lsof` per scan interval is the
// cheap way to do this on macOS — the alternative, reading the kernel's socket
// table directly, needs cgo and buys a few milliseconds nobody will notice.
//
// What matters more is the interval and the caching: ports change rarely, so
// scanning every few seconds and remembering the answer costs almost nothing.

// scan lists listening TCP ports with their owning process.
func scan(ctx context.Context) ([]Port, error) {
	// -iTCP -sTCP:LISTEN  only listening TCP sockets
	// -P -n               no port-name or host-name lookups, which are slow
	// -F pcnPt            machine-readable: pid, command, name, protocol, type
	cmd := exec.CommandContext(ctx, "lsof", "-iTCP", "-sTCP:LISTEN", "-P", "-n", "-F", "pcnPt")
	out, err := cmd.Output()
	if err != nil {
		// lsof exits non-zero when nothing is listening, which is not an
		// error — it is an empty machine.
		if len(out) == 0 {
			return nil, nil
		}
	}
	return parseLsof(string(out)), nil
}

// parseLsof reads lsof's field output.
//
// Each record starts with p<pid>, then c<command>, then one or more n<address>
// lines. Parsed rather than regexed because the field format is stable and a
// regex over lsof's human output is how this breaks on a macOS update.
func parseLsof(out string) []Port {
	var (
		result  []Port
		pid     int
		command string
		isIPv4  bool
	)
	seen := map[string]bool{}

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'c':
			command = line[1:]
		case 't':
			isIPv4 = line[1:] == "IPv4"
		case 'n':
			addr := line[1:]
			port, host, ok := splitListenAddress(addr)
			if !ok {
				continue
			}
			// One process often listens on both IPv4 and IPv6 for the same
			// port. Showing it twice would be noise.
			key := strconv.Itoa(port) + "/" + command
			if seen[key] {
				continue
			}
			seen[key] = true

			result = append(result, Port{
				Port:    port,
				Process: command,
				PID:     pid,
				// Loopback-only is the interesting case: it is precisely what
				// cannot be reached from the other machine without a tunnel.
				LoopbackOnly: isLoopback(host),
				IPv4:         isIPv4,
			})
		}
	}
	return result
}

// splitListenAddress parses lsof's address forms: "*:3000", "127.0.0.1:5432",
// "[::1]:8080".
func splitListenAddress(addr string) (port int, host string, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0, "", false
	}
	host = addr[:i]
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return 0, "", false
	}
	host = strings.Trim(host, "[]")
	return p, host, true
}

func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return strings.HasPrefix(host, "127.")
}
