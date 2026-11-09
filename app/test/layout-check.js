// Does the window keep its chrome in place while a list scrolls?
//
// The whole document used to scroll, so a long Ports or Trace list carried
// the header, the tabs and the filter field you were typing into off the top.
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");
require(join(__dirname, "..", "out", "main", "main.js"));
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
  let win; const d = Date.now() + 15000;
  while (Date.now() < d) { [win] = BrowserWindow.getAllWindows(); if (win) break; await wait(100); }
  if (!win) { fail("no window"); return app.exit(1); }
  if (win.webContents.isLoading()) await new Promise((r) => win.webContents.once("did-finish-load", r));
  // A short window, so any real list has to scroll. On a tall test window
  // everything fits and the scroll assertions never run.
  win.setSize(900, 420);
  await wait(3500);
  const js = (c) => win.webContents.executeJavaScript(c);

  if (!(await js(`!!document.querySelector('[data-tab="ports"]')`))) {
    console.log("SKIP — not paired, no tabs"); return app.exit(0);
  }

  // The document itself must never scroll.
  const doc = await js(`({ scroll: document.documentElement.scrollHeight, client: document.documentElement.clientHeight })`);
  if (doc.scroll > doc.client + 1) {
    fail(`the document scrolls (${doc.scroll} > ${doc.client}); chrome will ride away with the list`);
  }

  await js(`document.querySelector('[data-tab="ports"]').click()`);
  await wait(1500);

  // The header must be compact off the Devices tab.
  if (!(await js(`!!document.querySelector(".app-header.is-compact")`))) {
    fail("the header is not compact on the Ports tab");
  }
  if (await js(`!!document.querySelector(".app-header .dev-addr")`)) {
    fail("the key fingerprint is still shown on the Ports tab");
  }

  // Scroll the list and check the chrome did not move.
  const before = await js(`(() => {
    const r = (s) => { const e = document.querySelector(s); return e ? e.getBoundingClientRect().top : null; };
    return { tabs: r(".app-tabs"), controls: r(".app-controls"), header: r(".app-header"),
             scrollable: document.querySelector(".app-main").scrollHeight >
                         document.querySelector(".app-main").clientHeight };
  })()`);
  if (!before.scrollable) {
    // Ports can be short on a quiet machine; the trace is not.
    await js(`document.querySelector('[data-tab="trace"]').click()`);
    await wait(1800);
    const canScroll = await js(`(() => { const m = document.querySelector(".app-main");
      return m.scrollHeight > m.clientHeight; })()`);
    if (!canScroll) {
      const d = await js(`(() => { const m = document.querySelector(".app-main");
        const first = document.querySelector(".trc-row");
        return JSON.stringify({ scrollH: m.scrollHeight, clientH: m.clientHeight,
          rows: document.querySelectorAll("[data-trace-row]").length,
          rowH: first ? Math.round(first.getBoundingClientRect().height) : null,
          mainTop: Math.round(m.getBoundingClientRect().top),
          winH: innerHeight, tab: document.querySelector(".seg-item.is-active")?.textContent }); })()`);
      console.log("SKIP — no list long enough to scroll " + d);
      return app.exit(failed ? 1 : 0);
    }
    Object.assign(before, await js(`(() => {
      const r = (s) => { const e = document.querySelector(s); return e ? e.getBoundingClientRect().top : null; };
      return { tabs: r(".app-tabs"), controls: r(".app-controls"), header: r(".app-header") };
    })()`));
  }

  // Be explicit about the tab: the Shell pane scrolls its own output rather
  // than .app-main, so a test that happens to land there measures the wrong
  // element and reports a working layout as broken.
  await js(`(() => {
    const t = document.querySelector('[data-tab="trace"]') ||
              document.querySelector('[data-tab="ports"]');
    if (t) t.click();
  })()`);
  await wait(1500);

  Object.assign(before, await js(`(() => {
    const r = (s) => { const e = document.querySelector(s); return e ? e.getBoundingClientRect().top : null; };
    return { tabs: r(".app-tabs"), controls: r(".app-controls"), header: r(".app-header") };
  })()`));

  await js(`document.querySelector(".app-main").scrollTop = 600`);
  await wait(400);
  const after = await js(`(() => {
    const r = (s) => { const e = document.querySelector(s); return e ? e.getBoundingClientRect().top : null; };
    return { tabs: r(".app-tabs"), controls: r(".app-controls"), header: r(".app-header"),
             scrolled: document.querySelector(".app-main").scrollTop };
  })()`);

  if (after.scrolled < 100) fail(`the list did not scroll (scrollTop ${after.scrolled})`);
  for (const k of ["header", "tabs", "controls"]) {
    if (before[k] === null || after[k] === null) continue;
    if (Math.abs(before[k] - after[k]) > 1) {
      fail(`the ${k} moved while the list scrolled (${before[k]} -> ${after[k]})`);
    }
  }

  if (failed) return app.exit(1);
  console.log("LAYOUT PASS");
  console.log(`  document        does not scroll (${doc.scroll}px in ${doc.client}px)`);
  console.log(`  list scrolled   ${after.scrolled}px`);
  console.log(`  header/tabs/controls stayed put`);
  app.exit(0);
});
