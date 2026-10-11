# syntax=docker/dockerfile:1
#
# Runtime image (does not build inside the image): only installs common tools and drops in a **pre-compiled Linux single binary**.
# The binary is cross-compiled by the CI binaries job (pure Go, no QEMU), placed in the build context at
# dist/<TARGETARCH>/artex. This way multi-arch builds only need to emulate the apt layer for arm64,
# no longer emulating Next/Go compilation, which is much faster.
#
# For local manual image builds, prepare the binary first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a set of common reconnaissance tools (add/remove as needed).
# Node is installed from NodeSource 20.x: bookworm's bundled apt nodejs is 18, Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Pre-install Playwright MCP and CLI (globally), so runtime no longer needs npx to download from the network.
# @playwright/mcp: browser MCP via direct `npx @playwright/mcp` (already globally installed, no -y/@latest needed).
# @playwright/cli: provides playwright-cli, --help verifies it's executable after install.
# Also install playwright (provides browser management), then use --with-deps to pre-install chromium and its
# system dependencies, so the MCP/CLI works on first launch inside the container without downloading browsers.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Pre-compiled binary for the target architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Daemon launch script: decides whether to restart based on exit code, the one-click update page relies on it for binary swap.
# It also forwards SIGTERM to artex — docker stop only sends the signal to PID 1,
# without forwarding artex wouldn't receive it and couldn't do graceful shutdown, getting SIGKILL'd after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]