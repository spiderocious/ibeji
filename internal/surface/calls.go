package surface

// The fourteen calls, defined once.
//
// The MCP tool list, `ibeji help --json` and the human help all generate from
// here, so a change lands in all three or in none.

func calls() []Call {
	return []Call{
		// --- knowing the far machine ---
		{
			Name:     "list_devices",
			Summary:  "The paired device: its name, address, whether it is reachable, and its permission mode.",
			Endpoint: "/v1/devices",
			Method:   "GET",
			Returns: map[string]any{"devices": []map[string]any{{
				"name": "Desktop", "address": "192.168.1.31",
				"status": "live", "mode": "prompt",
			}}},
			Errors: []ErrorCode{ErrNotPaired},
			Notes: []string{
				"status is live or unreachable. Unreachable is normal on a LAN — the other " +
					"machine is asleep — not an error to report as a failure.",
				"mode tells you whether side effects will prompt a human: in prompt mode, " +
					"expect run_remote_command to take up to 60 seconds or be declined.",
			},
			CLI: "ibeji devices",
		},
		{
			Name:     "device_info",
			Summary:  "The far machine's OS, architecture and the versions of tools on its PATH.",
			Endpoint: "/v1/device/info",
			Method:   "GET",
			Returns: map[string]any{
				"os": "darwin", "arch": "arm64", "shell": "/bin/zsh",
				"toolchain": map[string]string{"git": "git version 2.48.0", "node": "v24.2.0"},
			},
			Errors: []ErrorCode{ErrPeerOffline, ErrNotPaired},
			Notes: []string{
				"The versions are what a remote command will actually see, read through the " +
					"same login shell — so a tool missing here will be missing there too.",
			},
			CLI: "ibeji info <device>",
		},

		// --- reading ---
		{
			Name:     "list_remote_dir",
			Summary:  "List a directory on the far machine.",
			Endpoint: "/v1/fs/list",
			Method:   "GET",
			Params: []Param{
				{Name: "path", Type: String, Required: true,
					Description: "Directory to list. ~ is expanded on the far machine."},
			},
			Returns: map[string]any{"entries": []map[string]any{
				{"name": "src", "isDir": true, "size": 96},
			}},
			Errors:  []ErrorCode{ErrNotFound, ErrPeerOffline},
			Example: map[string]any{"path": "~/projects/medcord"},
			Notes:   []string{"Reads never prompt, in either permission mode."},
			CLI:     "ibeji ls <device>:<path>",
		},
		{
			Name:     "read_remote_file",
			Summary:  "Read a file from the far machine.",
			Endpoint: "/v1/fs/read",
			Method:   "GET",
			Params: []Param{
				{Name: "path", Type: String, Required: true, Description: "File to read."},
			},
			Errors:  []ErrorCode{ErrNotFound, ErrPeerOffline},
			Example: map[string]any{"path": "~/projects/medcord/package.json"},
			Notes: []string{
				"Prefer search_remote_files when looking for something: reading a repository " +
					"one file at a time is slow and usually unnecessary.",
			},
			CLI: "ibeji cat <device>:<path>",
		},
		{
			Name: "search_remote_files",
			Summary: "Grep file contents and glob file names under a directory on the far " +
				"machine. One search replaces many reads.",
			Endpoint: "/v1/fs/search",
			Method:   "POST",
			Params: []Param{
				{Name: "root", Type: String, Required: true, Description: "Directory to search under."},
				{Name: "query", Type: String, Description: "Text to find in file contents."},
				{Name: "glob", Type: String, Description: "Only files whose name matches, e.g. *.ts."},
				{Name: "regex", Type: Bool, Default: false, Description: "Treat query as a regular expression."},
				{Name: "ignoreCase", Type: Bool, Default: false, Description: "Fold case when matching."},
				{Name: "maxResults", Type: Int, Default: 200, Description: "Stop after this many matches."},
			},
			Returns: map[string]any{
				"matches":   []map[string]any{{"path": "src/a.ts", "line": 84, "text": "// TODO: retry"}},
				"truncated": false,
			},
			Errors: []ErrorCode{ErrNotFound, ErrBadRequest, ErrPeerOffline},
			Example: map[string]any{
				"root": "~/projects/medcord", "query": "TODO", "glob": "*.ts",
			},
			Notes: []string{
				"node_modules, .git and build output are skipped automatically.",
				"truncated true means the limit was hit: the answer is partial, so do not " +
					"conclude anything from its absence.",
				"A glob with no query searches file NAMES, which answers 'where is the file " +
					"called this'.",
			},
			CLI: "ibeji grep <device>:<path> <query> --glob '*.ts'",
		},

		// --- writing and running ---
		{
			Name:     "write_remote_file",
			Summary:  "Write a file, or create a directory, on the far machine.",
			Endpoint: "/v1/fs/write",
			Method:   "POST",
			Mutating: true,
			Params: []Param{
				{Name: "path", Type: String, Required: true, Description: "Where to write."},
				{Name: "content", Type: String, Description: "The file's contents."},
				{Name: "mkdir", Type: Bool, Default: false, Description: "Create a directory instead."},
			},
			Errors:  []ErrorCode{ErrDeclined, ErrTimedOut, ErrPeerOffline, ErrNotPaired},
			Example: map[string]any{"path": "~/notes.md", "content": "# notes\n"},
			Notes: []string{
				"A side effect: in prompt mode the far machine's user approves it first.",
				"Written atomically — a failed write leaves the previous version intact.",
				"If the file is inside a space, prefer editing it locally and letting it sync.",
			},
		},
		{
			Name: "run_remote_command",
			Summary: "Run a shell command on the far machine, in the user's login shell, " +
				"streaming its output back.",
			Endpoint: "/v1/exec",
			Method:   "POST",
			Mutating: true,
			Params: []Param{
				{Name: "command", Type: String, Required: true,
					Description: "The command line to run."},
				{Name: "cwd", Type: String, Default: "~",
					Description: "Working directory on the far machine."},
				{Name: "timeoutSeconds", Type: Int, Default: 300,
					Description: "Give up after this long."},
				{Name: "detach", Type: Bool, Default: false,
					Description: "Run in the background and return a process id.",
					Note: "Use for dev servers and anything long-lived. A detached process " +
						"survives this call and is listed by list_remote_processes."},
				{Name: "afterSync", Type: String,
					Description: "Wait for this space to converge before running.",
					Note: "Use this whenever the command reads files you just edited here. " +
						"Without it the command can see a half-synced tree, which fails " +
						"intermittently and is very hard to diagnose later."},
			},
			Returns: map[string]any{
				"exit": map[string]any{"code": 0, "durationMs": 1420},
			},
			Errors: []ErrorCode{
				ErrDeclined, ErrTimedOut, ErrExecTimeout, ErrSyncConflict,
				ErrPeerOffline, ErrNotPaired,
			},
			Example: map[string]any{
				"command":   "git push origin main",
				"cwd":       "~/projects/medcord",
				"afterSync": "medcord",
			},
			Notes: []string{
				"This is the main reason Ìbejì exists: run the command where the credentials, " +
					"the VPN or the toolchain live.",
				"The command runs as the logged-in user in their login shell, so their full " +
					"PATH is available — nvm, pnpm, homebrew.",
				"A non-zero exit code is the command's own answer, not a failure of the call. " +
					"Report what it said rather than retrying.",
			},
			CLI: "ibeji run <device> --cwd DIR -- <command>",
		},

		// --- processes ---
		{
			Name:     "list_remote_processes",
			Summary:  "Processes this machine started on the far one and left running.",
			Endpoint: "/v1/processes",
			Method:   "GET",
			Returns: map[string]any{"processes": []map[string]any{{
				"id": "p-7a4", "command": "npm run dev", "running": true,
			}}},
			Errors: []ErrorCode{ErrPeerOffline, ErrNotPaired},
			CLI:    "ibeji ps <device>",
		},
		{
			Name:     "kill_remote_process",
			Summary:  "Stop a process on the far machine, and everything it spawned.",
			Endpoint: "/v1/processes/kill",
			Method:   "POST",
			Mutating: true,
			Params: []Param{
				{Name: "id", Type: String, Required: true,
					Description: "Process id from list_remote_processes."},
			},
			Errors: []ErrorCode{
				ErrNotFound, ErrPeerOffline, ErrDeclined, ErrTimedOut,
			},
			Example: map[string]any{"id": "p-7a4"},
			Notes: []string{
				"Kills the whole process group, so a dev server's children die with it.",
				"Stopping something on someone else's machine is a side effect, so in prompt " +
					"mode its user approves it first.",
			},
			CLI: "ibeji kill <device> <id>",
		},

		// --- spaces ---
		{
			Name:     "list_spaces",
			Summary:  "Shared folders, with their sync state and any unresolved conflicts.",
			Endpoint: "/v1/spaces",
			Method:   "GET",
			Returns: map[string]any{"spaces": []map[string]any{{
				"name": "medcord", "role": "replica", "state": "in-sync",
			}}},
			Notes: []string{
				"state is one of in-sync, syncing, paused, offline, conflict.",
				"A conflict state means a file needs a person to decide; run_remote_command " +
					"with after_sync on that space will fail until they do.",
			},
			CLI: "ibeji spaces",
		},
		{
			Name: "wait_for_sync",
			Summary: "Block until a space has converged on both machines, so what you write " +
				"here is readable there.",
			Endpoint: "/v1/spaces/wait",
			Method:   "POST",
			Params: []Param{
				{Name: "name", Type: String, Required: true, Description: "The space to wait for."},
				{Name: "timeoutSeconds", Type: Int, Default: 120,
					Description: "Give up after this long."},
			},
			Errors:  []ErrorCode{ErrSyncConflict, ErrNotFound, ErrTimedOut},
			Example: map[string]any{"name": "medcord"},
			Notes: []string{
				"Usually unnecessary: pass after_sync to run_remote_command instead, which " +
					"does the same wait as part of the call.",
			},
		},

		// --- ports ---
		{
			Name: "list_remote_ports",
			Summary: "What is listening on both machines — every dev server, database and " +
				"service, with the process behind it.",
			Endpoint: "/v1/ports",
			Method:   "GET",
			Returns: map[string]any{"ports": []map[string]any{{
				"port": 3000, "service": "Next.js", "process": "node",
				"device": "Desktop", "loopbackOnly": true,
			}}},
			Errors: []ErrorCode{ErrPeerOffline},
			Notes: []string{
				"loopbackOnly true means the server is bound to 127.0.0.1 there, so it " +
					"cannot be reached from here without a tunnel — which is what open_tunnel " +
					"is for.",
			},
			CLI: "ibeji ports <device>",
		},
		{
			Name: "open_tunnel",
			Summary: "Forward a port from the other machine to this one, so its dev server " +
				"answers on localhost here.",
			Endpoint: "/v1/tunnels/open",
			Method:   "POST",
			Params: []Param{
				{Name: "port", Type: Int, Required: true,
					Description: "The port number on the other machine."},
				{Name: "name", Type: String,
					Description: "A friendly label, for a tunnel worth keeping."},
			},
			Returns: map[string]any{
				"tunnel": map[string]any{"remotePort": 3000, "localPort": 3000, "remapped": false},
			},
			Errors:  []ErrorCode{ErrPeerOffline, ErrNotFound},
			Example: map[string]any{"port": 3000},
			Notes: []string{
				"The server on the other machine does not change: it stays bound to its own " +
					"loopback, and this makes it reachable anyway.",
				"Opening a tunnel is a read, not a side effect, so it never prompts — though " +
					"it is recorded in the log.",
				"If the port is already taken here the tunnel is remapped to a free one and " +
					"remapped is true. ALWAYS tell the user the local port in that case; " +
					"assuming it matched is how an hour goes missing wondering why the wrong " +
					"app is answering.",
			},
			CLI: "ibeji tunnel <device>:<port>",
		},

		// --- diagnosis ---
		{
			Name: "check_connectivity",
			Summary: "Ask the far machine whether it can reach a host or URL — which answers " +
				"'whose network is the problem'.",
			Endpoint: "/v1/check",
			Method:   "POST",
			Params: []Param{
				{Name: "target", Type: String, Required: true,
					Description: "A URL, or host:port."},
				{Name: "timeoutSeconds", Type: Int, Default: 10, Description: "Give up after this long."},
			},
			Returns: map[string]any{
				"reachable": true, "statusCode": 200, "latencyMs": 142,
			},
			Errors:  []ErrorCode{ErrPeerOffline, ErrBadRequest},
			Example: map[string]any{"target": "https://gitlab.com"},
			Notes: []string{
				"Often the fastest way to explain a failure: if a push failed here but the " +
					"far machine reaches the host fine, the problem is this machine's network.",
			},
			CLI: "ibeji check <device> <url>",
		},
		{
			Name: "read_trace",
			Summary: "What the daemon did recently, with payloads, timings and why anything " +
				"failed. Read this before guessing.",
			Endpoint: "/v1/trace",
			Method:   "GET",
			Params: []Param{
				{Name: "since", Type: String, Default: "1h",
					Description: "How far back to look, as a duration like 10m."},
				{Name: "errors", Type: Bool, Default: false, Description: "Only failures."},
				{Name: "provenance", Type: String,
					Description: "Filter by origin: app, cli, mcp or api.",
					Note: "provenance=mcp shows only what agents did, which is usually what " +
						"you want when debugging your own call."},
				{Name: "limit", Type: Int, Default: 100, Description: "How many to return."},
			},
			Errors:  []ErrorCode{ErrBadRequest},
			Example: map[string]any{"since": "10m", "errors": true},
			Notes: []string{
				"Entries carry a hint naming the likely cause where one is knowable — for " +
					"instance that a prompt expired because the far machine's window was closed.",
				"Clipboard contents and file bodies are never recorded.",
				"The far machine's trace is readable too, which is how you see the other " +
					"side of a call that failed.",
			},
			CLI: "ibeji trace --since 10m --errors",
		},
	}
}
