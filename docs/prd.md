# Ìbejì — MVP Product Requirements

A LAN-only companion app (Electron + background daemon) that links two of your own computers into a trusted pair. Once paired, the machines can share files, keep folders in sync through shared spaces, expose each other's dev-server ports as if they were local, and offer a CLI / HTTP API / MCP surface so both you and your AI agents can browse files, search code, and run commands across machines. Built for the "work on one PC, run it on the other" workflow — including the case where one machine is the only one allowed to talk to corporate infrastructure (VPN, SSO) and the other is where the actual work happens.

**Name note:** *Ìbejì* (ee-BAY-jee) is Yoruba for "twins" — two machines acting as one pair. v1 is exactly two devices; a mesh of more is deferred. Follows the Awòyè / Ìrísí / Ajala naming tradition.

**Core flow:** install Ìbejì on each machine → daemons discover each other over the LAN (mDNS) → pair once with a short code → name each device → from then on, every feature (spaces, ports, remote API, handoff) flows over the encrypted device-to-device link, gated by that device's permission mode (Prompt or Bypass) and its remembered rules.

**Non-goal, stated up front:** nothing in this product touches the internet. No accounts, no cloud, no relay, no web app. If the two machines are not on the same LAN, Ìbejì does nothing.

**Second non-goal: Ìbejì never shares, bridges, or forwards a VPN.** It does not make one machine a gateway for the other, does not enable IP forwarding, and does not route the other machine's traffic through a corporate tunnel. This is a permanent exclusion, not a deferral — and it is also where the product's real claim lives. Three reasons, in order of weight:

1. **It would not work.** The thing gating corporate access is rarely the source IP. It is a posture-checking client (Netskope and peers), an SSO session bound to a managed browser profile, or an identity gate — none of which care which interface the packet left. Routing the laptop's traffic through the desktop's tunnel fails the posture check while fixing nothing.
2. **It is the one feature that would make this tool a policy violation.** Everything else in Ìbejì is one person using two machines they own. Bridging a managed tunnel to an unmanaged machine is the line where "my dev setup" becomes circumventing a network control — and the machine it gets flagged on is the one you cannot afford to lose.
3. **Moving the work beats moving the network.** The VPN stays on exactly one machine; what crosses the LAN is a command (Module 6), a tunnel to a localhost port (Module 5), or a URL (Module 7). `git push` leaves from the machine that owns the credentials. A tunnel reaches anything that machine can reach on its own loopback. An SSO re-auth opens in the browser where the session is valid.

The one case this genuinely cannot serve: a tool running locally on the unprivileged machine that needs direct routed access to an internal host — an IDE's database browser pointed at a corporate Postgres, say. Port Lens covers most of it, because the tunnel reaches whatever the privileged machine reaches on `localhost`. Where it does not, the answer is to run that tool on the other machine and tunnel its port, not to move the network.

---

## Devices & roles

- **Device** — a machine running the Ìbejì daemon + app. Each device has a user-chosen name (e.g. "Desktop", "Laptop") and a pinned identity key.
- **Paired device** — the device that has completed one-time pairing with this one. All features require pairing. **v1 supports exactly one pair (two devices);** a mesh of 2+ is deferred to v2.
- **Master / replica** — roles that exist *per shared space*, not per device. Either device can be master of one space and replica of another.
- (No user accounts. The trust unit is the device pair. No admin/team concepts.)

---

## Module 1 — Pairing & Device Identity

- The daemon must start on login and run in the background independently of the Electron window being open; closing the window must not disconnect the link.
- **On macOS the daemon must run as a LaunchAgent inside the logged-in user's session — never a root LaunchDaemon.** Remote commands need the user's keychain, GUI session, and environment; a system daemon gets "user interaction not allowed" from the keychain and cannot open browsers for handoff. This placement is a requirement, not an implementation detail.
- Devices on the same LAN must discover each other automatically via mDNS; a user can also add a device manually by IP address.
- First connection between two devices must require a one-time pairing step: a short numeric code shown on one machine and entered (or confirmed) on the other. On success, each device pins the other's public key; subsequent connections must verify the pinned key and never re-prompt.
- A user must be able to name each device, and rename it later; names propagate to the peer.
- The device view must always show, for the paired device: **name, current LAN IP address, online/offline status**, and its permission mode. IPs must be plainly visible, never hidden behind a detail view.
- All device-to-device traffic must be encrypted (mutual-key TLS or equivalent), even though it never leaves the LAN.
- A user can unpair at any time; unpairing must revoke the pinned key and immediately close all active tunnels, syncs, and sessions.

## Module 2 — Permission Modes & Remembered Rules

- Every paired device must operate in exactly one of two modes, set on the *receiving* device and changeable at any time:
  - **Prompt mode (default):** every incoming action with side effects (file write into a space from a replica, command execution, app launch, browser handoff) surfaces an accept/decline prompt in the app on the receiving machine before it runs.
  - **Bypass mode:** all incoming actions from the paired device run without prompting. Bypass ships in v1 — it is the right mode between two machines the same person owns and sits at; Prompt is the right default for a corporate-managed machine.
- **Remembered rules (v1):** the Prompt dialog must offer a third action alongside Accept and Decline — **"Always allow this exact command in this working directory from this device."** Accepting it creates a rule; future exec requests matching the rule (exact command line + cwd + device) run without prompting, and are logged as rule-matched. This is the feature that makes Prompt mode livable for agent workflows (e.g. `git push` in one repo, approved once) without opening up the whole machine the way Bypass does.
  - Rules apply to command execution and app/browser launch actions. File writes into a space are governed by the space, not by rules.
  - The app must show a per-device **rules list** — every remembered rule, when it was created, how often it has fired — with one-click revoke.
- Reads (listing files, reading file contents, searching, viewing ports) must not prompt in either mode; prompting is for side effects.
- **Read scope in v1 is the whole filesystem, by decision.** Remote reads are not path-scoped and not denied anywhere: a paired device can read and grep anything the logged-in user can, `~/.ssh` and `~/.aws` included. The trust unit is the device pair, both machines belong to the same person, and a prompt on every read would make Module 6 unusable for agents. The consequence is worth stating plainly: **in v1, Prompt mode means "cannot run anything, can read everything."** Anyone pairing with a machine they do not fully trust should not pair. Path-scoped rules and a secret deny-list are the v2 answer; v1 does not pretend to have one.
- A prompt must state which device is asking and exactly what will happen (e.g. "Laptop wants to run `npm test` in ~/projects/medcord"). Decline must return a clear "declined by Desktop" error to the caller.
- **An unanswered prompt must expire, not hang.** A prompt that goes unanswered for its timeout — default **60 seconds**, configurable per device — expires: the prompt is dismissed on the receiving machine and the caller receives a `timed_out` error distinguishable from both `declined` and a transport failure, naming the machine that did not answer ("timed out awaiting approval on Desktop"). The expiry is recorded in the activity log as expired, never as declined. There is no pending queue and no retroactive approval in v1: an exec approved twenty minutes after it was requested is approving a command whose context has moved on, and the caller is long gone. **The caller must learn quickly that nobody is there** — an agent that hangs indefinitely on a locked screen is the failure mode this timeout exists to prevent. Retrying is the caller's job.
- Every incoming action — accepted, declined, bypassed, or rule-matched — must be recorded in a per-device activity log viewable in the app.
- Mode is per direction: Desktop can be in Bypass toward Laptop while Laptop stays in Prompt toward Desktop.

## Module 3 — File Drop & Clipboard

*(Slimmed from a full chat in an earlier draft; persistent chat history, search, and inline previews are deferred to v2. The scratchpad in Module 8 covers "leave a note.")*

- A user can send files and folders (sent as archives) to the paired device from the app, the tray menu, or `ibeji cp`.
- Received files must land in a known, configurable **inbox folder** (default `~/Ibeji/Inbox`), with a notification carrying a "reveal in folder" action.
- Sending must work whenever both devices are online; if the peer is offline, the send must fail visibly (no cloud queueing — LAN only, no store-and-forward in v1).
- A user can enable a **clipboard sync toggle** for the pair: while on, copying on one machine makes the content (text and images) available to paste on the other within a second. Off by default.
- Clipboard sync must be visibly indicated while active, and clipboard contents must never be written to any log or history.

## Module 4 — Shared Spaces (master-authoritative sync)

- A user can create a named **space** by picking a local folder and inviting the paired device; the inviting device's folder becomes the space's initial content.
- Each space must have exactly one **master device**, chosen at creation. The other participant is a **replica** holding a real, full copy of the files on disk (not a network mount).
  - **Guidance the app should surface at creation:** for a code repository, the master should be the machine that owns the git credentials / corporate access — because master-wins is the correct conflict outcome there (a `git pull` on the master beats an unsynced local edit on the replica, which is preserved as a conflict copy).
- File watchers on every participant must detect local changes and sync them: replica changes push to the master; the master pushes changes to the replica.
- **Conflict rule:** if the same file changed on the master and the replica before syncing, the master's version wins; the replica's version must be preserved alongside as a conflict copy (e.g. `report.conflict-from-laptop.md`), and the conflict must be surfaced in the space's activity feed — never silently discarded.
- **Resolving a conflict must not require walking to the other machine.** Every conflict surfaced in the activity feed must offer, from either device, two actions: **promote the conflict copy** (its contents replace the master's file at the original path, and the promotion syncs back as a normal master change) and **discard the conflict copy** (delete it, keep the master's version). Both are recorded in the activity feed with the paths involved. Without promotion, a conflict copy is a dead end that has to be hand-carried across machines — which is the exact chore this product exists to remove. Promotion is a write into the space and so obeys Module 2 like any other write.
- A replica that was offline must re-converge automatically on reconnect, applying the same conflict rule.
- Every space must support **ignore patterns** from creation, with sensible defaults pre-filled (`node_modules/`, build output dirs, `.DS_Store`) and user-editable per space.
  - **`.git/` must be ignored by default, and the default should be strong** (a warning if the user removes it). Syncing `.git` across machines corrupts repos (`index.lock` races, half-written objects) and defeats the product's core pattern: sync the working tree, run git on the machine that owns the credentials.
  - If the chosen folder contains a `.gitignore`, the app must offer to **import its patterns** as the space's initial ignore list.
- A space can be created as **one-way ("mirror")**: changes flow master → replica only; replica-side edits are neither pushed nor conflict-copied. Intended for build-artifact and output folders.
- Each space must have an **activity feed**: what synced, from where, when, and any conflicts, with the file paths involved.
- **The space's sync state (`in-sync`, `syncing`, …) must be exposed on the local API, including a way to wait for convergence** — this is what the exec barrier in Module 6 is built on.
- A user can pause syncing for a space (per device) and resume later; resume behaves like a reconnect.
- A user can leave a space (stops sync, keeps local files) or delete a space (master only; the replica is notified and its copy is left in place but disconnected).
- Large-file transfers must be chunked and resumable; a sync interrupted mid-file must not leave a corrupt partial visible at the destination path.

## Module 5 — Port Lens

- Each daemon must continuously enumerate its own machine's listening TCP ports with the owning process name.
- The app must show a combined **port view** across both devices when online, labelling well-known dev tools where recognizable ("Vite on 5173", "Next.js on 3000", "Postgres on 5432") and falling back to process name + port.
- A user can click any remote port to open a **tunnel**: the local daemon listens on the same port number on `localhost` and forwards all connections over the LAN to the remote daemon, which connects to its own `localhost:<port>` internally — so servers bound to 127.0.0.1 on the remote machine are reachable without changing their bind address, firewall rules, or using IP addresses.
- If the same port number is already in use locally, the tunnel must auto-remap to a free port, and the active mapping ("Desktop:3000 → localhost:43000") must always be visible.
- Active tunnels must be listed with one-click disconnect.
- A user can **pin** a port with a friendly name (e.g. "Medcord API"); pinned tunnels must persist across app/daemon restarts and auto-reconnect when both devices are online.
- Every HTTP(S) port row must have an **open-in-browser** action that opens the local tunnel address in the default browser.
- Tunnels count as reads/connections, not side effects: clicking a tunnel must not prompt on the remote machine even in Prompt mode (the remote user already exposed the mesh by pairing). The activity log still records tunnel opens, **including the connecting process name where knowable** — the log is part of the accountability story on a managed machine.

## Module 6 — Remote API: Daemon, CLI & MCP

*(This module is the product. It absorbs the offload runner that an earlier draft kept in Module 7.)*

- The daemon must expose a **local HTTP API** on localhost (loopback only, authenticated with a local token) as the single programmable surface; the Electron UI, the CLI, and the MCP server are all clients of it.
- The API must support, against the online paired device, subject to that device's permission mode and remembered rules:
  - list directory contents and stat paths
  - read file contents
  - **search: grep file contents and glob file names under a remote directory** — an agent exploring a repo one `read_file` at a time is unusable; one search call replaces fifty reads
  - write/create files and directories
  - run a shell command in a given working directory, with **streamed stdout/stderr**, exit code, and a configurable timeout
  - list devices, list ports, open/close tunnels
  - query a space's sync state and **wait for convergence**
  - **connectivity probe:** ask the remote device to attempt a TCP/HTTP request to a given host and report the outcome — "can *that* machine reach gitlab.com, and what status does it get?" (diagnosing which machine's network/VPN is the problem is a daily task this answers in one call)
  - **device info:** OS, arch, versions of common toolchains on PATH, for agents sizing up the far machine
- **Execution environment:** remote commands must run inside the user's **login shell** (`zsh -lc` on macOS), as the logged-in user, with no privilege escalation — otherwise PATH misses nvm/pnpm/homebrew and git can't find its credential helper, and every first run fails mysteriously.
- **Sync barrier:** exec must accept an `--after-sync <space>` option (API flag, CLI flag, MCP parameter) that blocks until the named space reports `in-sync` before running the command. Without it there is a built-in race: save a file on the replica, immediately run `git commit` on the master, and the commit captures a half-synced tree. The barrier is the blessed way to sequence "edit here, run there."
- **Process management:** exec must support detached runs for long-lived processes (dev servers). The API/CLI must be able to **list running Ìbejì-started processes, re-attach to their streamed output, and kill them** (`ibeji ps desktop`, `ibeji attach`, `ibeji kill`). A started process must be stoppable without walking to the other machine.
- **Saved commands:** a user can save named commands per device (e.g. "Run Medcord tests" = `npm test` in a given cwd) and trigger them from the paired device via a command palette; output streams back live and run history is kept. A saved command is just a named exec — same permission path, same logs.
- A **CLI** (`ibeji`) must be registered on PATH at install, as a thin client over the API. Representative commands:
  - `ibeji devices` — names, IPs, status
  - `ibeji ls desktop:~/projects` / `ibeji cat desktop:~/projects/notes.md`
  - `ibeji grep desktop:~/projects/medcord "TODO" --glob "*.ts"`
  - `ibeji cp ./build.zip desktop:~/Ibeji/Inbox/`
  - `ibeji run desktop --cwd ~/projects/medcord --after-sync medcord -- git push` (streams output, exits with the remote exit code)
  - `ibeji ps desktop` / `ibeji kill desktop <id>` / `ibeji attach desktop <id>`
  - `ibeji ports desktop` / `ibeji tunnel desktop:3000`
  - `ibeji check desktop https://gitlab.com` — connectivity probe
  - `ibeji trace` — the event trace, tailed or filtered (Module 9)
  - `ibeji help --json` — the machine-readable surface description, for agents (Module 9)
  - `ibeji doctor` — diagnose a broken install or link (Module 9)
- An **MCP server** must ship with the app (stdio, one-line registration for Claude Code and compatible agents) exposing tools equivalent to the API: `list_devices`, `device_info`, `list_remote_dir`, `read_remote_file`, `search_remote_files`, `write_remote_file`, `run_remote_command` (with `after_sync` and `detach`), `list_remote_processes`, `kill_remote_process`, `list_remote_ports`, `open_tunnel`, `check_connectivity`, `wait_for_sync` — so an agent on one machine can treat the other as extended workspace.
- All write and exec calls from CLI/API/MCP must flow through Module 2 exactly like app-originated actions: in Prompt mode the remote machine's user approves each one (or a remembered rule matches); calls must block awaiting the decision and return a distinguishable "declined" error. **The approval wait has a default timeout of 60 seconds**; see Module 2 for expiry behaviour.
- Every exec must be recorded in the activity log with the full command line.

## Module 7 — Handoff

- **Browser handoff:** a user can send a URL to the paired device to open in its default browser (from the app, the CLI, or the tray menu). This is also the escape hatch for corporate auth: when a SAML/SSO session needs refreshing on the machine that owns it, hand the sign-in URL across and click through there.
- **App launch:** a user can launch an application on the paired device, optionally with a file or folder argument (e.g. "open VS Code on Desktop at space Medcord's folder").
- **Handoff, not teleport:** moving a "running app" between machines is defined as relaunching the same app on the target device pointed at the same context (same URL, same file, same space folder — which sync makes equivalent). No live window migration in v1.
- **Notifications:** Ìbejì's own events (sync conflict, command finished, incoming prompt, inbox arrival) must mirror reliably to the paired device as toasts, per-pair toggle. *(OS-level notification capture — mirroring other apps' notifications — is deferred to v2; on macOS it is a permissions tar pit that would dominate the module.)*
- All of the above are side effects and obey Prompt/Bypass mode and remembered rules.

## Module 8 — Scratchpad

- The device pair must share one persistent **scratchpad**: a single markdown note editable from both sides, for content too small for a space. Last-writer-wins per save is acceptable; edits sync live when both are online.
- *(Secret spaces — encrypted-at-rest sync for `.env` files and keys, keychain unlock, always-prompt reads — are deferred to v2. The design is kept: same master/replica + conflict rules as normal spaces, contents never in logs or inbox.)*

---

## Module 9 — Observability & agent onboarding

*(Added after the module list was first drafted. It exists because the first user of this product's API is an agent, and an agent cannot read a README or ask a colleague.)*

- **The trace.** The daemon must record every API call, every link event and every permission decision into a queryable trace, holding: event type, timestamp, originating device, provenance (`app` / `cli` / `mcp` / `api`), the **request payload**, the **result or error**, duration, and the permission verdict where one applied. Retained in a **capped ring buffer** (default 50 MB or 7 days, whichever comes first, both configurable); oldest entries drop first. This is a superset of the Module 2 activity log, which remains the human-facing, permission-focused view; the trace is the diagnostic one.
  - Exposed as **`ibeji trace`** (tail, follow, and filter by device, event type, provenance or time window) and as a **Trace pane in the app** with the same filters.
  - Exposed on the local API so an agent can read it, and **readable from the paired device over the link** — a read, so it never prompts in either mode. This is the point of it: an agent on one machine debugging why its own call failed must be able to see the other machine's side of that call.
  - **Clipboard contents are never traced** (Module 3), and a file write's payload is traced as path plus byte count, never the file body.
- **`ibeji help`, written for an agent.** Beyond usage text, the CLI must be able to emit a complete, self-describing surface: every command and API endpoint with its parameters, types, defaults, an example request payload, an example success response, and the full set of error codes it can return (including `declined`, `timed_out`, and the sync-barrier failures). `ibeji help --json` must return this as a machine-readable document, so an agent that has never seen Ìbejì can onboard from one call instead of guessing at payload shapes.
  - The MCP server's tool descriptions must be generated from the same source as this document, so the two can never drift.
- **`ibeji doctor`.** One command that checks, and names what is broken and how to fix it: the LaunchAgent is installed and running, the keychain is reachable, the API token works, mDNS resolves the peer, the link is up with a matching pinned key, and the login shell provides the PATH that exec depends on. Half the support burden of a LAN product is "it is not working" with no way to tell which of six things failed.

---

## Cross-cutting requirements

- **LAN-only is a hard boundary:** the daemon must never open a listener on a non-local interface except for paired-device traffic on the LAN, and must never make WAN requests (no telemetry, no update pings in the data path; app updates are a separate, explicit action).
- Everything device-to-device is encrypted with pinned keys; a key mismatch (device reinstalled, possible impersonation) must hard-fail with a re-pair prompt, never auto-trust.
- The daemon must be light: idle CPU near zero, port scanning and file watching must not measurably tax either machine.
- Every remote-originated side effect is attributable: device name + timestamp + action in the activity log, on both sides.
- **Everything the daemon does is traceable** (Module 9): every API call and link event, with its payload and result, into a capped ring buffer readable by you and by an agent. The two exceptions are absolute — **clipboard contents are never traced, and a file write's payload is traced as path plus byte count, never the file body.**
- All transfers (file drops, space sync) must survive interruption without corrupting the destination (write-to-temp then atomic rename).
- The app must handle sleep/wake and network changes gracefully: reconnect automatically, re-converge spaces, re-establish pinned tunnels, without user action.
- macOS first (both of the user's machines); code paths that are platform-specific (notifications, launch, login shell) must degrade gracefully rather than crash on other platforms.

---

## Build order

**All nine modules are in scope for v1.** The order below is about sequencing and risk, not about what ships: the product is not done until Port Lens, file drop, handoff and the scratchpad are done too.

Modules 1 + 2 + 6 + 4 are the first milestone — pairing, permissions with remembered rules, remote exec/search/barrier via CLI+MCP, and repo-shaped spaces. That alone retires the "git must run on the other machine" problem. Ports (5), file drop (3), handoff (7), and scratchpad (8) follow.

**Module 9 is split across the whole build**, because it is diagnostic infrastructure rather than a feature: `ibeji doctor` lands in the first milestone (it is what makes a broken install diagnosable while the install path is still new), the trace lands with the API in the second (so every call is traced from the first call), and `ibeji help --json` lands with the MCP server, generated from the same source as the tool descriptions.

**Inside that milestone the order is not 1 → 2 → 6 → 4.** Module 6's local HTTP API is the surface every other module is a client of, so it cannot be built fourth — there would be nothing for the permission layer to intercept or the sync barrier to expose. The real sequence:

1. **Module 1** — daemon as LaunchAgent, mDNS discovery, short-code pairing, pinned keys, encrypted link, and **`ibeji doctor`** (Module 9). Nothing works without a link, and nothing is diagnosable without doctor.
2. **Module 6, skeleton** — the local HTTP API with token auth, and `ls` / `cat` / `search` / `exec` against the peer in **Bypass only**. Reads and exec working end to end over the link, before any permission machinery exists. This is the walking skeleton; a `ibeji run desktop -- echo hi` that returns `hi` is the milestone's first real signal. **The trace (Module 9) goes in with the API**, not after it — retrofitting tracing onto calls that already exist means missing the ones you forget.
3. **Module 2** — permission modes, the prompt, remembered rules, the activity log, interposed on the calls that already work. Built against a live caller rather than a hypothetical one, which is what keeps the prompt copy honest.
4. **Module 4** — spaces, watchers, master-wins conflicts, and the `in-sync` state on the API.
5. **Module 6, remainder** — the sync barrier (needs Module 4's state), detached processes with `ps` / `attach` / `kill`, saved commands, the connectivity probe, and the MCP server over the finished API — with **`ibeji help --json`** (Module 9) generated from the same source as the MCP tool descriptions, so the two cannot drift.

The MCP server comes last on purpose: it is a thin shell over the API, and shipping it before the API's shape has settled means rewriting its tool surface twice.

## Deferred to v2

- **Mesh of more than two devices** (pairing matrix, name propagation, per-pair modes at n>2).
- **Chat** (persistent searchable history, message bubbles, inline image previews; v1 keeps only file drop → inbox).
- **Secret spaces** (encrypted-at-rest sync for `.env`/keys, keychain unlock, always-prompt reads).
- **OS-level notification mirroring** (v1 mirrors only Ìbejì's own events).
- **Wake-on-LAN** — cut from v1 entirely: over Wi-Fi on modern Macs it fails more than it works.
- **Screen sharing & remote control** (WebRTC view + native input injection, view-only and full-control modes, on-screen "being controlled" indicator) — designed, deliberately cut from MVP.
- Finder/Files integration (FUSE or File Provider mount of a remote filesystem).
- Internet relay / working across different networks (or Tailscale interop) — **the first v2 item this product will actually miss**, the day the two machines are not on the same LAN. Note this is reaching *your own* device across networks; it is not VPN bridging, which is a permanent non-goal.
- Store-and-forward (queue sends for offline peers).
- Single-window streaming ("just show me the build terminal").
- Full granular permissions (path-scoped rules, command patterns/wildcards — v1's remembered rules cover only exact command + cwd + device). **This is also where remote reads get scoped**; v1 reads the whole filesystem by decision (Module 2).
- UDP port tunneling.
- Windows/Linux polish as first-class targets.
- Multi-user / guest pairing (sharing with a machine that isn't yours).

---

## Reference: Permission behavior by action

| Action | Prompt mode | Bypass mode |
|---|---|---|
| File/folder received into inbox | no prompt | no prompt |
| List/read/search files | no prompt (unscoped in v1 — Module 2) | no prompt |
| Write file via API / replica edit into master | prompt | runs |
| Promote / discard a conflict copy | prompt | runs |
| Run command / saved command | prompt (or remembered rule → runs, logged) | runs |
| Launch app / browser handoff | prompt (or remembered rule → runs, logged) | runs |
| Open tunnel to a port | no prompt (logged, with process name) | no prompt (logged) |

Every prompt in the left column expires after 60 seconds (configurable per device), returning `timed_out` to the caller and logging as expired — never as declined.

## Reference: Space sync states

| State | Meaning |
|---|---|
| `in-sync` | Both participants converged (the state the exec barrier waits for) |
| `syncing` | Transfers in flight |
| `paused` | User paused this device's participation |
| `offline` | A participant is unreachable; will re-converge on reconnect |
| `conflict` | Master won a collision; conflict copy written and surfaced, promotable or discardable from either device |

## Reference: Worked example (a normal Tuesday)

Feranmi codes on **Laptop**; **Desktop** sits across the room and is the only machine the company's VPN and SSO trust. Both run Ìbejì, paired; Desktop is in Prompt mode with remembered rules for the handful of commands that matter. A space "medcord" (master: Desktop, `.git/` ignored) keeps the working tree identical on both. He edits on Laptop; saves sync to Desktop in seconds. Claude Code on Laptop uses the Ìbejì MCP server to `run_remote_command("git push", after_sync: "medcord")` — the barrier waits for the last save to land, the remembered rule lets it run without a prompt, and the push goes out with Desktop's credentials on Desktop's VPN. The pinned saved command "Medcord dev" starts `npm run dev` detached on Desktop; the port lens shows "Next.js on 3000 — Desktop", he clicks it, and `localhost:3000` on Laptop now serves Desktop's dev server. When GitLab's SSO session lapses, he hands the sign-in URL to Desktop's browser and clicks through with Beamer. Before stepping out he copies a snippet on Desktop and pastes it on Laptop via clipboard sync, jots a TODO in the shared scratchpad, runs `ibeji kill desktop` on the dev server, and closes the lid. When the Laptop wakes on the same Wi-Fi, the space re-converges and the pinned tunnel comes back by itself.
