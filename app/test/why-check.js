// Does "Why?" answer the question?
//
// It used to navigate to the Health tab. That is not an answer — it is a
// filing cabinet, and handing somebody a log when they asked a question is
// what makes software feel contemptuous. The user's words: "clicking why on
// the top notification doesnt even show me why, wtf, it takes me to the logs,
// how does that even make sense".
//
// This raises the unreachable notice the way the product does, clicks Why?,
// and asserts an explanation appears in place — leading with the cause that
// actually bit: the other Mac having been given a new address. That
// possibility was never mentioned anywhere in the product, which is why a day
// went into chasing a macOS permission that was fine.
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");

const root = join(__dirname, "..");
require(join(root, "out", "main", "main.js"));

process.on("unhandledRejection", (err) => {
  console.error("  ✗ script error in the test:", err && err.message ? err.message : err);
  process.exit(1);
});
const HARD_TIMEOUT_MS = Number(process.env.IBEJI_TEST_TIMEOUT_MS || 90000);
setTimeout(() => {
  console.error(`  ✗ timed out after ${HARD_TIMEOUT_MS}ms`);
  process.exit(1);
}, HARD_TIMEOUT_MS).unref();

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
    console.log("WHY FAIL: no window");
    return app.exit(1);
  }
  await new Promise((r) => setTimeout(r, 4000));

  // The notice is normally raised by the daemon losing a peer, which cannot be
  // forced from a test. The renderer exposes a hook for exactly this so the
  // real banner, the real button and the real dispatch all run — only the
  // trigger is synthetic.
  const banner = await win.webContents.executeJavaScript(`(async () => {
    if (!window.__ibejiTest?.notify) return { hook: false };
    window.__ibejiTest.notify("The paired device is not reachable.", "attention");
    await new Promise((r) => setTimeout(r, 400));
    return {
      hook: true,
      banner: !!document.querySelector(".banner-attention"),
      why: !!document.querySelector('[data-action="why"]'),
      // The old wiring sent this button to the Health tab. If that action is
      // what the banner offers, the fix is not in.
      goHealthInBanner: !!document.querySelector('.banner [data-action="go-health"]'),
    };
  })()`);

  if (!banner.hook) {
    console.log("WHY FAIL: the renderer test hook is missing");
    return app.exit(1);
  }
  check(banner.banner, "an unreachable notice raises a banner");
  check(banner.why, "the banner offers a Why? button");
  check(
    !banner.goHealthInBanner,
    'the banner no longer sends Why? straight to the logs',
  );

  // --- clicking it must explain, in place ---------------------------------
  const opened = await win.webContents.executeJavaScript(`(async () => {
    const tabBefore = document.querySelector(".seg-item.is-active")?.textContent?.trim() ?? null;
    document.querySelector('[data-action="why"]').click();
    await new Promise((r) => setTimeout(r, 400));
    const why = document.querySelector(".why");
    return {
      opened: !!why,
      // The point of the fix: it explains where you are, rather than moving
      // you somewhere else.
      tabAfter: document.querySelector(".seg-item.is-active")?.textContent?.trim() ?? null,
      tabBefore,
      titles: why ? [...why.querySelectorAll(".why-title")].map((e) => e.textContent.trim()) : [],
      hasSweep: !!document.querySelector('[data-action="why-sweep"]'),
      bodyLen: why ? why.textContent.trim().length : 0,
    };
  })()`);

  check(opened.opened, "clicking Why? opens an explanation");
  check(
    opened.tabAfter === opened.tabBefore,
    "it explains in place rather than navigating away",
    `tab went from ${JSON.stringify(opened.tabBefore)} to ${JSON.stringify(opened.tabAfter)}`,
  );
  check(
    opened.titles.length >= 3,
    "it lists the causes that look identical from this side",
    `${opened.titles.length} listed`,
  );
  // The ordering is the finding from the real failure: on a home network with
  // DHCP a moved address is the most likely cause by a wide margin, and it is
  // the only one Ìbejì can resolve by itself.
  check(
    /new address/i.test(opened.titles[0] ?? ""),
    "it leads with the address having changed, the likeliest cause and the one Ìbejì can check",
    `first reason was ${JSON.stringify(opened.titles[0])}`,
  );
  check(opened.hasSweep, "it offers to scan the network for the moved Mac");
  check(
    opened.titles.some((t) => /macOS|blocking/i.test(t)),
    "it still covers the macOS permission block",
  );
  check(
    opened.bodyLen > 300,
    "the explanation actually explains rather than gesturing at an answer",
    `${opened.bodyLen} characters`,
  );

  // --- and it closes again -------------------------------------------------
  const closed = await win.webContents.executeJavaScript(`(async () => {
    document.querySelector('[data-action="why"]').click();
    await new Promise((r) => setTimeout(r, 300));
    return !document.querySelector(".why");
  })()`);
  check(closed, "clicking it again hides the explanation");

  if (fails.length > 0) {
    console.log(`WHY FAIL: ${fails.length} check(s) failed`);
    return app.exit(1);
  }
  console.log("WHY OK");
  app.exit(0);
});
