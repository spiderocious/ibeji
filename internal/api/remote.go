package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/feranmi/ibeji/internal/remote"
)

// The remote endpoints: what the window, the CLI and an agent all call to
// reach the other machine.
//
// They are thin. The daemon's remote client does the work and the permission
// engine on the FAR side decides — this layer only translates HTTP into the
// link protocol, so there is no second place where a rule could be applied or
// forgotten.

// RemoteState is what the API needs to reach the peer.
type RemoteState interface {
	// Remote returns a client for the paired device, or an error when there
	// is no link — which on a LAN is normal rather than broken.
	Remote() (*remote.Client, error)

	// PeerName names the paired device, for error messages.
	PeerName() string
}

// RegisterRemoteRoutes wires the filesystem, exec and probe endpoints.
func (s *Server) RegisterRemoteRoutes(state RemoteState) {
	s.Handle(Route{
		Method: "GET", Path: "/fs/list",
		Summary: "List a directory on the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			WriteError(w, Errf(CodeBadRequest, "path is required"))
			return
		}
		entries, err := client.List(r.Context(), path)
		if err != nil {
			WriteError(w, remoteError(err))
			return
		}
		if entries == nil {
			entries = []remote.DirEntry{}
		}
		WriteJSON(w, map[string]any{"entries": entries})
	})

	s.Handle(Route{
		Method: "GET", Path: "/fs/stat",
		Summary: "Describe a path on the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			WriteError(w, Errf(CodeBadRequest, "path is required"))
			return
		}
		info, err := client.Stat(r.Context(), path)
		if err != nil {
			WriteError(w, remoteError(err))
			return
		}
		WriteJSON(w, info)
	})

	s.Handle(Route{
		Method: "GET", Path: "/fs/read",
		Summary: "Read a file from the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			WriteError(w, Errf(CodeBadRequest, "path is required"))
			return
		}
		// Streamed rather than buffered: a caller piping a large file should
		// see it arrive, and the daemon should not hold it in memory.
		w.Header().Set("Content-Type", "application/octet-stream")
		if _, err := client.Read(r.Context(), path, w); err != nil {
			// Headers are already sent, so the error cannot change the status.
			// Logging is all that is left; the short read tells the caller.
			s.Logger.Warn("api: remote read failed mid-stream", "path", path, "err", err)
		}
	})

	s.Handle(Route{
		Method: "POST", Path: "/fs/search",
		Summary: "Grep file contents and glob file names on the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		var req remote.SearchRequest
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.Root == "" {
			WriteError(w, Errf(CodeBadRequest, "root is required"))
			return
		}
		matches, truncated, err := client.Search(r.Context(), req)
		if err != nil {
			WriteError(w, remoteError(err))
			return
		}
		if matches == nil {
			matches = []remote.Match{}
		}
		WriteJSON(w, map[string]any{"matches": matches, "truncated": truncated})
	})

	s.Handle(Route{
		Method: "POST", Path: "/fs/write", Mutating: true,
		Summary: "Write a file or create a directory on the paired device",
	}, func(w http.ResponseWriter, r *http.Request) {
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			WriteError(w, Errf(CodeBadRequest, "path is required"))
			return
		}
		mkdir := r.URL.Query().Get("mkdir") == "true"

		req := remote.WriteRequest{Mkdir: mkdir}
		if !mkdir {
			size, err := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64)
			if err != nil || size < 0 {
				WriteError(w, Errf(CodeBadRequest, "a write needs a Content-Length"))
				return
			}
			req.Bytes = size
		}
		if err := client.Write(r.Context(), path, req, r.Body); err != nil {
			WriteError(w, remoteError(err))
			return
		}
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "GET", Path: "/device/info",
		Summary: "The paired device's OS, arch and toolchain versions",
	}, func(w http.ResponseWriter, r *http.Request) {
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		info, err := client.Info(r.Context())
		if err != nil {
			WriteError(w, remoteError(err))
			return
		}
		WriteJSON(w, info)
	})

	s.Handle(Route{
		Method: "POST", Path: "/check",
		Summary: "Ask the paired device whether it can reach a target",
	}, func(w http.ResponseWriter, r *http.Request) {
		client, err := state.Remote()
		if err != nil {
			WriteError(w, err)
			return
		}
		var req remote.CheckRequest
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.Target == "" {
			WriteError(w, Errf(CodeBadRequest, "target is required"))
			return
		}
		res, err := client.Check(r.Context(), req)
		if err != nil {
			WriteError(w, remoteError(err))
			return
		}
		WriteJSON(w, res)
	})

	s.Handle(Route{
		Method: "POST", Path: "/exec", Mutating: true,
		Summary: "Run a command on the paired device, streaming its output",
	}, func(w http.ResponseWriter, r *http.Request) {
		s.handleExec(w, r, state)
	})
}

// handleExec streams a command's output as newline-delimited JSON.
//
// NDJSON rather than SSE: the CLI is the main consumer and reads this with a
// scanner, and keeping stdout and stderr distinguishable matters more than
// browser-native framing. The window reads it the same way.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request, state RemoteState) {
	client, err := state.Remote()
	if err != nil {
		WriteError(w, err)
		return
	}

	var req remote.ExecRequest
	if err := DecodeBody(r, &req); err != nil {
		WriteError(w, err)
		return
	}
	if req.Command == "" {
		WriteError(w, Errf(CodeBadRequest, "command is required"))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, Errf(CodeInternal, "streaming is not supported"))
		return
	}

	// Detached runs answer immediately with a process id rather than streaming.
	if req.Detach {
		_, started, err := client.Exec(r.Context(), req, nil)
		if err != nil {
			WriteError(w, remoteError(err))
			return
		}
		WriteJSON(w, map[string]any{"process": started})
		return
	}

	// Once the first frame is written the status is fixed, so the permission
	// decision — which can take as long as a prompt — must happen before it.
	// The client's Exec does that: it reads the acknowledgement, which the far
	// side only sends after the guard passes.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")

	enc := json.NewEncoder(w)
	wroteHeader := false
	emit := func(v any) error {
		if !wroteHeader {
			w.WriteHeader(http.StatusOK)
			wroteHeader = true
		}
		if err := enc.Encode(v); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	sink := remote.SinkFunc(func(kind remote.OutputKind, b []byte) error {
		stream := "stdout"
		if kind == remote.OutStderr {
			stream = "stderr"
		}
		return emit(map[string]any{"stream": stream, "text": string(b)})
	})

	exit, started, err := client.Exec(r.Context(), req, sink)
	if err != nil {
		if !wroteHeader {
			// Nothing streamed yet, so a proper HTTP error is still possible —
			// and a declined or timed-out command has not produced output.
			WriteError(w, remoteError(err))
			return
		}
		_ = emit(map[string]any{"stream": "error", "text": err.Error()})
		return
	}
	_ = emit(map[string]any{
		"stream": "exit",
		"exit":   exit,
		"info":   started,
	})
}

// remoteError maps a failure from the far machine onto this API's codes.
//
// The code the peer reported is kept: an agent that sees "declined" from the
// CLI and "declined" from MCP is seeing the same condition, which is the whole
// reason the codes are stable.
func remoteError(err error) error {
	var re *remote.RemoteError
	if errors.As(err, &re) {
		e := &Error{Code: Code(re.Code), Message: re.Message, Hint: re.Hint}
		if e.Code == "" {
			e.Code = CodeInternal
		}
		return e
	}
	return AsError(err)
}
