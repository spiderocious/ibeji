// Copy the renderer's non-TypeScript assets into out/, preserving layout.
// Kept as a script rather than a bundler: the renderer is a handful of files
// consuming the design system's CSS directly, and a bundler would only add a
// place for that CSS to be transformed.
import { cp, mkdir } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const pairs = [
  ["src/renderer/index.html", "out/renderer/index.html"],
  ["src/renderer/app.css", "out/renderer/app.css"],
  ["src/styles", "out/styles"],
];

for (const [from, to] of pairs) {
  const dest = join(root, to);
  await mkdir(dirname(dest), { recursive: true });
  await cp(join(root, from), dest, { recursive: true });
  console.log(`  ${from} -> ${to}`);
}
