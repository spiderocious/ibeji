// Does an open trace row stay open when new events arrive?
//
// The window polls every 5s and re-renders. If the open set is keyed by
// anything that shifts as rows are prepended — an array index, say — then
// reading a payload while the daemon is busy is impossible: the row you
// opened closes, or worse, a different row opens in its place.
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");
const root = join(__dirname, "..");
require(join(root, "out", "main", "main.js"));
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

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


let failed = false;
const fail = (m) => { console.error("  ✗ " + m); failed = true; };

app.whenReady().then(async () => {
  let win;
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) { [win] = BrowserWindow.getAllWindows(); if (win) break; await wait(100); }
  if (!win) { fail("no window"); return app.exit(1); }
  if (win.webContents.isLoading()) await new Promise((r) => win.webContents.once("did-finish-load", r));
  await wait(3500);
  const js = (c) => win.webContents.executeJavaScript(c);

  if (!(await js(`!!document.querySelector('[data-tab="trace"]')`))) {
    console.log("SKIP — no trace tab"); return app.exit(0);
  }
  await js(`document.querySelector('[data-tab="trace"]').click()`);
  await wait(1800);

  const rows = await js(`document.querySelectorAll("[data-trace-row]").length`);
  if (rows === 0) { console.log("SKIP — trace empty"); return app.exit(0); }

  // Open a row and remember WHICH one, by its key and its visible call name.
  const opened = await js(`(() => {
    const btn = document.querySelector("[data-trace-row]");
    const key = btn.getAttribute("data-trace-row");
    btn.click();
    const row = document.querySelector(".trc-row.is-open");
    return { key, type: row ? row.querySelector(".run-dev").textContent : null };
  })()`);
  await wait(400);
  if (!(await js(`!!document.querySelector(".trc-detail")`))) {
    fail("the row did not open at all");
    return app.exit(1);
  }
  console.log(`  opened ${opened.type} (key ${opened.key})`);

  // Now generate traffic so new events land, and wait out two poll cycles.
  console.log("  generating new trace events, waiting through 2 polls…");
  for (let i = 0; i < 6; i++) {
    await js(`window.ibeji.doctor()`);
    await wait(300);
  }
  await wait(11000);

  // What does the open set hold, versus what keys are on screen?
  const diag = await js(`(() => {
    const keys = [...document.querySelectorAll("[data-trace-row]")]
      .slice(0, 5).map(b => b.getAttribute("data-trace-row"));
    return JSON.stringify({ firstKeysOnScreen: keys });
  })()`);
  console.log("  " + diag);

  const after = await js(`(() => {
    const open = document.querySelectorAll(".trc-detail").length;
    const row = document.querySelector(".trc-row.is-open");
    return {
      openCount: open,
      key: row ? row.querySelector("[data-trace-row]").getAttribute("data-trace-row") : null,
      type: row ? row.querySelector(".run-dev").textContent : null,
      total: document.querySelectorAll("[data-trace-row]").length,
    };
  })()`);

  if (after.openCount === 0) {
    fail(`the open row closed when new events arrived (${rows} rows -> ${after.total})`);
  } else if (after.key !== opened.key) {
    fail(`a DIFFERENT row is open: expected ${opened.key} (${opened.type}), got ${after.key} (${after.type})`);
  }

  if (failed) return app.exit(1);
  console.log("TRACE STABILITY PASS");
  console.log(`  rows grew       ${rows} -> ${after.total}`);
  console.log(`  still open      ${after.type} (key ${after.key})`);
  app.exit(0);
});
