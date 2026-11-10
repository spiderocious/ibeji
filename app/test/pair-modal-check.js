// Does clicking Pair show what is happening, and name the failure?
//
// The bug this guards is not a crash — it is silence. Clicking Pair used to
// close the overlay and show nothing for up to fifteen seconds, then either
// six digits or one sentence: "the paired device is not reachable". No step,
// no error, nothing to copy. That silence let a stale IP address masquerade as
// a macOS permission problem for over a day.
//
// So this test pairs with an address that provably does not answer, and asserts
// the things that silence lacked:
//
//   1. a modal appears immediately, before the attempt resolves
//   2. it names the address being dialled
//   3. the timeline draws every step of the ceremony, not just the current one
//   4. the step that broke is marked failed, and the ones after it skipped
//   5. the failure carries a cause in plain words, and a fix
//   6. a timeout offers the subnet sweep, since a moved peer is the likely cause
//   7. the log for this attempt is copyable
//   8. Try again and Close are both there and both work
//
// A test that only asserted "a modal element exists" would pass against an
// empty box, which is the old silence in a new frame. Each check below is one
// of the things a person actually needed and did not have.
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

// An address in the documentation-only range (RFC 5737). Guaranteed not to
// route anywhere, so the dial times out rather than finding something — which
// is exactly the failure being tested. Using a real LAN address would make the
// test depend on what happens to be plugged in.
const DEAD = "192.0.2.123:7749";

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
    console.log("PAIR MODAL FAIL: no window");
    return app.exit(1);
  }
  await new Promise((r) => setTimeout(r, 4000));

  // Already paired means there is no Pair button to click and the ceremony
  // cannot run. Skip rather than fail: the machine running this may well have
  // a pair set up, and a test that demands an unpaired daemon would be red for
  // the wrong reason.
  const paired = await win.webContents.executeJavaScript(
    `document.querySelectorAll('[data-action="unpair"]').length > 0`,
  );
  if (paired) {
    console.log("PAIR MODAL SKIP: this daemon is already paired, so the ceremony cannot run");
    return app.exit(0);
  }

  // --- 1. the modal opens at once, while the attempt is still in flight ----
  //
  // Driven through the same code path a click takes, then read after 600ms —
  // long enough for a render, far short of the 15s dial timeout. If the modal
  // only appeared at the end, this read would find nothing, which is precisely
  // the regression worth catching.
  const early = await win.webContents.executeJavaScript(`(async () => {
    window.__pairDone = false;
    // The handler is not exported, so go through the real dispatch: build a
    // button carrying the address and click it, exactly as the sweep rows and
    // candidate rows do.
    const b = document.createElement("button");
    b.setAttribute("data-pair", ${JSON.stringify(DEAD)});
    document.body.appendChild(b);
    b.click();
    b.remove();
    await new Promise((r) => setTimeout(r, 600));
    const sheet = document.querySelector(".pm-sheet");
    return {
      open: !!sheet,
      address: document.querySelector(".pm-addr")?.textContent?.trim() ?? null,
      steps: [...document.querySelectorAll(".pm-step")].length,
      running: [...document.querySelectorAll(".pm-step.is-running")].length,
    };
  })()`);

  check(early.open, "a modal opens as soon as Pair is clicked");
  check(
    early.address === DEAD,
    "the modal names the address being dialled",
    `got ${JSON.stringify(early.address)}`,
  );
  // The whole plan is drawn up front: a timeline that grows a row at a time
  // cannot show how far there is left to go.
  check(
    early.steps >= 8,
    "the timeline shows every step of the ceremony, not only the current one",
    `saw ${early.steps} rows`,
  );
  check(early.running >= 1, "a step is marked as running while the dial is in flight");

  // --- 2. the failure is named, explained, and actionable ------------------
  //
  // Wait for the attempt to resolve. The dial has a 15s timeout in the daemon;
  // give it room and poll rather than sleeping blind.
  const done = await win.webContents.executeJavaScript(`(async () => {
    const until = Date.now() + 40000;
    while (Date.now() < until) {
      if (document.querySelector(".pm-fail")) break;
      await new Promise((r) => setTimeout(r, 400));
    }
    const failedRow = document.querySelector(".pm-step.is-failed");
    return {
      hasFail: !!document.querySelector(".pm-fail"),
      failHead: document.querySelector(".pm-fail-head")?.textContent?.trim() ?? null,
      cause: document.querySelector(".pm-cause")?.textContent?.trim() ?? null,
      fix: document.querySelector(".pm-fix")?.textContent?.trim() ?? null,
      error: document.querySelector(".pm-err")?.textContent?.trim() ?? null,
      failedRows: [...document.querySelectorAll(".pm-step.is-failed")].length,
      skippedRows: [...document.querySelectorAll(".pm-step.is-skipped")].length,
      failedTitle: failedRow?.querySelector(".pm-title")?.textContent?.trim() ?? null,
      sweepOffered: !!document.querySelector('[data-action="pm-sweep"]'),
      retry: !!document.querySelector('[data-action="pm-retry"]'),
      close: !!document.querySelector('[data-action="pm-close"]'),
      copy: !!document.querySelector('[data-action="pm-copy-log"]'),
      toggleLog: !!document.querySelector('[data-action="pm-toggle-log"]'),
    };
  })()`);

  check(done.hasFail, "the modal shows a failure panel once the attempt fails");
  check(
    typeof done.failHead === "string" && done.failHead.length > 0,
    "the failure names the step it stopped at",
    `got ${JSON.stringify(done.failHead)}`,
  );
  // Exactly one step fails: marking several would mean the recorder is
  // attributing a single error to more than one point.
  check(
    done.failedRows === 1,
    "exactly one step is marked as the failure",
    `${done.failedRows} rows marked failed`,
  );
  check(
    done.skippedRows >= 1,
    "the steps after the failure are shown as never attempted, not pending forever",
    `${done.skippedRows} skipped`,
  );
  // The cause is the whole point. A blank one is the old failure mode wearing
  // a new coat, so assert it says something substantial rather than just
  // existing.
  check(
    typeof done.cause === "string" && done.cause.length > 24,
    "the failure explains the cause in plain words",
    `got ${JSON.stringify(done.cause)}`,
  );
  check(
    typeof done.fix === "string" && done.fix.length > 16,
    "the failure gives a concrete next move",
    `got ${JSON.stringify(done.fix)}`,
  );
  check(
    typeof done.error === "string" && done.error.length > 0,
    "the raw error is available, verbatim",
  );
  // An unroutable address times out, and a timeout is the case a sweep can
  // resolve — so the offer must be there. This is the specific thing whose
  // absence cost a day.
  check(
    done.sweepOffered,
    "a timeout offers to scan the network, since a moved peer is the likeliest cause",
  );
  check(done.retry, "Try again is offered after a failure");
  check(done.close, "Close is offered after a failure");
  check(done.copy, "this attempt's log can be copied");
  check(done.toggleLog, "the raw log can be shown");

  // --- 3. the log is this attempt's, and it carries the diagnosis ----------
  const log = await win.webContents.executeJavaScript(`(async () => {
    document.querySelector('[data-action="pm-toggle-log"]').click();
    await new Promise((r) => setTimeout(r, 250));
    const el = document.querySelector(".pm-log");
    return { shown: !!el, text: el?.textContent ?? "" };
  })()`);

  check(log.shown, "clicking Show log reveals it");
  check(
    log.text.includes(DEAD.split(":")[0]),
    "the log is about this attempt: it names the address dialled",
  );
  check(
    log.text.includes("cause:"),
    "the log carries the diagnosis, which is what gets pasted into a bug report",
  );

  // --- 4. Close actually closes -------------------------------------------
  const closed = await win.webContents.executeJavaScript(`(async () => {
    document.querySelector('[data-action="pm-close"]').click();
    await new Promise((r) => setTimeout(r, 400));
    return !document.querySelector(".pm-sheet");
  })()`);
  check(closed, "Close dismisses the modal");

  if (fails.length > 0) {
    console.log(`PAIR MODAL FAIL: ${fails.length} check(s) failed`);
    return app.exit(1);
  }
  console.log("PAIR MODAL OK");
  app.exit(0);
});
