// Renderer smoke test.
//
// Opens the real window against whatever daemon is running, waits for the
// first data load, then checks three things a clean exit would not show:
//
//   - the design system's stylesheets actually loaded and its tokens resolve,
//   - the preload bridge attached and the renderer got real data through it,
//   - every CSS class the renderer used is defined somewhere. The design
//     system is frozen, so a class that is not in it is the app drifting.
//
// It reuses the real main process rather than building its own window, so what
// is tested is what ships: the same IPC handlers, the same preload, the same
// API client. Writes a screenshot for a human to look at.
//
// Run with: npm run smoke
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");
const { readFileSync, writeFileSync } = require("node:fs");

const root = join(__dirname, "..");
const OUT = process.env.IBEJI_SMOKE_PNG || join(root, "out", "smoke.png");

let failed = false;
function fail(msg) {
  console.error(`  ✗ ${msg}`);
  failed = true;
}

// Load the real main process. It registers the IPC handlers, creates the
// window and wires the event stream — all the things a hand-built window in a
// test would skip, and therefore not test.
require(join(root, "out", "main", "main.js"));

app.whenReady().then(async () => {
  // The main process creates its own window; wait for it rather than making
  // another, so this exercises the real one.
  const deadline = Date.now() + 10_000;
  let win;
  while (Date.now() < deadline) {
    [win] = BrowserWindow.getAllWindows();
    if (win) break;
    await new Promise((r) => setTimeout(r, 100));
  }
  if (!win) {
    fail("the main process never created a window");
    return app.exit(1);
  }

  const consoleMessages = [];
  win.webContents.on("console-message", (e) => {
    consoleMessages.push(`[${e.level}] ${e.message}`);
  });

  if (win.webContents.isLoading()) {
    await new Promise((r) => win.webContents.once("did-finish-load", r));
  }

  // Wait for the first data load to land rather than guessing a delay: the
  // first paint says "Connecting…" and real content replaces it.
  let settled = false;
  const contentDeadline = Date.now() + 10_000;
  while (Date.now() < contentDeadline) {
    settled = await win.webContents.executeJavaScript(
      `!!document.querySelector(".app-header, .es, .dev-row")`,
    );
    if (settled) break;
    await new Promise((r) => setTimeout(r, 200));
  }
  if (!settled) {
    console.error("  console:", consoleMessages.join(" | ") || "(none)");
    const html = await win.webContents.executeJavaScript(`document.body.innerHTML.slice(0,500)`);
    console.error("  body:", html);
    fail("the renderer never rendered past its loading state");
    return app.exit(1);
  }

  const probe = await win.webContents.executeJavaScript(`(() => {
    const r = getComputedStyle(document.documentElement);
    const body = getComputedStyle(document.body);
    return {
      bodyBg: body.backgroundColor,
      tokenCanvas: r.getPropertyValue("--surface-canvas").trim(),
      tokenOceanic: r.getPropertyValue("--sea-600").trim(),
      tokenTerritory: r.getPropertyValue("--font-territory").trim(),
      tokenPulse: r.getPropertyValue("--pulse-period").trim(),
      sheetCount: document.styleSheets.length,
      bridgePresent: typeof window.ibeji === "object" && window.ibeji !== null,
      bridgeKeys: window.ibeji ? Object.keys(window.ibeji).sort() : [],
      headerName: (document.querySelector(".app-header .dev-name") || {}).textContent || "",
      hasEmptyState: !!document.querySelector(".es"),
      hasDeviceRow: !!document.querySelector(".dev-row"),
      hasPairingCode: !!document.querySelector(".figure-xl"),
      pairingCode: (document.querySelector(".figure-xl") || {}).textContent || "",
      pairingCodeFont: document.querySelector(".figure-xl")
        ? getComputedStyle(document.querySelector(".figure-xl")).fontFamily : "",
      emptyStateMsg: (document.querySelector(".es-msg") || {}).textContent || "",
      daemonDown: document.body.textContent.includes("Ìbejì is not running"),
      usedClasses: [...new Set([...document.querySelectorAll("[class]")]
        .flatMap((el) => [...el.classList]))].sort(),
    };
  })()`);

  writeFileSync(OUT, (await win.webContents.capturePage()).toPNG());

  const checks = [
    [probe.sheetCount >= 3, `expected >=3 stylesheets, got ${probe.sheetCount}`],
    [probe.tokenCanvas === "#F4F1E9", `--surface-canvas = ${probe.tokenCanvas} (design system not loaded)`],
    [probe.tokenOceanic === "#1B6B72", `--sea-600 = ${probe.tokenOceanic}`],
    [probe.tokenTerritory.includes("Spectral"), `--font-territory = ${probe.tokenTerritory}`],
    [probe.tokenPulse === "2.6s", `--pulse-period = ${probe.tokenPulse}`],
    [probe.bodyBg === "rgb(244, 241, 233)", `body background = ${probe.bodyBg}, want paper-200`],
    [probe.bridgePresent, "window.ibeji is missing: the preload bridge did not attach"],
    [probe.bridgeKeys.includes("devices"), `bridge is missing devices(): ${probe.bridgeKeys.join(",")}`],
    [probe.bridgeKeys.includes("startPairing"), "bridge is missing startPairing()"],
    [probe.headerName.length > 0 || probe.hasEmptyState || probe.daemonDown,
      "the renderer shows neither a device header nor an empty state"],
    // The pairing code is the one deliberate exception to the type law: a
    // record value set in territory type, because it is read aloud across a
    // room rather than scanned in a log.
    [!probe.hasPairingCode || /^\d{3} \d{3}$/.test(probe.pairingCode.trim()),
      `the pairing code renders as "${probe.pairingCode}", want six digits grouped in threes`],
    [!probe.hasPairingCode || probe.pairingCodeFont.includes("Spectral"),
      `the pairing code is set in ${probe.pairingCodeFont}; the design system makes it the one ` +
      `record value in territory type, because it is read aloud rather than scanned`],
    [!consoleMessages.some((m) => m.startsWith("[3]")), `renderer console errors: ${consoleMessages.join(" | ")}`],
  ];
  for (const [ok, msg] of checks) if (!ok) fail(msg);

  // Hold the markup against the frozen design system: every class used must be
  // defined in it or in the app's own chrome.
  const css = ["styles/_components.css", "styles/_foundation.css", "renderer/app.css"]
    .map((f) => readFileSync(join(root, "out", f), "utf8"))
    .join("\n");
  const unknown = probe.usedClasses.filter((c) => !css.includes("." + c));
  if (unknown.length) {
    fail(`classes not defined in the design system or app chrome: ${unknown.join(", ")}`);
  }

  if (failed) return app.exit(1);

  console.log("SMOKE PASS");
  console.log(`  stylesheets      ${probe.sheetCount}`);
  console.log(`  tokens           canvas ${probe.tokenCanvas} · oceanic ${probe.tokenOceanic}`);
  console.log(`  bridge           ${probe.bridgeKeys.length} functions`);
  console.log(`  device name      ${probe.headerName || "(none)"}`);
  console.log(
    `  state            ${
      probe.daemonDown
        ? "daemon not running"
        : probe.hasPairingCode
          ? `pairing ceremony (code ${probe.pairingCode.trim()})`
          : probe.hasDeviceRow
            ? "device row"
            : "empty state"
    }`,
  );
  if (probe.emptyStateMsg) console.log(`  empty state      "${probe.emptyStateMsg.trim()}"`);
  console.log(`  classes used     ${probe.usedClasses.length}, all defined`);
  console.log(`  screenshot       ${OUT}`);
  app.exit(0);
});
