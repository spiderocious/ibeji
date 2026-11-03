package api

import (
	"io"
	"net/http"
	"strconv"
	"time"
)

// File drop, clipboard, handoff and the scratchpad.

// TransferState is what the API needs for these.
type TransferState interface {
	// SendFile drops a file into the peer's inbox.
	SendFile(r *http.Request, name string, size int64, content io.Reader) error

	// InboxPath is where received files land, so the UI can reveal it.
	InboxPath() (string, error)

	// OpenURL asks the peer to open a URL in its browser.
	OpenURL(r *http.Request, url string) error

	// LaunchApp asks the peer to open an application.
	LaunchApp(r *http.Request, app, path string) error

	// Scratchpad returns the shared note.
	Scratchpad() (string, time.Time)

	// SetScratchpad replaces it and sends it across.
	SetScratchpad(r *http.Request, text string) error

	// ClipboardEnabled reports whether clipboard sync is on.
	ClipboardEnabled() bool

	// SetClipboardEnabled turns it on or off.
	SetClipboardEnabled(on bool)
}

// RegisterTransferRoutes wires file drop, handoff and the scratchpad.
func (s *Server) RegisterTransferRoutes(state TransferState) {
	s.Handle(Route{
		Method: "POST", Path: "/drop", Mutating: true,
		Summary: "Send a file to the paired device's inbox",
	}, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			WriteError(w, Errf(CodeBadRequest, "name is required"))
			return
		}
		size, err := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64)
		if err != nil || size < 0 {
			WriteError(w, Errf(CodeBadRequest, "a drop needs a Content-Length"))
			return
		}
		if err := state.SendFile(r, name, size, r.Body); err != nil {
			// No store-and-forward: a send to a sleeping machine fails
			// visibly rather than being queued, because a queue that silently
			// held the file until tomorrow is worse than an error today.
			WriteError(w, AsError(err))
			return
		}
		s.events.Publish(EventTransferChanged, map[string]string{"name": name})
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "GET", Path: "/inbox",
		Summary: "Where files from the other machine land",
	}, func(w http.ResponseWriter, r *http.Request) {
		path, err := state.InboxPath()
		if err != nil {
			WriteError(w, AsError(err))
			return
		}
		WriteJSON(w, map[string]string{"path": path})
	})

	s.Handle(Route{
		Method: "POST", Path: "/handoff/url", Mutating: true,
		Summary: "Open a URL in the paired device's browser",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL string `json:"url"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.URL == "" {
			WriteError(w, Errf(CodeBadRequest, "url is required"))
			return
		}
		if err := state.OpenURL(r, req.URL); err != nil {
			WriteError(w, AsError(err))
			return
		}
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "POST", Path: "/handoff/app", Mutating: true,
		Summary: "Open an application on the paired device, optionally at a path",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			App  string `json:"app"`
			Path string `json:"path"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.App == "" {
			WriteError(w, Errf(CodeBadRequest, "app is required"))
			return
		}
		if err := state.LaunchApp(r, req.App, req.Path); err != nil {
			WriteError(w, AsError(err))
			return
		}
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "GET", Path: "/scratchpad",
		Summary: "The note shared with the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		text, at := state.Scratchpad()
		WriteJSON(w, map[string]any{"text": text, "updatedAt": at})
	})

	s.Handle(Route{
		Method: "POST", Path: "/scratchpad", Mutating: true,
		Summary: "Replace the shared note",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if err := state.SetScratchpad(r, req.Text); err != nil {
			WriteError(w, AsError(err))
			return
		}
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "GET", Path: "/clipboard",
		Summary: "Whether clipboard sync is on",
	}, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, map[string]bool{"enabled": state.ClipboardEnabled()})
	})

	s.Handle(Route{
		Method: "POST", Path: "/clipboard", Mutating: true,
		Summary: "Turn clipboard sync on or off",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		// Off by default, and a deliberate choice to turn on: a shared
		// clipboard carries whatever you last copied out of a password
		// manager.
		state.SetClipboardEnabled(req.Enabled)
		WriteJSON(w, nil)
	})
}
