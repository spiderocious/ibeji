// Package remote carries file and command operations between the two devices.
//
// Everything here rides a stream on the single link: a request frame, then
// either a response frame or a stream of output frames ending in an exit. The
// shapes are JSON because they are small, infrequent and worth being able to
// read in a trace — the bulk data (file contents, command output) travels as
// raw bytes after its header, not inside it.
package remote

import (
	"errors"
	"time"
)

// Op identifies what a request asks for. A wire contract: values are never
// renumbered, and an unknown op is refused rather than guessed at.
type Op string

const (
	// OpList lists a directory.
	OpList Op = "fs.list"

	// OpStat describes one path.
	OpStat Op = "fs.stat"

	// OpRead reads a file's contents.
	OpRead Op = "fs.read"

	// OpWrite writes a file or creates a directory.
	OpWrite Op = "fs.write"

	// OpSearch greps contents and globs names under a directory.
	//
	// The one read that makes the API usable for an agent: exploring a repo
	// one read at a time is unusable, and one search replaces fifty reads.
	OpSearch Op = "fs.search"

	// OpExec runs a command with streamed output.
	OpExec Op = "exec"

	// OpInfo reports the far machine's OS, arch and toolchain versions.
	OpInfo Op = "device.info"

	// OpCheck asks the far machine to probe a URL — "can *that* machine reach
	// gitlab.com, and with what status".
	OpCheck Op = "net.check"

	// OpPS lists processes this device started on the far machine.
	OpPS Op = "process.list"

	// OpKill stops one, and everything it spawned.
	OpKill Op = "process.kill"

	// OpAttach returns a process's remembered output, so attaching shows
	// context rather than starting mid-sentence.
	OpAttach Op = "process.attach"

	// OpPorts lists what is listening on the far machine.
	OpPorts Op = "ports.list"
)

// Known reports whether this build understands the op.
func (o Op) Known() bool {
	switch o {
	case OpList, OpStat, OpRead, OpWrite, OpSearch, OpExec, OpInfo, OpCheck,
		OpPS, OpKill, OpAttach, OpPorts:
		return true
	default:
		return false
	}
}

// Mutating reports whether the op has side effects, and therefore passes
// through the permission engine.
//
// Reads never prompt, in either mode — the PRD is explicit, and it is what
// makes the API usable for an agent. Getting this wrong in either direction is
// serious: a mutating op marked read bypasses permissions entirely, and a read
// marked mutating makes Prompt mode unusable.
func (o Op) Mutating() bool {
	switch o {
	case OpWrite, OpExec, OpKill:
		// Killing a process the user started is a side effect on their
		// machine, so it passes through the permission engine like any other.
		return true
	default:
		return false
	}
}

// Request is the first frame on a remote stream.
type Request struct {
	Op Op `json:"op"`

	// Provenance says who asked: app, cli, mcp or api. Self-declared and so
	// not a security boundary — stated plainly in the PRD — but it is what
	// makes an agent's actions legible in the log and the trace.
	Provenance string `json:"provenance,omitempty"`

	// Path is the subject of a filesystem op.
	Path string `json:"path,omitempty"`

	// Exec carries the command to run.
	Exec *ExecRequest `json:"exec,omitempty"`

	// Search carries the query.
	Search *SearchRequest `json:"search,omitempty"`

	// Write carries what to write.
	Write *WriteRequest `json:"write,omitempty"`

	// Check carries the probe target.
	Check *CheckRequest `json:"check,omitempty"`

	// ProcessID addresses one process, for kill and attach.
	ProcessID string `json:"processId,omitempty"`
}

// ExecRequest asks the far machine to run a command.
type ExecRequest struct {
	// Command is the full command line, run through the user's login shell.
	Command string `json:"command"`

	// Cwd is where to run it. Empty means the user's home directory.
	Cwd string `json:"cwd,omitempty"`

	// TimeoutSeconds bounds the command itself. Zero means the default.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// Detach runs the command in the background, surviving the caller.
	Detach bool `json:"detach,omitempty"`

	// AfterSync blocks until the named space reports in-sync before running.
	// The blessed way to sequence "edit here, run there".
	AfterSync string `json:"afterSync,omitempty"`
}

// DefaultExecTimeout bounds a command that did not ask for one.
//
// Five minutes suits a test run or a build without stranding a caller behind
// something that will never finish. Long-lived work should detach instead.
const DefaultExecTimeout = 5 * time.Minute

// SearchRequest greps contents and globs names under a directory.
type SearchRequest struct {
	// Root is the directory to search under.
	Root string `json:"root"`

	// Query is a literal string or a regular expression.
	Query string `json:"query,omitempty"`

	// Glob filters by file name, e.g. "*.ts".
	Glob string `json:"glob,omitempty"`

	// Regex treats Query as a regular expression rather than a literal.
	Regex bool `json:"regex,omitempty"`

	// MaxResults bounds the answer. Zero means the default.
	MaxResults int `json:"maxResults,omitempty"`

	// IgnoreCase folds case when matching contents.
	IgnoreCase bool `json:"ignoreCase,omitempty"`
}

// DefaultSearchResults bounds a search that did not ask for a limit, so a
// careless query over a home directory cannot stream for minutes.
const DefaultSearchResults = 200

// WriteRequest writes a file or creates a directory.
type WriteRequest struct {
	// Bytes is how many bytes of content follow the request frame.
	Bytes int64 `json:"bytes"`

	// Mkdir creates a directory instead of writing a file.
	Mkdir bool `json:"mkdir,omitempty"`

	// Mode is the file mode. Zero means 0644, or 0755 for a directory.
	Mode uint32 `json:"mode,omitempty"`
}

// CheckRequest probes a target from the far machine.
type CheckRequest struct {
	// Target is a URL or host:port.
	Target string `json:"target"`

	// TimeoutSeconds bounds the probe. Zero means 10.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// Response is the reply frame. Exactly one of its payload fields is set.
type Response struct {
	// Code is empty on success, or one of the API's stable error codes.
	Code string `json:"code,omitempty"`

	// Message is the human sentence for a failure.
	Message string `json:"message,omitempty"`

	// Hint names the likely cause, when one is knowable.
	Hint string `json:"hint,omitempty"`

	Entries []DirEntry   `json:"entries,omitempty"`
	Stat    *FileInfo    `json:"stat,omitempty"`
	Matches []Match      `json:"matches,omitempty"`
	Info    *DeviceInfo  `json:"info,omitempty"`
	Check   *CheckResult `json:"check,omitempty"`
	Exec    *ExecStarted `json:"exec,omitempty"`

	Processes []ProcessInfo     `json:"processes,omitempty"`
	Output    []OutputChunkJSON `json:"output,omitempty"`
	Ports     []PortInfo        `json:"ports,omitempty"`

	// Bytes is how many bytes of content follow, for a read.
	Bytes int64 `json:"bytes,omitempty"`

	// Truncated reports that the answer was cut at its limit.
	Truncated bool `json:"truncated,omitempty"`
}

// DirEntry is one item in a directory listing.
type DirEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	IsDir   bool      `json:"isDir"`
	ModTime time.Time `json:"modTime"`
	// Symlink names the target when this entry is a link.
	Symlink string `json:"symlink,omitempty"`
}

// FileInfo describes one path.
type FileInfo struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	IsDir   bool      `json:"isDir"`
	ModTime time.Time `json:"modTime"`
}

// Match is one search hit.
type Match struct {
	Path string `json:"path"`
	// Line is the 1-based line number, or 0 for a filename-only match.
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}

// DeviceInfo describes the far machine, for an agent sizing it up.
type DeviceInfo struct {
	OS        string            `json:"os"`
	Arch      string            `json:"arch"`
	Hostname  string            `json:"hostname"`
	User      string            `json:"user"`
	Home      string            `json:"home"`
	Shell     string            `json:"shell"`
	Toolchain map[string]string `json:"toolchain"`
}

// CheckResult reports what the far machine saw.
//
// This is the "whose network is broken" answer: run from the machine that
// owns the VPN, it says in one call what would otherwise take a walk across
// the room.
type CheckResult struct {
	Target     string `json:"target"`
	Reachable  bool   `json:"reachable"`
	StatusCode int    `json:"statusCode,omitempty"`
	LatencyMS  int64  `json:"latencyMs,omitempty"`
	Error      string `json:"error,omitempty"`
	ResolvedIP string `json:"resolvedIp,omitempty"`
}

// ExecStarted acknowledges a command, before its output streams.
type ExecStarted struct {
	// ProcessID identifies a detached process, for ps/attach/kill.
	ProcessID string `json:"processId,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Shell     string `json:"shell"`
	Cwd       string `json:"cwd"`
}

// ProcessInfo describes a process started on this machine by the peer.
type ProcessInfo struct {
	ID       string    `json:"id"`
	Command  string    `json:"command"`
	Cwd      string    `json:"cwd"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Running  bool      `json:"running"`
	ExitCode int       `json:"exitCode"`
}

// PortInfo is one listening port on the far machine.
type PortInfo struct {
	Port         int    `json:"port"`
	Process      string `json:"process"`
	PID          int    `json:"pid"`
	Service      string `json:"service,omitempty"`
	LoopbackOnly bool   `json:"loopbackOnly"`
}

// OutputChunkJSON is one remembered chunk of a process's output, on the wire.
type OutputChunkJSON struct {
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// OutputKind distinguishes the streams a command writes to.
type OutputKind uint8

const (
	// OutStdout is the command's standard output.
	OutStdout OutputKind = 1

	// OutStderr is its standard error, kept separate so a caller can route
	// them differently — a CLI writes them to different file descriptors.
	OutStderr OutputKind = 2

	// OutExit is the final frame, carrying the exit code.
	OutExit OutputKind = 3
)

// ExitStatus ends an exec stream.
type ExitStatus struct {
	Code int `json:"code"`
	// DurationMS is how long the command ran.
	DurationMS int64 `json:"durationMs"`
	// TimedOut reports that the command exceeded its timeout rather than
	// exiting on its own.
	TimedOut bool `json:"timedOut,omitempty"`
}

var (
	// ErrUnknownOp is returned for an op this build does not understand —
	// refused rather than guessed at, so a newer peer gets a clear answer.
	ErrUnknownOp = errors.New("remote: unknown operation")

	// ErrMalformed is returned for a request that does not parse.
	ErrMalformed = errors.New("remote: malformed request")
)
