// Package surface describes Ìbejì's programmable surface, once.
//
// One definition generates three things: `ibeji help --json`, the MCP server's
// tool descriptions, and the human help text. Hand-written tool descriptions
// drift from the API within two weeks, and a drifted description is worse than
// a missing one — because the agent trusts it and acts on it.
//
// This exists because the first user of this API is an agent, and an agent
// cannot read a README or ask a colleague. Everything it needs to call a tool
// correctly the first time has to be here: parameters with types and defaults,
// an example request, an example response, and every error code the call can
// return.
package surface

import (
	"encoding/json"
	"sort"
)

// Kind is a parameter's type, in the vocabulary a JSON schema uses.
type Kind string

const (
	String Kind = "string"
	Int    Kind = "integer"
	Bool   Kind = "boolean"
	Object Kind = "object"
	Array  Kind = "array"
)

// Param is one input to a call.
type Param struct {
	Name     string `json:"name"`
	Type     Kind   `json:"type"`
	Required bool   `json:"required,omitempty"`

	// Default is what happens when it is omitted. Stated rather than implied,
	// because "what if I leave this out" is the first question anyone has.
	Default any `json:"default,omitempty"`

	// Description says what it is for. One sentence; an agent reads hundreds.
	Description string `json:"description"`

	// Note carries the non-obvious part — the thing that would otherwise be
	// learned by getting it wrong.
	Note string `json:"note,omitempty"`
}

// ErrorCode is a failure a call can return, and what to do about it.
type ErrorCode struct {
	Code string `json:"code"`
	When string `json:"when"`

	// Do says how a caller should respond. An agent that sees "declined"
	// should stop; one that sees "peer_offline" should retry later. Spelling
	// that out here is the difference between an agent that behaves sensibly
	// and one that retries a refusal three times.
	Do string `json:"do"`
}

// Call is one operation on the surface.
type Call struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`

	// Endpoint is the HTTP route behind it.
	Endpoint string `json:"endpoint"`
	Method   string `json:"method"`

	// Mutating marks a side effect, which passes through the permission
	// engine on the far machine.
	Mutating bool `json:"mutating,omitempty"`

	Params  []Param     `json:"params,omitempty"`
	Returns any         `json:"returns,omitempty"`
	Errors  []ErrorCode `json:"errors,omitempty"`

	// Example is a request that works, so an agent has a shape to copy
	// rather than one to infer.
	Example any `json:"example,omitempty"`

	// Notes carry what cannot be read off the signature.
	Notes []string `json:"notes,omitempty"`

	// CLI is the equivalent command, for a human reading the same document.
	CLI string `json:"cli,omitempty"`
}

// Document is the whole surface.
type Document struct {
	Product string `json:"product"`
	Version string `json:"version"`

	// About explains what the product is for, in two sentences, because an
	// agent handed a tool list with no context guesses at the rest.
	About string `json:"about"`

	Calls []Call `json:"calls"`

	// Concepts explains the handful of ideas a caller must hold to use the
	// surface correctly.
	Concepts map[string]string `json:"concepts"`
}

// Common error codes, shared by most calls.
var (
	ErrDeclined = ErrorCode{
		Code: "declined",
		When: "The person at the other machine refused this action.",
		Do:   "Stop. Tell the user it was declined; do not retry.",
	}
	ErrTimedOut = ErrorCode{
		Code: "timed_out",
		When: "Nobody answered the permission prompt within 60 seconds.",
		Do: "Nobody said no — nobody was there. Say so, and suggest the other " +
			"machine may be asleep or its window closed. Retrying is reasonable once.",
	}
	ErrPeerOffline = ErrorCode{
		Code: "peer_offline",
		When: "The paired device is not reachable right now.",
		Do: "On a LAN this is normal: the other machine is asleep or on another " +
			"network. Retry later rather than treating it as a failure.",
	}
	ErrNotPaired = ErrorCode{
		Code: "not_paired",
		When: "No device is paired with this one.",
		Do:   "Tell the user to pair their two machines in the Ìbejì window.",
	}
	ErrNotFound = ErrorCode{
		Code: "not_found",
		When: "The path, space, process or device does not exist.",
		Do:   "Check the name. `list_remote_dir` and `list_spaces` show what exists.",
	}
	ErrBadRequest = ErrorCode{
		Code: "bad_request",
		When: "A parameter was missing or malformed.",
		Do:   "Fix the request; retrying it unchanged will fail the same way.",
	}
	ErrSyncConflict = ErrorCode{
		Code: "sync_conflict",
		When: "A space has an unresolved conflict, so it cannot converge.",
		Do: "Tell the user which file conflicts. They promote or discard the " +
			"conflict copy, then the command can run.",
	}
	ErrExecTimeout = ErrorCode{
		Code: "exec_timeout",
		When: "The command itself ran longer than its timeout.",
		Do:   "Distinct from timed_out, which is about approval. Consider detach for long work.",
	}
)

// Describe returns the full surface.
func Describe(version string) *Document {
	doc := &Document{
		Product: "Ìbejì",
		Version: version,
		About: "Ìbejì links two of one person's computers over the LAN. These calls act on " +
			"the OTHER machine — the paired device — not on this one. Use them when work has " +
			"to happen where the credentials, the toolchain or the network access live.",
		Concepts: map[string]string{
			"the pair": "Exactly two devices. Every call acts on the paired device, so none " +
				"of them takes a device name.",
			"permission modes": "The receiving machine is in Prompt mode (every side effect " +
				"asks its user first) or Bypass (nothing asks). Reads never prompt in either mode.",
			"remembered rules": "When a user approves a command they may remember it: that " +
				"exact command, in that directory, from that device runs unasked next time. " +
				"Nothing else is covered.",
			"the sync barrier": "Spaces sync in the background, so a file saved here may not " +
				"have arrived there yet. Pass after_sync to run_remote_command to wait for " +
				"convergence first. Without it, a commit can capture a half-synced tree.",
			"master and replica": "Each space has one master, whose version wins a collision. " +
				"The replica's version is never deleted: it is kept beside the file as a " +
				"conflict copy and surfaced for a person to resolve.",
			"provenance": "Calls from an agent are recorded as mcp, so the user can tell what " +
				"you did from what they did. It is for legibility, not a security boundary.",
		},
		Calls: calls(),
	}
	sort.Slice(doc.Calls, func(i, j int) bool { return doc.Calls[i].Name < doc.Calls[j].Name })
	return doc
}

// JSON renders the surface for `ibeji help --json`.
func (d *Document) JSON() ([]byte, error) { return json.MarshalIndent(d, "", "  ") }

// Lookup finds a call by name.
func (d *Document) Lookup(name string) (*Call, bool) {
	for i := range d.Calls {
		if d.Calls[i].Name == name {
			return &d.Calls[i], true
		}
	}
	return nil, false
}
