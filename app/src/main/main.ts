/**
 * The Electron main process.
 *
 * It owns the window and the tray, and it is the only place that talks to the
 * daemon's local API — the renderer reaches it through a narrow preload bridge
 * with context isolation on, so a renderer bug cannot read the API token or the
 * filesystem.
 *
 * Closing the window must never disconnect the link: the daemon is a
 * LaunchAgent and runs independently, so the window closing hides it rather
 * than quitting the app.
 */

import { app, BrowserWindow, ipcMain, shell, Tray, Menu, nativeImage } from "electron";
import { join } from "node:path";
import { ApiClient, ApiError, DaemonUnreachableError } from "./api.js";
import { ensureInstalled } from "./selfinstall.js";

/**
 * IBEJI_HANDOFF overrides where the window looks for the daemon's endpoint and
 * token. The window does not normally read its environment — it is launched
 * from the Dock and inherits nothing — but the smoke test launches it directly
 * and must be able to point it at a throwaway daemon. Without this, the test
 * silently falls back to the real ~/.ibeji: on a clean machine that means the
 * daemon-down empty state, and every populated view goes unchecked.
 */
const api = new ApiClient(process.env.IBEJI_HANDOFF || undefined);

let win: BrowserWindow | undefined;
let tray: Tray | undefined;
let unsubscribeEvents: (() => void) | undefined;

/** Paper-200, the design system's canvas. Set so the window never flashes white. */
const CANVAS = "#F4F1E9";

/**
 * Wait, briefly, for a just-installed daemon to start answering.
 *
 * launchd's bootstrap returns as soon as the job is loaded, not when the
 * process is serving, so a window drawn immediately afterwards races it and
 * shows the empty state. Polling beats a fixed sleep: it is usually instant.
 */
async function waitForDaemon(timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      await api.self();
      return;
    } catch {
      await new Promise((r) => setTimeout(r, 150));
    }
  }
}

function createWindow(): BrowserWindow {
  const w = new BrowserWindow({
    width: 980,
    height: 680,
    minWidth: 620,
    minHeight: 460,
    title: "Ìbejì",
    backgroundColor: CANVAS,
    titleBarStyle: "hiddenInset",
    show: false,
    webPreferences: {
      preload: join(__dirname, "../preload/preload.js"),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  });

  w.once("ready-to-show", () => w.show());

  // External links open in the user's browser, never in an Electron window.
  w.webContents.setWindowOpenHandler(({ url }) => {
    if (url.startsWith("https://") || url.startsWith("http://")) {
      void shell.openExternal(url);
    }
    return { action: "deny" };
  });

  void w.loadFile(join(__dirname, "../renderer/index.html"));
  return w;
}

function showWindow(): void {
  if (!win || win.isDestroyed()) {
    win = createWindow();
    return;
  }
  if (win.isMinimized()) win.restore();
  win.show();
  win.focus();
}

function createTray(): void {
  // An empty image keeps the tray slot without shipping a placeholder icon;
  // the real mark is a design-system asset and is not invented here.
  tray = new Tray(nativeImage.createEmpty());
  tray.setToolTip("Ìbejì");
  tray.setContextMenu(
    Menu.buildFromTemplate([
      { label: "Open Ìbejì", click: showWindow },
      { type: "separator" },
      { label: "Quit Ìbejì", click: () => app.quit() },
    ]),
  );
}

/**
 * Shape an error for the renderer.
 *
 * The renderer gets a stable code and a sentence it can show, never a stack —
 * and `daemon_unreachable` is distinguished from an API error so the window can
 * render "the daemon is not running" rather than a generic failure.
 */
function toRendererError(err: unknown): { code: string; message: string } {
  if (err instanceof DaemonUnreachableError) {
    api.invalidate();
    return { code: "daemon_unreachable", message: err.message };
  }
  if (err instanceof ApiError) {
    return { code: err.code, message: err.message };
  }
  return { code: "unknown", message: err instanceof Error ? err.message : String(err) };
}

type Handler = (...args: never[]) => Promise<unknown>;

/** Register an IPC handler that returns a discriminated result, never a throw. */
function handle(channel: string, fn: Handler): void {
  ipcMain.handle(channel, async (_evt, ...args) => {
    try {
      return { ok: true, value: await fn(...(args as never[])) };
    } catch (err) {
      return { ok: false, error: toRendererError(err) };
    }
  });
}

function registerHandlers(): void {
  handle("self", () => api.self());
  handle("devices", () => api.devices());
  handle("candidates", () => api.candidates());
  handle("pending", () => api.pending());

  handle("spaces", () => api.spaces());
  handle("spaces:promote", (name: string, path: string) => api.promote(name, path));
  handle("spaces:discard", (name: string, path: string) => api.discard(name, path));

  handle("ports", () => api.ports());
  handle("tunnels", () => api.tunnels());
  handle("tunnels:open", (port: number, name?: string) => api.openTunnel(port, name));
  handle("tunnels:close", (port: number) => api.closeTunnel(port));

  handle("prompts", () => api.prompts());
  handle("trace", (opts?: { errors?: boolean; provenance?: string }) => api.trace(opts));
  handle("trace:stats", () => api.traceStats());
  handle("prompts:answer", (id: string, accept: boolean, remember: boolean) =>
    api.answerPrompt(id, accept, remember),
  );
  handle("doctor", () => api.doctor());

  handle("pair:start", (address: string) => api.startPairing(address));
  handle("pair:confirm", (sessionId: string) => api.confirmPairing(sessionId));
  handle("pair:cancel", (sessionId: string) => api.cancelPairing(sessionId));

  handle("unpair", (deviceId: string) => api.unpair(deviceId));
  handle("rename", (name: string) => api.rename(name));
  handle("mode:set", (deviceId: string, mode: "prompt" | "bypass") =>
    api.setMode(deviceId, mode),
  );
}

/**
 * Forward the daemon's event stream to the renderer, retrying while the daemon
 * is down. A LAN product's daemon restarts — on update, on logout — and the
 * window must recover without the user reopening it.
 */
async function subscribeEvents(): Promise<void> {
  const attach = async (): Promise<void> => {
    try {
      unsubscribeEvents = await api.events((e) => {
        if (win && !win.isDestroyed()) {
          win.webContents.send("daemon:event", e);
        }
      });
    } catch {
      api.invalidate();
      setTimeout(() => void attach(), 2_000);
    }
  };
  await attach();
}

// Single instance: a second launch focuses the existing window instead of
// opening a second one that would fight over the same daemon.
if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", showWindow);

  void app.whenReady().then(async () => {
    registerHandlers();
    createTray();

    // Set the machine up before the window draws. Doing this first means a
    // fresh install opens onto a working app rather than onto "Ìbejì is not
    // running" and a README instruction to go and fix it by hand.
    const setup = await ensureInstalled();
    if (setup.daemon === "installed" || setup.daemon === "repointed") {
      // launchd returns before the daemon is listening; give it a moment so
      // the first render finds an API rather than a closed port.
      await waitForDaemon(5000);
    }

    win = createWindow();
    await subscribeEvents();

    app.on("activate", () => {
      if (BrowserWindow.getAllWindows().length === 0) win = createWindow();
      else showWindow();
    });
  });

  // On macOS, closing the window leaves the app in the tray. The daemon is
  // unaffected either way, which is the PRD's requirement.
  app.on("window-all-closed", () => {
    if (process.platform !== "darwin") app.quit();
  });

  app.on("before-quit", () => {
    unsubscribeEvents?.();
    tray?.destroy();
  });
}
