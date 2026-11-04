// Capture retina screenshots of the real window against a live paired daemon,
// for the landing page.
//
// It names each file after the tab the DOM reports as selected, not after the
// tab it meant to click: an early version produced a "trace.png" that was
// actually the ports view, because the click landed elsewhere and nothing
// checked. It also trims the empty canvas below the content, so a short view
// does not ship as mostly background.
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");
const { writeFileSync } = require("node:fs");

const root = join(__dirname, "..");
const OUT = process.env.SHOT_DIR;
require(join(root, "out", "main", "main.js"));

const wait = (ms) => new Promise((r) => setTimeout(r, ms));

app.whenReady().then(async () => {
  let win;
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    [win] = BrowserWindow.getAllWindows();
    if (win) break;
    await wait(100);
  }
  if (!win) { console.error("no window"); return app.exit(1); }

  win.setSize(1180, 860);
  if (win.webContents.isLoading()) {
    await new Promise((r) => win.webContents.once("did-finish-load", r));
  }
  await wait(3500);

  // Which tab does the DOM say is showing?
  const activeTab = () => win.webContents.executeJavaScript(`(() => {
    const on = document.querySelector('[data-tab].is-selected, [data-tab][aria-selected="true"]');
    if (on) return on.getAttribute("data-tab");
    return "";
  })()`);

  // How tall is the real content? Used to crop the empty canvas beneath it.
  const contentHeight = () => win.webContents.executeJavaScript(`(() => {
    let max = 0;
    for (const el of document.querySelectorAll("body *")) {
      const r = el.getBoundingClientRect();
      if (r.width > 0 && r.height > 0) max = Math.max(max, r.bottom);
    }
    return Math.ceil(max);
  })()`);

  const shot = async (name) => {
    const h = await contentHeight();
    const { width, height } = win.getContentBounds();
    // Crop to the content plus a little breathing room. Clamped to the window
    // because capturePage cannot photograph what was never rendered.
    const want = Math.min(Math.max(Math.ceil(h) + 28, 320), height);
    const rect = { x: 0, y: 0, width, height: want };
    console.log(`    (content ${h}px -> crop ${want}px of ${height}px)`);
    const img = await win.webContents.capturePage(rect);
    writeFileSync(join(OUT, name + ".png"), img.toPNG());
    const s = img.getSize();
    console.log(`  ${name}.png  ${s.width}x${s.height}`);
  };

  await shot("devices");

  for (const tab of ["spaces", "ports", "trace"]) {
    const clicked = await win.webContents.executeJavaScript(
      `(() => { const el = document.querySelector('[data-tab="${tab}"]');
                if (!el) return false; el.click(); return true; })()`);
    if (!clicked) { console.log(`  (no ${tab} tab)`); continue; }
    await wait(2200);
    const got = await activeTab();
    if (got && got !== tab) {
      console.log(`  WARNING: clicked ${tab} but ${got} is showing; saving as ${got}`);
    }
    await shot(got || tab);
  }
  console.log("SHOTS DONE");
  app.exit(0);
});
