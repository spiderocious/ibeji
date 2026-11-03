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
  Result,
  SelfInfo,
} from "../shared/types.js";

declare global {
  interface Window {
    ibeji: IbejiBridge;
  }
}

// --- state ------------------------------------------------------------------

type Tone = "live" | "attention" | "irreversible";

interface State {
  self: SelfInfo | undefined;
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
  const [self, devices, candidates, pending] = await Promise.all([
    window.ibeji.self(),
    window.ibeji.devices(),
    window.ibeji.candidates(),
    window.ibeji.pending(),
  ]);

  state.self = unwrap(self) ?? state.self;
  state.devices = unwrap(devices)?.devices ?? [];
  const found = unwrap(candidates);
  state.candidates = found?.candidates ?? [];
  state.hint = found?.hint ?? undefined;
  // Pairing state lives in the daemon, so a window opened mid-ceremony shows
  // the code rather than an empty device list.
  state.pairing = unwrap(pending)?.pairing ?? undefined;
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

  let body: string;
  if (state.pairing) {
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
  const target = (e.target as HTMLElement).closest("[data-action],[data-pair]");
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
