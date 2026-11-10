// Guard: a UI test must not destroy the pairing on the machine it runs on.
//
// The Electron app reads ~/.ibeji/api.json directly and ignores
// IBEJI_INSTANCE, so every window test drives the *real* daemon — the one
// holding a real pairing with a real second Mac. On 2026-11-10 a test run
// ended with "No device paired": the pairing was gone, the other Mac was
// still trusting a key this one had thrown away, and re-pairing needed
// physical access to both machines.
//
// The trace recorded `api.unpair provenance=api status=204` but not the
// caller, so which test did it could not be established. That is the point:
// an invariant nobody can attribute is one worth enforcing mechanically
// rather than by reading the suite and hoping.
//
// Usage, around any test that drives the window:
//   node test/pairing-guard.js snapshot   # before
//   node test/pairing-guard.js verify     # after; exits 1 if the pair is gone
const { readFileSync, writeFileSync, existsSync } = require("node:fs");
const { join } = require("node:path");
const { homedir, tmpdir } = require("node:os");

const HANDOFF = join(homedir(), ".ibeji", "api.json");
const SNAP = join(tmpdir(), "ibeji-pairing-guard.json");

async function devices() {
  if (!existsSync(HANDOFF)) return { reachable: false, devices: [] };
  let h;
  try {
    h = JSON.parse(readFileSync(HANDOFF, "utf8"));
  } catch {
    return { reachable: false, devices: [] };
  }
  try {
    const r = await fetch(`${h.endpoint}/v1/devices`, {
      headers: { Authorization: `Bearer ${h.token}` },
      signal: AbortSignal.timeout(5000),
    });
    if (!r.ok) return { reachable: false, devices: [] };
    const body = await r.json();
    return { reachable: true, devices: body.devices ?? [] };
  } catch {
    return { reachable: false, devices: [] };
  }
}

(async () => {
  const mode = process.argv[2];
  const now = await devices();

  if (mode === "snapshot") {
    writeFileSync(SNAP, JSON.stringify(now));
    if (!now.reachable) {
      console.log("  - pairing guard: no daemon reachable; nothing to protect");
    } else if (now.devices.length === 0) {
      console.log("  - pairing guard: daemon is unpaired; nothing to protect");
    } else {
      console.log(`  - pairing guard: protecting the pair with ${now.devices[0].name}`);
    }
    return;
  }

  if (mode !== "verify") {
    console.error("usage: pairing-guard.js snapshot|verify");
    process.exit(2);
  }

  if (!existsSync(SNAP)) {
    console.log("  - pairing guard: no snapshot taken; skipping the check");
    return;
  }
  const before = JSON.parse(readFileSync(SNAP, "utf8"));

  // Only a pairing that existed before can be destroyed by the run. A daemon
  // that was already unpaired, or unreachable, has nothing to lose — and a
  // daemon that went unreachable is a different problem, not this one.
  if (!before.reachable || before.devices.length === 0) return;

  if (!now.reachable) {
    console.log("  ✗ pairing guard: the daemon became unreachable during the test run");
    process.exit(1);
  }
  if (now.devices.length === 0) {
    const name = before.devices[0].name ?? "the other Mac";
    console.log(`  ✗ pairing guard: THE TEST RUN UNPAIRED ${name}.`);
    console.log("    A UI test drove the real daemon and destroyed real state.");
    console.log("    The other Mac still trusts a key this one has discarded, so");
    console.log("    recovering it needs the six-digit ceremony on both machines.");
    console.log("    Give the window tests their own daemon before running them again.");
    process.exit(1);
  }
  console.log(`  - pairing guard: the pair with ${now.devices[0].name} survived`);
})();
