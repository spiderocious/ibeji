/**
 * The renderer.
 *
 * It renders what the API reports and sends intents back through the preload
 * bridge. It holds no product logic: no permission decisions, no retry policy,
 * no sync state — those live in the daemon, which the CLI and MCP also use.
 *
 * Markup uses the design system's component classes directly. If a class here
 * is not in `_components.css`, that is a bug: the system is frozen as built,
 * and a screen that needs something new needs it added upstream, not invented
 * in the app.
 */

import type {
  UpdateStatus,
  Candidate,
  DaemonEvent,
  Device,
  IbejiBridge,
  PairingSession,
  PendingPrompt,
  PortMeta,
  PortRow,
  Result,
  Health,
  HealthCheck,
  HealthSide,
  SelfInfo,
  SpaceStatus,
  TraceEvent,
  TunnelRow,
} from "../shared/types.js";

declare global {
  interface Window {
    ibeji: IbejiBridge;
  }
}

// --- state ------------------------------------------------------------------

type Tone = "live" | "attention" | "irreversible";

type Tab = "devices" | "spaces" | "ports" | "trace" | "shell" | "health";

interface State {
  tab: Tab;
  self: SelfInfo | undefined;
  spaces: SpaceStatus[];
  ports: PortRow[];
  portMeta: PortMeta | undefined;
  tunnels: TunnelRow[];
  prompts: PendingPrompt[];
  trace: TraceEvent[];
  traceErrorsOnly: boolean;
  /** Free-text filter over the trace: matches type, device, error or payload. */
  traceQuery: string;
  /** Which trace rows are expanded, by id. */
  traceOpen: Set<string>;
  /** Open rows kept on screen after the trace window scrolled past them. */
  tracePinned: Map<string, TraceEvent>;
  /** Both machines at a glance, for the Health tab. */
  health: Health | undefined;
  /** The daemon's log, for the Health tab. */
  logs: string[];
  /** True while the log is being re-read. */
  logsBusy: boolean;

  /** The Shell tab's sub-tabs, in the order they were opened. */
  shells: ShellTab[];
  /** Which sub-tab is showing. */
  activeShell: string | undefined;
  /** True while a shell is being opened, so the + cannot be double-pressed. */
  shellBusy: boolean;

  /** Which machines the ports tab is showing. */
  portScope: "all" | "mine" | "theirs";
  /** Free-text filter over ports: matches port number, service or process. */
  portQuery: string;
  devices: Device[];
  candidates: Candidate[];
  hint: string | undefined;
  pairing: PairingSession | undefined;
  /** Set when the daemon cannot be reached; everything else is stale then. */
  daemonDown: string | undefined;
  /** A transient message shown under the header. */
  notice: { text: string; tone: Tone } | undefined;
  update: UpdateStatus | undefined;
  updateBusy: boolean;
}

const state: State = {
  tab: "devices",
  spaces: [],
  ports: [],
  portMeta: undefined,
  tunnels: [],
  prompts: [],
  trace: [],
  traceErrorsOnly: false,
  traceQuery: "",
  traceOpen: new Set<string>(),
  tracePinned: new Map<string, TraceEvent>(),
  health: undefined,
  logs: [],
  logsBusy: false,
  shells: [],
  activeShell: undefined,
  shellBusy: false,
  portScope: "all",
  portQuery: "",
  self: undefined,
  devices: [],
  candidates: [],
  hint: undefined,
  pairing: undefined,
  daemonDown: undefined,
  notice: undefined,
  update: undefined,
  updateBusy: false,
};

/**
 * One Shell sub-tab.
 *
 * The scrollback lives here rather than being re-fetched: the daemon keeps a
 * bounded window and the pane should not lose what scrolled past it. `seq` is
 * how far this tab has read, so a reconnecting window replays exactly what it
 * missed instead of starting blank or repeating itself.
 */
interface ShellTab {
  id: string;
  cwd: string;
  /** Everything the shell has said, as rendered text. */
  text: string;
  /** The daemon's next sequence for this shell. */
  seq: number;
  /** Commands typed here, newest last, for the up arrow. */
  history: string[];
  /** Where the up/down arrows currently sit in history. */
  historyAt: number;
  /** Set once the shell has exited. */
  done: boolean;
  /** True while a read is in flight, so only one poll runs per tab. */
  polling: boolean;
}

// --- helpers ----------------------------------------------------------------

function esc(s: string): string {
  return s.replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c] ?? c,
  );
}

/** Unwrap a bridge result, recording a daemon outage rather than throwing. */
function unwrap<T>(r: Result<T>): T | undefined {
  if (r.ok) {
    if (state.daemonDown) state.daemonDown = undefined;
    return r.value;
  }
  if (r.error.code === "daemon_unreachable") {
    state.daemonDown = r.error.message;
  } else {
    state.notice = { text: r.error.message, tone: "attention" };
  }
  return undefined;
}

function notify(text: string, tone: Tone): void {
  state.notice = { text, tone };
  render();
  // Notices are transient: they report what just happened, and the device list
  // below them is the durable truth.
  setTimeout(() => {
    if (state.notice?.text === text) {
      state.notice = undefined;
      render();
    }
  }, 6000);
}

// --- data -------------------------------------------------------------------

async function refresh(): Promise<void> {
  const [self, devices, candidates, pending, prompts] = await Promise.all([
    window.ibeji.self(),
    window.ibeji.devices(),
    window.ibeji.candidates(),
    window.ibeji.pending(),
    window.ibeji.prompts(),
  ]);

  state.self = unwrap(self) ?? state.self;
  state.devices = unwrap(devices)?.devices ?? [];
  const found = unwrap(candidates);
  state.candidates = found?.candidates ?? [];
  state.hint = found?.hint ?? undefined;
  // Pairing state lives in the daemon, so a window opened mid-ceremony shows
  // the code rather than an empty device list.
  state.pairing = unwrap(pending)?.pairing ?? undefined;
  // A prompt nobody can see expires in sixty seconds and makes the product
  // look broken, so it is read on every refresh whichever tab is open.
  state.prompts = unwrap(prompts)?.prompts ?? [];

  // The other tabs only load when they are showing: a port scan and a space
  // status on every poll would make the daemon busy for nothing.
  if (state.tab === "spaces") {
    state.spaces = unwrap(await window.ibeji.spaces())?.spaces ?? [];
  } else if (state.tab === "ports") {
    const [ports, tunnels] = await Promise.all([
      window.ibeji.ports(),
      window.ibeji.tunnels(),
    ]);
    const p = unwrap(ports);
    state.ports = p?.ports ?? [];
    state.portMeta = p?.meta ?? undefined;
    state.tunnels = unwrap(tunnels)?.tunnels ?? [];
  } else if (state.tab === "trace") {
    const opts = state.traceErrorsOnly ? { errors: true } : undefined;
    state.trace = unwrap(await window.ibeji.trace(opts))?.events ?? [];
  } else if (state.tab === "health") {
    // Asking the far machine takes a round trip, so this runs only while
    // the tab is showing rather than on every poll.
    state.health = unwrap(await window.ibeji.health());
    if (state.logs.length === 0) void loadLogs();
  }
  render();
}

// --- views ------------------------------------------------------------------

/** The daemon is not running: the one state where nothing else is true. */
function daemonDownView(): string {
  return `
    <div class="es">
      <div class="es-title">Ìbejì is not running</div>
      <div class="es-msg">
        The daemon holds the link to your other machine and starts at login.
        Run <code class="rec">ibeji doctor</code> in a terminal to find out why it is not.
      </div>
    </div>`;
}

/** The device mark: filled when reachable, hollow when not. */
function deviceMark(live: boolean): string {
  const inner = live ? `<circle cx="13" cy="13" r="3.4" fill="currentColor"/>` : "";
  return `<span class="dev-mark${live ? "" : " is-off"}">
    <svg width="26" height="26" viewBox="0 0 26 26" fill="none" aria-hidden="true">
      <circle cx="13" cy="13" r="9.5" stroke="currentColor" stroke-width="1.2"/>${inner}
    </svg></span>`;
}

/**
 * A paired device.
 *
 * The address is always rendered. Device.Address is a required slot in the
 * design system because the PRD says IPs are never hidden behind a detail
 * view, and a row without one does not satisfy the contract.
 */
function deviceRow(d: Device): string {
  const live = d.status === "live";
  return `
    <div class="dev-row" data-device="${esc(d.id)}">
      ${deviceMark(live)}
      <div class="dev-id">
        <div class="dev-name">${esc(d.name)}</div>
        <div class="dev-addr">${esc(d.address || "address unknown")}</div>
      </div>
      <div class="dev-meta">
        <span class="sl ${live ? "sl-live" : "sl-unreachable"}"><span class="dot"></span>${
          live ? "Live" : "Unreachable"
        }</span>
        <span class="mode${d.mode === "bypass" ? " mode-bypass" : ""}">${
          d.mode === "bypass" ? "Bypass" : "Prompt"
        }</span>
      </div>
    </div>`;
}

/**
 * This machine's own address, for typing into the other Mac.
 *
 * Pairing by IP needs an address from *somewhere*, and before this the product
 * asked for one it never showed — leaving people to find it in System Settings
 * while holding a dialog open. The window knows it, so the window says it.
 *
 * Only rendered while unpaired: once the link is up the address is noise.
 */
function selfAddressView(): string {
  const addrs = state.self?.addresses ?? [];
  const best = addrs[0];
  if (best === undefined) return "";
  const port = state.self?.port ?? 0;
  const shown = port > 0 ? `${best}:${port}` : best;
  // Extra interfaces are worth offering — a Mac on both Wi-Fi and Ethernet
  // has two, and the first is not always the one the other machine can reach
  // — but they are secondary, so they sit quietly beneath.
  const others = addrs.slice(1);
  return `
    <div class="selfaddr">
      <div class="selfaddr-label">This Mac is at</div>
      <button class="selfaddr-chip" data-copy="${esc(shown)}" title="Copy to clipboard">
        <span class="selfaddr-ip">${esc(shown)}</span>
        <span class="selfaddr-act">Copy</span>
      </button>
      <div class="selfaddr-hint">Type this into Ìbejì on your other Mac.</div>
      ${
        others.length > 0
          ? `<div class="selfaddr-hint">Also at ${others.map((a) => esc(a)).join(", ")}</div>`
          : ""
      }
    </div>`;
}

function candidateRow(c: Candidate): string {
  const disabled = c.compatible ? "" : " disabled";
  return `
    <div class="dev-row">
      ${deviceMark(true)}
      <div class="dev-id">
        <div class="dev-name">${esc(c.name)}</div>
        <div class="dev-addr">${esc(c.address)}:${c.port}</div>
      </div>
      <div class="dev-meta">
        ${
          c.compatible
            ? ""
            : `<span class="sl sl-attention"><span class="dot"></span>Version ${esc(c.apiVersion)}</span>`
        }
        <button class="btn btn-secondary btn-sm" data-pair="${esc(c.address)}:${c.port}"${disabled}>Pair</button>
      </div>
    </div>`;
}

/**
 * The pairing ceremony.
 *
 * The code is the one deliberate exception to the type law: a record value set
 * in territory type at 52px, because it is read aloud across a room rather
 * than scanned in a log. The fingerprint beside it is what the person compares
 * if they want more than six digits of assurance.
 */
function pairingView(p: PairingSession): string {
  return `
    <div class="cd cd-territory">
      <div class="cd-head"><div class="cd-title">Drawing two stations onto one map</div></div>
      <div class="cd-body" style="text-align:center">
        <div class="figure figure-xl app-pairing-code">${esc(p.code.slice(0, 3))} ${esc(p.code.slice(3))}</div>
        <p class="tx-body" style="max-width:40ch;margin:12px auto 0">
          The same six digits should be showing on ${esc(p.peerName)}.
          If they match, confirm on both machines.
        </p>
        <div class="lst" style="margin-top:16px;text-align:left">
          <div class="lst-div"></div>
          <div class="run-row"><span class="run-dev">Peer</span><span class="rec">${esc(p.peerName)}</span></div>
          <div class="run-row"><span class="run-dev">Address</span><span class="rec">${esc(p.peerAddress)}</span></div>
          <div class="run-row"><span class="run-dev">Key</span><span class="rec">${esc(p.peerFingerprint)}</span></div>
        </div>
      </div>
      <div class="cd-foot">
        <button class="btn btn-tertiary" data-action="cancel-pairing">Cancel</button>
        <button class="btn btn-primary" data-action="confirm-pairing">They match — confirm</button>
      </div>
    </div>`;
}

/**
 * A permission prompt.
 *
 * Asker and Effect are required slots in the design system: an unattributed
 * prompt cannot render, because a LAN tool that asks "allow this?" without
 * naming the asker is how you approve the wrong machine. The renderer refuses
 * to draw one that is missing either, rather than drawing a prompt that would
 * be answered blind.
 */
function promptView(p: PendingPrompt): string {
  if (!p.asker || !p.effect) {
    return `<div class="callout callout-attention"><div class="callout-body">
      A request arrived without saying who is asking or what it would do. It was not shown,
      and it will expire unanswered.</div></div>`;
  }
  const seconds = Math.max(0, Math.round((new Date(p.expiresAt).getTime() - Date.now()) / 1000));
  return `
    <div class="cd" data-prompt="${esc(p.id)}">
      <div class="cd-head"><div class="cd-title">${esc(p.asker)} wants to ${esc(p.kind)}</div></div>
      <div class="cd-body">
        <div class="rec" style="font-size:var(--t-record);margin-bottom:var(--s-3)">${esc(p.effect)}</div>
        ${p.cwd ? `<div class="tx-caption rec">in ${esc(p.cwd)}</div>` : ""}
        ${
          p.provenance === "mcp"
            ? `<div style="margin-top:var(--s-3)"><span class="bdg bdg-provenance">Machine-originated</span></div>`
            : ""
        }
        <div class="tx-micro" style="margin-top:var(--s-3)">expires in ${seconds}s</div>
      </div>
      <div class="cd-foot">
        <button class="btn btn-primary btn-sm" data-answer="accept" data-id="${esc(p.id)}">Accept</button>
        <button class="btn btn-secondary btn-sm" data-answer="decline" data-id="${esc(p.id)}">Decline</button>
        <button class="btn btn-tertiary btn-sm" data-answer="remember" data-id="${esc(p.id)}">
          Always allow this here
        </button>
      </div>
    </div>`;
}

/** A shared folder, with its conflicts resolvable in place. */
function spaceView(s: SpaceStatus): string {
  const conflicts = s.conflicts ?? [];
  const badge =
    s.state === "in-sync"
      ? "bdg-converged"
      : s.state === "conflict"
        ? "bdg-attention"
        : "bdg-neutral";

  return `
    <div class="cd">
      <div class="cd-head">
        <div class="cd-title">${esc(s.name)}</div>
        <span class="bdg ${badge}">${esc(s.state)}</span>
      </div>
      <div class="cd-body">
        <div class="tx-caption rec">${esc(s.path)}</div>
        <div class="tx-micro">${s.role} · ${s.files} files · ${formatBytes(s.bytes)}</div>
        ${
          conflicts.length === 0
            ? ""
            : `<div style="margin-top:var(--s-4)">${conflicts
                .map(
                  (c) => `
          <div class="callout callout-attention">
            <div class="callout-title">${esc(c.rel)}</div>
            <div class="callout-body">
              The master's version is in place. Yours is kept as
              <span class="rec">${esc(c.copy)}</span>.
            </div>
            <div style="margin-top:var(--s-3);display:flex;gap:var(--s-2)">
              <button class="btn btn-secondary btn-sm"
                data-promote="${esc(c.rel)}" data-space="${esc(s.name)}">Promote mine</button>
              <button class="btn btn-tertiary btn-sm"
                data-discard="${esc(c.rel)}" data-space="${esc(s.name)}">Discard mine</button>
            </div>
          </div>`,
                )
                .join("")}</div>`
        }
      </div>
    </div>`;
}

/** One listening port, with its tunnel if it has one. */
function portView(p: PortRow, tunnel: TunnelRow | undefined, local: boolean): string {
  const name = p.service || p.process;
  return `
    <div class="prt-row">
      <span class="prt-num">${p.port}</span>
      <div style="min-width:0">
        <div class="prt-svc">${esc(name)}</div>
        <div class="prt-proc">${esc(p.process)} · ${esc(p.device ?? "")}</div>
      </div>
      <div class="dev-meta">
        ${
          tunnel
            ? `<span class="prt-route">→ localhost:${tunnel.localPort}${
                tunnel.remapped ? " (remapped)" : ""
              }</span>
               <button class="btn btn-tertiary btn-sm" data-untunnel="${p.port}">Disconnect</button>`
            : local
              ? `<span class="tx-micro">here</span>`
              : `<button class="btn btn-secondary btn-sm" data-tunnel="${p.port}">Link</button>`
        }
      </div>
    </div>`;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = n / 1024;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  return `${value.toFixed(1)} ${units[i]}`;
}

/**
 * The trace: what the daemon did, so a failure can be explained.
 *
 * Oldest first, because a trace reads like a story. The hint is the part that
 * saves a round of guessing, so it is never collapsed away.
 */
/**
 * Port Lens.
 *
 * Both machines routinely listen on the same port numbers — ARDAgent on 3283,
 * a dev server on 5000 — so a single flat list makes it genuinely hard to tell
 * whose port you are looking at, and which "Link" button does what. The scopes
 * split that; the All tab keeps the device column so duplicates stay legible.
 */
/**
 * The Health tab: both machines at a glance, and the daemon's log.
 *
 * Answering "are both ends up?" used to mean running doctor here, walking to
 * the other machine, and running it again — and when the two could not see
 * each other, neither report said whether the far end was even alive.
 * Diagnosing anything past that meant opening Terminal and tailing a file.
 */
function healthView(): string {
  const h = state.health;
  if (!h) {
    return `<div class="es"><div class="es-title">Checking…</div>
      <div class="es-msg">Asking both machines how they are.</div></div>`;
  }

  const side = (s: HealthSide | undefined, who: string): string => {
    if (!s) {
      return `<div class="cd"><div class="cd-head"><div class="cd-title">${esc(who)}</div></div>
        <div class="cd-body"><div class="tx-caption">Nothing paired yet.</div></div></div>`;
    }
    const checks = (s.checks ?? [])
      .map(
        (c: HealthCheck) => `
        <div class="hl-check hl-${esc(c.severity)}">
          <span class="hl-mark" aria-hidden="true"></span>
          <div class="hl-text">
            <div class="hl-name">${esc(c.name)}</div>
            <div class="tx-micro">${esc(c.detail)}</div>
            ${c.fix ? `<div class="hl-fix">${esc(c.fix)}</div>` : ""}
          </div>
        </div>`,
      )
      .join("");

    return `
      <div class="cd">
        <div class="cd-head">
          <div class="cd-title">${esc(who)}</div>
          <span class="${s.reachable ? "sl sl-live" : "sl sl-unreachable"}">
            <span class="dot"></span>${s.reachable ? "reachable" : "unreachable"}
          </span>
        </div>
        <div class="cd-body">
          <div class="hl-row"><span class="tx-micro">Name</span><span>${esc(s.name || "—")}</span></div>
          ${s.version ? `<div class="hl-row"><span class="tx-micro">Version</span><span class="mono">${esc(s.version)}</span></div>` : ""}
          ${s.os ? `<div class="hl-row"><span class="tx-micro">System</span><span class="mono">${esc(s.os)}/${esc(s.arch ?? "")}</span></div>` : ""}
          ${
            s.addresses && s.addresses.length > 0
              ? `<div class="hl-row"><span class="tx-micro">Address</span><span class="mono">${esc(s.addresses[0] ?? "")}${s.port ? ":" + s.port : ""}</span></div>`
              : ""
          }
          ${s.unreachable ? `<div class="hl-why">${esc(s.unreachable)}</div>` : ""}
          ${checks}
        </div>
      </div>`;
  };

  const logs = state.logs.length
    ? `<pre class="hl-log" id="hl-log">${esc(state.logs.join("\n"))}</pre>`
    : `<div class="tx-caption" style="padding:var(--s-3)">No log yet.</div>`;

  return `
    <div class="hl-sides">
      ${side(h.here, "This Mac")}
      ${h.paired ? side(h.there, h.there?.name || "The other Mac") : ""}
    </div>
    <div class="cd">
      <div class="cd-head">
        <div class="cd-title">Daemon log</div>
        <button class="btn btn-tertiary btn-sm" data-action="logs-refresh"${state.logsBusy ? " disabled" : ""}>
          ${state.logsBusy ? "Reading…" : "Refresh"}
        </button>
        <button class="btn btn-tertiary btn-sm" data-action="logs-copy">Copy</button>
      </div>
      <div class="cd-body" style="padding:0">${logs}</div>
    </div>`;
}

/**
 * The Shell tab.
 *
 * A terminal-shaped pane over a long-lived shell on the other machine. Not a
 * TTY: there is no terminal device, so no vim, no top, no password prompts.
 * What it is, is a shell that remembers — `cd` and `export` carry between
 * commands, which is the thing `ibeji run` cannot do.
 */
function shellControls(): string {
  const tabs = state.shells
    .map((t) => {
      const on = t.id === state.activeShell;
      const label = t.done ? `${t.id} · ended` : t.id;
      return `<button class="seg-item${on ? " is-active" : ""}" data-shell-tab="${esc(t.id)}"
                title="${esc(t.cwd)}">${esc(label)}</button>`;
    })
    .join("");

  return `
    <div class="trc-controls">
      <div class="seg">${tabs || `<span class="tx-micro" style="padding:0 8px">No shells yet</span>`}</div>
      <button class="btn btn-secondary btn-sm" data-action="shell-new"${state.shellBusy ? " disabled" : ""}
              title="Open another shell on the other machine">${state.shellBusy ? "Opening…" : "+"}</button>
      ${
        state.activeShell
          ? `<button class="btn btn-tertiary btn-sm" data-action="shell-interrupt"
                     title="Stop the running command (Ctrl-C)">Stop</button>
             <button class="btn btn-tertiary btn-sm" data-action="shell-close"
                     title="Close this shell and everything it started">Close</button>`
          : ""
      }
    </div>`;
}

function shellView(): string {
  const t = state.shells.find((x) => x.id === state.activeShell);
  if (!t) {
    return `
      <div class="es">
        <div class="es-title">No shell open</div>
        <div class="es-msg">A shell runs on your other Mac and keeps its working
        directory and environment between commands — unlike a one-off
        <code>ibeji run</code>. It is not a full terminal: no vim, no top, no
        password prompts.</div>
        <button class="btn btn-primary btn-sm" data-action="shell-new"${state.shellBusy ? " disabled" : ""}>
          ${state.shellBusy ? "Opening…" : "Open a shell"}
        </button>
      </div>`;
  }

  return `
    <div class="sh-pane">
      <pre class="sh-out" id="sh-out">${esc(t.text)}</pre>
      <div class="sh-input-row">
        <span class="sh-prompt" aria-hidden="true">$</span>
        <input class="sh-input" id="sh-input" type="text" autocomplete="off"
               spellcheck="false" data-shell-input
               placeholder="${t.done ? "This shell has ended" : "Type a command and press Return"}"
               ${t.done ? "disabled" : ""}>
      </div>
      <div class="sh-foot">
        <span class="tx-micro">${esc(t.cwd)}</span>
        <span class="tx-micro">${t.done ? "ended" : "Ctrl-C stops the running command"}</span>
      </div>
    </div>`;
}

/** The ports controls, rendered outside the scroll area so they stay put. */
function portsControls(): string {
  const selfName = state.self?.name ?? "";
  const theirName =
    state.devices.find((d) => d.name !== selfName)?.name ?? "the other Mac";
  const mine = state.ports.filter((p) => p.device === selfName);
  const theirs = state.ports.filter((p) => p.device !== selfName);
  return `
    <div class="trc-controls">
      <div class="seg">
        <button class="seg-item${state.portScope === "all" ? " is-active" : ""}"
                data-port-scope="all">All <span class="seg-n">${state.ports.length}</span></button>
        <button class="seg-item${state.portScope === "mine" ? " is-active" : ""}"
                data-port-scope="mine">This Mac <span class="seg-n">${mine.length}</span></button>
        <button class="seg-item${state.portScope === "theirs" ? " is-active" : ""}"
                data-port-scope="theirs">${esc(theirName)} <span class="seg-n">${theirs.length}</span></button>
      </div>
      <input class="fld fld-sm trc-search" type="search" id="port-search"
             placeholder="Filter by port, service or process…"
             value="${esc(state.portQuery)}" data-port-query>
    </div>`;
}

/** The trace controls, likewise pinned above the scrolling list. */
function traceControls(): string {
  return `
    <div class="trc-controls">
      <div class="seg">
        <button class="seg-item${state.traceErrorsOnly ? "" : " is-active"}"
                data-trace-filter="all">Everything</button>
        <button class="seg-item${state.traceErrorsOnly ? " is-active" : ""}"
                data-trace-filter="errors">Failures only</button>
      </div>
      <input class="fld fld-sm trc-search" type="search" id="trace-search"
             placeholder="Filter by call, device, error or payload…"
             value="${esc(state.traceQuery)}" data-trace-query>
      ${
        state.traceOpen.size > 0
          ? `<button class="btn btn-tertiary btn-sm" data-action="trace-collapse">Collapse all</button>`
          : ""
      }
    </div>`;
}

function portsView(): string {
  const byPort = new Map(state.tunnels.map((t) => [t.remotePort, t]));
  const selfName = state.self?.name ?? "";
  const theirName =
    state.devices.find((d) => d.name !== selfName)?.name ?? "the other Mac";

  const mine = state.ports.filter((p) => p.device === selfName);
  const theirs = state.ports.filter((p) => p.device !== selfName);

  const scoped =
    state.portScope === "mine" ? mine : state.portScope === "theirs" ? theirs : state.ports;

  const q = state.portQuery.trim().toLowerCase();
  const rows = q
    ? scoped.filter((p) =>
        [String(p.port), p.service, p.process, p.device]
          .join(" ")
          .toLowerCase()
          .includes(q),
      )
    : scoped;

  if (rows.length === 0) {
    const why = q
      ? `Nothing matches “${esc(state.portQuery)}”.`
      : state.portScope === "theirs"
        ? "Nothing is listening on the other machine, or it is unreachable."
        : state.portScope === "mine"
          ? "Nothing is listening on this Mac."
          : "No dev servers or databases are running on either machine.";
    return `<div class="es"><div class="es-title">Nothing listening</div>
         <div class="es-msg">${why}</div></div>`;
  }

  return (
    `<div class="cd"><div class="cd-body" style="padding:0">${rows
      .map((p) => portView(p, byPort.get(p.port), p.device === selfName))
      .join("")}</div></div>` +
    (state.portMeta?.remoteStale && state.portMeta.reason
      ? `<div class="banner banner-stale">${esc(state.portMeta.reason)}</div>`
      : "")
  );
}

/** Render a payload for reading: pretty JSON, bounded so one huge body cannot
 *  push the rest of the trace off screen. */
function payloadBlock(label: string, value: unknown): string {
  if (value === undefined || value === null || value === "") return "";
  let text: string;
  try {
    text = typeof value === "string" ? value : JSON.stringify(value, null, 2);
  } catch {
    text = String(value);
  }
  if (!text || text === "{}" || text === "null") return "";
  const CAP = 4000;
  const clipped = text.length > CAP;
  if (clipped) text = text.slice(0, CAP) + "\n… " + (text.length - CAP) + " more characters";
  return `
    <div class="trc-field">
      <div class="trc-label">${esc(label)}</div>
      <pre class="trc-pre">${esc(text)}</pre>
    </div>`;
}

/** A stable key for a row, so an expanded row stays open across the 5s poll. */
function traceKey(e: TraceEvent, i: number): string {
  // The daemon's row id when there is one. The index fallback is only for a
  // peer too old to send ids, and it is unstable by nature — a row keyed by
  // position changes identity the moment anything is prepended.
  return e.id !== undefined ? String(e.id) : `${e.at}#${e.type}#${i}`;
}

function traceView(): string {
  const q = state.traceQuery.trim().toLowerCase();
  const all = [...state.trace].reverse();

  // Keep rows you have open, even once the window has moved past them.
  //
  // The trace is the newest 60 events. On a busy daemon that window turns
  // over in seconds, so a row opened to read its payload was simply gone by
  // the next poll — it looked like the row had slammed shut on its own, and
  // reading anything during activity was impossible. Pinning them keeps the
  // thing you are looking at on screen until you close it.
  for (const e of all) {
    const k = traceKey(e, -1);
    if (state.traceOpen.has(k)) state.tracePinned.set(k, e);
  }
  for (const [k, e] of state.tracePinned) {
    if (!state.traceOpen.has(k)) {
      state.tracePinned.delete(k);
      continue;
    }
    if (!all.some((x) => traceKey(x, -1) === k)) all.push(e);
  }
  // Newest first, with any pinned stragglers back in their place.
  all.sort((a, b) => new Date(b.at).getTime() - new Date(a.at).getTime());

  // Match across everything the row can show, payloads included — the point
  // of a trace is finding the one call that went wrong.
  const rows = q
    ? all.filter((e) => {
        const hay = [
          e.type, e.device, e.provenance, e.code, e.error, e.hint,
          typeof e.request === "string" ? e.request : JSON.stringify(e.request ?? ""),
          typeof e.response === "string" ? e.response : JSON.stringify(e.response ?? ""),
        ].join(" ").toLowerCase();
        return hay.includes(q);
      })
    : all;

  if (rows.length === 0) {
    const why = q
      ? `Nothing matches “${esc(state.traceQuery)}”.`
      : state.traceErrorsOnly
        ? "Nothing has failed in the last hour."
        : "No activity in the last hour.";
    return `<div class="es"><div class="es-title">Nothing to show</div>
         <div class="es-msg">${why}</div></div>`;
  }

  const count =
    q || state.traceErrorsOnly
      ? `<div class="trc-count">${rows.length} of ${all.length}</div>`
      : "";

  return (
    count +
    `<div class="cd"><div class="cd-body" style="padding:0">${rows
      .map((e, i) => {
        const key = traceKey(e, i);
        const open = state.traceOpen.has(key);
        const detail = open
          ? `<div class="trc-detail">
               ${payloadBlock("When", new Date(e.at).toLocaleString())}
               ${e.type ? payloadBlock("Call", e.type) : ""}
               ${e.provenance ? payloadBlock("Asked by", e.provenance) : ""}
               ${e.device ? payloadBlock("Device", e.device) : ""}
               ${e.durationMs ? payloadBlock("Took", e.durationMs + " ms") : ""}
               ${e.code ? payloadBlock("Error code", e.code) : ""}
               ${e.error ? payloadBlock("Error", e.error) : ""}
               ${e.hint ? payloadBlock("Hint", e.hint) : ""}
               ${payloadBlock("Request", e.request)}
               ${payloadBlock("Response", e.response)}
             </div>`
          : "";
        return `
      <div class="trc-row${open ? " is-open" : ""}">
        <button class="trc-summary" data-trace-row="${esc(key)}"
                aria-expanded="${open ? "true" : "false"}">
          <span class="trc-chev" aria-hidden="true"></span>
          <span class="run-ts">${esc(new Date(e.at).toLocaleTimeString())}</span>
          <span class="run-dev">${esc(e.type)}</span>
          <span class="trc-mid">
            ${e.provenance ? `<span class="tg tg-record">${esc(e.provenance)}</span>` : ""}
            ${e.device ? `<span class="tx-micro">${esc(e.device)}</span>` : ""}
            ${e.durationMs ? `<span class="tx-micro">${e.durationMs}ms</span>` : ""}
            ${e.error ? `<span class="tx-caption">${esc(e.error)}</span>` : ""}
          </span>
          ${e.code ? `<span class="bdg bdg-attention">${esc(e.code)}</span>` : ""}
        </button>
        ${detail}
      </div>`;
      })
      .join("")}</div></div>`
  );
}

function tabsView(): string {
  const tabs: [Tab, string][] = [
    ["devices", "Devices"],
    ["spaces", "Spaces"],
    ["ports", "Ports"],
    ["trace", "Trace"],
    ["shell", "Shell"],
    ["health", "Health"],
  ];
  return `<div class="seg">${tabs
    .map(
      ([id, label]) =>
        `<button class="seg-item${state.tab === id ? " is-active" : ""}" data-tab="${id}">${label}</button>`,
    )
    .join("")}</div>`;
}

/**
 * The update banner.
 *
 * Only shown when there is genuinely something newer: a "you are up to date"
 * bar is noise on every launch. A failed check says nothing here either — it
 * answers the Check for updates button instead, where someone is waiting for
 * a reply.
 */
/**
 * The version on the check-for-updates button.
 *
 * A released build is "v0.1.3". A local one carries a git SHA — "522d5ba-dirty"
 * — which is meaningless on a button and overflows it; those read "dev".
 */
function versionLabel(version: string | undefined): string {
  if (!version) return "Updates";
  return /^\d+(\.\d+)*$/.test(version.split("-")[0] ?? "") ? `v${version}` : "dev";
}

function updateView(): string {
  const u = state.update;
  if (!u || u.state !== "available") return "";
  return `
    <div class="banner banner-attention upd">
      <div class="upd-text">
        <strong>Ìbejì ${esc(u.version)} is available.</strong>
        ${u.notes ? ` ${esc(u.notes)}` : ""}
      </div>
      <div class="upd-acts">
        <button class="btn btn-secondary btn-sm" data-action="update-dismiss">Later</button>
        <button class="btn btn-primary btn-sm" data-action="update-run"${state.updateBusy ? " disabled" : ""}>
          ${state.updateBusy ? "Opening Terminal…" : "Update now"}
        </button>
      </div>
    </div>`;
}

function noticeView(): string {
  if (!state.notice) return "";
  const n = state.notice;
  return `<div class="banner banner-${n.tone === "live" ? "live" : "attention"}">${esc(n.text)}</div>`;
}

function render(): void {
  const root = document.getElementById("root");
  if (!root) return;

  if (state.daemonDown) {
    root.innerHTML = `<div class="app-titlebar-pad"></div><main class="app-main">${daemonDownView()}</main>`;
    return;
  }

  const self = state.self;
  const paired = state.devices[0];

  // This machine's name and key fingerprint are context for the Devices tab,
  // where you are comparing two machines. On Ports, Spaces, Trace or Shell
  // they are three lines of fixed furniture above the thing you came to read,
  // so they collapse to the status chips alone.
  const onDevices = state.tab === "devices";
  const header = `
    <header class="app-header${onDevices ? "" : " is-compact"}">
      ${
        onDevices
          ? `<div>
               <div class="tx-overline">This device</div>
               <div class="dev-name">${esc(self?.name ?? "…")}</div>
               <div class="dev-addr">${esc(self?.fingerprint ?? "")}</div>
             </div>`
          : `<div class="hdr-mini">
               <span class="dev-name-sm">${esc(self?.name ?? "…")}</span>
             </div>`
      }
      <div class="legend">
        <span class="legend-daemon"><span class="legend-sw" style="background:var(--live)"></span>
          <span class="tx-micro">daemon · alive</span></span>
        <button class="btn btn-tertiary btn-sm" data-action="agent-setup"
                title="Register Ìbejì's MCP server, or copy a briefing for your agent">For agents</button>
        <button class="btn btn-secondary btn-sm" data-action="update-check"
                title="Check GitHub for a newer version">${esc(versionLabel(self?.version))}</button>
      </div>
    </header>`;

  // A waiting prompt outranks every other view: it blocks a caller on the
  // other machine, and it expires in sixty seconds.
  if (state.prompts.length > 0) {
    root.innerHTML = `
      <div class="app-titlebar-pad"></div>
      ${header}
      ${updateView()}
      ${noticeView()}
      <main class="app-main">${state.prompts.map(promptView).join("")}</main>`;
    return;
  }

  let body: string;
  if (state.tab === "spaces") {
    // The create action lives beside the list, not only in an empty state:
    // a second space is as ordinary as the first, and until now there was no
    // way to make either one from the window at all.
    const createBtn = `
      <button class="btn btn-primary btn-sm" data-action="create-space">New space…</button>`;
    body =
      state.spaces.length > 0
        ? state.spaces.map(spaceView).join("") +
          `<div class="cd-more">${createBtn}</div>`
        : `<div class="es">
             <div class="es-title">No spaces</div>
             <div class="es-msg">A space keeps a folder identical on both machines.
             One side is master and wins conflicts; the other keeps a full copy
             on disk.</div>
             ${createBtn}
           </div>`;
  } else if (state.tab === "ports") {
    body = portsView();
  } else if (state.tab === "trace") {
    body = traceView();
  } else if (state.tab === "shell") {
    body = shellView();
  } else if (state.tab === "health") {
    body = healthView();
  } else if (state.pairing) {
    body = pairingView(state.pairing);
  } else if (paired) {
    body = `
      <div class="cd">
        <div class="cd-head"><div class="cd-title">Paired device</div></div>
        <div class="cd-body">${deviceRow(paired)}</div>
        <div class="cd-foot">
          <button class="btn btn-tertiary btn-sm" data-action="toggle-mode" data-device="${esc(paired.id)}">
            Switch to ${paired.mode === "bypass" ? "Prompt" : "Bypass"}
          </button>
          ${
            paired.status === "live"
              ? ""
              : `<button class="btn btn-primary btn-sm" data-action="reconnect"
                         title="The other Mac moved, or is on a new address">Reconnect…</button>`
          }
          <button class="btn btn-secondary btn-sm" data-action="unpair" data-device="${esc(paired.id)}"
                  data-name="${esc(paired.name)}">Unpair…</button>
        </div>
      </div>`;
  } else if (state.candidates.length > 0) {
    // Discovery found something, but the address and the manual route stay
    // available: the machine on screen is not always the one you meant, and
    // a second Mac may still be arriving. Hiding them the moment anything
    // appears is how the address became unreachable in the first place.
    body = `
      <div class="cd">
        <div class="cd-head"><div class="cd-title">On this network</div>
          <div class="cd-count">${state.candidates.length}</div></div>
        <div class="cd-body">${state.candidates.map(candidateRow).join("")}</div>
      </div>
      <div class="cd-more">
        <button class="btn btn-secondary btn-sm" data-action="add-by-ip">Add by IP address</button>
        ${selfAddressView()}
      </div>`;
  } else {
    // The hint comes from the daemon, which is what knows *why* the list is
    // empty: a silent network and a network where the other machine is simply
    // off need different advice.
    body = `
      <div class="es">
        <div class="es-title">No devices paired</div>
        <div class="es-msg">${esc(state.hint ?? "Looking for your other machine…")}</div>
        <button class="btn btn-secondary btn-sm" data-action="add-by-ip">Add by IP address</button>
        ${selfAddressView()}
      </div>`;
  }

  // A view's own controls sit between the tabs and the scroll area, not
  // inside it. Scrolling a long list must not carry the filter you are
  // typing into off the top of the window.
  let controls = "";
  if (state.devices.length > 0) {
    if (state.tab === "ports") controls = portsControls();
    else if (state.tab === "trace") controls = traceControls();
    else if (state.tab === "shell") controls = shellControls();
  }

  root.innerHTML = `
    <div class="app-titlebar-pad"></div>
    ${header}
    ${state.devices.length > 0 ? `<div class="app-tabs">${tabsView()}</div>` : ""}
    ${updateView()}
    ${noticeView()}
    ${controls ? `<div class="app-controls">${controls}</div>` : ""}
    <main class="app-main">${body}</main>`;
}

// --- dialogs ----------------------------------------------------------------

/**
 * An in-app modal, because Electron has no window.prompt or window.confirm.
 *
 * Chromium disables both in Electron: window.prompt() *throws*
 * "prompt() is not supported", which means a click handler calling it dies on
 * that line and the rest of the handler never runs. The symptom is a button
 * that does nothing at all — no dialog, no error, no clue. "Add by IP address"
 * and "Unpair…" were both dead this way, and window.confirm returns false
 * rather than throwing, so Bypass silently refused instead.
 *
 * Resolves to the typed string, or undefined when cancelled. A confirm is the
 * same thing with no field, resolving to "" for OK.
 */
function ask(opts: {
  title: string;
  body: string;
  field?: { label: string; placeholder?: string };
  confirmLabel: string;
  critical?: boolean;
  gravity?: string;
}): Promise<string | undefined> {
  return new Promise((resolve) => {
    const host = document.createElement("div");
    host.className = "app-modal-host";
    host.innerHTML = `
      <div class="ovl-scrim"></div>
      <div class="mdl${opts.critical ? " is-critical" : ""}" role="dialog" aria-modal="true"
           aria-label="${esc(opts.title)}">
        <div class="mdl-head">
          <h2 class="mdl-title">${esc(opts.title)}</h2>
          <p class="mdl-sub">${esc(opts.body)}</p>
        </div>
        ${
          opts.field
            ? `<div class="mdl-body">
                 <div class="fld-wrap">
                   <label class="fld-label" for="mo-input">${esc(opts.field.label)}</label>
                   <input class="fld" id="mo-input" type="text" autocomplete="off"
                          autocapitalize="off" spellcheck="false"
                          placeholder="${esc(opts.field.placeholder ?? "")}">
                 </div>
               </div>`
            : ""
        }
        <div class="mdl-foot">
          ${opts.gravity ? `<span class="gravity">${esc(opts.gravity)}</span>` : ""}
          <button class="btn btn-secondary btn-sm" data-mo="cancel">Cancel</button>
          <button class="btn btn-primary btn-sm${opts.critical ? " is-destructive" : ""}"
                  data-mo="ok">${esc(opts.confirmLabel)}</button>
        </div>
      </div>`;
    document.body.appendChild(host);

    const input = host.querySelector<HTMLInputElement>("#mo-input");
    input?.focus();

    let done = false;
    const close = (value: string | undefined) => {
      if (done) return;
      done = true;
      document.removeEventListener("keydown", onKey, true);
      host.remove();
      resolve(value);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close(undefined);
      // Enter submits only from the field. A stray Enter after tabbing would
      // otherwise confirm something irreversible.
      if (e.key === "Enter" && document.activeElement === input) close(input?.value ?? "");
    };
    document.addEventListener("keydown", onKey, true);
    host.addEventListener("click", (e) => {
      const act = (e.target as HTMLElement).closest("[data-mo]")?.getAttribute("data-mo");
      if (act === "ok") close(input ? input.value : "");
      else if (act === "cancel") close(undefined);
      else if ((e.target as HTMLElement).classList.contains("ovl-scrim")) close(undefined);
    });
  });
}

// --- intents ----------------------------------------------------------------

async function startPairing(address: string): Promise<void> {
  const res = await window.ibeji.startPairing(address);
  unwrap(res);
  await refresh();
}

async function confirmPairing(): Promise<void> {
  const p = state.pairing;
  if (!p) return;
  const res = await window.ibeji.confirmPairing(p.id);
  if (unwrap(res)) {
    state.pairing = undefined;
    notify("Paired. The link will come up in a moment.", "live");
  }
  await refresh();
}

async function cancelPairing(): Promise<void> {
  const p = state.pairing;
  if (!p) return;
  await window.ibeji.cancelPairing(p.id);
  state.pairing = undefined;
  await refresh();
}

/**
 * Unpairing is one of the four places oxblood is spent, and it is guarded by
 * typed confirmation of the device name — the PRD requires it to revoke the
 * pinned key and close every tunnel, sync and session at once.
 */
async function unpair(deviceId: string, name: string): Promise<void> {
  const typed = await ask({
    title: `Unpair ${name}?`,
    body:
      `This revokes its key and closes every tunnel, sync and session at once. ` +
      `To pair again you repeat the six-digit ceremony on both machines.`,
    field: { label: `Type ${name} to confirm`, placeholder: name },
    confirmLabel: "Unpair",
    critical: true,
    gravity: "Irreversible",
  });
  if (typed !== name) {
    if (typed !== undefined) notify("The name did not match. Nothing was unpaired.", "attention");
    return;
  }
  const res = await window.ibeji.unpair(deviceId);
  if (res.ok) notify(`Unpaired ${name}.`, "irreversible");
  else unwrap(res);
  await refresh();
}

/**
 * Flipping to Bypass authorises every future command from that machine, so it
 * is never a one-click toggle. The design system guards it with hold-to-confirm;
 * until that control exists here, typed confirmation carries the same weight.
 */
async function toggleMode(deviceId: string): Promise<void> {
  const current = state.devices[0]?.mode;
  const next = current === "bypass" ? "prompt" : "bypass";

  if (next === "bypass") {
    const ok = await ask({
      title: "Switch to Bypass mode?",
      body:
        "Bypass runs every future command from that machine without asking. " +
        "It is not destructive now — it is destructive later, on every command " +
        "that follows.",
      confirmLabel: "Switch to Bypass",
      critical: true,
      gravity: "Affects every future command",
    });
    if (ok === undefined) return;
  }
  const res = await window.ibeji.setMode(deviceId, next);
  if (res.ok) notify(`Now in ${next === "bypass" ? "Bypass" : "Prompt"} mode.`, next === "bypass" ? "attention" : "live");
  else unwrap(res);
  await refresh();
}

async function answerPrompt(id: string, accept: boolean, remember: boolean): Promise<void> {
  const res = await window.ibeji.answerPrompt(id, accept, remember);
  if (res.ok) {
    notify(
      accept ? (remember ? "Allowed, and remembered." : "Allowed.") : "Declined.",
      accept ? "live" : "attention",
    );
  } else {
    unwrap(res);
  }
  await refresh();
}

async function resolveConflict(space: string, path: string, promote: boolean): Promise<void> {
  const res = promote
    ? await window.ibeji.promote(space, path)
    : await window.ibeji.discard(space, path);
  if (res.ok) {
    notify(promote ? "Your version was promoted." : "Your version was discarded.", "live");
  } else {
    unwrap(res);
  }
  await refresh();
}

async function openTunnel(port: number): Promise<void> {
  const res = await window.ibeji.openTunnel(port);
  const value = unwrap(res);
  if (value) {
    // Always report the local port: assuming it matched is how an hour goes
    // missing wondering why the wrong app is answering.
    notify(
      value.tunnel.remapped
        ? `Port ${port} is taken here — it is on localhost:${value.tunnel.localPort}.`
        : `localhost:${value.tunnel.localPort} now serves it.`,
      value.tunnel.remapped ? "attention" : "live",
    );
  }
  await refresh();
}

async function closeTunnel(port: number): Promise<void> {
  unwrap(await window.ibeji.closeTunnel(port));
  await refresh();
}

async function addByIP(): Promise<void> {
  const address = await ask({
    title: "Add by IP address",
    body:
      "Multicast is often blocked on corporate and guest Wi-Fi, so the other " +
      "machine may not appear by itself. Its address is shown in the Ìbejì " +
      "window on that Mac.",
    field: { label: "Address", placeholder: "192.168.1.42" },
    confirmLabel: "Pair",
  });
  if (address === undefined) return;
  const trimmed = address.trim();
  if (!trimmed) {
    notify("Enter the other machine's IP address.", "attention");
    return;
  }
  await startPairing(trimmed);
}

/**
 * Create a space.
 *
 * Three questions, asked one at a time with the existing single-field modal,
 * rather than one wide form: the folder comes from the OS picker (typing an
 * absolute path is how you create a space over the wrong directory), and the
 * role needs explaining at the moment it is chosen, not in a tooltip.
 */
async function createSpace(): Promise<void> {
  const name = await ask({
    title: "New space",
    body:
      "A space keeps one folder identical on both machines — real files on " +
      "each disk, not a network mount. Give it a short name; both machines " +
      "use the same one.",
    field: { label: "Name", placeholder: "atlas" },
    confirmLabel: "Next",
  });
  if (name === undefined) return;
  const trimmed = name.trim();
  if (!trimmed) {
    notify("A space needs a name.", "attention");
    return;
  }

  const picked = await window.ibeji.pickFolder();
  const path = unwrap(picked);
  if (!path) return; // cancelled in the OS dialog

  // Master wins conflicts, so which side holds it is a real decision. For a
  // repository it should be the machine with the git credentials — that makes
  // master-wins the correct outcome rather than a surprise.
  const role = await ask({
    title: "Which machine is master?",
    body:
      `If the same file changes on both machines before syncing, the master's ` +
      `version wins and the other is kept beside it as a conflict copy. For a ` +
      `code repository, master should be the machine that owns the git ` +
      `credentials.\n\nFolder: ${path}`,
    field: { label: "Role for this Mac", placeholder: "master" },
    confirmLabel: "Create",
  });
  if (role === undefined) return;
  const r = role.trim().toLowerCase() === "replica" ? "replica" : "master";

  const res = await window.ibeji.createSpace({
    name: trimmed,
    path,
    role: r,
    mode: "two-way",
  });
  const out = unwrap(res);
  if (!out) {
    notify(res.ok ? "Could not create the space." : res.error.message, "attention");
    return;
  }

  await refresh();
  const other = r === "master" ? "replica" : "master";
  notify(
    `Created ${trimmed}. On the other Mac run: ibeji spaces create ${trimmed} <folder> --role ${other}`,
    "live",
  );
  // The daemon's .git warning is not advisory noise — syncing .git corrupts
  // repositories — so it is surfaced rather than swallowed.
  if (out.warning) notify(out.warning, "attention");
}

/**
 * Tell an agent about this install.
 *
 * An MCP server has shipped inside the bundle since M3 and the product never
 * mentioned it anywhere, so the feature built specifically for agents was the
 * hardest one to discover. This offers both routes: the registration command
 * (after which the agent discovers the tools itself and nothing goes stale),
 * and a pasteable briefing for agents without MCP.
 */
async function agentSetup(): Promise<void> {
  const cmdRes = await window.ibeji.mcpCommand();
  const cmd = unwrap(cmdRes) ?? "";
  const briefRes = await window.ibeji.agentBriefing();
  const brief = unwrap(briefRes) ?? "";

  const choice = await ask({
    title: "Connect your agent",
    body:
      "Ìbejì ships an MCP server, so an agent can read, search, run commands " +
      "and open tunnels on your other Mac — through the same permission gate " +
      "you use.\n\nRegister it:\n\n" +
      cmd +
      "\n\nType 'command' to copy that, or 'briefing' to copy a longer " +
      "description you can paste into any agent.",
    field: { label: "Copy", placeholder: "command" },
    confirmLabel: "Copy",
  });
  if (choice === undefined) return;

  const wantBriefing = choice.trim().toLowerCase().startsWith("b");
  const text = wantBriefing ? brief : cmd;
  if (!text) {
    notify("Nothing to copy.", "attention");
    return;
  }
  void navigator.clipboard
    .writeText(text)
    .then(() => notify(wantBriefing ? "Briefing copied." : "Command copied.", "live"))
    .catch(() => notify("Could not copy to the clipboard.", "attention"));
}

/**
 * Tell the daemon where the other Mac went.
 *
 * A changed address is not a trust event — the keys stay pinned — so this
 * must not drag the user back through the six-digit ceremony. Until it
 * existed, an unreachable device offered only "Unpair", so working around a
 * network problem meant throwing away a working pairing.
 */
async function reconnectPeer(): Promise<void> {
  const current = state.devices[0];
  const address = await ask({
    title: "Reconnect",
    body:
      `${current?.name ?? "The other Mac"} is not answering at ` +
      `${current?.address ?? "its last address"}. If it moved to a new address, ` +
      `type it here — you will not have to pair again, because its key is already ` +
      `trusted.\n\nThe address is shown in the Ìbejì window on that Mac.`,
    field: { label: "Address", placeholder: "192.168.1.42" },
    confirmLabel: "Reconnect",
  });
  if (address === undefined) return;
  const trimmed = address.trim();
  if (!trimmed) {
    notify("Enter the other machine's address.", "attention");
    return;
  }
  const res = await window.ibeji.reconnect(trimmed);
  if (!res.ok) {
    notify(res.error.message, "attention");
    return;
  }
  await refresh();
  notify(`Reconnected to ${res.value.device.name}.`, "live");
}

/** Read the tail of this daemon's log. */
async function loadLogs(): Promise<void> {
  if (state.logsBusy) return;
  state.logsBusy = true;
  render();
  try {
    const res = await window.ibeji.logs(400);
    const got = unwrap(res);
    state.logs = got?.lines ?? [];
    if (!got && !res.ok) notify(res.error.message, "attention");
  } finally {
    state.logsBusy = false;
    render();
  }
}

// --- the Shell tab ---------------------------------------------------------

/** Open another shell on the other machine and switch to it. */
async function openShell(): Promise<void> {
  if (state.shellBusy) return;
  state.shellBusy = true;
  render();
  try {
    const res = await window.ibeji.shellOpen();
    const info = unwrap(res);
    if (!info) {
      notify(res.ok ? "Could not open a shell." : res.error.message, "attention");
      return;
    }
    const tab: ShellTab = {
      id: info.shell.id,
      cwd: info.shell.cwd,
      text: "",
      seq: 0,
      history: [],
      historyAt: -1,
      done: false,
      polling: false,
    };
    state.shells.push(tab);
    state.activeShell = tab.id;
    render();
    focusShellInput();
    void pollShell(tab.id);
  } finally {
    state.shellBusy = false;
    render();
    focusShellInput();
  }
}

/**
 * Follow one shell's output.
 *
 * A long poll rather than a timer: a shell is idle most of the time, and the
 * daemon holds the request until something is said. One poll per tab, so
 * switching tabs does not stack readers on the same shell.
 */
async function pollShell(id: string): Promise<void> {
  const tab = state.shells.find((t) => t.id === id);
  if (!tab || tab.polling) return;
  tab.polling = true;
  try {
    while (state.shells.some((t) => t.id === id) && !tab.done) {
      const res = await window.ibeji.shellRead(id, tab.seq, 8000);
      const got = unwrap(res);
      if (!got) {
        // The link dropped or the shell is gone. Say so in the pane rather
        // than leaving it looking merely quiet.
        if (!res.ok && res.error.code !== "peer_offline") {
          tab.text += `\n[${res.error.message}]\n`;
          tab.done = true;
          render();
          return;
        }
        await new Promise((r) => setTimeout(r, 1500));
        continue;
      }
      if (got.output.length > 0) {
        for (const chunk of got.output) tab.text += chunk.data;
        tab.seq = got.seq;
        // Only redraw when this tab is the one on screen.
        if (state.tab === "shell" && state.activeShell === id) {
          render();
          scrollShellToBottom();
          focusShellInput();
        }
      } else {
        tab.seq = got.seq;
      }
      if (got.done) {
        tab.done = true;
        tab.text += "\n[the shell ended]\n";
        render();
        return;
      }
    }
  } finally {
    tab.polling = false;
  }
}

/** Send what was typed, echoing it so the pane reads like a transcript. */
async function sendShellLine(line: string): Promise<void> {
  const tab = state.shells.find((t) => t.id === state.activeShell);
  if (!tab || tab.done) return;
  const cmd = line.trim();

  // Echo the command. Without a PTY the shell does not echo what it was
  // given, so a pane showing only output reads as though it answered a
  // question nobody asked.
  tab.text += (tab.text.endsWith("\n") || tab.text === "" ? "" : "\n") + "$ " + line + "\n";
  if (cmd !== "") {
    tab.history.push(line);
    if (tab.history.length > 200) tab.history.shift();
  }
  tab.historyAt = -1;
  render();
  scrollShellToBottom();
  focusShellInput();

  const res = await window.ibeji.shellWrite(tab.id, line);
  if (!res.ok) {
    tab.text += `[${res.error.message}]\n`;
    render();
    scrollShellToBottom();
  }
  void pollShell(tab.id);
}

async function interruptShell(): Promise<void> {
  const tab = state.shells.find((t) => t.id === state.activeShell);
  if (!tab) return;
  const res = await window.ibeji.shellInterrupt(tab.id);
  if (!res.ok) notify(res.error.message, "attention");
  else {
    tab.text += "^C\n";
    render();
    scrollShellToBottom();
    focusShellInput();
  }
}

async function closeShell(): Promise<void> {
  const tab = state.shells.find((t) => t.id === state.activeShell);
  if (!tab) return;
  const res = await window.ibeji.shellClose(tab.id);
  if (!res.ok) {
    notify(res.error.message, "attention");
    return;
  }
  state.shells = state.shells.filter((t) => t.id !== tab.id);
  state.activeShell = state.shells[state.shells.length - 1]?.id;
  render();
  focusShellInput();
}

function scrollShellToBottom(): void {
  const el = document.getElementById("sh-out");
  if (el) el.scrollTop = el.scrollHeight;
}

/** Keep the caret in the shell: a terminal you have to click into is wrong. */
function focusShellInput(): void {
  if (state.tab !== "shell") return;
  const el = document.getElementById("sh-input") as HTMLInputElement | null;
  if (el && document.activeElement !== el) el.focus();
}

/**
 * Check GitHub for a newer release.
 *
 * `quiet` is the launch check: it must not interrupt with "you are up to
 * date" or "could not reach GitHub" on a window someone just opened. The
 * button passes false, because a button that says nothing looks broken —
 * which is the bug this screen already had once.
 */
async function checkUpdate(quiet: boolean): Promise<void> {
  const res = await window.ibeji.checkUpdate();
  if (!res.ok) {
    if (!quiet) notify("Could not check for updates.", "attention");
    return;
  }
  state.update = res.value;
  if (!quiet) {
    if (res.value.state === "current") notify(`Ìbejì ${res.value.version} is the latest version.`, "live");
    else if (res.value.state === "unknown") notify("Could not reach GitHub to check for updates.", "attention");
  }
  render();
}

/**
 * Run the installer in Terminal.
 *
 * It replaces /Applications and restarts the daemon, so it runs where it can
 * be watched rather than silently inside the app — and the script quits this
 * app partway through, which a process cannot do to the bundle it is
 * executing from.
 */
async function runUpdate(): Promise<void> {
  state.updateBusy = true;
  render();
  const res = await window.ibeji.runUpdate();
  state.updateBusy = false;
  if (!res.ok) {
    const cmd = await window.ibeji.updateCommand();
    notify(
      cmd.ok ? `Could not open Terminal. Run: ${cmd.value}` : "Could not start the update.",
      "attention",
    );
  } else {
    notify("Updating in Terminal. Ìbejì will close and reopen.", "live");
  }
  render();
}

// --- wiring -----------------------------------------------------------------

document.addEventListener("click", (e) => {
  const el = e.target as HTMLElement;

  const tab = el.closest("[data-tab]")?.getAttribute("data-tab");
  if (tab) {
    state.tab = tab as Tab;
    void refresh();
    return;
  }

  // A trace row opens to show what the call was actually made with. Toggled
  // in local state and keyed by the daemon's row id, so the 5-second poll
  // does not snap an open row shut while it is being read.
  const traceRow = el.closest("[data-trace-row]")?.getAttribute("data-trace-row");
  if (traceRow) {
    if (state.traceOpen.has(traceRow)) state.traceOpen.delete(traceRow);
    else state.traceOpen.add(traceRow);
    render();
    return;
  }

  const shellTab = el.closest("[data-shell-tab]")?.getAttribute("data-shell-tab");
  if (shellTab) {
    state.activeShell = shellTab;
    render();
    focusShellInput();
    void pollShell(shellTab);
    return;
  }

  const portScope = el.closest("[data-port-scope]")?.getAttribute("data-port-scope");
  if (portScope) {
    state.portScope = portScope as "all" | "mine" | "theirs";
    render();
    return;
  }

  const traceFilter = el.closest("[data-trace-filter]")?.getAttribute("data-trace-filter");
  if (traceFilter) {
    state.traceErrorsOnly = traceFilter === "errors";
    void refresh();
    return;
  }

  // Copy this machine's address, so it can be typed on the other Mac without
  // transcription errors. Clipboard writes can reject (no focus, no
  // permission); a silent failure here would look like a dead button, which
  // is the exact bug this screen already had once.
  const copy = el.closest("[data-copy]")?.getAttribute("data-copy");
  if (copy) {
    void navigator.clipboard
      .writeText(copy)
      .then(() => notify(`Copied ${copy}.`, "live"))
      .catch(() => notify(`Could not copy. The address is ${copy}.`, "attention"));
    return;
  }

  const answer = el.closest("[data-answer]");
  if (answer) {
    const id = answer.getAttribute("data-id") ?? "";
    const kind = answer.getAttribute("data-answer");
    void answerPrompt(id, kind !== "decline", kind === "remember");
    return;
  }

  const promote = el.closest("[data-promote]");
  if (promote) {
    void resolveConflict(
      promote.getAttribute("data-space") ?? "",
      promote.getAttribute("data-promote") ?? "",
      true,
    );
    return;
  }
  const discard = el.closest("[data-discard]");
  if (discard) {
    void resolveConflict(
      discard.getAttribute("data-space") ?? "",
      discard.getAttribute("data-discard") ?? "",
      false,
    );
    return;
  }

  const tunnel = el.closest("[data-tunnel]");
  if (tunnel) {
    void openTunnel(Number(tunnel.getAttribute("data-tunnel")));
    return;
  }
  const untunnel = el.closest("[data-untunnel]");
  if (untunnel) {
    void closeTunnel(Number(untunnel.getAttribute("data-untunnel")));
    return;
  }

  const target = el.closest("[data-action],[data-pair]");
  if (!target) return;

  const pairAddress = target.getAttribute("data-pair");
  if (pairAddress) {
    void startPairing(pairAddress);
    return;
  }
  switch (target.getAttribute("data-action")) {
    case "trace-collapse":
      state.traceOpen.clear();
      state.tracePinned.clear();
      render();
      break;
    case "reconnect":
      void reconnectPeer();
      break;
    case "logs-refresh":
      void loadLogs();
      break;
    case "logs-copy":
      void navigator.clipboard
        .writeText(state.logs.join("\n"))
        .then(() => notify(`Copied ${state.logs.length} lines.`, "live"))
        .catch(() => notify("Could not copy the log.", "attention"));
      break;
    case "shell-new":
      void openShell();
      break;
    case "shell-interrupt":
      void interruptShell();
      break;
    case "shell-close":
      void closeShell();
      break;
    case "create-space":
      void createSpace();
      break;
    case "agent-setup":
      void agentSetup();
      break;
    case "confirm-pairing":
      void confirmPairing();
      break;
    case "cancel-pairing":
      void cancelPairing();
      break;
    case "update-run":
      void runUpdate();
      return;
    case "update-dismiss":
      state.update = undefined;
      render();
      return;
    case "update-check":
      void checkUpdate(false);
      return;
    case "unpair":
      void unpair(target.getAttribute("data-device") ?? "", target.getAttribute("data-name") ?? "");
      break;
    case "toggle-mode":
      void toggleMode(target.getAttribute("data-device") ?? "");
      break;
    case "add-by-ip":
      void addByIP();
      break;
  }
});

// The two search fields. These are `input`, not `click`: filtering has to
// happen as you type. The value is kept in state and written back into the
// field on render, and the caret is restored afterwards — re-rendering the
// whole view on every keystroke otherwise sends the cursor to position zero
// on the second character, which makes the field unusable.
document.addEventListener("input", (e) => {
  const el = e.target as HTMLInputElement | null;
  if (!el) return;

  const isTrace = el.hasAttribute("data-trace-query");
  const isPort = el.hasAttribute("data-port-query");
  if (!isTrace && !isPort) return;

  const caret = el.selectionStart;
  if (isTrace) state.traceQuery = el.value;
  else state.portQuery = el.value;

  render();

  const again = document.getElementById(el.id) as HTMLInputElement | null;
  if (again) {
    again.focus();
    if (caret !== null) again.setSelectionRange(caret, caret);
  }
});

// The Shell tab's keyboard.
//
// Return sends, Ctrl-C interrupts, and the arrows walk history — the three
// things a terminal does that a text field does not. Bound at the document
// so the handler survives the re-render after every keystroke.
document.addEventListener("keydown", (e) => {
  const el = e.target as HTMLInputElement | null;
  if (!el || !el.hasAttribute("data-shell-input")) return;

  // Ctrl-C with no selection is an interrupt, not a copy. With a selection it
  // is a copy, because taking that away would be worse than the alternative.
  if (e.key === "c" && e.ctrlKey && !window.getSelection()?.toString()) {
    e.preventDefault();
    void interruptShell();
    return;
  }

  const tab = state.shells.find((t) => t.id === state.activeShell);
  if (!tab) return;

  if (e.key === "Enter") {
    e.preventDefault();
    const line = el.value;
    el.value = "";
    void sendShellLine(line);
    return;
  }

  if (e.key === "ArrowUp") {
    if (tab.history.length === 0) return;
    e.preventDefault();
    tab.historyAt = tab.historyAt < 0 ? tab.history.length - 1 : Math.max(0, tab.historyAt - 1);
    el.value = tab.history[tab.historyAt] ?? "";
    el.setSelectionRange(el.value.length, el.value.length);
    return;
  }

  if (e.key === "ArrowDown") {
    if (tab.historyAt < 0) return;
    e.preventDefault();
    tab.historyAt += 1;
    if (tab.historyAt >= tab.history.length) {
      tab.historyAt = -1;
      el.value = "";
    } else {
      el.value = tab.history[tab.historyAt] ?? "";
    }
    el.setSelectionRange(el.value.length, el.value.length);
  }
});

// Events are a nudge to re-read, never the only copy of a fact: a dropped one
// costs a refresh, not correctness.
window.ibeji.onEvent((_e: DaemonEvent) => {
  void refresh();
});

// A slow poll as a backstop, so a missed event cannot leave the window stale
// indefinitely — and so a daemon that starts after the window is noticed.
setInterval(() => void refresh(), 5000);

void refresh();

// One quiet check at launch. Quiet because a window that opens with "could
// not reach GitHub" over a perfectly working app is worse than a missed
// update; the banner appears only when there is genuinely something newer.
void checkUpdate(true);

// And once a day, for a window left open for weeks — without this, the only
// check is at launch, which a long-lived app never does again.
setInterval(() => void checkUpdate(true), 24 * 60 * 60 * 1000);
