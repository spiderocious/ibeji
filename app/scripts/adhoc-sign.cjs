/**
 * Ad-hoc sign the finished app bundle.
 *
 * Why this exists: v0.1.0 shipped with `identity: null`, which skips signing
 * entirely. The bundle kept the stock Electron executable's linker signature,
 * which no longer described its contents once our code and the Go binaries
 * were injected. On Apple Silicon — where every binary must carry at least an
 * ad-hoc signature — Gatekeeper rejected it with:
 *
 *   "Ibeji" is damaged and can't be opened. You should move it to the Trash.
 *
 * That is not the ordinary unsigned-app prompt, and right-click → Open does
 * not get past it. Codesign verification named the real cause:
 *   "code has no resources but signature indicates they must be present"
 *
 * Ad-hoc (`-`) is a real signature carrying no certificate. It costs nothing,
 * needs no Apple account, and makes the bundle internally consistent. The app
 * is still unidentified, so the first launch still needs right-click → Open —
 * but it is no longer "damaged".
 *
 * Nested code must be signed before the bundle that contains it, so this walks
 * depth-first: Go binaries, then frameworks and helpers, then the app itself.
 */
const { execFileSync } = require("node:child_process");
const { readdirSync, statSync, existsSync } = require("node:fs");
const { join } = require("node:path");

const sign = (path, extra = []) =>
  execFileSync(
    "codesign",
    ["--force", "--sign", "-", "--timestamp=none", ...extra, path],
    { stdio: ["ignore", "ignore", "pipe"] },
  );

/**
 * Sign a .framework by its versioned directory.
 *
 * codesign wants the versioned path (Versions/A), not the symlinked top level,
 * and helpers bundled inside a framework must be signed before it is sealed.
 */
function signFramework(fw) {
  const versions = join(fw, "Versions");
  if (!existsSync(versions)) {
    sign(fw);
    return;
  }
  for (const v of readdirSync(versions)) {
    if (v === "Current") continue;
    const vdir = join(versions, v);
    if (!statSync(vdir).isDirectory()) continue;

    // Helper apps and libraries nested inside the framework first.
    const helpers = join(vdir, "Helpers");
    if (existsSync(helpers)) {
      for (const h of readdirSync(helpers)) {
        const hp = join(helpers, h);
        if (hp.endsWith(".app")) {
          const macos = join(hp, "Contents", "MacOS");
          if (existsSync(macos)) {
            for (const f of readdirSync(macos)) sign(join(macos, f));
          }
        }
        sign(hp);
      }
    }
    const libs = join(vdir, "Libraries");
    if (existsSync(libs)) {
      for (const l of readdirSync(libs)) {
        const lp = join(libs, l);
        if (statSync(lp).isFile()) sign(lp);
      }
    }
    sign(vdir);
  }
}

exports.default = async function adhocSign(context) {
  if (context.electronPlatformName !== "darwin") return;

  const app = join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  const contents = join(app, "Contents");

  // 1. Our own Go binaries, which arrive via extraResources unsigned.
  const bin = join(contents, "Resources", "bin");
  if (existsSync(bin)) {
    for (const f of readdirSync(bin)) sign(join(bin, f));
  }

  // 2. Frameworks and helper apps, deepest first. Signing an outer bundle
  //    seals what is inside it, so anything signed afterwards invalidates it.
  //
  //    `--deep` is deliberately NOT used here. Signing an x86_64 framework
  //    with --deep from an arm64 host fails with "internal error in Code
  //    Signing subsystem", which is how the Intel build first broke. Walking
  //    the pieces explicitly works for either architecture.
  const frameworks = join(contents, "Frameworks");
  if (existsSync(frameworks)) {
    const entries = readdirSync(frameworks).map((n) => join(frameworks, n));
    const order = (p) => (p.endsWith(".app") ? 0 : p.endsWith(".framework") ? 1 : 2);
    for (const p of entries.sort((a, b) => order(a) - order(b))) {
      if (p.endsWith(".app")) {
        const macos = join(p, "Contents", "MacOS");
        if (existsSync(macos)) {
          for (const f of readdirSync(macos)) sign(join(macos, f));
        }
        sign(p);
      } else if (p.endsWith(".framework")) {
        signFramework(p);
      } else {
        sign(p);
      }
    }
  }

  // 3. The app bundle last, sealing everything above it.
  sign(app);

  // Prove it rather than assume it: this is exactly the check that would have
  // caught the broken v0.1.0 before it was published.
  try {
    execFileSync("codesign", ["--verify", "--deep", "--strict", app], {
      stdio: ["ignore", "ignore", "pipe"],
    });
  } catch (err) {
    const detail = (err.stderr || Buffer.from("")).toString().trim();
    throw new Error(`ad-hoc signature does not verify: ${detail}`);
  }
  console.log(`  ad-hoc signed and verified ${app}`);
};
