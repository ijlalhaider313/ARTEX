#!/usr/bin/env bash
# ARTEX update script: ① Docker update (pull new image and rebuild)  ② Local build update (rebuild binary)
# Corresponds to install.sh: install handles first-time deployment, update handles upgrading to a new version.
# DB migration requires no manual execution — artex idempotently re-runs schema.sql on every startup (using
# ADD COLUMN/CREATE INDEX IF NOT EXISTS), so "restart = migration". Data (pgdata volume, ./data, ./skills) is unaffected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: sync repo to latest code (compose/scripts/local build source all rely on this) ───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or branch diverged) — please resolve manually and retry, using current code for now"
  fi
}

# ── ① Docker update ────────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not detected, please use ./install.sh for first-time deployment"
  [ -f .env ] || die ".env not found, please run ./install.sh to complete first-time deployment"

  # Optional: upgrade to a specific version tag (leave empty to keep ARTEX_TAG from .env, default latest)
  local tag; tag="$(ask 'Target image tag (Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG set to ${tag}"
  fi

  # Only touch artex: postgres is a fixed 16-alpine, no need to upgrade with it (pulling it is a waste of
  # bandwidth, and major version changes carry compatibility risk). artex declares depends_on postgres, so
  # when bringing up the service by name, if pg isn't running it will be started automatically; if already
  # running it is left as-is without rebuild.
  info "Pulling new image (artex only)…"
  docker compose pull artex
  info "Rebuilding and starting (schema auto-migrates on artex restart)…"
  docker compose up -d artex
  ok "Update complete → http://localhost:8787"
  info "View logs: docker compose logs -f artex"
  info "Clean old images (optional): docker image prune -f"
}

# ── ② Local build update ───────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go not detected (>=1.26): https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found — if first-time deployment, use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Recompiling embedded single binary…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not detected: building backend **without embedded frontend** (run npm run dev separately for frontend)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete → ./artex"
  warn "Please restart the running artex process to apply (schema auto-migrates on restart)"
}

echo "=============================="
echo "  ARTEX Update"
echo "  1) Docker update (pull new image and rebuild)"
echo "  2) Local update (go recompile)"
echo "=============================="
case "$(ask 'Select' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid selection" ;;
esac