package cli

import (
	"fmt"
	"strings"
)

// Target is a device and optionally a path on it: `desktop:~/projects`.
//
// The shape is borrowed from scp, which is what people already have in their
// fingers for "a path on another machine". A bare device name is a target too,
// for commands that act on the machine rather than a path.
type Target struct {
	Device string
	Path   string
}

// ParseTarget splits `device:path`, `device:` or `device`.
//
// A Windows-style drive letter is not a concern here — the product is macOS
// first and paths are POSIX — but an absolute path with no device is, because
// `ibeji cat /etc/hosts` is a natural thing to type and must fail with advice
// rather than being read as a device called "/etc/hosts".
func ParseTarget(s string) (Target, error) {
	if s == "" {
		return Target{}, fmt.Errorf("expected a device, like `desktop` or `desktop:~/projects`")
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".") {
		return Target{}, fmt.Errorf(
			"%q looks like a local path. Say which machine: `desktop:%s`", s, s)
	}

	device, path, found := strings.Cut(s, ":")
	if device == "" {
		return Target{}, fmt.Errorf("no device in %q", s)
	}
	if !found {
		return Target{Device: device}, nil
	}
	return Target{Device: device, Path: path}, nil
}

// PathOr returns the target's path, or a fallback when it has none.
func (t Target) PathOr(fallback string) string {
	if t.Path == "" {
		return fallback
	}
	return t.Path
}

// String renders the target as it was typed.
func (t Target) String() string {
	if t.Path == "" {
		return t.Device
	}
	return t.Device + ":" + t.Path
}
