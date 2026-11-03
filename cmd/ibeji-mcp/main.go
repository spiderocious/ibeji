// Command ibeji-mcp is the Ìbejì MCP server.
//
// It speaks MCP over stdio and maps each tool onto the daemon's local HTTP
// API, stamping every call with provenance "mcp" so an agent's actions are
// distinguishable from the user's own in the log and the trace.
//
// Its tool descriptions are generated from the same surface definition as
// `ibeji help --json`, so the two cannot drift — and a drifted tool
// description is worse than a missing one, because the agent trusts it.
//
// It is built last on purpose: it is a thin shell over the API, and shipping
// it before the API settled would have meant rewriting its tool surface twice.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/feranmi/ibeji/internal/cli"
	"github.com/feranmi/ibeji/internal/surface"
)

// Set by the linker; see the Makefile.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

// protocolVersion is the MCP revision this server implements.
const protocolVersion = "2024-11-05"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Printf("ibeji-mcp %s (%s, built %s)\n", version, commit, buildDate)
			return
		case "--tools":
			// Printing the tool list without a daemon is how you check what an
			// agent will see.
			doc := surface.Describe(version)
			out, _ := json.MarshalIndent(doc.Tools(), "", "  ")
			fmt.Println(string(out))
			return
		}
	}

	if err := serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "ibeji-mcp: %v\n", err)
		os.Exit(1)
	}
}

// --- JSON-RPC over stdio ---

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func serve(in io.Reader, out io.Writer) error {
	doc := surface.Describe(version)
	enc := json.NewEncoder(out)
	scanner := bufio.NewScanner(in)
	// Tool results can be large — a search, a file read — so the line buffer
	// has to be too.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue // a malformed line is not worth tearing the session down
		}

		result, rpcErr := dispatch(doc, req)
		// A notification has no id and takes no reply.
		if len(req.ID) == 0 {
			continue
		}
		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func dispatch(doc *surface.Document, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ibeji", "version": version},
			"instructions":    doc.About + "\n\n" + conceptsText(doc),
		}, nil

	case "tools/list":
		return map[string]any{"tools": doc.Tools()}, nil

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid parameters"}
		}
		return callTool(doc, params.Name, params.Arguments)

	case "ping":
		return map[string]any{}, nil

	default:
		return nil, &rpcError{Code: -32601, Message: "no method " + req.Method}
	}
}

// conceptsText renders the handful of ideas a caller must hold.
func conceptsText(doc *surface.Document) string {
	var b strings.Builder
	b.WriteString("Things to know:\n")
	for _, key := range []string{
		"the pair", "permission modes", "reads never prompt",
		"the sync barrier", "master and replica", "remembered rules", "provenance",
	} {
		if text, ok := doc.Concepts[key]; ok {
			fmt.Fprintf(&b, "  %s: %s\n", key, text)
		}
	}
	return b.String()
}

// callTool performs one tool call against the local API.
func callTool(doc *surface.Document, name string, args json.RawMessage) (any, *rpcError) {
	call, ok := doc.Lookup(name)
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "no tool " + name}
	}

	client, err := cli.Connect()
	if err != nil {
		// The commonest failure, so it says what to do rather than dumping a
		// connection error an agent cannot act on.
		return toolError("The Ìbejì daemon is not running on this machine, so the other " +
			"machine cannot be reached. Ask the user to start Ìbejì, or to run `ibeji doctor`."), nil
	}

	var argMap map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argMap); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	body, err := invoke(ctx, client, call, argMap)
	if err != nil {
		return toolError(explain(err)), nil
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": body}},
	}, nil
}

// invoke performs the HTTP call behind a tool.
func invoke(ctx context.Context, client *cli.Client, call *surface.Call, args map[string]any) (string, error) {
	path := call.Endpoint

	switch call.Method {
	case "GET":
		// GET parameters travel in the query string.
		if len(args) > 0 {
			q := url.Values{}
			for k, v := range args {
				q.Set(k, fmt.Sprint(v))
			}
			path += "?" + q.Encode()
		}
		resp, err := client.Do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		return string(out), err

	default:
		resp, err := client.Do(ctx, http.MethodPost, path, args)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return "", err
		}
		// Exec streams NDJSON; fold it into something an agent can read in
		// one piece rather than handing back a transcript of frames.
		if strings.Contains(resp.Header.Get("Content-Type"), "ndjson") {
			return foldExec(string(out)), nil
		}
		return string(out), nil
	}
}

// foldExec turns an exec's NDJSON stream into a readable result.
func foldExec(ndjson string) string {
	var stdout, stderr strings.Builder
	exit := -1
	timedOut := false

	for _, line := range strings.Split(ndjson, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame struct {
			Stream string `json:"stream"`
			Text   string `json:"text"`
			Exit   *struct {
				Code     int  `json:"code"`
				TimedOut bool `json:"timedOut"`
			} `json:"exit"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		switch frame.Stream {
		case "stdout":
			stdout.WriteString(frame.Text)
		case "stderr":
			stderr.WriteString(frame.Text)
		case "exit":
			if frame.Exit != nil {
				exit = frame.Exit.Code
				timedOut = frame.Exit.TimedOut
			}
		}
	}

	var b strings.Builder
	if stdout.Len() > 0 {
		b.WriteString(stdout.String())
	}
	if stderr.Len() > 0 {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString("stderr:\n")
		b.WriteString(stderr.String())
	}
	switch {
	case timedOut:
		b.WriteString("\n[the command ran past its timeout and was stopped]")
	case exit == 0:
		b.WriteString("\n[exit 0]")
	case exit > 0:
		// A non-zero exit is the command's answer, not a failure of the call.
		fmt.Fprintf(&b, "\n[exit %d — the command itself failed; report what it said rather than retrying]", exit)
	}
	return b.String()
}

// explain turns an API error into something an agent can act on.
func explain(err error) string {
	var apiErr *cli.APIError
	if errors.As(err, &apiErr) {
		msg := apiErr.Message
		if apiErr.Hint != "" {
			msg += " " + apiErr.Hint
		}
		// The code is included so an agent can branch on it rather than
		// parsing prose.
		return fmt.Sprintf("%s (code: %s)", msg, apiErr.Code)
	}
	return err.Error()
}

// toolError reports a failure as tool content rather than a protocol error,
// so the agent sees the explanation and can decide what to do.
func toolError(text string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}
