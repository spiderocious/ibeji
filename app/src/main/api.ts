/**
 * A typed client for the daemon's local HTTP API.
 *
 * The window renders API state and sends intents; it holds no product logic of
 * its own. Every capability the window has is one the CLI and an MCP agent also
 * have, because all three call the same endpoints — that is what stops the UI
 * growing private features.
 */

import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import type {
  Candidate,
  Device,
  DiscoveryResult,
  PendingPrompt,
  PortMeta,
  PortRow,
  SpaceStatus,
  TraceEvent,
  TraceStats,
  TunnelRow,
  DoctorCheck,
  Mode,
  PairingSession,
  SelfInfo,
} from "../shared/types.js";

export type {
  Candidate,
  Device,
  DiscoveryResult,
  DoctorCheck,
  Mode,
  PairingSession,
  SelfInfo,
} from "../shared/types.js";

/** Where the daemon writes its endpoint and token for local clients. */
const HANDOFF_PATH = join(homedir(), ".ibeji", "api.json");

/** An error the API returned, carrying the daemon's stable error code. */
export class ApiError extends Error {
  constructor(
    readonly code: string,
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** Raised when the daemon is not running or not reachable on loopback. */
export class DaemonUnreachableError extends Error {
  constructor(cause: string) {
    super(`the Ìbejì daemon is not reachable: ${cause}`);
    this.name = "DaemonUnreachableError";
  }
}

interface Handoff {
  endpoint: string;
  token: string;
}

/**
 * Client for the local API.
 *
 * The endpoint and token come from a 0600 handoff file the daemon writes at
 * startup, rather than from an environment variable: the window is launched by
 * the user from the Dock and inherits no environment from the daemon.
 */
export class ApiClient {
  private handoff: Handoff | undefined;

  constructor(private readonly handoffPath: string = HANDOFF_PATH) {}

  private async credentials(): Promise<Handoff> {
    if (this.handoff) return this.handoff;
    let raw: string;
    try {
      raw = await readFile(this.handoffPath, "utf8");
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err);
      throw new DaemonUnreachableError(
        `could not read ${this.handoffPath} (${reason}) — is the daemon running?`,
      );
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      throw new DaemonUnreachableError(`${this.handoffPath} is not valid JSON`);
    }
    const h = parsed as Partial<Handoff>;
    if (typeof h.endpoint !== "string" || typeof h.token !== "string") {
      throw new DaemonUnreachableError(`${this.handoffPath} is missing endpoint or token`);
    }
    this.handoff = { endpoint: h.endpoint, token: h.token };
    return this.handoff;
  }

  /** Forget cached credentials, so the next call re-reads the handoff file. */
  invalidate(): void {
    this.handoff = undefined;
  }

  private async request<T>(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<T> {
    const { endpoint, token } = await this.credentials();
    const init: RequestInit = {
      method,
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: "application/json",
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    };

    let res: Response;
    try {
      res = await fetch(`${endpoint}${path}`, init);
    } catch (err) {
      // A connection refused here almost always means the daemon stopped after
      // we read its handoff file, so drop the cache and say so plainly.
      this.invalidate();
      const reason = err instanceof Error ? err.message : String(err);
      throw new DaemonUnreachableError(reason);
    }

    if (res.status === 204) return undefined as T;

    const text = await res.text();
    let parsed: unknown;
    try {
      parsed = text === "" ? {} : JSON.parse(text);
    } catch {
      throw new ApiError("bad_response", `${method} ${path}: response was not JSON`, res.status);
    }

    if (!res.ok) {
      const e = parsed as { code?: string; error?: string; message?: string };
      throw new ApiError(
        e.code ?? "unknown",
        e.message ?? e.error ?? `${method} ${path} failed with ${res.status}`,
        res.status,
      );
    }
    return parsed as T;
  }

  // --- reads ---------------------------------------------------------------

  self(): Promise<SelfInfo> {
    return this.request<SelfInfo>("GET", "/v1/self");
  }

  devices(): Promise<{ devices: Device[] }> {
    return this.request<{ devices: Device[] }>("GET", "/v1/devices");
  }

  candidates(): Promise<DiscoveryResult> {
    return this.request<DiscoveryResult>("GET", "/v1/discovery");
  }

  spaces(): Promise<{ spaces: SpaceStatus[] }> {
    return this.request("GET", "/v1/spaces");
  }

  promote(name: string, path: string): Promise<void> {
    return this.request("POST", "/v1/spaces/promote", { name, path });
  }

  discard(name: string, path: string): Promise<void> {
    return this.request("POST", "/v1/spaces/discard", { name, path });
  }

  ports(): Promise<{ ports: PortRow[]; meta: PortMeta }> {
    return this.request("GET", "/v1/ports");
  }

  tunnels(): Promise<{ tunnels: TunnelRow[] }> {
    return this.request("GET", "/v1/tunnels");
  }

  openTunnel(port: number, name?: string): Promise<{ tunnel: TunnelRow }> {
    return this.request("POST", "/v1/tunnels/open", { port, name: name ?? "" });
  }

  closeTunnel(port: number): Promise<void> {
    return this.request("POST", "/v1/tunnels/close", { port });
  }

  trace(opts?: { errors?: boolean; provenance?: string }): Promise<{ events: TraceEvent[] }> {
    const q = new URLSearchParams({ since: "1h", limit: "60" });
    if (opts?.errors) q.set("errors", "true");
    if (opts?.provenance) q.set("provenance", opts.provenance);
    return this.request("GET", `/v1/trace?${q.toString()}`);
  }

  traceStats(): Promise<TraceStats> {
    return this.request("GET", "/v1/trace/stats");
  }

  prompts(): Promise<{ prompts: PendingPrompt[] }> {
    return this.request("GET", "/v1/prompts");
  }

  answerPrompt(id: string, accept: boolean, remember: boolean): Promise<void> {
    return this.request("POST", "/v1/prompts/answer", { id, accept, remember });
  }

  pending(): Promise<{ pairing: PairingSession | null }> {
    return this.request<{ pairing: PairingSession | null }>("GET", "/v1/pair");
  }

  doctor(): Promise<{ checks: DoctorCheck[] }> {
    return this.request<{ checks: DoctorCheck[] }>("GET", "/v1/doctor");
  }

  // --- intents -------------------------------------------------------------

  /** Begin pairing with a candidate; returns the code to compare. */
  startPairing(address: string): Promise<PairingSession> {
    return this.request<PairingSession>("POST", "/v1/pair/start", { address });
  }

  /** Confirm the codes matched. Both devices must confirm. */
  confirmPairing(sessionId: string): Promise<{ device: Device }> {
    return this.request<{ device: Device }>("POST", "/v1/pair/confirm", { sessionId });
  }

  /** Abandon a pairing in progress. */
  cancelPairing(sessionId: string): Promise<void> {
    return this.request<void>("POST", "/v1/pair/cancel", { sessionId });
  }

  /**
   * Unpair. Revokes the pinned key and closes every tunnel, sync and session.
   * The caller must have obtained typed confirmation of the device name first —
   * this is one of the four places oxblood is spent.
   */
  unpair(deviceId: string): Promise<void> {
    return this.request<void>("POST", "/v1/unpair", { deviceId });
  }

  /** Rename this device. The name propagates to the peer. */
  rename(name: string): Promise<void> {
    return this.request<void>("POST", "/v1/self/name", { name });
  }

  /**
   * Set a paired device's permission mode. Mode is per direction and set on the
   * receiving device. Flipping to bypass requires hold-to-confirm in the UI.
   */
  setMode(deviceId: string, mode: Mode): Promise<void> {
    return this.request<void>("POST", "/v1/devices/mode", { deviceId, mode });
  }

  /** Subscribe to the daemon's event stream. Returns a disposer. */
  async events(onEvent: (e: { type: string; data: unknown }) => void): Promise<() => void> {
    const { endpoint, token } = await this.credentials();
    const controller = new AbortController();

    void (async () => {
      try {
        const res = await fetch(`${endpoint}/v1/events`, {
          headers: { Authorization: `Bearer ${token}`, Accept: "text/event-stream" },
          signal: controller.signal,
        });
        if (!res.body) return;
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          // SSE frames are separated by a blank line.
          let split: number;
          while ((split = buffer.indexOf("\n\n")) !== -1) {
            const frame = buffer.slice(0, split);
            buffer = buffer.slice(split + 2);
            let type = "message";
            const dataLines: string[] = [];
            for (const line of frame.split("\n")) {
              if (line.startsWith("event:")) type = line.slice(6).trim();
              else if (line.startsWith("data:")) dataLines.push(line.slice(5).trim());
            }
            if (dataLines.length === 0) continue;
            try {
              onEvent({ type, data: JSON.parse(dataLines.join("\n")) });
            } catch {
              // A malformed frame is the daemon's bug, not a reason to tear
              // down the stream; skip it.
            }
          }
        }
      } catch {
        // Aborted or the daemon went away. The caller resubscribes on the next
        // reconnect; nothing to report from here.
      }
    })();

    return () => controller.abort();
  }
}
