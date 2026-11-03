/**
 * Install the daemon on first launch, from inside the app.
 *
 * A packaged Mac app has no business asking anyone to open a terminal, paste
 * an /Applications path and learn a --prefix flag before it will work. The
 * first version of this shipped exactly that, and it was indefensible: the
 * window said "Ìbejì is not running" and the fix lived in a README.
 *
 * So the app now does it itself, on every launch:
 *
 *   - installs the LaunchAgent if it is missing, so the daemon starts at login
 *   - repoints it if the app has moved (a stale plist names a path that is
 *     gone, and launchd fails silently)
 *   - links `ibeji` into a directory already on PATH, for terminal use
 *
 * Every step is best-effort. Nothing here may block or fail the launch: a
 * window that opens with a broken link is recoverable, a window that refuses
 * to open is not. Failures are reported to the renderer, never thrown.
 */

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { constants } from "node:fs";
import { access, mkdir, readlink, rm, symlink } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { app } from "electron";

const run = promisify(execFile);

/** What the attempt did, for the window to show if something needs saying. */
export type SetupResult = {
  daemon: "already-running" | "installed" | "repointed" | "failed";
  cli: "already-linked" | "linked" | "skipped" | "failed";
  cliPath?: string;
  error?: string;
};

/** Where the Go binaries live inside the bundle, or in a dev checkout. */
function binDir(): string {
  if (app.isPackaged) return join(process.resourcesPath, "bin");
  return join(app.getAppPath(), "..", "..", "bin");
}

/**
 * Pick a bin directory that is on PATH and writable without sudo.
 *
 * /usr/local/bin is root-owned on a stock Mac, so the obvious default is the
 * one place a GUI app cannot write. Preferring ~/.local/bin avoids asking for
 * a password the app has no way to collect.
 */
async function writableBinDir(): Promise<string | undefined> {
  const path = (process.env.PATH ?? "").split(":").filter(Boolean);
  const candidates = [
    join(homedir(), ".local", "bin"),
    "/usr/local/bin",
    join(homedir(), "bin"),
  ];
  for (const dir of candidates) {
    if (!path.includes(dir)) continue;
    try {
      await access(dir, constants.W_OK);
      return dir;
    } catch {
      // Not writable — try the next one rather than asking for sudo.
    }
  }
  // Nothing on PATH is writable. ~/.local/bin is the conventional answer, and
  // creating it costs nothing even if the shell does not yet look there.
  const fallback = join(homedir(), ".local", "bin");
  try {
    await mkdir(fallback, { recursive: true });
    return fallback;
  } catch {
    return undefined;
  }
}

/** Ask the daemon's own CLI whether a usable LaunchAgent is already loaded. */
async function agentProgram(): Promise<string | undefined> {
  const plist = join(homedir(), "Library", "LaunchAgents", "com.ibeji.daemon.plist");
  try {
    const { stdout } = await run("/usr/libexec/PlistBuddy", [
      "-c",
      "Print :ProgramArguments:0",
      plist,
    ]);
    return stdout.trim();
  } catch {
    return undefined;
  }
}

/**
 * Make the machine ready, quietly.
 *
 * Returns what happened. Callers must not treat a failure as fatal.
 */
export async function ensureInstalled(): Promise<SetupResult> {
  const result: SetupResult = { daemon: "already-running", cli: "skipped" };
  if (process.platform !== "darwin") return result;

  const bin = binDir();
  const cli = join(bin, "ibeji");
  const daemon = join(bin, "ibeji-daemon");
  try {
    await access(daemon, constants.X_OK);
  } catch {
    return { daemon: "failed", cli: "skipped", error: `ibeji-daemon is missing from ${bin}` };
  }

  // 1. The LaunchAgent. Reinstall when absent, or when it points somewhere the
  //    app no longer is — moving Ibeji.app leaves a plist naming a dead path,
  //    and launchd reports that as nothing at all.
  const current = await agentProgram();
  try {
    if (current === undefined) {
      await run(cli, ["install", "--prefix", await safePrefix()]);
      result.daemon = "installed";
    } else if (current !== daemon) {
      await run(cli, ["install", "--prefix", await safePrefix()]);
      result.daemon = "repointed";
    }
  } catch (err) {
    result.daemon = "failed";
    result.error = err instanceof Error ? err.message : String(err);
    return result;
  }

  // 2. The terminal shortcut. `ibeji install` already links it, so this only
  //    reports what happened — and repairs a link left dangling by an upgrade.
  const dir = await writableBinDir();
  if (!dir) return result;
  const link = join(dir, "ibeji");
  result.cliPath = link;
  try {
    const target = await readlink(link).catch(() => undefined);
    if (target === cli) {
      result.cli = "already-linked";
    } else {
      await mkdir(dirname(link), { recursive: true });
      // Replace only a symlink. A real file there belongs to something else.
      if (target !== undefined) await rm(link, { force: true });
      await symlink(cli, link);
      result.cli = "linked";
    }
  } catch {
    result.cli = "failed";
  }
  return result;
}

/** The prefix handed to `ibeji install`, chosen to never need sudo. */
async function safePrefix(): Promise<string> {
  return (await writableBinDir()) ?? join(homedir(), ".local", "bin");
}
