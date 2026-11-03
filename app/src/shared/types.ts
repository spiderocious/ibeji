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
  port: number;
  fingerprint: string;
  apiVersion: string;
  /** False when the peer speaks a protocol version this build does not. */
  compatible: boolean;
  /** "mdns" or "manual" — both are first-class ways to find a device. */
  source: string;
}

/** What GET /v1/discovery returns: what was found, and why it may be empty. */
export interface DiscoveryResult {
  candidates: Candidate[];
  /**
   * Why the list looks the way it does. The daemon supplies it because only
   * the daemon knows whether multicast is silent or the other machine is off,
   * and the two need different advice.
   */
  hint: string;
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

/**
 * What the preload bridge exposes on `window.ibeji`.
 *
 * Declared here rather than inferred from the preload module so the renderer
 * can type it without importing that module: the renderer compiles as a plain
 * ES module for a sandboxed page, and pulling the preload in made TypeScript
 * re-emit it as ESM, which Electron cannot load as a preload script.
 */
export interface IbejiBridge {
  self(): Promise<Result<SelfInfo>>;
  devices(): Promise<Result<{ devices: Device[] }>>;
  candidates(): Promise<Result<DiscoveryResult>>;
  /** The ceremony in flight, if any — so a reopened window shows the code. */
  pending(): Promise<Result<{ pairing: PairingSession | null }>>;
  doctor(): Promise<Result<{ checks: DoctorCheck[] }>>;

  startPairing(address: string): Promise<Result<PairingSession>>;
  confirmPairing(sessionId: string): Promise<Result<{ device: Device }>>;
  cancelPairing(sessionId: string): Promise<Result<void>>;

  unpair(deviceId: string): Promise<Result<void>>;
  rename(name: string): Promise<Result<void>>;
  setMode(deviceId: string, mode: Mode): Promise<Result<void>>;

  onEvent(fn: (e: DaemonEvent) => void): () => void;
}
