// Does a trace row open, and does it show the payload the daemon recorded?
//
// The request/response were on the wire all along and the window discarded
// them, so this asserts the detail actually renders rather than that a class
// was applied.
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");

const root = join(__dirname, "..");
require(join(root, "out", "main", "main.js"));
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

let failed = false;
const fail = (m) => { console.error("  ✗ " + m); failed = true; };

app.whenReady().then(async () => {
  let win;
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) { [win] = BrowserWindow.getAllWindows(); if (win) break; await wait(100); }
  if (!win) { fail("no window"); return app.exit(1); }
  if (win.webContents.isLoading()) await new Promise((r) => win.webContents.once("did-finish-load", r));
  await wait(3500);

  const js = (code) => win.webContents.executeJavaScript(code);

  if (!(await js(`!!document.querySelector('[data-tab="trace"]')`))) {
    console.log("SKIP — no trace tab (daemon unpaired?)"); return app.exit(0);
  }
  await js(`document.querySelector('[data-tab="trace"]').click()`);
  await wait(1800);

  // The selected tab must look selected.
  const active = await js(`(() => {
    const el = document.querySelector('[data-tab="trace"]');
    return el && el.classList.contains("is-active");
  })()`);
  if (!active) fail("the Trace tab is not marked active when selected");

  const rows = await js(`document.querySelectorAll("[data-trace-row]").length`);
  if (rows === 0) { console.log("SKIP — trace is empty"); return app.exit(failed ? 1 : 0); }

  const before = await js(`document.querySelectorAll(".trc-detail").length`);
  await js(`document.querySelector("[data-trace-row]").click()`);
  await wait(600);
  const after = await js(`document.querySelectorAll(".trc-detail").length`);
  if (after <= before) fail(`clicking a trace row opened no detail (${before} -> ${after})`);

  const detail = await js(`(() => {
    const d = document.querySelector(".trc-detail");
    if (!d) return null;
    return {
      labels: [...d.querySelectorAll(".trc-label")].map(e => e.textContent),
      hasPre: !!d.querySelector(".trc-pre"),
      preText: (d.querySelector(".trc-pre") || {}).textContent || "",
    };
  })()`);
  if (!detail) fail("no .trc-detail rendered");
  else {
    if (!detail.hasPre) fail("the detail has no payload block");
    if (!detail.labels.includes("When")) fail(`detail labels missing When: ${detail.labels}`);
  }

  // Filtering must narrow the list.
  const all = await js(`document.querySelectorAll("[data-trace-row]").length`);
  await js(`(() => {
    const f = document.querySelector("[data-trace-query]");
    f.value = "zzz-no-such-call";
    f.dispatchEvent(new Event("input", { bubbles: true }));
  })()`);
  await wait(500);
  const none = await js(`document.querySelectorAll("[data-trace-row]").length`);
  if (none !== 0) fail(`a nonsense filter still matched ${none} rows`);
  await js(`(() => {
    const f = document.querySelector("[data-trace-query]");
    f.value = "";
    f.dispatchEvent(new Event("input", { bubbles: true }));
  })()`);
  await wait(500);
  const back = await js(`document.querySelectorAll("[data-trace-row]").length`);
  if (back !== all) fail(`clearing the filter restored ${back} of ${all} rows`);

  if (failed) return app.exit(1);
  console.log("TRACE PASS");
  console.log(`  rows            ${all}`);
  console.log(`  tab active      yes`);
  console.log(`  row expands     yes (${detail.labels.length} fields)`);
  console.log(`  payload shown   ${detail.preText.slice(0, 48).replace(/\n/g, " ")}…`);
  console.log(`  filter          narrows to 0 and restores to ${back}`);
  app.exit(0);
});
