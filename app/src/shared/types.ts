/**
 * Types shared by the main process, the preload bridge and the renderer.
 *
 * They mirror the daemon's API responses. Kept in one file so the three
 * processes cannot disagree about a shape — the same reasoning that puts the
 * daemon, CLI and MCP server in one Go module.
 */

/** One of the six closed semantic types from the design system's enum. */
export type SemanticType =
  | "live"
  | "attention"
  | "irreversible"
  | "converged"
  | "provenance"
  | "unreachable";

/** Permission mode, set on the receiving device, per direction. */
export type Mode = "prompt" | "bypass";

/** A paired device, as the device view renders it. */
export interface Device {
  id: string;
  name: string;
  /** The LAN address. Never omitted: Device.Address is a required slot. */
  address: string;
  status: "live" | "unreachable";
  mode: Mode;
  fingerprint: string;
  pairedAt?: string;
  lastSeen?: string;
}

/** A device seen on the LAN that has not been paired with. */
export interface Candidate {
  name: string;
  address: string;
  fingerprint: string;
  apiVersion: string;
}

/** The daemon's own view of itself. */
export interface SelfInfo {
  name: string;
  fingerprint: string;
  version: string;
  os: string;
  arch: string;
  paired: boolean;
}

/** A pairing in progress: the code to compare across the room. */
export interface PairingSession {
  id: string;
  code: string;
  peerName: string;
  peerAddress: string;
  peerFingerprint: string;
  expiresAt: string;
}

/** One check from `ibeji doctor`. */
export interface DoctorCheck {
  name: string;
  ok: boolean;
  severity: "pass" | "warn" | "fail";
  detail: string;
  /** What to do about it. Present whenever ok is false. */
  fix?: string;
}

/** An event from the daemon's stream. */
export interface DaemonEvent {
  type: string;
  data: unknown;
}

/** What every bridged call resolves to: never a throw across IPC. */
export type Result<T> =
  | { ok: true; value: T }
  | { ok: false; error: { code: string; message: string } };
