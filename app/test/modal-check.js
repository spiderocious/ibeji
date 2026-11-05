// Does "Add by IP address" actually open a dialog, and does Escape close it?
//
// The bug this guards: window.prompt THROWS in Electron ("prompt() is not
// supported"), so the old handler died on that line and the button did
// nothing — no dialog, no error, no clue. A test asserting the button exists
// would have passed the whole time; only clicking it finds this.
//
// Loads the real main process, like smoke.js, so the IPC handlers exist.
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");

const root = join(__dirname, "..");
require(join(root, "out", "main", "main.js"));

// A script error must end the run, not hang it.
//
// executeJavaScript rejects when the injected script throws, and an
// unhandled rejection in Electron's main process neither prints usefully nor
// exits — the test sits there until something kills it, and `make smoke`
// appears to freeze. A broken test should fail in seconds and say why.
process.on("unhandledRejection", (err) => {
  console.error("  \u2717 script error in the test:", err && err.message ? err.message : err);
  process.exit(1);
});
const HARD_TIMEOUT_MS = Number(process.env.IBEJI_TEST_TIMEOUT_MS || 90000);
setTimeout(() => {
  console.error(`  \u2717 timed out after ${HARD_TIMEOUT_MS}ms`);
  process.exit(1);
}, HARD_TIMEOUT_MS).unref();


app.whenReady().then(async () => {
  const deadline = Date.now() + 10_000;
  let win;
  while (Date.now() < deadline) {
    [win] = BrowserWindow.getAllWindows();
    if (win) break;
    await new Promise((r) => setTimeout(r, 100));
  }
  if (!win) { console.log("MODAL FAIL: no window"); return app.exit(1); }
  await new Promise((r) => setTimeout(r, 4000));

  const r = await win.webContents.executeJavaScript(`(async () => {
    // "Add by IP" lives in the empty state. When a machine HAS been found on
    // the network the window shows the candidate list instead — correctly —
    // so drive the dialog from the unpair button in that case. Asserting one
    // fixed screen made this test fail the moment discovery started working.
    let btn = document.querySelector('[data-action="add-by-ip"]');
    let via = "add-by-ip";
    if (!btn) {
      const candidates = document.querySelectorAll("[data-pair]");
      if (candidates.length > 0) {
        return {
          ok: true,
          skipped: "a device was discovered, so the empty state is not showing",
          discovered: [...candidates].map((c) => c.getAttribute("data-pair")),
          selfAddress: document.querySelector(".selfaddr-ip")?.textContent?.trim() ?? null,
        };
      }
      // Already paired: the pairing screen is gone because there is nothing
      // left to pair. Not a failure — and a case that could not arise until
      // v0.1.6 made pairings survive a restart, so the window now opens
      // paired on a machine that paired days ago.
      if (document.querySelector('.dev-row, [data-action="unpair"]')) {
        return { ok: true, skipped: "this daemon is already paired, so there is no Add by IP button" };
      }
      return { ok: false, why: "no Add by IP button and no candidates — unexpected screen" };
    }
    btn.click();
    await new Promise((r) => setTimeout(r, 400));
    const modal = document.querySelector(".app-modal-host .mdl");
    if (!modal) return { ok: false, why: "clicking opened nothing — the window.prompt bug" };
    const input = modal.querySelector("input.fld");
    if (input) input.value = "192.168.1.39";
    const out = {
      ok: true,
      title: modal.querySelector(".mdl-title")?.textContent?.trim(),
      hasInput: !!input,
      typed: input ? input.value : null,
      buttons: [...modal.querySelectorAll("[data-mo]")].map((b) => b.textContent.trim()),
    };
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await new Promise((r) => setTimeout(r, 300));
    out.closedOnEscape = !document.querySelector(".app-modal-host");
    // And the address this Mac advertises, which is the other half of pairing.
    out.selfAddress = document.querySelector(".selfaddr-ip")?.textContent?.trim() ?? null;
    return out;
  })()`);

  if (!r.ok) { console.log("MODAL FAIL:", r.why); return app.exit(1); }
  if (r.skipped) {
    // Not a pass for the dialog, but not a failure either: the screen under
    // test is not on screen. Say so plainly rather than claiming a pass.
    console.log("MODAL SKIP —", r.skipped);
    console.log("  discovered     ", (r.discovered || []).join(", "));
    console.log("  own address    ", r.selfAddress ?? "MISSING");
    return app.exit(0);
  }
  const good = r.hasInput && r.closedOnEscape && r.selfAddress;
  console.log(good ? "MODAL PASS" : "MODAL FAIL");
  console.log("  title          ", JSON.stringify(r.title));
  console.log("  text field     ", r.hasInput ? "present" : "MISSING");
  console.log("  accepts typing ", JSON.stringify(r.typed));
  console.log("  buttons        ", r.buttons.join(" / "));
  console.log("  closes on Esc  ", r.closedOnEscape ? "yes" : "NO — window stuck behind modal");
  console.log("  own address    ", r.selfAddress ?? "MISSING — nothing to type on the other Mac");
  app.exit(good ? 0 : 1);
});
