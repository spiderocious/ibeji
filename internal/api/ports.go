package api

import (
	"net/http"
	"strconv"

	"github.com/feranmi/ibeji/internal/ports"
)

// Port Lens: one list of every listening port across both machines, each a
// click from being local.

// PortState is what the API needs for the port view.
type PortState interface {
	// AllPorts returns both machines' ports, merged.
	AllPorts(r *http.Request) ([]ports.Port, PortMeta, error)

	// Tunnels returns the open tunnels.
	Tunnels() []ports.Tunnel

	// OpenTunnel forwards a remote port to this machine.
	OpenTunnel(r *http.Request, port int, name string) (*ports.Tunnel, error)

	// CloseTunnel shuts one.
	CloseTunnel(port int) error

	// PinTunnel marks one to reopen automatically.
	PinTunnel(port int, name string) error
}

// PortMeta describes the freshness of a port list.
//
// Stale renders; stale does not act. A cached list from a sleeping peer is
// readable — with its age shown — but the tunnel button disables and says why,
// rather than silently presenting old data as current.
type PortMeta struct {
	// RemoteAgeSeconds is how old the peer's list is, 0 when live.
	RemoteAgeSeconds int `json:"remoteAgeSeconds"`

	// RemoteStale marks a list that could not be refreshed.
	RemoteStale bool `json:"remoteStale"`

	// Reason explains staleness, for the UI to show rather than invent.
	Reason string `json:"reason,omitempty"`
}

// RegisterPortRoutes wires the port view and tunnels.
func (s *Server) RegisterPortRoutes(state PortState) {
	s.Handle(Route{
		Method: "GET", Path: "/ports",
		Summary: "Every listening port on both machines, with the process behind it",
	}, func(w http.ResponseWriter, r *http.Request) {
		list, meta, err := state.AllPorts(r)
		if err != nil && len(list) == 0 {
			WriteError(w, err)
			return
		}
		if list == nil {
			list = []ports.Port{}
		}
		WriteJSON(w, map[string]any{"ports": list, "meta": meta})
	})

	s.Handle(Route{
		Method: "GET", Path: "/tunnels",
		Summary: "Open tunnels, with their local mapping",
	}, func(w http.ResponseWriter, r *http.Request) {
		list := state.Tunnels()
		if list == nil {
			list = []ports.Tunnel{}
		}
		WriteJSON(w, map[string]any{"tunnels": list})
	})

	// Opening a tunnel is NOT marked mutating: it is a read, per the PRD —
	// the remote user already exposed the mesh by pairing, and a prompt per
	// click would make the port view unusable. It is still logged.
	s.Handle(Route{
		Method: "POST", Path: "/tunnels/open",
		Summary: "Forward a port from the paired device to this machine",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Port int    `json:"port"`
			Name string `json:"name"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.Port <= 0 || req.Port > 65535 {
			WriteError(w, Errf(CodeBadRequest, "%d is not a port", req.Port))
			return
		}
		tunnel, err := state.OpenTunnel(r, req.Port, req.Name)
		if err != nil {
			WriteError(w, AsError(err))
			return
		}
		s.events.Publish(EventTunnelChanged, tunnel)
		WriteJSON(w, map[string]any{"tunnel": tunnel})
	})

	s.Handle(Route{
		Method: "POST", Path: "/tunnels/close",
		Summary: "Close a tunnel",
	}, func(w http.ResponseWriter, r *http.Request) {
		port, err := portFromBody(r)
		if err != nil {
			WriteError(w, err)
			return
		}
		if err := state.CloseTunnel(port); err != nil {
			WriteError(w, Errf(CodeNotFound, "%v", err))
			return
		}
		s.events.Publish(EventTunnelChanged, nil)
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "POST", Path: "/tunnels/pin", Mutating: true,
		Summary: "Pin a tunnel so it reopens by itself when both devices are online",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Port int    `json:"port"`
			Name string `json:"name"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if err := state.PinTunnel(req.Port, req.Name); err != nil {
			WriteError(w, Errf(CodeNotFound, "%v", err))
			return
		}
		WriteJSON(w, nil)
	})
}

func portFromBody(r *http.Request) (int, error) {
	var req struct {
		Port int `json:"port"`
	}
	if err := DecodeBody(r, &req); err != nil {
		return 0, err
	}
	if req.Port <= 0 {
		// Accept a query parameter too, so a quick curl works.
		if q := r.URL.Query().Get("port"); q != "" {
			n, err := strconv.Atoi(q)
			if err == nil {
				return n, nil
			}
		}
		return 0, Errf(CodeBadRequest, "port is required")
	}
	return req.Port, nil
}
