/**
 * Checking for a newer release, and running the installer to get it.
 *
 * Not electron-updater: a silent in-place swap needs a notarized app, and
 * this one is ad-hoc signed, so macOS would refuse the replacement binary.
 * What works without a paid Apple account is re-running the same install
 * script the README gives out — it already quits the app, stops the daemon,
 * replaces the bundle and restarts, and it leaves ~/.ibeji and the keychain
 * alone, so pairing survives an upgrade.
 *
 * The tag comes from the /releases/latest redirect rather than the JSON API,
 * which allows 60 unauthenticated requests an hour per IP. An app that checks
 * on every launch would spend that quota on a shared office address and then
 * report "could not check" forever.
 */

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { app } from "electron";

const run = promisify(execFile);

const REPO = "spiderocious/ibeji";
const INSTALL_URL = `https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh`;

export type UpdateStatus =
  | { state: "current"; version: string }
  | { state: "available"; version: string; current: string; notes: string; url: string }
  | { state: "unknown"; reason: string };

/**
 * Compare two dotted versions numerically: "0.1.10" is newer than "0.1.9".
 *
 * Only the numeric core is compared. A suffix — "0.1.2-dirty" from a local
 * build, or "-rc1" — counts as *older* than the same numbers released clean,
 * so a developer running a modified 0.1.2 is still offered the published
 * 0.1.2. Treating the suffix as equal would hide the update from exactly the
 * person most likely to be testing it.
 */
function isNewer(candidate: string, current: string): boolean {
  const core = (v: string) => v.replace(/^v/, "").split("-")[0] ?? "";
  const nums = (v: string) =>
    core(v)
      .split(".")
      .map((p) => Number.parseInt(p, 10))
      .map((n) => (Number.isNaN(n) ? 0 : n));
  // A build that is not a dotted version at all — "522d5ba-dirty" from a
  // local `make build` — has nothing to compare, and parsing it numerically
  // yields [0], which would make it look NEWER than every release and hide
  // updates forever. Treat an unrecognisable current version as outdated.
  const dotted = /^\d+(\.\d+)*$/;
  if (!dotted.test(core(current))) return true;
  if (!dotted.test(core(candidate))) return false;

  const a = nums(candidate);
  const b = nums(current);
  for (let i = 0; i < Math.max(a.length, b.length); i += 1) {
    const x = a[i] ?? 0;
    const y = b[i] ?? 0;
    if (x !== y) return x > y;
  }
  // Numerically equal: a clean release beats a suffixed local build.
  const candidateIsClean = !candidate.replace(/^v/, "").includes("-");
  const currentIsClean = !current.replace(/^v/, "").includes("-");
  return candidateIsClean && !currentIsClean;
}

/**
 * Ask GitHub what the latest release is.
 *
 * Never throws: a failed check is a quiet "unknown", because a window that
 * refuses to open because GitHub was unreachable would be a far worse bug
 * than a missed update.
 */
export async function checkForUpdate(): Promise<UpdateStatus> {
  const current = app.getVersion();
  try {
    // -I and -o /dev/null: we want the redirect target, not the page.
    const { stdout } = await run(
      "curl",
      [
        "-fsSLI",
        "-o",
        "/dev/null",
        "-w",
        "%{url_effective}",
        "--max-time",
        "10",
        `https://github.com/${REPO}/releases/latest`,
      ],
      { timeout: 15_000 },
    );
    const tag = stdout.trim().split("/tag/")[1];
    if (!tag) return { state: "unknown", reason: "GitHub did not name a latest release." };

    const version = tag.replace(/^v/, "");
    if (!isNewer(version, current)) return { state: "current", version: current };

    // The notes are a nicety: an update people cannot read about is one they
    // have no reason to trust, but a failure here must not hide the update.
    let notes = "";
    try {
      const { stdout: body } = await run(
        "curl",
        ["-fsSL", "--max-time", "10", `https://api.github.com/repos/${REPO}/releases/tags/${tag}`],
        { timeout: 15_000 },
      );
      const parsed = JSON.parse(body) as { body?: string };
      notes = summarize(parsed.body ?? "");
    } catch {
      notes = "";
    }

    return {
      state: "available",
      version,
      current,
      notes,
      url: `https://github.com/${REPO}/releases/tag/${tag}`,
    };
  } catch (err) {
    return {
      state: "unknown",
      reason: err instanceof Error ? err.message : "Could not reach GitHub.",
    };
  }
}

/**
 * First readable sentence or two of a release body, for the banner.
 *
 * Release notes open with headings, tables and code fences that say nothing
 * in one line of UI, so those are skipped rather than truncated.
 */
function summarize(body: string): string {
  const lines = body.split("\n");
  let inFence = false;
  for (const raw of lines) {
    const line = raw.trim();
    if (line.startsWith("```")) {
      inFence = !inFence;
      continue;
    }
    if (inFence || line === "") continue;
    if (line.startsWith("#") || line.startsWith("|") || line.startsWith("---")) continue;
    const plain = line.replace(/[*_`]/g, "").replace(/\[([^\]]+)\]\([^)]+\)/g, "$1");
    if (plain.length < 12) continue;
    return plain.length > 180 ? `${plain.slice(0, 177)}…` : plain;
  }
  return "";
}

/**
 * Run the installer in Terminal, where it is visible.
 *
 * Deliberately not run headless inside the app: this replaces /Applications
 * and restarts the daemon, and work of that weight should happen somewhere
 * the person can watch it and read the failure. It also sidesteps the
 * impossible part — a process cannot cleanly overwrite the bundle it is
 * executing from, so the script quits this app partway through.
 */
export async function runInstaller(): Promise<void> {
  // osascript's `do script` takes an AppleScript string literal, so a quote
  // or backslash in the command would end the literal early. Neither appears
  // in the URL, but the escaping belongs here rather than in a comment.
  // --yes: the banner already asked, and the installer's own "already
  // installed" question would be a second prompt for a decision the person
  // just made. It still re-reads the version, so a stale banner cannot
  // trigger a pointless reinstall — it only skips asking twice.
  const command = `curl -fsSL ${INSTALL_URL} | sh -s -- --yes`;
  const escaped = command.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  await run("osascript", [
    "-e",
    `tell application "Terminal" to do script "${escaped}"`,
    "-e",
    'tell application "Terminal" to activate',
  ]);
}

/** The command itself, so the window can offer it for copying. */
export function installCommand(): string {
  return `curl -fsSL ${INSTALL_URL} | sh`;
}
