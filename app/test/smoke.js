// Renderer smoke test.
//
// Opens the real window, waits for the renderer to run, then reads back
// computed styles to prove the design system's stylesheets actually loaded —
// a clean exit alone would not show that. Writes a screenshot so a human (or a
// later visual diff) can see what rendered.
//
// Run with: npm run smoke
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");
const { writeFileSync } = require("node:fs");

const OUT = process.env.IBEJI_SMOKE_PNG || join(__dirname, "..", "out", "smoke.png");
const root = join(__dirname, "..");

function fail(msg) {
  console.error(`SMOKE FAIL: ${msg}`);
  app.exit(1);
}

app.whenReady().then(async () => {
  const consoleMessages = [];
  const win = new BrowserWindow({
    width: 980,
    height: 680,
    show: false,
    backgroundColor: "#F4F1E9",
    webPreferences: {
      preload: join(root, "out", "preload", "preload.js"),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  });
  win.webContents.on("console-message", (e) => {
    consoleMessages.push(`[${e.level}] ${e.message}`);
  });

  try {
    await win.loadFile(join(root, "out", "renderer", "index.html"));
  } catch (err) {
    return fail(`loadFile: ${err}`);
  }
  // Let the renderer's async boot() settle.
  await new Promise((r) => setTimeout(r, 1500));

  const probe = await win.webContents.executeJavaScript(`(() => {
    const root = getComputedStyle(document.documentElement);
    const body = getComputedStyle(document.body);
    return {
      bodyBg: body.backgroundColor,
      bodyFontFamily: body.fontFamily,
      tokenCanvas: root.getPropertyValue("--surface-canvas").trim(),
      tokenOceanic: root.getPropertyValue("--sea-600").trim(),
      tokenTerritory: root.getPropertyValue("--font-territory").trim(),
      tokenPulse: root.getPropertyValue("--pulse-period").trim(),
      sheetCount: document.styleSheets.length,
      bridgePresent: typeof window.ibeji === "object" && window.ibeji !== null,
      bridgeKeys: window.ibeji ? Object.keys(window.ibeji).sort() : [],
      bootText: (document.querySelector(".app-boot") || {}).textContent || "",
    };
  })()`);

  writeFileSync(OUT, (await win.webContents.capturePage()).toPNG());

  // Assertions. Each one is a thing that silently breaks and would otherwise
  // only be noticed by eye much later.
  const checks = [
    [probe.sheetCount >= 3, `expected >=3 stylesheets, got ${probe.sheetCount}`],
    [probe.tokenCanvas === "#F4F1E9", `--surface-canvas = ${probe.tokenCanvas}, want #F4F1E9 (design system not loaded)`],
    [probe.tokenOceanic === "#1B6B72", `--sea-600 = ${probe.tokenOceanic}, want #1B6B72`],
    [probe.tokenTerritory.includes("Spectral"), `--font-territory = ${probe.tokenTerritory}, want Spectral`],
    [probe.tokenPulse === "2.6s", `--pulse-period = ${probe.tokenPulse}, want 2.6s`],
    [probe.bodyBg === "rgb(244, 241, 233)", `body background = ${probe.bodyBg}, want paper-200`],
    [probe.bridgePresent, "window.ibeji is missing: the preload bridge did not attach"],
    [probe.bridgeKeys.includes("devices"), `bridge is missing devices(): ${probe.bridgeKeys.join(",")}`],
    [probe.bridgeKeys.includes("startPairing"), "bridge is missing startPairing()"],
    [probe.bootText.length > 0, "the renderer rendered nothing"],
    [!consoleMessages.some((m) => m.startsWith("[3]")), `renderer console errors: ${consoleMessages.join(" | ")}`],
  ];

  let failed = false;
  for (const [ok, msg] of checks) {
    if (!ok) {
      console.error(`  ✗ ${msg}`);
      failed = true;
    }
  }
  if (failed) return fail("assertions above");

  console.log("SMOKE PASS");
  console.log(`  stylesheets      ${probe.sheetCount}`);
  console.log(`  canvas token     ${probe.tokenCanvas}`);
  console.log(`  oceanic token    ${probe.tokenOceanic}`);
  console.log(`  territory font   ${probe.tokenTerritory}`);
  console.log(`  bridge           ${probe.bridgeKeys.length} functions`);
  console.log(`  rendered         "${probe.bootText.trim()}"`);
  console.log(`  screenshot       ${OUT}`);
  app.exit(0);
});
