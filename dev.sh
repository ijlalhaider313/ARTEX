#!/usr/bin/env bash
# Dev mode: run backend (:8787) + traffic proxy (:8788) together with frontend next dev (:5173).
# The frontend /api is reverse-proxied to the backend; Ctrl-C exits everything.
#
# For the single-binary (embedded frontend) mode, see the "Single binary" section in the README;
# that path does not use this script.
set -euo pipefail
cd "$(dirname "$0")"

# On exit, kill all child processes in this process group (backend + frontend).
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Backend (plain go run, frontend not embedded); the number of concurrent worker agents is configured in "System settings".
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# Frontend hot reload (Vite/Next dev server, /api reverse-proxied to :8787).
( cd web && npm run dev ) &

echo "[dev] Backend :8787 / Proxy :8788 / Frontend http://localhost:5173  (Ctrl-C to exit)"
wait