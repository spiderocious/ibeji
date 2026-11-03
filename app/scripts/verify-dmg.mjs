/**
 * Refuse to ship a DMG whose Go binaries do not match its Electron binary.
 *
 * This is not hypothetical: the first build produced an Intel DMG containing
 * an arm64 daemon. The window opened and the daemon died with "Bad CPU type in
 * executable", so the app looked installed and simply never linked — the worst
 * kind of failure, because nothing says what is wrong.
 */
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const want = process.argv[2] === "x64" ? "x86_64" : "arm64";
const root = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const dmg = join(root, "dist", want === "arm64" ? "Ibeji-0.1.0-arm64.dmg" : "Ibeji-0.1.0.dmg");

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
  console.log(`  OK — every binary in ${dmg.split("/").pop()} is ${want}`);
} finally {
  try { execFileSync("hdiutil", ["detach", mnt, "-quiet"]); } catch {}
  rmSync(mnt, { recursive: true, force: true });
}
