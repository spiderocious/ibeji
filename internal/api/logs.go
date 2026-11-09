package api

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The daemon's log, readable from the window.
//
// Diagnosing anything meant asking a person to open Terminal and tail a file
// in ~/Library/Logs. That is a fine instruction for whoever wrote the daemon
// and a poor one for anyone else, and it is the difference between a bug
// report that says what happened and one that says "it didn't work".

// logPath is where the LaunchAgent sends the daemon's stderr.
func logPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "Ibeji", "daemon.err.log"), nil
}

// RegisterLogRoutes wires the log view.
func RegisterLogRoutes(s *Server) {
	s.Handle(Route{
		Method: "GET", Path: "/logs",
		Summary: "The tail of this daemon's log, for diagnosing a failure",
	}, func(w http.ResponseWriter, r *http.Request) {
		limit := 300
		if n := r.URL.Query().Get("limit"); n != "" {
			if parsed, err := strconv.Atoi(n); err == nil && parsed > 0 {
				limit = parsed
			}
		}
		// Bounded: the window shows a tail, and handing back a log that has
		// been growing for weeks would stall it.
		if limit > 5000 {
			limit = 5000
		}
		filter := strings.ToLower(r.URL.Query().Get("q"))

		path, err := logPath()
		if err != nil {
			WriteError(w, err)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				// No log is not an error: a daemon started by hand writes to
				// the terminal instead.
				WriteJSON(w, map[string]any{"path": path, "lines": []string{}})
				return
			}
			WriteError(w, err)
			return
		}
		defer f.Close()

		// A ring buffer, so a large log costs memory proportional to what is
		// asked for rather than to the file.
		ring := make([]string, 0, limit)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if filter != "" && !strings.Contains(strings.ToLower(line), filter) {
				continue
			}
			if len(ring) == limit {
				ring = ring[1:]
			}
			ring = append(ring, line)
		}
		WriteJSON(w, map[string]any{"path": path, "lines": ring})
	})
}
