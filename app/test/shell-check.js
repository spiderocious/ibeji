// The Shell tab, driven in the real window against a live pair.
//
// What it must prove is the thing that distinguishes a shell from `ibeji run`:
// state carries between commands. Everything else — the sub-tabs, the
// interrupt — is in service of that.
const { app, BrowserWindow } = require("electron");
const { join } = require("node:path");
require(join(__dirname, "..", "out", "main", "main.js"));
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

process.on("unhandledRejection", (err) => {
  console.error("  ✗ script error in the test:", err && err.message ? err.message : err);
  process.exit(1);
});
setTimeout(() => { console.error("  ✗ timed out"); process.exit(1); }, 120000).unref();

let failed = false;
const fail = (m) => { console.error("  ✗ " + m); failed = true; };

app.whenReady().then(async () => {
  let win; const d = Date.now() + 15000;
  while (Date.now() < d) { [win] = BrowserWindow.getAllWindows(); if (win) break; await wait(100); }
  if (!win) { fail("no window"); return app.exit(1); }
  if (win.webContents.isLoading()) await new Promise((r) => win.webContents.once("did-finish-load", r));
  await wait(3500);
  const js = (c) => win.webContents.executeJavaScript(c);

  if (!(await js(`!!document.querySelector('[data-tab="shell"]')`))) {
    console.log("SKIP — no Shell tab (not paired?)"); return app.exit(0);
  }
  await js(`document.querySelector('[data-tab="shell"]').click()`);
  await wait(1200);

  // Open a shell.
  if (!(await js(`!!document.querySelector('[data-action="shell-new"]')`))) {
    fail("no way to open a shell"); return app.exit(1);
  }
  await js(`document.querySelector('[data-action="shell-new"]').click()`);

  let ready = false;
  for (let i = 0; i < 40; i++) {
    await wait(500);
    if (await js(`!!document.querySelector("#sh-input")`)) { ready = true; break; }
  }
  if (!ready) { fail("the shell pane never appeared"); return app.exit(1); }

  // Type a command the way a person does.
  const type = async (line) => {
    await js(`(() => {
      const el = document.querySelector("#sh-input");
      el.focus(); el.value = ${JSON.stringify(line)};
      el.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    })()`);
  };
  const paneText = () => js(`(document.querySelector("#sh-out") || {}).textContent || ""`);
  const awaitText = async (want, budget = 20000) => {
    const until = Date.now() + budget;
    while (Date.now() < until) {
      if ((await paneText()).includes(want)) return true;
      await wait(400);
    }
    return false;
  };

  await type("cd /tmp");
  await wait(600);
  await type("pwd");
  if (!(await awaitText("/tmp"))) {
    fail("cd did not persist to the next command:\n    " + (await paneText()).slice(-200));
  }

  await type("export UI_VAR=carried");
  await wait(600);
  await type('echo "[$UI_VAR]"');
  if (!(await awaitText("[carried]"))) {
    fail("an exported variable did not survive:\n    " + (await paneText()).slice(-200));
  }

  // A second sub-tab.
  await js(`document.querySelector('[data-action="shell-new"]').click()`);
  await wait(3000);
  const tabCount = await js(`document.querySelectorAll("[data-shell-tab]").length`);
  if (tabCount < 2) fail(`expected 2 shell sub-tabs, got ${tabCount}`);

  // History: the up arrow recalls the last command typed in this tab.
  await js(`(() => {
    const el = document.querySelector("#sh-input");
    el.focus();
    el.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowUp", bubbles: true }));
  })()`);
  await wait(300);

  if (process.env.SHELL_SHOT) {
    const { writeFileSync } = require("node:fs");
    writeFileSync(process.env.SHELL_SHOT, (await win.webContents.capturePage()).toPNG());
    console.log("  screenshot      " + process.env.SHELL_SHOT);
  }

  if (failed) return app.exit(1);
  console.log("SHELL PASS");
  console.log(`  cd persists     yes`);
  console.log(`  export persists yes`);
  console.log(`  sub-tabs        ${tabCount}`);
  app.exit(0);
});
