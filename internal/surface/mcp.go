package surface

import (
	"fmt"
	"strings"
)

// MCP tool descriptions, generated from the same definition as `ibeji help
// --json`.
//
// Generated rather than hand-written because hand-written tool descriptions
// drift from the API within two weeks — and a drifted description is worse
// than a missing one, since the agent trusts it and acts on it.

// Tool is one MCP tool, in the shape the protocol expects.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Tools renders every call as an MCP tool.
func (d *Document) Tools() []Tool {
	out := make([]Tool, 0, len(d.Calls))
	for _, c := range d.Calls {
		out = append(out, Tool{
			Name:        c.Name,
			Description: c.description(),
			InputSchema: c.schema(),
		})
	}
	return out
}

// description builds the text an agent reads before choosing a tool.
//
// It carries the summary, then the things that are learned the hard way
// otherwise: what the parameters mean, what can go wrong, and what to do about
// each failure. Verbose, deliberately — an agent that reads this once makes
// fewer wrong calls than one that discovers the same facts by failing.
func (c Call) description() string {
	var b strings.Builder
	b.WriteString(c.Summary)

	if c.Mutating {
		b.WriteString("\n\nThis has side effects on the other machine. If that machine is in " +
			"prompt mode its user approves it first, so the call can take up to a minute or " +
			"be declined.")
	}

	for _, n := range c.Notes {
		b.WriteString("\n\n")
		b.WriteString(n)
	}

	if len(c.Errors) > 0 {
		b.WriteString("\n\nFailures:")
		for _, e := range c.Errors {
			fmt.Fprintf(&b, "\n  %s — %s %s", e.Code, e.When, e.Do)
		}
	}
	return b.String()
}

// schema renders a call's parameters as a JSON schema.
func (c Call) schema() map[string]any {
	props := map[string]any{}
	var required []string

	for _, p := range c.Params {
		prop := map[string]any{
			"type":        string(p.Type),
			"description": p.describe(),
		}
		if p.Default != nil {
			prop["default"] = p.Default
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// describe builds a parameter's description, folding in its note.
func (p Param) describe() string {
	if p.Note == "" {
		return p.Description
	}
	return p.Description + " " + p.Note
}
