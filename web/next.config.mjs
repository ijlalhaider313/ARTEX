import { fileURLToPath } from "node:url";

// Static export: `NEXT_EXPORT=1 next build` produces a purely static directory in web/out that can be
// dropped directly into an nginx web root. Development (next dev) does not set this variable, preserving /api proxy and HMR.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: the entire site runs on mock data, no backend, no /api proxy needed.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // Prevent the parent directory's lockfile from interfering with root inference and asset path generation.
  turbopack: {
    root: fileURLToPath(new URL(".", import.meta.url)),
    // html2canvas is pulled in only by @antv/g's (unused) canvas->image export, and
    // ships CJK CSS-counter numeral tables. Alias it to a stub so the chart chunk
    // stays English; see stubs/html2canvas.js.
    resolveAlias: { html2canvas: "./stubs/html2canvas.js" },
  },
  reactCompiler: true,
  // Allow access to dev assets (HMR) from LAN IPs; add or remove as needed.
  // During dev, allow any IPv4 source to access /_next/* and HMR (unaffected by LAN IP changes).
  // Note: Next forbids a bare "*" for security; use segmented wildcards instead; "*.*.*.*" matches any IPv4.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // Pure static export: no Node runtime; images are not optimized; each route produces <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: no backend, no /api proxy needed.
          images: { unoptimized: true },
        }
      : {
          // Development: proxy /api/* to the Go backend (default :8787, overridable via AUTOPENTEST_API).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;