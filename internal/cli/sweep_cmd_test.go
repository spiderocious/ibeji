package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The sweep's output is read by a person at the end of a bad debugging
// session, and by an agent that has to act on it. Both need the same three
// things: which machines answered, which one is this Mac, and the exact next
// command. A table of bare IP addresses satisfies none of them.

func TestSweepNamesWhatItFoundAndWhatToDoNext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pair/sweep" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subnet":  "192.168.1.35/24",
			"port":    7749,
			"scanned": 254,
			"took":    "1.7s",
			"hint":    "MONB-1375-LP is listening at 192.168.1.39:7749. That is almost certainly your other Mac — pair with it.",
			"found": []map[string]any{
				{"address": "192.168.1.39:7749", "name": "MONB-1375-LP", "ibeji": true},
				{"address": "192.168.1.35:7749", "name": "This-Mac", "self": true, "ibeji": true},
			},
		})
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	env := &Env{
		Client: &Client{Endpoint: srv.URL, Token: "t", HTTP: srv.Client()},
		Out:    &out,
		Err:    &errOut,
	}
	if err := cmdSweep.Run(context.Background(), env, nil); err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	got := out.String()

	// The peer, by name. This is the line that answers "where did my other
	// Mac go?" — the question a bare address list leaves open.
	if !strings.Contains(got, "MONB-1375-LP") {
		t.Error("the sweep did not name the machine it found")
	}
	if !strings.Contains(got, "192.168.1.39:7749") {
		t.Error("the sweep did not print the address found")
	}
	// This machine is marked, not hidden. A sweep that silently drops a
	// result is harder to trust than one that explains it.
	if !strings.Contains(got, "this Mac") {
		t.Error("the sweep did not mark this machine among its results")
	}
	// And the next command, spelled out. Working out which of reconnect and
	// pair applies is a small cruelty at the end of a long session.
	if !strings.Contains(got, "ibeji reconnect 192.168.1.39:7749") {
		t.Error("the sweep did not offer the reconnect command for the machine it found")
	}
	if !strings.Contains(got, "ibeji pair 192.168.1.39:7749") {
		t.Error("the sweep did not offer the pair command for the machine it found")
	}
	// It must not suggest pairing with ourselves: that was a real bug in the
	// window, where the sweep's own result carried a Pair button aimed here.
	if strings.Contains(got, "reconnect 192.168.1.35:7749") ||
		strings.Contains(got, "pair 192.168.1.35:7749") {
		t.Error("the sweep offered to pair with this very machine")
	}
}

func TestSweepSaysSoWhenItFindsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subnet":  "10.0.0.5/24",
			"port":    7749,
			"scanned": 254,
			"took":    "2.1s",
			"hint":    "Nothing else on 10.0.0.5/24 is listening on port 7749. Check that Ìbejì is open on the other Mac.",
			"found":   []map[string]any{},
		})
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	env := &Env{
		Client: &Client{Endpoint: srv.URL, Token: "t", HTTP: srv.Client()},
		Out:    &out,
		Err:    &errOut,
	}
	if err := cmdSweep.Run(context.Background(), env, nil); err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	got := out.String()

	// An empty result is still an answer, and it has to say what it covered —
	// "nothing found" without "I checked 254 addresses on this subnet" leaves
	// the reader unsure whether the sweep ran at all.
	if !strings.Contains(got, "254") {
		t.Error("an empty sweep did not say how much it checked")
	}
	if !strings.Contains(got, "Ìbejì is open on the other Mac") {
		t.Error("an empty sweep did not say what to try next")
	}
}
