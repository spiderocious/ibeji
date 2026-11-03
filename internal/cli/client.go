// Package cli implements the `ibeji` command.
//
// It is a thin client over the daemon's local HTTP API and holds no logic the
// API does not have. If the CLI can do it, the window and an agent can do it
// too, because all three call the same endpoints — that is what stops any one
// surface growing private features.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to the local daemon.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

// Handoff is what the daemon publishes so local clients can find it.
type Handoff struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
}

// ErrDaemonUnreachable means the daemon is not running, or not answering.
//
// The commonest failure by far, and the one whose message has to lead
// somewhere: "run ibeji doctor" is the answer, not a connection-refused dump.
var ErrDaemonUnreachable = errors.New("the Ìbejì daemon is not running")

// Connect reads the handoff file and returns a client.
func Connect() (*Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w", err)
	}
	path := filepath.Join(home, ".ibeji", "api.json")
	if inst := os.Getenv("IBEJI_INSTANCE"); inst != "" {
		path = filepath.Join(home, ".ibeji", "instances", inst, "api.json")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w (no %s)", ErrDaemonUnreachable, path)
	}
	var h Handoff
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if h.Endpoint == "" || h.Token == "" {
		return nil, fmt.Errorf("%s is missing an endpoint or token", path)
	}
	return &Client{
		Endpoint: h.Endpoint,
		Token:    h.Token,
		// No overall timeout: exec streams for as long as the command runs.
		HTTP: &http.Client{},
	}, nil
}

// APIError is a failure the daemon reported, carrying its stable code.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Status  int    `json:"-"`
}

func (e *APIError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s\n  %s", e.Message, e.Hint)
	}
	return e.Message
}

// ExitCode maps an error to the process exit status.
//
// Distinct codes rather than a blanket 1, so a script can tell a decline from
// a sleeping peer from a daemon that is not running. 125 and up, to stay clear
// of the exit codes a remote command might itself return.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "declined":
			return 126
		case "timed_out", "exec_timeout":
			return 124
		case "peer_offline":
			return 125
		case "not_paired":
			return 127
		case "sync_conflict":
			return 75
		}
	}
	if errors.Is(err, ErrDaemonUnreachable) {
		return 69
	}
	return 1
}

// Do performs a request against the daemon.
func (c *Client) Do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	// Every call says it came from the CLI, so a command you ran and a command
	// an agent ran are never confused in the log.
	req.Header.Set("X-Ibeji-Provenance", "cli")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDaemonUnreachable, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		apiErr := &APIError{Status: resp.StatusCode}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err := json.Unmarshal(raw, apiErr); err != nil || apiErr.Message == "" {
			apiErr.Code = "unknown"
			apiErr.Message = strings.TrimSpace(string(raw))
			if apiErr.Message == "" {
				apiErr.Message = resp.Status
			}
		}
		return nil, apiErr
	}
	return resp, nil
}

// GetJSON performs a GET and decodes the result.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// PostJSON performs a POST and decodes the result.
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	resp, err := c.Do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Timeout returns a context bounded by d, or unbounded when d is zero.
func Timeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, d)
}
