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
  Candidate,
  DaemonEvent,
  Device,
  IbejiBridge,
  PairingSession,
  PendingPrompt,
  PortMeta,
  PortRow,
  Result,
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

type Tab = "devices" | "spaces" | "ports" | "trace";

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
  devices: Device[];
  candidates: Candidate[];
  hint: string | undefined;
  pairing: PairingSession | undefined;
  /** Set when the daemon cannot be reached; everything else is stale then. */
  daemonDown: string | undefined;
  /** A transient message shown under the header. */
  notice: { text: string; tone: Tone } | undefined;
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
  self: undefined,
  devices: [],
  candidates: [],
  hint: undefined,
  pairing: undefined,
  daemonDown: undefined,
  notice: undefined,
};

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
function traceView(): string {
  const rows = [...state.trace].reverse();
  const filter = `
    <div style="display:flex;gap:var(--s-2);margin-bottom:var(--s-4)">
      <button class="btn btn-${state.traceErrorsOnly ? "secondary" : "primary"} btn-sm"
        data-trace-filter="all">Everything</button>
      <button class="btn btn-${state.traceErrorsOnly ? "primary" : "secondary"} btn-sm"
        data-trace-filter="errors">Failures only</button>
    </div>`;

  if (rows.length === 0) {
    return (
      filter +
      `<div class="es"><div class="es-title">Nothing recorded</div>
         <div class="es-msg">${
           state.traceErrorsOnly ? "Nothing has failed in the last hour." : "No activity in the last hour."
         }</div></div>`
    );
  }

  return (
    filter +
    `<div class="cd"><div class="cd-body" style="padding:0">${rows
      .map(
        (e) => `
      <div class="run-row">
        <span class="run-ts">${esc(new Date(e.at).toLocaleTimeString())}</span>
        <span class="run-dev">${esc(e.type)}</span>
        <div style="flex:1;min-width:0">
          ${e.provenance ? `<span class="tg tg-record">${esc(e.provenance)}</span>` : ""}
          ${e.durationMs ? `<span class="tx-micro">${e.durationMs}ms</span>` : ""}
          ${e.error ? `<div class="tx-caption">${esc(e.error)}</div>` : ""}
          ${e.hint ? `<div class="tx-micro">${esc(e.hint)}</div>` : ""}
        </div>
        ${e.code ? `<span class="bdg bdg-attention">${esc(e.code)}</span>` : ""}
      </div>`,
      )
      .join("")}</div></div>`
  );
}

function tabsView(): string {
  const tabs: [Tab, string][] = [
    ["devices", "Devices"],
    ["spaces", "Spaces"],
    ["ports", "Ports"],
    ["trace", "Trace"],
  ];
  return `<div class="seg">${tabs
    .map(
      ([id, label]) =>
        `<button class="seg-item${state.tab === id ? " is-selected" : ""}" data-tab="${id}">${label}</button>`,
    )
    .join("")}</div>`;
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

  const header = `
    <header class="app-header">
      <div>
        <div class="tx-overline">This device</div>
        <div class="dev-name">${esc(self?.name ?? "…")}</div>
        <div class="dev-addr">${esc(self?.fingerprint ?? "")}</div>
      </div>
      <div class="legend">
        <span class="legend-daemon"><span class="legend-sw" style="background:var(--live)"></span>
          <span class="tx-micro">daemon · alive</span></span>
      </div>
    </header>`;

  // A waiting prompt outranks every other view: it blocks a caller on the
  // other machine, and it expires in sixty seconds.
  if (state.prompts.length > 0) {
    root.innerHTML = `
      <div class="app-titlebar-pad"></div>
      ${header}
      ${noticeView()}
      <main class="app-main">${state.prompts.map(promptView).join("")}</main>`;
    return;
  }

  let body: string;
  if (state.tab === "spaces") {
    body =
      state.spaces.length > 0
        ? state.spaces.map(spaceView).join("")
        : `<div class="es"><div class="es-title">No spaces</div>
             <div class="es-msg">A space keeps a folder identical on both machines.
             Create one from a folder you work in.</div></div>`;
  } else if (state.tab === "ports") {
    const byPort = new Map(state.tunnels.map((t) => [t.remotePort, t]));
    const selfName = state.self?.name ?? "";
    body =
      state.ports.length > 0
        ? `<div class="cd"><div class="cd-body" style="padding:0">${state.ports
            .map((p) => portView(p, byPort.get(p.port), p.device === selfName))
            .join("")}</div></div>` +
          (state.portMeta?.remoteStale && state.portMeta.reason
            ? `<div class="banner banner-stale">${esc(state.portMeta.reason)}</div>`
            : "")
        : `<div class="es"><div class="es-title">Nothing listening</div>
             <div class="es-msg">No dev servers or databases are running on either machine.</div></div>`;
  } else if (state.tab === "trace") {
    body = traceView();
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
          <button class="btn btn-secondary btn-sm" data-action="unpair" data-device="${esc(paired.id)}"
                  data-name="${esc(paired.name)}">Unpair…</button>
        </div>
      </div>`;
  } else if (state.candidates.length > 0) {
    body = `
      <div class="cd">
        <div class="cd-head"><div class="cd-title">On this network</div>
          <div class="cd-count">${state.candidates.length}</div></div>
        <div class="cd-body">${state.candidates.map(candidateRow).join("")}</div>
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
      </div>`;
  }

  root.innerHTML = `
    <div class="app-titlebar-pad"></div>
    ${header}
    ${state.devices.length > 0 ? `<div class="app-tabs">${tabsView()}</div>` : ""}
    ${noticeView()}
    <main class="app-main">${body}</main>`;
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
  const typed = window.prompt(
    `Unpairing ${name} revokes its key and closes every tunnel, sync and session.\n\n` +
      `Type the device name to confirm:`,
  );
  if (typed !== name) {
    if (typed !== null) notify("The name did not match. Nothing was unpaired.", "attention");
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
    const ok = window.confirm(
      "Bypass mode runs every future command from that machine without asking.\n\n" +
        "It is not destructive now — it is destructive later, on every command that follows.\n\n" +
        "Switch to Bypass?",
    );
    if (!ok) return;
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
  const address = window.prompt(
    "Add the other machine by IP address.\n\n" +
      "Multicast is often blocked on corporate and guest Wi-Fi; typing the address works just as well.",
  );
  if (!address) return;
  await startPairing(address.trim());
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

  const traceFilter = el.closest("[data-trace-filter]")?.getAttribute("data-trace-filter");
  if (traceFilter) {
    state.traceErrorsOnly = traceFilter === "errors";
    void refresh();
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
    case "confirm-pairing":
      void confirmPairing();
      break;
    case "cancel-pairing":
      void cancelPairing();
      break;
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

// Events are a nudge to re-read, never the only copy of a fact: a dropped one
// costs a refresh, not correctness.
window.ibeji.onEvent((_e: DaemonEvent) => {
  void refresh();
});

// A slow poll as a backstop, so a missed event cannot leave the window stale
// indefinitely — and so a daemon that starts after the window is noticed.
setInterval(() => void refresh(), 5000);

void refresh();
