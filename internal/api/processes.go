package api

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/feranmi/ibeji/internal/remote"
)

// Processes started on the paired device, and saved commands.
//
// A dev server started from the other machine must be stoppable without
// walking over to it — that is the whole reason detached execution exists.

// ProcessState is what the API needs for the process table.
type ProcessState interface {
	// RemoteProcesses lists what this device started on the peer.
	RemoteProcesses() []ProcessView

	// KillRemote stops one.
	KillRemote(id string) error

	// AttachRemote returns a process's remembered output.
	AttachRemote(id string) ([]OutputLine, error)
}

// ProcessView is one running or finished process, as the API reports it.
type ProcessView struct {
	ID       string    `json:"id"`
	Command  string    `json:"command"`
	Cwd      string    `json:"cwd"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Running  bool      `json:"running"`
	ExitCode int       `json:"exitCode"`
	Device   string    `json:"device"`
}

// OutputLine is one remembered chunk of a process's output.
type OutputLine struct {
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// SavedCommand is a named exec.
//
// Just a named command: same permission path, same log, same rules. The name
// is a convenience, never a way around a decision.
type SavedCommand struct {
	Name    string `json:"name"`
	Device  string `json:"device"`
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	Detach  bool   `json:"detach,omitempty"`

	// AfterSync names a space to wait for before running.
	AfterSync string `json:"afterSync,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	LastRun   time.Time `json:"lastRun,omitempty"`
	RunCount  int       `json:"runCount"`
}

// SavedCommands holds the named commands for this device.
type SavedCommands struct {
	mu       sync.RWMutex
	commands map[string]*SavedCommand
}

// NewSavedCommands builds an empty set.
func NewSavedCommands() *SavedCommands {
	return &SavedCommands{commands: map[string]*SavedCommand{}}
}

// Save stores a command under its name, replacing any earlier one.
func (s *SavedCommands) Save(c SavedCommand) (*SavedCommand, error) {
	if c.Name == "" {
		return nil, Errf(CodeBadRequest, "a saved command needs a name")
	}
	if c.Command == "" {
		return nil, Errf(CodeBadRequest, "a saved command needs a command")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.commands[c.Name]; ok {
		// Keep the history: renaming what a command does should not erase
		// the record of how often it has been run.
		c.CreatedAt = existing.CreatedAt
		c.RunCount = existing.RunCount
		c.LastRun = existing.LastRun
	} else {
		c.CreatedAt = time.Now()
	}
	cp := c
	s.commands[c.Name] = &cp
	out := cp
	return &out, nil
}

// Get returns a saved command.
func (s *SavedCommands) Get(name string) (*SavedCommand, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.commands[name]
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// List returns every saved command, by name.
func (s *SavedCommands) List() []SavedCommand {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SavedCommand, 0, len(s.commands))
	for _, c := range s.commands {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Delete removes a saved command.
func (s *SavedCommands) Delete(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.commands[name]; !ok {
		return false
	}
	delete(s.commands, name)
	return true
}

// Ran records a run, for the history the palette shows.
func (s *SavedCommands) Ran(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.commands[name]; ok {
		c.RunCount++
		c.LastRun = time.Now()
	}
}

// RegisterProcessRoutes wires ps, kill, attach and saved commands.
func (s *Server) RegisterProcessRoutes(state ProcessState, saved *SavedCommands) {
	s.Handle(Route{
		Method: "GET", Path: "/processes",
		Summary: "Processes this device started on the paired machine",
	}, func(w http.ResponseWriter, r *http.Request) {
		list := state.RemoteProcesses()
		if list == nil {
			list = []ProcessView{}
		}
		WriteJSON(w, map[string]any{"processes": list})
	})

	s.Handle(Route{
		Method: "POST", Path: "/processes/kill", Mutating: true,
		Summary: "Stop a process started on the paired machine, and its children",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.ID == "" {
			WriteError(w, Errf(CodeBadRequest, "id is required"))
			return
		}
		if err := state.KillRemote(req.ID); err != nil {
			WriteError(w, Errf(CodeNotFound, "%v", err))
			return
		}
		s.events.Publish(EventProcessChanged, map[string]string{"id": req.ID, "action": "killed"})
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "GET", Path: "/processes/attach",
		Summary: "A process's recent output, so attaching shows context",
	}, func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			WriteError(w, Errf(CodeBadRequest, "id is required"))
			return
		}
		lines, err := state.AttachRemote(id)
		if err != nil {
			WriteError(w, Errf(CodeNotFound, "%v", err))
			return
		}
		if lines == nil {
			lines = []OutputLine{}
		}
		WriteJSON(w, map[string]any{"output": lines})
	})

	s.Handle(Route{
		Method: "GET", Path: "/commands",
		Summary: "Saved commands, with how often each has been run",
	}, func(w http.ResponseWriter, r *http.Request) {
		list := saved.List()
		if list == nil {
			list = []SavedCommand{}
		}
		WriteJSON(w, map[string]any{"commands": list})
	})

	s.Handle(Route{
		Method: "POST", Path: "/commands/save", Mutating: true,
		Summary: "Save a named command for a device",
	}, func(w http.ResponseWriter, r *http.Request) {
		var c SavedCommand
		if err := DecodeBody(r, &c); err != nil {
			WriteError(w, err)
			return
		}
		out, err := saved.Save(c)
		if err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, map[string]any{"command": out})
	})

	s.Handle(Route{
		Method: "POST", Path: "/commands/delete", Mutating: true,
		Summary: "Delete a saved command",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if !saved.Delete(req.Name) {
			WriteError(w, Errf(CodeNotFound, "no saved command called %q", req.Name))
			return
		}
		WriteJSON(w, nil)
	})
}

// ProcessViewsFrom converts the remote package's processes to the API's view.
func ProcessViewsFrom(procs []*remote.Process, device string) []ProcessView {
	out := make([]ProcessView, 0, len(procs))
	for _, p := range procs {
		exited, code := p.Exited()
		out = append(out, ProcessView{
			ID: p.ID, Command: p.Command, Cwd: p.Cwd, PID: p.PID,
			Started: p.Started, Running: !exited, ExitCode: code, Device: device,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// OutputLinesFrom converts remembered output to the API's view.
func OutputLinesFrom(chunks []remote.OutputChunk) []OutputLine {
	out := make([]OutputLine, 0, len(chunks))
	for _, c := range chunks {
		stream := "stdout"
		if c.Kind == remote.OutStderr {
			stream = "stderr"
		}
		out = append(out, OutputLine{Stream: stream, Text: c.Text})
	}
	return out
}
