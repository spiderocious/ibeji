//go:build !darwin

package ports

import "context"

// scan is unimplemented away from macOS.
//
// The PRD is macOS-first and asks that platform-specific paths degrade
// gracefully rather than crash. An empty list is the graceful answer: the port
// view shows nothing rather than the app failing to start.
func scan(ctx context.Context) ([]Port, error) { return nil, nil }
