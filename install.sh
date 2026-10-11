#!/usr/bin/env bash
# ARTEX install script: ① all-Docker  ② local build & run
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── skills ────────────────────────────────────────────────────────
# The shipped skills/ directory is the source of truth; the app loads skills from
#   ARTEX_SKILL_DIR  >  config.json "skill_dir"  >  <directory of the artex binary>/skills
# (see config.SkillDir()). Copy skills/ into that directory so a binary deployed
# outside the repo never serves stale skill content — the failure mode being that a
# re-translated or re-added skill never reaches the running instance.
# Update-only, deliberately no --delete: skills created or edited at runtime survive.
resolve_skill_dir(){
  local d="${ARTEX_SKILL_DIR:-}"
  if [ -z "$d" ] && [ -f config.json ]; then
    d="$(sed -n 's/.*"skill_dir"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' config.json | head -n1)"
  fi
  [ -n "$d" ] || d="$(pwd)/skills"
  printf '%s\n' "$d"
}
sync_skills(){
  [ -d skills ] || { warn "skills/ not found — skipping skill sync"; return 0; }
  local src dest
  src="$(cd skills && pwd)"
  dest="$(resolve_skill_dir)"
  mkdir -p "$dest"
  dest="$(cd "$dest" && pwd)"
  if [ "$src" = "$dest" ]; then
    info "Skills: app reads $src directly (no copy needed)"
    return 0
  fi
  if command -v rsync >/dev/null 2>&1; then
    rsync -a skills/ "$dest"/
  else
    cp -R skills/. "$dest"/
  fi
  ok "Skills synced → $dest ($(find "$dest" -name SKILL.md -type f | wc -l | tr -d ' ') skills present)"
}

# ── docker environment detection / auto-install ───────────────────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker and docker compose detected"; return
  fi
  warn "docker / docker compose not found"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Auto-install Docker? (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker installed (group changes require re-login to use docker without sudo)"
      else
        die "Please install docker yourself and retry"
      fi ;;
    Darwin) die "On macOS install Docker Desktop: https://www.docker.com/products/docker-desktop/" ;;
    *)      die "Please install docker yourself and retry" ;;
  esac
}

# ── ① all-Docker ─────────────────────────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres password (Enter for random)' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY (can be empty, configure later in the UI)' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env generated (POSTGRES_PASSWORD set)"
  else
    info "Reusing existing .env"
  fi
  info "Pulling images and starting..."
  docker compose pull || true
  docker compose up -d
  ok "Started → http://localhost:8787"
  # docker-compose.yml bind-mounts ./skills into the container, so the mounted copy
  # IS the source — nothing to sync here, unlike a binary deployed outside the repo.
  info "Skills: ./skills is bind-mounted into the container (edit + restart, no image rebuild)"
  info "View logs: docker compose logs -f artex"
}

# ── ② local build & run ─────────────────────────────────────────
install_local(){
  echo "Database setup method:"
  echo "  1) Connect to an existing PostgreSQL"
  echo "  2) Start a PostgreSQL with Docker (requires docker)"
  case "$(ask 'Choose' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres password (Enter for random)' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask 'Database host' 127.0.0.1)"
      DB_PORT="$(ask 'Port' 5432)"
      DB_USER="$(ask 'User' artex)"
      DB_PASS="$(ask 'Password' '')"
      DB_NAME="$(ask 'Database name' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # Generate config.json
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json generated"

  # Go environment check
  command -v go >/dev/null 2>&1 || die "Go not found, please install Go (>=1.26) first: https://go.dev/dl/"
  ok "Go: $(go version)"

  # Embedding the frontend needs node to produce the static assets
  if command -v npm >/dev/null 2>&1; then
    info "Building frontend static assets..."
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Compiling embedded single binary..."
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: will build a backend **without embedded frontend** (run npm run dev separately for the frontend)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete → ./artex"

  sync_skills

  info "Starting... (Ctrl-C to exit)"
  ./artex
}

echo "=============================="
echo "  ARTEX install"
echo "  1) All-Docker install"
echo "  2) Local run (go build)"
echo "=============================="
case "$(ask 'Choose' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "Invalid choice" ;;
esac