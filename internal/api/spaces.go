package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/feranmi/ibeji/internal/space"
)

// The space endpoints, and the barrier the exec path waits on.

// SpaceState is what the API needs to serve spaces.
type SpaceState interface {
	Spaces() *space.Manager

	// WaitForPeerSync blocks until BOTH devices report a space converged.
	//
	// One side being in-sync is not convergence: the race the barrier exists
	// to prevent is exactly the window where this machine has finished
	// writing and the other has not finished receiving.
	WaitForPeerSync(r *http.Request, name string, timeout time.Duration) error
}

// RegisterSpaceRoutes wires the space endpoints.
func (s *Server) RegisterSpaceRoutes(state SpaceState) {
	s.Handle(Route{
		Method: "GET", Path: "/spaces",
		Summary: "Every space on this device, with its state and conflicts",
	}, func(w http.ResponseWriter, r *http.Request) {
		list := state.Spaces().List()
		if list == nil {
			list = []space.Status{}
		}
		WriteJSON(w, map[string]any{"spaces": list})
	})

	s.Handle(Route{
		Method: "GET", Path: "/spaces/status",
		Summary: "One space's state, conflicts and activity feed",
	}, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			WriteError(w, Errf(CodeBadRequest, "name is required"))
			return
		}
		status, err := state.Spaces().Status(name)
		if err != nil {
			WriteError(w, Errf(CodeNotFound, "%v", err))
			return
		}
		WriteJSON(w, status)
	})

	s.Handle(Route{
		Method: "POST", Path: "/spaces/create", Mutating: true,
		Summary: "Create a space from a local folder",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   string   `json:"name"`
			Path   string   `json:"path"`
			Role   string   `json:"role"`
			Mode   string   `json:"mode"`
			Ignore []string `json:"ignore"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		sp := space.Space{
			Name:   req.Name,
			Path:   req.Path,
			Role:   space.Role(req.Role),
			Mode:   space.Mode(req.Mode),
			Ignore: req.Ignore,
		}
		if sp.Mode == "" {
			sp.Mode = space.TwoWay
		}
		created, err := state.Spaces().Create(sp)
		if err != nil {
			WriteError(w, Errf(CodeBadRequest, "%v", err))
			return
		}
		s.events.Publish(EventSyncState, created)
		// The .git/ warning travels with the response rather than being left
		// to the UI to remember: syncing .git corrupts repos, and the person
		// removing the ignore is the one who needs telling.
		resp := map[string]any{"space": created}
		if !created.IgnoresGit() {
			resp["warning"] = space.GitWarning
		}
		WriteJSON(w, resp)
	})

	s.Handle(Route{
		Method: "POST", Path: "/spaces/pause", Mutating: true,
		Summary: "Pause this device's participation in a space",
	}, func(w http.ResponseWriter, r *http.Request) {
		s.spaceAction(w, r, state, func(name string) error {
			return state.Spaces().Pause(name)
		})
	})

	s.Handle(Route{
		Method: "POST", Path: "/spaces/resume", Mutating: true,
		Summary: "Resume a paused space; behaves like a reconnect",
	}, func(w http.ResponseWriter, r *http.Request) {
		s.spaceAction(w, r, state, func(name string) error {
			return state.Spaces().Resume(name)
		})
	})

	s.Handle(Route{
		Method: "POST", Path: "/spaces/leave", Mutating: true,
		Summary: "Stop syncing a space, keeping the local files",
	}, func(w http.ResponseWriter, r *http.Request) {
		s.spaceAction(w, r, state, func(name string) error {
			return state.Spaces().Leave(name)
		})
	})

	s.Handle(Route{
		Method: "POST", Path: "/spaces/delete", Mutating: true,
		Summary: "Delete a space (master only); the files are left in place",
	}, func(w http.ResponseWriter, r *http.Request) {
		s.spaceAction(w, r, state, func(name string) error {
			return state.Spaces().Delete(name)
		})
	})

	s.Handle(Route{
		Method: "POST", Path: "/spaces/promote", Mutating: true,
		Summary: "Promote a conflict copy over the master's version",
	}, func(w http.ResponseWriter, r *http.Request) {
		s.conflictAction(w, r, state, state.Spaces().Promote)
	})

	s.Handle(Route{
		Method: "POST", Path: "/spaces/discard", Mutating: true,
		Summary: "Discard a conflict copy, keeping the master's version",
	}, func(w http.ResponseWriter, r *http.Request) {
		s.conflictAction(w, r, state, state.Spaces().Discard)
	})

	s.Handle(Route{
		Method: "POST", Path: "/spaces/wait", Mutating: false,
		Summary: "Block until a space has converged on BOTH devices",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name           string `json:"name"`
			TimeoutSeconds int    `json:"timeoutSeconds"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.Name == "" {
			WriteError(w, Errf(CodeBadRequest, "name is required"))
			return
		}
		timeout := time.Duration(req.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = DefaultBarrierTimeout
		}
		if err := state.WaitForPeerSync(r, req.Name, timeout); err != nil {
			WriteError(w, barrierError(err, req.Name))
			return
		}
		WriteJSON(w, map[string]any{"state": space.InSync})
	})
}

// DefaultBarrierTimeout bounds a barrier that did not ask for one.
//
// Long enough for a real repository to transfer over Wi-Fi, short enough that
// a space which will never converge fails in a coffee break rather than
// overnight.
const DefaultBarrierTimeout = 2 * time.Minute

func (s *Server) spaceAction(w http.ResponseWriter, r *http.Request, state SpaceState, fn func(string) error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := DecodeBody(r, &req); err != nil {
		WriteError(w, err)
		return
	}
	if req.Name == "" {
		WriteError(w, Errf(CodeBadRequest, "name is required"))
		return
	}
	if err := fn(req.Name); err != nil {
		WriteError(w, spaceError(err))
		return
	}
	s.events.Publish(EventSyncState, map[string]string{"space": req.Name})
	WriteJSON(w, nil)
}

func (s *Server) conflictAction(w http.ResponseWriter, r *http.Request, state SpaceState, fn func(string, string) error) {
	var req struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := DecodeBody(r, &req); err != nil {
		WriteError(w, err)
		return
	}
	if req.Name == "" || req.Path == "" {
		WriteError(w, Errf(CodeBadRequest, "name and path are required"))
		return
	}
	if err := fn(req.Name, req.Path); err != nil {
		WriteError(w, spaceError(err))
		return
	}
	s.events.Publish(EventSyncConflict, map[string]string{"space": req.Name, "path": req.Path})
	WriteJSON(w, nil)
}

// spaceError maps the space package's errors onto stable API codes.
func spaceError(err error) error {
	switch {
	case errors.Is(err, space.ErrNotFound):
		return Errf(CodeNotFound, "%v", err)
	case errors.Is(err, space.ErrExists):
		return Errf(CodeBadRequest, "%v", err)
	case errors.Is(err, space.ErrNotMaster):
		return Errf(CodeBadRequest, "%v", err)
	default:
		return Errf(CodeInternal, "%v", err)
	}
}

// barrierError explains why a barrier could not proceed.
//
// A barrier that failed with a bare timeout would tell the caller nothing
// about what to do, so each cause carries its own code and a hint.
func barrierError(err error, name string) error {
	switch {
	case errors.Is(err, space.ErrUnresolvedConflict):
		return Errf(CodeSyncConflict,
			"space %q has an unresolved conflict", name).
			WithHint("Resolve it — promote or discard the conflict copy — then try again.")
	case errors.Is(err, space.ErrNotFound):
		return Errf(CodeNotFound, "no space called %q", name)
	default:
		return Errf(CodeTimedOut, "space %q did not converge in time", name).
			WithHint("The other machine may be asleep, or the transfer may be large. " +
				"Check `ibeji spaces` for its state.")
	}
}
