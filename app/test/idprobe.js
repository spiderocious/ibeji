const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");
require(join(__dirname, "..", "out", "main", "main.js"));
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
app.whenReady().then(async () => {
  let win; const d = Date.now() + 15000;
  while (Date.now() < d) { [win] = BrowserWindow.getAllWindows(); if (win) break; await wait(100); }
  if (win.webContents.isLoading()) await new Promise((r) => win.webContents.once("did-finish-load", r));
  await wait(3000);
  const out = await win.webContents.executeJavaScript(`(async () => {
    const r = await window.ibeji.trace();
    const ev = (r.ok ? r.value : {}).events || [];
    return JSON.stringify({ n: ev.length, first: ev[0] ? Object.keys(ev[0]) : null, id: ev[0] ? ev[0].id : "no events" });
  })()`);
  console.log("RENDERER SEES:", out);
  app.exit(0);
});
