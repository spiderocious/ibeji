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
    const btn = document.querySelector('[data-action="add-by-ip"]');
    if (!btn) return { ok: false, why: "no Add by IP button (is a device already paired?)" };
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
