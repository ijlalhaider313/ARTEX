// Stub for html2canvas.
//
// @antv/g's ESM bundle imports html2canvas at module top level to implement its
// canvas -> image export. ARTEX never exports a graph as an image, but that
// static import drags the whole library — including its CJK CSS-counter numeral
// tables (cjk-decimal / cjk-earthly-branch / cjk-heavenly-stem) — into the
// lazily-loaded chart chunk, leaving Chinese bytes in an otherwise English build.
//
// next.config.mjs aliases "html2canvas" to this file, so the import resolves,
// the dead code drops out, and no image-export code path can run. If ARTEX ever
// adds graph image export, remove the alias and the real package returns.
export default function html2canvas() {
  return Promise.reject(
    new Error("html2canvas is stubbed out in this build (graph image export is not enabled)"),
  );
}
