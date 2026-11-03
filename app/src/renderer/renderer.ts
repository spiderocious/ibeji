/**
 * The renderer entry point.
 *
 * It renders what the API reports and sends intents back through the preload
 * bridge. It holds no product logic: no permission decisions, no sync state, no
 * retry policy — those live in the daemon, which the CLI and MCP also use.
 */

import type { IbejiBridge } from "../preload/preload.js";

declare global {
  interface Window {
    ibeji: IbejiBridge;
  }
}

const root = document.getElementById("root");

async function boot(): Promise<void> {
  if (!root) return;

  const self = await window.ibeji.self();
  if (!self.ok) {
    // The daemon not running is the expected first-run state, not a crash, so
    // it gets a plain sentence and the fix rather than an error dump.
    root.innerHTML = `
      <div class="app-titlebar-pad"></div>
      <p class="app-boot">${
        self.error.code === "daemon_unreachable"
          ? "The Ìbejì daemon is not running. Run <code>ibeji doctor</code> to find out why."
          : escapeHtml(self.error.message)
      }</p>`;
    return;
  }

  root.innerHTML = `
    <div class="app-titlebar-pad"></div>
    <p class="app-boot">Linked as ${escapeHtml(self.value.name)} · ${escapeHtml(self.value.version)}</p>`;
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c] ?? c,
  );
}

void boot();
