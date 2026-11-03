package surface

import (
	"encoding/json"
	"strings"
	"testing"
)

// The surface exists because the first user of this API is an agent, and an
// agent cannot read a README or ask a colleague. These tests check that it
// actually carries what such a caller needs — not that the struct compiles.

func TestEveryCallIsUsableWithoutAskingAnyone(t *testing.T) {
	doc := Describe("test")
	if len(doc.Calls) == 0 {
		t.Fatal("the surface describes nothing")
	}

	for _, c := range doc.Calls {
		if c.Name == "" {
			t.Error("a call has no name")
			continue
		}
		if c.Summary == "" {
			t.Errorf("%s has no summary; an agent choosing a tool reads this first", c.Name)
		}
		if c.Endpoint == "" || c.Method == "" {
			t.Errorf("%s has no endpoint or method", c.Name)
		}
		// Every failure an agent can hit must say what to do about it,
		// otherwise it retries a refusal three times.
		for _, e := range c.Errors {
			if e.When == "" {
				t.Errorf("%s: error %q does not say when it happens", c.Name, e.Code)
			}
			if e.Do == "" {
				t.Errorf("%s: error %q does not say what to do about it", c.Name, e.Code)
			}
		}
		// A required parameter with no description is a guess waiting to
		// happen.
		for _, p := range c.Params {
			if p.Description == "" {
				t.Errorf("%s: parameter %q has no description", c.Name, p.Name)
			}
			if p.Type == "" {
				t.Errorf("%s: parameter %q has no type", c.Name, p.Name)
			}
		}
	}
}

// A side effect that listed no failures would leave an agent with no idea
// that a human might refuse it.
func TestMutatingCallsDocumentRefusal(t *testing.T) {
	doc := Describe("test")
	for _, c := range doc.Calls {
		if !c.Mutating {
			continue
		}
		var hasDeclined, hasTimedOut bool
		for _, e := range c.Errors {
			if e.Code == "declined" {
				hasDeclined = true
			}
			if e.Code == "timed_out" {
				hasTimedOut = true
			}
		}
		if !hasDeclined {
			t.Errorf("%s has side effects but does not document being declined", c.Name)
		}
		if !hasTimedOut {
			t.Errorf("%s has side effects but does not document the prompt expiring", c.Name)
		}
	}
}

// The PRD names fourteen tools. They must all be here, under those names,
// because an agent configured against one of them finding it missing is a
// silent capability gap.
func TestThePRDsToolsAllExist(t *testing.T) {
	doc := Describe("test")
	required := []string{
		"list_devices", "device_info",
		"list_remote_dir", "read_remote_file", "search_remote_files", "write_remote_file",
		"run_remote_command", "list_remote_processes", "kill_remote_process",
		"list_remote_ports", "open_tunnel",
		"check_connectivity", "wait_for_sync",
	}
	for _, name := range required {
		if _, ok := doc.Lookup(name); !ok {
			t.Errorf("the PRD names %s and it is missing", name)
		}
	}
}

// Exec is the reason this product exists, so its description has to carry the
// things that are otherwise learned by getting them wrong.
func TestRunRemoteCommandExplainsTheBarrier(t *testing.T) {
	doc := Describe("test")
	call, ok := doc.Lookup("run_remote_command")
	if !ok {
		t.Fatal("run_remote_command is missing")
	}

	var afterSync *Param
	for i := range call.Params {
		if call.Params[i].Name == "afterSync" {
			afterSync = &call.Params[i]
		}
	}
	if afterSync == nil {
		t.Fatal("run_remote_command has no afterSync parameter; the barrier is unreachable")
	}
	// The note is what stops an agent skipping it and getting an
	// intermittent, very hard to diagnose failure.
	if !strings.Contains(strings.ToLower(afterSync.Note), "half-synced") {
		t.Errorf("afterSync does not explain what goes wrong without it: %q", afterSync.Note)
	}

	joined := strings.ToLower(strings.Join(call.Notes, " "))
	if !strings.Contains(joined, "login shell") {
		t.Error("run_remote_command does not mention the login shell, so an agent cannot explain a PATH failure")
	}
	if !strings.Contains(joined, "non-zero exit") {
		t.Error("run_remote_command does not say a non-zero exit is the command's own answer; an agent would retry it")
	}
}

// A remapped tunnel that an agent reports as "localhost:3000" sends the user
// to the wrong place.
func TestOpenTunnelWarnsAboutRemapping(t *testing.T) {
	doc := Describe("test")
	call, ok := doc.Lookup("open_tunnel")
	if !ok {
		t.Fatal("open_tunnel is missing")
	}
	joined := strings.ToLower(strings.Join(call.Notes, " "))
	if !strings.Contains(joined, "remap") {
		t.Error("open_tunnel does not warn that the local port may differ")
	}
}

// An agent handed a tool list with no context guesses at the rest.
func TestTheDocumentExplainsItself(t *testing.T) {
	doc := Describe("test")
	if doc.About == "" {
		t.Fatal("the surface does not say what the product is")
	}
	if !strings.Contains(strings.ToLower(doc.About), "other machine") {
		t.Error("the About text does not make clear these calls act on the OTHER machine")
	}
	for _, key := range []string{"the pair", "permission modes", "the sync barrier"} {
		if doc.Concepts[key] == "" {
			t.Errorf("the surface does not explain %q", key)
		}
	}
}

// --- MCP generation ---

func TestToolsGenerateFromTheSameDefinition(t *testing.T) {
	doc := Describe("test")
	tools := doc.Tools()

	if len(tools) != len(doc.Calls) {
		t.Fatalf("%d tools from %d calls", len(tools), len(doc.Calls))
	}
	for _, tool := range tools {
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("%s has no input schema", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s's schema is not an object", tool.Name)
		}
		// The schema has to be valid JSON, since it goes over the wire.
		if _, err := json.Marshal(tool); err != nil {
			t.Errorf("%s does not marshal: %v", tool.Name, err)
		}
	}
}

func TestToolSchemasCarryRequiredParameters(t *testing.T) {
	doc := Describe("test")
	for _, c := range doc.Calls {
		var wantRequired []string
		for _, p := range c.Params {
			if p.Required {
				wantRequired = append(wantRequired, p.Name)
			}
		}
		if len(wantRequired) == 0 {
			continue
		}
		schema := c.schema()
		got, _ := schema["required"].([]string)
		if len(got) != len(wantRequired) {
			t.Errorf("%s: schema requires %v, the call requires %v", c.Name, got, wantRequired)
		}
	}
}

// The description is what an agent reads before choosing a tool, so the
// failure guidance has to be in it rather than only in the document.
func TestToolDescriptionsCarryFailureGuidance(t *testing.T) {
	doc := Describe("test")
	for _, tool := range doc.Tools() {
		call, _ := doc.Lookup(tool.Name)
		if len(call.Errors) == 0 {
			continue
		}
		if !strings.Contains(tool.Description, "Failures:") {
			t.Errorf("%s's description lists no failures", tool.Name)
		}
		for _, e := range call.Errors {
			if !strings.Contains(tool.Description, e.Code) {
				t.Errorf("%s's description omits the %q failure", tool.Name, e.Code)
			}
		}
	}
}

func TestMutatingToolsSayTheyMayPrompt(t *testing.T) {
	doc := Describe("test")
	for _, tool := range doc.Tools() {
		call, _ := doc.Lookup(tool.Name)
		if !call.Mutating {
			continue
		}
		if !strings.Contains(strings.ToLower(tool.Description), "approve") {
			t.Errorf("%s has side effects but its description does not say a human may be asked", tool.Name)
		}
	}
}

func TestDocumentJSONIsValid(t *testing.T) {
	doc := Describe("1.2.3")
	body, err := doc.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var into map[string]any
	if err := json.Unmarshal(body, &into); err != nil {
		t.Fatalf("the surface document is not valid JSON: %v", err)
	}
	if into["version"] != "1.2.3" {
		t.Errorf("version = %v", into["version"])
	}
}

func TestCallsAreSortedSoTheListIsStable(t *testing.T) {
	doc := Describe("test")
	for i := 1; i < len(doc.Calls); i++ {
		if doc.Calls[i-1].Name > doc.Calls[i].Name {
			t.Fatalf("calls are not sorted: %q before %q", doc.Calls[i-1].Name, doc.Calls[i].Name)
		}
	}
}
