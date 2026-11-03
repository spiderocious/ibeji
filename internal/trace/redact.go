package trace

import (
	"encoding/json"
	"strings"
)

// Redaction is enforced here, at the recorder, rather than at each call site.
//
// A filter applied where events are written is one place to get right. A
// filter applied where they are read is a promise that every future reader
// remembers — and a trace that quietly accumulated the contents of everything
// you ever synced would be the worst artefact this product could leave on
// disk. So the rule is: nothing sensitive reaches the database at all, and a
// caller cannot opt out.

// Two exclusions are absolute, per the PRD:
//
//   - Clipboard contents are never traced. Not in any mode, not with verbose
//     on, not as a hash. A shared clipboard carries passwords and tokens.
//   - A file write's payload is traced as path plus byte count, never the
//     body. The point of a trace is to explain what happened, and "wrote 4.2
//     KB to src/main.go" explains it as well as the bytes would.
const (
	clipboardRedacted = "[clipboard contents are never traced]"
	bodyRedacted      = "[body not traced]"
)

// sensitiveKeys are fields whose values never reach the database, wherever
// they appear in a payload.
//
// Matched case-insensitively against the whole key, because a payload shape
// can change and a trace must not start leaking because someone renamed a
// field from "token" to "apiToken".
var sensitiveKeys = []string{
	"clipboard",
	"token",
	"password",
	"passphrase",
	"secret",
	"apikey",
	"api_key",
	"privatekey",
	"private_key",
	"seed",
	"credential",
	"authorization",
	"cookie",
}

// bodyKeys hold file or stream content, which is recorded as a size instead.
var bodyKeys = []string{"body", "content", "contents", "data", "payload", "bytes"}

// Redact returns a payload safe to store.
//
// It walks the structure rather than matching on the top level only: a token
// nested three objects deep is exactly as sensitive as one at the root, and
// the shapes here will change as the product grows.
func Redact(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	cleaned := redactValue(decoded)

	out, err := json.Marshal(cleaned)
	if err != nil {
		return nil
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			switch {
			case isSensitive(k):
				if strings.Contains(strings.ToLower(k), "clipboard") {
					out[k] = clipboardRedacted
				} else {
					out[k] = "[redacted]"
				}
			case isBody(k):
				// Keep the shape of the fact — something was written, this
				// much of it — without the content.
				out[k] = describeBody(val)
			default:
				out[k] = redactValue(val)
			}
		}
		return out

	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = redactValue(item)
		}
		return out

	default:
		return v
	}
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func isBody(key string) bool {
	k := strings.ToLower(key)
	for _, b := range bodyKeys {
		if k == b {
			return true
		}
	}
	return false
}

// describeBody replaces content with its size.
func describeBody(v any) any {
	switch t := v.(type) {
	case string:
		return map[string]any{"redacted": bodyRedacted, "bytes": len(t)}
	case float64:
		// Already a byte count rather than content — a WriteRequest's Bytes
		// field, say. Keep it: that is the useful part.
		return t
	case []any:
		return map[string]any{"redacted": bodyRedacted, "items": len(t)}
	case nil:
		return nil
	default:
		return bodyRedacted
	}
}
