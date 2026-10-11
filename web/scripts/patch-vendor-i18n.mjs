// patch-vendor-i18n.mjs — make the third-party chart bundle English.
//
// @antv/component ships Chinese display labels for its optional TimeBar / Select
// widgets (button tooltips such as "播放"/Play, and the select placeholder
// "请选择"). ARTEX does not instantiate those widgets, but the strings still land
// in the lazily-loaded @antv/g6 chunk, so a production build contains Chinese.
//
// This script rewrites the shipped string literals to English, in both the CJS
// (lib/) and ESM (esm/) copies the bundler may resolve. It is idempotent: once a
// literal is English the replacement is a no-op. Only *string literals* are
// touched — the files' Chinese comments never reach the bundle (Turbopack strips
// them), and no code, identifier or format placeholder is altered.
//
// Run automatically from build.sh before the frontend build.

import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const webRoot = join(dirname(fileURLToPath(import.meta.url)), "..");

// Chinese literal -> English. Order matters: longer strings first so a short
// token can never partially rewrite a longer one.
const LABELS = [
  // TimeBar axis date format: "MM月" renders a Chinese month suffix; "MMM" is the
  // English month abbreviation (e.g. "MM月\nYYYY" -> "MMM\nYYYY").
  ["MM月", "MMM"],
  ["范围时间", "Time range"],
  ["单一时间", "Single time"],
  ["折线图", "Line chart"],
  ["条形图", "Bar chart"],
  ["请选择", "Please select"],
  ["重置", "Reset"],
  ["快退", "Rewind"],
  ["快进", "Fast-forward"],
  ["播放", "Play"],
  ["暂停", "Pause"],
];

const TARGETS = [
  "node_modules/@antv/component/esm/ui/timebar/icons.js",
  "node_modules/@antv/component/lib/ui/timebar/icons.js",
  "node_modules/@antv/component/esm/ui/timebar/timebar.js",
  "node_modules/@antv/component/lib/ui/timebar/timebar.js",
  "node_modules/@antv/component/esm/ui/select/select.js",
  "node_modules/@antv/component/lib/ui/select/select.js",
  "node_modules/@antv/component/esm/ui/timebar/utils.js",
  "node_modules/@antv/component/lib/ui/timebar/utils.js",
  "node_modules/@antv/component/esm/ui/timeline/constants.js",
  "node_modules/@antv/component/lib/ui/timeline/constants.js",
];

let filesChanged = 0;
let replacements = 0;

for (const rel of TARGETS) {
  const path = join(webRoot, rel);
  if (!existsSync(path)) continue;
  const original = readFileSync(path, "utf8");
  let next = original;
  for (const [zh, en] of LABELS) {
    // Plain substring replacement: in these files the only CJK occurs in comments
    // and string literals (CJK cannot appear in a JS identifier), and both should
    // end up English. LABELS is ordered longest-first so shorter tokens can never
    // partially rewrite a longer one.
    if (next.includes(zh)) {
      replacements += next.split(zh).length - 1;
      next = next.split(zh).join(en);
    }
  }
  if (next !== original) {
    writeFileSync(path, next);
    filesChanged++;
    console.log(`[vendor-i18n] patched ${rel}`);
  }
}

console.log(
  `[vendor-i18n] ${filesChanged} file(s) changed, ${replacements} literal(s) translated`,
);
