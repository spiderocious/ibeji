/**
 * Refuse to ship a DMG whose Go binaries do not match its Electron binary.
 *
 * This is not hypothetical: the first build produced an Intel DMG containing
 * an arm64 daemon. The window opened and the daemon died with "Bad CPU type in
 * executable", so the app looked installed and simply never linked — the worst
 * kind of failure, because nothing says what is wrong.
 */
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const want = process.argv[2] === "x64" ? "x86_64" : "arm64";
const root = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
// Read the version rather than hardcoding it: a stale literal here would make
// the verifier check a file that no longer exists, or silently pass an old one.
const { version } = JSON.parse(
  readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "package.json"), "utf8"),
);
const dmg = join(root, "dist", want === "arm64" ? `Ibeji-${version}-arm64.dmg` : `Ibeji-${version}.dmg`);

const mnt = mkdtempSync(join(tmpdir(), "ibeji-verify-"));
const archOf = (p) => {
  const out = execFileSync("file", ["-b", p], { encoding: "utf8" });
  return out.includes("x86_64") ? "x86_64" : out.includes("arm64") ? "arm64" : `unknown (${out.trim()})`;
};

try {
  execFileSync("hdiutil", ["attach", "-nobrowse", "-quiet", dmg, "-mountpoint", mnt]);
  const app = join(mnt, "Ibeji.app");
  const checks = {
    electron: join(app, "Contents", "MacOS", "Ibeji"),
    "ibeji": join(app, "Contents", "Resources", "bin", "ibeji"),
    "ibeji-daemon": join(app, "Contents", "Resources", "bin", "ibeji-daemon"),
    "ibeji-mcp": join(app, "Contents", "Resources", "bin", "ibeji-mcp"),
  };
  const bad = [];
  for (const [name, path] of Object.entries(checks)) {
    const got = archOf(path);
    console.log(`  ${name.padEnd(14)} ${got}`);
    if (got !== want) bad.push(`${name} is ${got}, want ${want}`);
  }
  if (bad.length) {
    console.error(`\n${dmg} is mixed-architecture:`);
    for (const b of bad) console.error(`  ${b}`);
    process.exit(1);
  }

  // The signature must verify from INSIDE the DMG, which is what a user
  // actually mounts. v0.1.0 passed every other check and still died with
  // "Ibeji is damaged and can't be opened", because the bundle wore stock
  // Electron's signature and nothing ever re-signed it. Running the binary
  // directly, as the earlier test did, bypasses Gatekeeper and hides this.
  try {
    execFileSync("codesign", ["--verify", "--deep", "--strict", app], {
      stdio: ["ignore", "ignore", "pipe"],
    });
    console.log("  signature     verifies");
  } catch (err) {
    const detail = (err.stderr || Buffer.from("")).toString().trim();
    console.error(`\n${dmg} has a broken signature: ${detail}`);
    console.error("  macOS will call this app damaged and refuse to open it.");
    process.exit(1);
  }

  // Gatekeeper will still reject an ad-hoc app as unidentified — that is
  // expected and is what right-click → Open is for. What must NOT appear is
  // any complaint about the code being damaged, modified or unsealed.
  const assess = (() => {
    try {
      execFileSync("spctl", ["-a", "-t", "exec", "-vv", app], { stdio: ["ignore", "ignore", "pipe"] });
      return "accepted";
    } catch (err) {
      return (err.stderr || Buffer.from("")).toString().trim();
    }
  })();
  if (/damaged|modified|resource|invalid|unsealed/i.test(assess)) {
    console.error(`\nGatekeeper reports a broken bundle: ${assess}`);
    process.exit(1);
  }
  console.log(`  gatekeeper    ${assess.split("\n")[0] || "rejected (unsigned — expected)"}`);

  // The daemon must carry a STABLE code-signing identifier, and the bundle
  // must declare why it wants the local network. Both are invisible when
  // wrong and both break the product completely.
  //
  // Without an explicit identifier codesign invents one from a hash of the
  // binary, so every build is a different app to macOS and the Local Network
  // permission granted to the last version does not carry over. Without the
  // usage string macOS has no prompt to show, so it never asks and silently
  // denies. Either way every LAN dial returns "no route to host" while the
  // internet still works — two Macs on one network that cannot see each
  // other, and nothing anywhere saying why.
  const daemonID = (() => {
    try {
      const out = execFileSync("codesign", ["-dv", join(app, "Contents/Resources/bin/ibeji-daemon")],
        { stdio: ["ignore", "ignore", "pipe"] });
      return (out || Buffer.from("")).toString();
    } catch (err) {
      return (err.stderr || Buffer.from("")).toString();
    }
  })();
  const idLine = /Identifier=(\S+)/.exec(daemonID)?.[1] ?? "";
  if (idLine !== "com.ibeji.ibeji-daemon") {
    console.error(`\nThe daemon's signing identifier is ${idLine || "missing"}, want com.ibeji.ibeji-daemon.`);
    console.error("  A per-build identifier makes macOS treat every release as a new app, so");
    console.error("  Local Network permission never carries over and the LAN goes dark.");
    process.exit(1);
  }
  console.log(`  signing id    ${idLine}`);

  const plist = execFileSync("plutil", ["-p", join(app, "Contents/Info.plist")],
    { stdio: ["ignore", "pipe", "pipe"] }).toString();
  for (const key of ["NSLocalNetworkUsageDescription", "NSBonjourServices"]) {
    if (!plist.includes(key)) {
      console.error(`\n${dmg} is missing ${key} in Info.plist.`);
      console.error("  macOS cannot prompt for local network access without it, so it denies");
      console.error("  silently and the two machines never see each other.");
      process.exit(1);
    }
  }
  console.log("  local network declared");

  console.log(`  OK — every binary in ${dmg.split("/").pop()} is ${want}`);
} finally {
  try { execFileSync("hdiutil", ["detach", mnt, "-quiet"]); } catch {}
  rmSync(mnt, { recursive: true, force: true });
}
