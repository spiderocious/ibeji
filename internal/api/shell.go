package api

import (
	"net/http"
	"time"

	"github.com/feranmi/ibeji/internal/remote"
)

// The Shell tab's endpoints.
//
// A long-lived shell on the paired machine that keeps its working directory
// and environment between commands. /v1/exec cannot back this: it spawns a
// fresh login shell per call, so `cd` and `export` are forgotten the moment
// it returns, and a terminal that forgets where it is between lines is not a
// terminal.
//
// Reading is a long poll. A shell is idle most of the time, and a blocking
// read costs one request instead of a busy loop; the caller keeps the
// sequence, so a window that reconnects replays exactly what it missed rather
// than starting blank.

// RegisterShellRoutes wires the shell endpoints.
func RegisterShellRoutes(s *Server, state RemoteState) {
	s.Handle(Route{
		Method: "GET", Path: "/shells",
		Summary: "Shells open on the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		list, err := client.ShellList(r.Context())
		if err != nil {
			WriteError(w, err)
			return
		}
		if list == nil {
			list = []remote.ShellInfo{}
		}
		WriteJSON(w, map[string]any{"shells": list})
	})

	s.Handle(Route{
		Method: "POST", Path: "/shells/open", Mutating: true,
		Summary: "Open a shell on the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Cwd string `json:"cwd"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		sess, err := client.ShellOpen(r.Context(), req.Cwd)
		if err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, map[string]any{"shell": sess})
	})

	s.Handle(Route{
		Method: "POST", Path: "/shells/write", Mutating: true,
		Summary: "Send a line to a shell on the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID   string `json:"id"`
			Data string `json:"data"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.ID == "" {
			WriteError(w, Errf(CodeBadRequest, "id is required"))
			return
		}
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		if err := client.ShellWrite(r.Context(), req.ID, req.Data); err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "POST", Path: "/shells/read",
		Summary: "Read a shell's output from a sequence, blocking briefly for more",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string `json:"id"`
			Since  int    `json:"since"`
			WaitMS int    `json:"waitMs"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.ID == "" {
			WriteError(w, Errf(CodeBadRequest, "id is required"))
			return
		}
		// Bounded: an unbounded wait from a client that then vanishes would
		// hold a stream open on both machines for as long as it liked.
		wait := time.Duration(req.WaitMS) * time.Millisecond
		if wait > 20*time.Second {
			wait = 20 * time.Second
		}
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		out, seq, done, err := client.ShellRead(r.Context(), req.ID, req.Since, wait)
		if err != nil {
			WriteError(w, err)
			return
		}
		if out == nil {
			out = []remote.ShellOutput{}
		}
		WriteJSON(w, map[string]any{"output": out, "seq": seq, "done": done})
	})

	s.Handle(Route{
		Method: "POST", Path: "/shells/interrupt", Mutating: true,
		Summary: "Stop the command running in a shell, leaving the shell open",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		if err := client.ShellInterrupt(r.Context(), req.ID); err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "POST", Path: "/shells/close", Mutating: true,
		Summary: "Close a shell and everything it started",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		if err := client.ShellClose(r.Context(), req.ID); err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, nil)
	})
}
