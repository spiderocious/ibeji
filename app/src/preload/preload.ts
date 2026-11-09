/**
 * The preload bridge.
 *
 * The renderer gets exactly these functions and nothing else: no Node, no
 * filesystem, no API token. Every call resolves to a discriminated Result
 * rather than throwing across the IPC boundary, so the renderer always has an
 * error code to branch on — `daemon_unreachable` renders differently from a
 * declined action, and neither looks like a crash.
 */

import { contextBridge, ipcRenderer } from "electron";
import type {
  DaemonEvent,
  DiscoveryResult,
  IbejiBridge,
  Device,
  DoctorCheck,
  Mode,
  PairingSession,
  PendingPrompt,
  PortMeta,
  PortRow,
  SpaceStatus,
  TraceEvent,
  TraceStats,
  TunnelRow,
  Result,
  Health,
  SelfInfo,
  ShellChunk,
  ShellInfo,
  UpdateStatus,
} from "../shared/types.js";

const invoke = <T>(channel: string, ...args: unknown[]): Promise<Result<T>> =>
  ipcRenderer.invoke(channel, ...args) as Promise<Result<T>>;

const bridge = {
  // --- reads ---------------------------------------------------------------
  self: () => invoke<SelfInfo>("self"),
  devices: () => invoke<{ devices: Device[] }>("devices"),
  candidates: () => invoke<DiscoveryResult>("candidates"),
  pending: () => invoke<{ pairing: PairingSession | null }>("pending"),
  doctor: () => invoke<{ checks: DoctorCheck[] }>("doctor"),

  // Updates. The check never throws in the main process, so a GitHub outage
  // arrives here as state "unknown" rather than an error banner.
  checkUpdate: () => invoke<UpdateStatus>("update:check"),
  runUpdate: () => invoke<void>("update:run"),
  updateCommand: () => invoke<string>("update:command"),

  // --- pairing -------------------------------------------------------------
  startPairing: (address: string) => invoke<PairingSession>("pair:start", address),
  confirmPairing: (sessionId: string) => invoke<{ device: Device }>("pair:confirm", sessionId),
  cancelPairing: (sessionId: string) => invoke<void>("pair:cancel", sessionId),

  // --- intents -------------------------------------------------------------
  /**
   * Unpair. The caller must have obtained typed confirmation of the device
   * name first — this is one of the four places oxblood is spent.
   */
  unpair: (deviceId: string) => invoke<void>("unpair", deviceId),
  reconnect: (address: string) => invoke<{ device: Device }>("reconnect", address),
  health: () => invoke<Health>("health"),
  logs: (limit?: number, q?: string) =>
    invoke<{ path: string; lines: string[] }>("logs", limit, q),
  rename: (name: string) => invoke<void>("rename", name),
  /**
   * Set a paired device's permission mode. Flipping to bypass requires
   * hold-to-confirm in the UI: a control that authorises every future command
   * from that machine cannot be one click.
   */
  setMode: (deviceId: string, mode: Mode) => invoke<void>("mode:set", deviceId, mode),

  // --- spaces, ports, prompts ----------------------------------------------
  spaces: () => invoke<{ spaces: SpaceStatus[] }>("spaces"),
  createSpace: (opts: { name: string; path: string; role: "master" | "replica"; mode: "two-way" | "mirror" }) =>
    invoke<{ warning?: string }>("spaces:create", opts),
  pickFolder: () => invoke<string | undefined>("pick-folder"),

  // --- agents --------------------------------------------------------------
  // An MCP server ships with the app, and nothing in the window ever said so.
  mcpCommand: () => invoke<string>("agent:mcp-command"),

  // --- the Shell tab -------------------------------------------------------
  shells: () => invoke<{ shells: ShellInfo[] }>("shells"),
  shellOpen: (cwd?: string) => invoke<{ shell: ShellInfo }>("shell:open", cwd),
  shellWrite: (id: string, data: string) => invoke<void>("shell:write", id, data),
  shellRead: (id: string, since: number, waitMs: number) =>
    invoke<{ output: ShellChunk[]; seq: number; done: boolean }>("shell:read", id, since, waitMs),
  shellInterrupt: (id: string) => invoke<void>("shell:interrupt", id),
  shellClose: (id: string) => invoke<void>("shell:close", id),
  agentBriefing: () => invoke<string>("agent:briefing"),
  promote: (name: string, path: string) => invoke<void>("spaces:promote", name, path),
  discard: (name: string, path: string) => invoke<void>("spaces:discard", name, path),

  ports: () => invoke<{ ports: PortRow[]; meta: PortMeta }>("ports"),
  tunnels: () => invoke<{ tunnels: TunnelRow[] }>("tunnels"),
  openTunnel: (port: number, name?: string) =>
    invoke<{ tunnel: TunnelRow }>("tunnels:open", port, name),
  closeTunnel: (port: number) => invoke<void>("tunnels:close", port),

  prompts: () => invoke<{ prompts: PendingPrompt[] }>("prompts"),
  trace: (opts?: { errors?: boolean; provenance?: string }) =>
    invoke<{ events: TraceEvent[] }>("trace", opts),
  traceStats: () => invoke<TraceStats>("trace:stats"),
  answerPrompt: (id: string, accept: boolean, remember: boolean) =>
    invoke<void>("prompts:answer", id, accept, remember),

  // --- events --------------------------------------------------------------
  /** Subscribe to daemon events. Returns a disposer. */
  onEvent: (fn: (e: DaemonEvent) => void): (() => void) => {
    const listener = (_evt: unknown, payload: DaemonEvent) => fn(payload);
    ipcRenderer.on("daemon:event", listener);
    return () => ipcRenderer.removeListener("daemon:event", listener);
  },
} as const;

// The shared declaration is the contract the renderer types against; this
// assertion is what stops the two drifting apart.
const _contract: IbejiBridge = bridge;
void _contract;

contextBridge.exposeInMainWorld("ibeji", bridge);
