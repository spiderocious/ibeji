/**
 * Cross-compile the Go binaries for the architecture being packaged.
 *
 * electron-builder copies extraResources verbatim, so without this the DMG
 * ships whatever architecture `make build` last produced — an Intel installer
 * full of arm64 binaries that die with "Bad CPU type in executable" the first
 * time the daemon starts. The arch is passed by the npm script and must match
 * the --arm64/--x64 flag handed to electron-builder.
 */
import { execFileSync } from "node:child_process";
import { mkdirSync, rmSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");

// Explicit parens: `a || b ? x : y` parses as `(a || b) ? x : y`, which made
// every build amd64 regardless of the argument. Caught by checking `file` on
// the output — the kind of mistake that otherwise ships a broken installer.
const requested = process.argv[2] ?? process.arch;
const goarch = requested === "x64" || requested === "amd64" ? "amd64" : "arm64";
const out = join(root, "bin");

rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });

// Stamp the same version the Makefile does: a binary that reports
// "dev (none, built unknown)" makes a bug report unanswerable.
const pkg = JSON.parse(readFileSync(join(here, "..", "package.json"), "utf8"));
const git = (args, fallback) => {
  try {
    return execFileSync("git", args, { cwd: root, encoding: "utf8" }).trim();
  } catch {
    return fallback;
  }
};
const ldflags = [
  "-s", "-w",
  `-X main.version=${pkg.version}`,
  `-X main.commit=${git(["rev-parse", "--short", "HEAD"], "none")}`,
  `-X main.buildDate=${new Date().toISOString().replace(/\.\d+Z$/, "Z")}`,
].join(" ");

for (const cmd of ["ibeji", "ibeji-daemon", "ibeji-mcp"]) {
  execFileSync(
    "go",
    ["build", "-trimpath", "-ldflags", ldflags, "-o", join(out, cmd), `./cmd/${cmd}`],
    { cwd: root, stdio: "inherit", env: { ...process.env, GOOS: "darwin", GOARCH: goarch, CGO_ENABLED: "0" } },
  );
}
console.log(`  built darwin/${goarch} binaries into bin/`);
