// Does a scrolled list stay where it was put?
//
// The bug: render() replaces the whole of #root, which destroys the scroll
// container and silently resets it to the top. A poll runs every 5 seconds, so
// anything you scrolled jerked back to the top every five seconds. It showed up
// worst on Health — the daemon log is long and also refreshes — which made it
// look like the log was the cause. It was not. It was every tab, all the time.
//
// This test scrolls, waits out more than one full poll, and asserts the
// position survived. The waiting is the point: a test that scrolled and read
// back immediately would pass against the broken version, because nothing had
// re-rendered yet.
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");

const root = join(__dirname, "..");
require(join(root, "out", "main", "main.js"));

process.on("unhandledRejection", (err) => {
  console.error("  ✗ script error in the test:", err && err.message ? err.message : err);
  process.exit(1);
});
const HARD_TIMEOUT_MS = Number(process.env.IBEJI_TEST_TIMEOUT_MS || 120000);
setTimeout(() => {
  console.error(`  ✗ timed out after ${HARD_TIMEOUT_MS}ms`);
  process.exit(1);
}, HARD_TIMEOUT_MS).unref();

// The renderer polls every 5s. Waiting 12 guarantees at least two renders have
// happened, so a position that survives really survived rather than being read
// back before anything touched it.
const WAIT_MS = 12_000;

const fails = [];
function check(ok, what, detail) {
  if (ok) {
    console.log(`  ✓ ${what}`);
    return;
  }
  console.log(`  ✗ ${what}${detail ? ` — ${detail}` : ""}`);
  fails.push(what);
}

app.whenReady().then(async () => {
  const deadline = Date.now() + 10_000;
  let win;
  while (Date.now() < deadline) {
    [win] = BrowserWindow.getAllWindows();
    if (win) break;
    await new Promise((r) => setTimeout(r, 100));
  }
  if (!win) {
    console.log("SCROLL FAIL: no window");
    return app.exit(1);
  }
  await new Promise((r) => setTimeout(r, 4000));

  // Health is the tab the complaint was about, and the only one guaranteed to
  // have enough content to scroll whether or not a device is paired: it always
  // shows this machine's checks plus the daemon log.
  const ready = await win.webContents.executeJavaScript(`(async () => {
    const tab = document.querySelector('[data-tab="health"]');
    if (!tab) return { ok: false, why: "no Health tab" };
    tab.click();
    // The log is fetched on first open, so give it a moment to arrive.
    await new Promise((r) => setTimeout(r, 3000));
    const main = document.querySelector(".app-main");
    const log = document.getElementById("hl-log");
    return {
      ok: true,
      mainScrollable: !!main && main.scrollHeight > main.clientHeight + 8,
      logScrollable: !!log && log.scrollHeight > log.clientHeight + 8,
      logLines: log ? log.textContent.split("\\n").length : 0,
    };
  })()`);

  if (!ready.ok) {
    console.log(`SCROLL FAIL: ${ready.why}`);
    return app.exit(1);
  }

  // --- the daemon log pane --------------------------------------------------
  if (ready.logScrollable) {
    const r = await win.webContents.executeJavaScript(`(async () => {
      const log = document.getElementById("hl-log");
      // Scroll to the bottom, which is where somebody reading a log goes and
      // where the jerk was most obvious.
      log.scrollTop = log.scrollHeight;
      const want = log.scrollTop;
      await new Promise((r) => setTimeout(r, ${WAIT_MS}));
      const el = document.getElementById("hl-log");
      return { want, got: el ? el.scrollTop : -1, present: !!el };
    })()`);
    check(r.present, "the log pane survives a re-render");
    // Allow a little slack: the log may have gained a line while waiting,
    // which legitimately moves the maximum scroll position.
    const drift = Math.abs(r.want - r.got);
    check(
      drift <= 40,
      "the daemon log stays where it was scrolled, across more than one poll",
      `was at ${r.want}, came back at ${r.got}`,
    );
  } else {
    console.log(`  - the daemon log is too short to scroll here (${ready.logLines} lines); skipping that check`);
  }

  // --- the main scroll area -------------------------------------------------
  //
  // This is the one that mattered beyond Health: every tab's list lives in
  // .app-main, so the reset hit all of them.
  if (ready.mainScrollable) {
    const r = await win.webContents.executeJavaScript(`(async () => {
      const main = document.querySelector(".app-main");
      main.scrollTop = Math.floor((main.scrollHeight - main.clientHeight) / 2);
      const want = main.scrollTop;
      await new Promise((r) => setTimeout(r, ${WAIT_MS}));
      const el = document.querySelector(".app-main");
      return { want, got: el ? el.scrollTop : -1 };
    })()`);
    check(
      Math.abs(r.want - r.got) <= 40,
      "the tab's own scroll position survives the 5-second poll",
      `was at ${r.want}, came back at ${r.got}`,
    );
  } else {
    console.log("  - the Health tab fits without scrolling here; skipping the main-area check");
  }

  // --- switching tabs must not inherit a stale position --------------------
  //
  // Positions are remembered per tab. Keying them globally would scroll a
  // short list to an offset borrowed from a long one, which looks like a
  // different bug of the same family.
  const switched = await win.webContents.executeJavaScript(`(async () => {
    const devices = document.querySelector('[data-tab="devices"]');
    if (!devices) return { skipped: true };
    devices.click();
    await new Promise((r) => setTimeout(r, 600));
    const main = document.querySelector(".app-main");
    return { skipped: false, top: main ? main.scrollTop : -1 };
  })()`);
  if (!switched.skipped) {
    check(
      switched.top === 0,
      "switching to a tab that was never scrolled starts at the top",
      `started at ${switched.top}`,
    );
  }

  // --- the pairing modal ---------------------------------------------------
  //
  // The modal body is its own scroll container, and toggling the log
  // re-renders the whole window — so it jerked to the top exactly like the
  // tabs did, at the moment somebody was reaching for the log to read it.
  const modal = await win.webContents.executeJavaScript(`(async () => {
    if (document.querySelector('[data-action="unpair"]')) return { skipped: "already paired" };
    const b = document.createElement("button");
    b.setAttribute("data-pair", "192.0.2.123:7749");
    document.body.appendChild(b); b.click(); b.remove();
    // Wait for the dial to time out, so the body is long enough to scroll.
    const until = Date.now() + 40000;
    while (Date.now() < until) {
      if (document.querySelector(".pm-fail")) break;
      await new Promise((r) => setTimeout(r, 400));
    }
    const body = document.querySelector(".pm-body");
    if (!body || body.scrollHeight <= body.clientHeight + 8) {
      return { skipped: "the modal fits without scrolling" };
    }
    body.scrollTop = body.scrollHeight;
    const want = body.scrollTop;
    // The reported action: click Show log.
    document.querySelector('[data-action="pm-toggle-log"]').click();
    await new Promise((r) => setTimeout(r, 500));
    const after = document.querySelector(".pm-body");
    return { skipped: null, want, got: after ? after.scrollTop : -1 };
  })()`);

  if (modal.skipped) {
    console.log(`  - modal scroll check skipped: ${modal.skipped}`);
  } else {
    check(
      modal.got > 0,
      "the pairing modal does not jerk to the top when the log is shown",
      `was at ${modal.want}, came back at ${modal.got}`,
    );
  }

  // Leave nothing open behind the test.
  await win.webContents.executeJavaScript(`(async () => {
    document.querySelector('[data-action="pm-close"]')?.click();
    await new Promise((r) => setTimeout(r, 300));
  })()`);

  if (fails.length > 0) {
    console.log(`SCROLL FAIL: ${fails.length} check(s) failed`);
    return app.exit(1);
  }
  console.log("SCROLL OK");
  app.exit(0);
});
