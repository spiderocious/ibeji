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
  Candidate,
  DaemonEvent,
  Device,
  DoctorCheck,
  Mode,
  PairingSession,
  Result,
  SelfInfo,
} from "../shared/types.js";

const invoke = <T>(channel: string, ...args: unknown[]): Promise<Result<T>> =>
  ipcRenderer.invoke(channel, ...args) as Promise<Result<T>>;

const bridge = {
  // --- reads ---------------------------------------------------------------
  self: () => invoke<SelfInfo>("self"),
  devices: () => invoke<{ devices: Device[] }>("devices"),
  candidates: () => invoke<{ candidates: Candidate[] }>("candidates"),
  doctor: () => invoke<{ checks: DoctorCheck[] }>("doctor"),

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
  rename: (name: string) => invoke<void>("rename", name),
  /**
   * Set a paired device's permission mode. Flipping to bypass requires
   * hold-to-confirm in the UI: a control that authorises every future command
   * from that machine cannot be one click.
   */
  setMode: (deviceId: string, mode: Mode) => invoke<void>("mode:set", deviceId, mode),

  // --- events --------------------------------------------------------------
  /** Subscribe to daemon events. Returns a disposer. */
  onEvent: (fn: (e: DaemonEvent) => void): (() => void) => {
    const listener = (_evt: unknown, payload: DaemonEvent) => fn(payload);
    ipcRenderer.on("daemon:event", listener);
    return () => ipcRenderer.removeListener("daemon:event", listener);
  },
} as const;

export type IbejiBridge = typeof bridge;

contextBridge.exposeInMainWorld("ibeji", bridge);
