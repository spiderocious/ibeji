package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/feranmi/ibeji/internal/doctor"
	"github.com/feranmi/ibeji/internal/permission"
	"github.com/feranmi/ibeji/internal/trace"
)

// The diagnostic surface: doctor, the trace, and the prompts waiting for an
// answer.
//
// All three exist because the first user of this API is an agent, and an agent
// cannot read a log over someone's shoulder or notice a prompt on a screen it
// cannot see.

// DiagnosticState is what these endpoints read.
type DiagnosticState interface {
	// Doctor runs the checks.
	Doctor() doctor.Report

	// Trace returns the recorder, or nil when tracing is off.
	Trace() *trace.Recorder

	// Permissions returns the engine, for the prompt queue and rules.
	Permissions() *permission.Engine
}

// RegisterDiagnosticRoutes wires doctor, trace, prompts and rules.
func (s *Server) RegisterDiagnosticRoutes(state DiagnosticState) {
	s.Handle(Route{
		Method: "GET", Path: "/doctor",
		Summary: "Diagnose the install and the link; every failure names its fix",
	}, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, state.Doctor())
	})

	s.Handle(Route{
		Method: "GET", Path: "/trace",
		Summary: "What the daemon did, with payloads, newest first",
	}, func(w http.ResponseWriter, r *http.Request) {
		rec := state.Trace()
		if rec == nil {
			WriteJSON(w, map[string]any{"events": []trace.Event{}})
			return
		}

		q := trace.Query{
			Type:       r.URL.Query().Get("type"),
			Device:     r.URL.Query().Get("device"),
			Provenance: r.URL.Query().Get("provenance"),
			ErrorsOnly: r.URL.Query().Get("errors") == "true",
		}
		if since := r.URL.Query().Get("since"); since != "" {
			d, err := time.ParseDuration(since)
			if err != nil {
				WriteError(w, Errf(CodeBadRequest,
					"since must be a duration like 10m or 2h, got %q", since))
				return
			}
			q.Since = time.Now().Add(-d)
		}
		if limit := r.URL.Query().Get("limit"); limit != "" {
			n, err := strconv.Atoi(limit)
			if err != nil || n < 0 {
				WriteError(w, Errf(CodeBadRequest, "limit must be a number, got %q", limit))
				return
			}
			q.Limit = n
		}

		events, err := rec.Events(r.Context(), q)
		if err != nil {
			WriteError(w, err)
			return
		}
		if events == nil {
			events = []trace.Event{}
		}
		WriteJSON(w, map[string]any{"events": events})
	})

	s.Handle(Route{
		Method: "GET", Path: "/trace/stats",
		Summary: "How much the trace is holding, and its retention limits",
	}, func(w http.ResponseWriter, r *http.Request) {
		rec := state.Trace()
		if rec == nil {
			WriteJSON(w, trace.Stats{})
			return
		}
		stats, err := rec.Stats()
		if err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, stats)
	})

	s.Handle(Route{
		Method: "GET", Path: "/prompts",
		Summary: "Permission prompts waiting for an answer",
	}, func(w http.ResponseWriter, r *http.Request) {
		engine := state.Permissions()
		if engine == nil {
			WriteJSON(w, map[string]any{"prompts": []any{}})
			return
		}
		pending := engine.Pending()
		out := make([]map[string]any, 0, len(pending))
		for _, p := range pending {
			// Asker and Effect are required slots: a prompt that does not name
			// the asking device and exactly what will happen cannot render,
			// and must not be answerable either.
			out = append(out, map[string]any{
				"id":         p.ID,
				"asker":      p.Asker(),
				"effect":     p.Effect(),
				"kind":       string(p.Action.Kind),
				"cwd":        p.Action.Cwd,
				"provenance": p.Action.Provenance,
				"at":         p.At,
				"expiresAt":  p.ExpiresAt,
			})
		}
		WriteJSON(w, map[string]any{"prompts": out})
	})

	s.Handle(Route{
		Method: "POST", Path: "/prompts/answer", Mutating: true,
		Summary: "Accept or decline a prompt, optionally remembering it",
	}, func(w http.ResponseWriter, r *http.Request) {
		engine := state.Permissions()
		if engine == nil {
			WriteError(w, Errf(CodeNotFound, "no permission engine"))
			return
		}
		var req struct {
			ID       string `json:"id"`
			Accept   bool   `json:"accept"`
			Remember bool   `json:"remember"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.ID == "" {
			WriteError(w, Errf(CodeBadRequest, "id is required"))
			return
		}
		if err := engine.Answer(req.ID, req.Accept, req.Remember); err != nil {
			WriteError(w, Errf(CodeNotFound, "%v", err))
			return
		}
		s.events.Publish(EventPromptResolved, map[string]any{"id": req.ID, "accepted": req.Accept})
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "GET", Path: "/rules",
		Summary: "Remembered rules: what they cover and how often they have fired",
	}, func(w http.ResponseWriter, r *http.Request) {
		engine := state.Permissions()
		if engine == nil {
			WriteJSON(w, map[string]any{"rules": []permission.Rule{}})
			return
		}
		rules := engine.Rules().List()
		if rules == nil {
			rules = []permission.Rule{}
		}
		WriteJSON(w, map[string]any{"rules": rules})
	})

	s.Handle(Route{
		Method: "POST", Path: "/rules/revoke", Mutating: true,
		Summary: "Revoke a remembered rule",
	}, func(w http.ResponseWriter, r *http.Request) {
		engine := state.Permissions()
		if engine == nil {
			WriteError(w, Errf(CodeNotFound, "no permission engine"))
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if !engine.Rules().Revoke(req.ID) {
			WriteError(w, Errf(CodeNotFound, "no rule %q", req.ID))
			return
		}
		WriteJSON(w, nil)
	})
}
