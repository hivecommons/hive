ARG VISUAL_HIVE_REF=59df58add8027c61e2a643801a6ef0e1b55aa8b0

FROM node:22.23.1-bookworm-slim AS visual-hive-builder
ARG VISUAL_HIVE_REF
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git init \
 && git remote add origin https://github.com/DavidDiaz0317/visual-hive.git \
 && git fetch --depth=1 origin "$VISUAL_HIVE_REF" \
 && git checkout --detach FETCH_HEAD \
 && test "$(git rev-parse HEAD)" = "$VISUAL_HIVE_REF"
RUN npm ci \
 && npm run build \
 && GITHUB_SHA="$VISUAL_HIVE_REF" node scripts/build-release-bundle.mjs --output /visual-hive-release \
 && node -e 'const fs=require("node:fs");const m=JSON.parse(fs.readFileSync("/visual-hive-release/release-manifest.json","utf8"));if(m.gitCommit!==process.argv[1])throw new Error(`Visual Hive release commit mismatch: ${m.gitCommit}`)' "$VISUAL_HIVE_REF" \
 && node /visual-hive-release/visual-hive.mjs --version

FROM golang:1.25.12-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src

# Layer 1: cache Go module downloads (only re-runs when go.mod/go.sum change)
COPY v2/go.mod v2/go.sum ./
RUN go mod download

# Layer 2: copy source and build (re-runs on any source change)
COPY .git /repo/.git
COPY v2/ .

ARG GIT_HASH=unknown
ARG GIT_BRANCH=unknown
RUN DETECTED_GIT_HASH=$(git -C /repo rev-parse HEAD 2>/dev/null || true) && \
    if [ "$GIT_HASH" = "unknown" ] || [ -z "$GIT_HASH" ]; then GIT_HASH=${DETECTED_GIT_HASH:-unknown}; fi && \
    GIT_SHORT=$(printf '%s' "$GIT_HASH" | cut -c1-7) && \
    if [ "$GIT_BRANCH" = "unknown" ] || [ -z "$GIT_BRANCH" ]; then GIT_BRANCH=$(git -C /repo rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown"); fi && \
    GOCACHE=/tmp/gobuild CGO_ENABLED=0 go build -ldflags "-X main.gitHash=${GIT_HASH} -X main.gitShort=${GIT_SHORT} -X main.gitBranch=${GIT_BRANCH}" -o /hive ./cmd/hive && \
    GOCACHE=/tmp/gobuild CGO_ENABLED=0 go build -o /bd ./cmd/bd


# --- tmux from source (cached until TMUX_VERSION changes) ---
FROM node:24-slim AS tmux-builder
ARG TMUX_VERSION=3.5a
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl gcc make pkg-config libevent-dev libncurses-dev bison ca-certificates \
 && curl -sL "https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/tmux-${TMUX_VERSION}.tar.gz" | tar xz -C /tmp \
 && cd /tmp/tmux-${TMUX_VERSION} && ./configure --prefix=/usr/local && make -j$(nproc) && make install \
 && rm -rf /tmp/tmux-${TMUX_VERSION}

FROM node:24-slim

# --- Layer 1: system packages (rarely changes) ---
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates bash bubblewrap curl procps bzip2 gosu iptables inotify-tools libevent-core-2.1-7 \
    gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*

COPY --from=tmux-builder /usr/local/bin/tmux /usr/local/bin/tmux

# su-exec: lightweight setuid exec for UID switching from non-root.
# gosu rejects SUID mode, so we use su-exec (which is designed for it)
# for the manager's dev→agent UID switches.
RUN curl -sL -o /tmp/su-exec.c https://raw.githubusercontent.com/ncopa/su-exec/master/su-exec.c \
 && gcc -Wall -o /usr/local/bin/su-exec /tmp/su-exec.c \
 && chmod u+s /usr/local/bin/su-exec \
 && rm /tmp/su-exec.c \
 && apt-get purge -y gcc libc6-dev && apt-get autoremove -y

# Non-root user for agent processes (Claude Code refuses --dangerously-skip-permissions as root)
# node:24-slim already has group 1000 (node), so reuse it
RUN useradd -m -u 1001 -g node -s /bin/bash dev

# --- Layer 2: ttyd (pinned version for reproducible builds) ---
ARG TTYD_VERSION=1.7.7
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then TTYD_ARCH=x86_64; \
    elif [ "$ARCH" = "aarch64" ]; then TTYD_ARCH=aarch64; \
    else TTYD_ARCH=x86_64; fi && \
    curl -sL -o /usr/local/bin/ttyd \
      "https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/ttyd.${TTYD_ARCH}" && \
    chmod +x /usr/local/bin/ttyd

# --- Layer 2b: GitHub CLI (off-PATH — only snapshot publisher uses it) ---
ARG GH_VERSION=2.74.0
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then GH_ARCH=amd64; \
    elif [ "$ARCH" = "aarch64" ]; then GH_ARCH=arm64; \
    else GH_ARCH=amd64; fi && \
    mkdir -p /opt/hive/bin && \
    curl -sL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${GH_ARCH}.tar.gz" | \
    tar xz --strip-components=2 -C /opt/hive/bin "gh_${GH_VERSION}_linux_${GH_ARCH}/bin/gh" && \
    mv /opt/hive/bin/gh /opt/hive/bin/gh-real && \
    chmod +x /opt/hive/bin/gh-real

# --- Layer 3: GitHub Copilot CLI (frequent updates) ---
# Pinned: 1.0.6x drops the Node.js fallback — the CLI becomes native-only,
# and the Rust binary's embedded TLS stack rejects the MITM proxy CA.
# 1.0.59 is the last version where removing the binary falls back to the
# Node.js path, which respects NODE_EXTRA_CA_CERTS.
ARG COPILOT_VERSION=1.0.59
RUN npm install -g @github/copilot@${COPILOT_VERSION} && npm cache clean --force || \
    echo "WARN: GitHub Copilot CLI install failed — skipping"
# Remove the native Rust binary so copilot falls back to the Node.js path.
# The Rust binary embeds its own TLS stack (rustls + webpki-roots) which rejects
# the MITM proxy's forged certificates. The Node.js path respects NODE_EXTRA_CA_CERTS.
RUN rm -f /usr/local/lib/node_modules/@github/copilot/node_modules/@github/copilot-linux-*/copilot 2>/dev/null; true

# --- Layer 7: Claude Code (most frequent updates) ---
ARG CLAUDE_CODE_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force

# --- Layer 7b: reviewed native Codex runtime for governed repair children ---
# Hive rejects npm/npx/script wrappers at the repair boundary. Keep the npm
# package only as the pinned distribution source, then copy its architecture-
# specific native vendor directory to a stable path that remains usable when
# Hive seals the exact executable for a one-shot child.
ARG CODEX_VERSION=0.144.1
RUN npm install -g @openai/codex@${CODEX_VERSION} \
 && CODEX_VENDOR="$(find /usr/local/lib/node_modules/@openai/codex/node_modules -type f -path '*/vendor/*/bin/codex' -printf '%h/..\n' | head -n 1)" \
 && test -n "$CODEX_VENDOR" \
 && mkdir -p /opt/hive/codex \
 && cp -a "$CODEX_VENDOR"/. /opt/hive/codex/ \
 && test -f /opt/hive/codex/bin/codex \
 && chmod 0555 /opt/hive/codex/bin/codex \
 && test "$(/opt/hive/codex/bin/codex --version)" = "codex-cli ${CODEX_VERSION}" \
 && npm cache clean --force
ENV PATH="/opt/hive/codex/bin:${PATH}"

# --- Layer 8: Goose CLI (aaif-goose/goose — supports custom providers via env vars) ---
ARG GOOSE_VERSION=1.37.0
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then GOOSE_ARCH=x86_64; \
    elif [ "$ARCH" = "aarch64" ]; then GOOSE_ARCH=aarch64; \
    else GOOSE_ARCH=x86_64; fi && \
    curl -fsSL "https://github.com/aaif-goose/goose/releases/download/v${GOOSE_VERSION}/goose-${GOOSE_ARCH}-unknown-linux-gnu.tar.gz" | \
    tar xz -C /usr/local/bin goose && \
    chmod +x /usr/local/bin/goose || \
    echo "WARN: Goose CLI install failed — skipping"

# --- Application layers ---
ENV HOME=/home/dev

# Proxy dependencies (only re-runs when package.json changes)
COPY v2/proxy/package.json v2/proxy/package-lock.json* /opt/hive/proxy/
RUN cd /opt/hive/proxy && npm install --omit=dev 2>/dev/null || true

# Cache-buster: re-declare in the final stage so COPY layers below
# are invalidated on every commit.
ARG GIT_HASH=unknown

# Go toolchain for agents that run `go test`, `go build`, etc.
COPY --from=builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

COPY --from=builder /hive /usr/local/bin/hive
COPY --from=builder /bd /usr/local/bin/bd

COPY --from=visual-hive-builder /visual-hive-release /opt/hive/visual-hive
ENV HIVE_VISUAL_HIVE_HOME=/opt/hive/visual-hive
RUN chmod -R a-w /opt/hive/visual-hive \
 && test -f /opt/hive/visual-hive/release-manifest.json \
 && node /opt/hive/visual-hive/visual-hive.mjs --version

COPY v2/proxy/ /opt/hive/proxy/

COPY v2/pkg/dashboard/static/index.html /opt/hive/proxy/public/

COPY dashboard/ /opt/hive/dashboard/
RUN chmod +x /opt/hive/dashboard/publish-snapshot*.sh

COPY examples/kubestellar/hive-project.yaml /etc/hive/hive-project.yaml
COPY examples/acmm/ /opt/hive/examples/acmm/
COPY examples/agents/ /opt/hive/examples/agents/
COPY v2/pkg/config/packs/ /opt/hive/packs/
COPY v2/deploy/data/ /opt/hive/seed-data/
COPY v2/deploy/ttyd-tmux.sh /usr/local/bin/ttyd-tmux.sh
COPY v2/deploy/hive-panes.sh /usr/local/bin/hive-panes
COPY bin/gh-app-token.sh bin/hive-config.sh bin/agent-launch.sh bin/git-credential-hive.sh /usr/local/bin/
COPY bin/gh-wrapper.sh /usr/local/bin/gh
COPY config/backends.conf /usr/local/etc/hive/backends.conf
RUN chmod +x /usr/local/bin/ttyd-tmux.sh /usr/local/bin/hive-panes /usr/local/bin/gh-app-token.sh \
    /usr/local/bin/hive-config.sh /usr/local/bin/agent-launch.sh /usr/local/bin/gh \
    /usr/local/bin/git-credential-hive.sh

# --- Nous framework (strategist experiment engine) ---
RUN (apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-venv python3-pip jq && rm -rf /var/lib/apt/lists/*) || \
    echo "WARN: apt-get failed — Nous framework will not be available"
COPY bin/nous-runner.sh bin/nous-sync.py bin/nous-hive-gate.py bin/nous-install.sh /tmp/hive/bin/
RUN chmod +x /tmp/hive/bin/*.sh
COPY examples/kubestellar/nous-campaign.yaml examples/kubestellar/nous-governor-campaign.yaml examples/kubestellar/nous-repo-campaign.yaml /etc/hive/
RUN mkdir -p /var/run/nous/governor /var/run/nous/repo /data/nous/snapshots /opt/nous
RUN pip3 install --no-cache-dir --break-system-packages pyyaml 2>/dev/null || \
    pip3 install --no-cache-dir pyyaml 2>/dev/null || \
    echo "WARN: system pyyaml install failed"
RUN pip3 install --no-cache-dir --break-system-packages specify-cli 2>/dev/null || \
    pip3 install --no-cache-dir specify-cli 2>/dev/null || \
    echo "WARN: spec-kit (specify-cli) install failed — inception will generate facts without it"
# pluk (TypeScript): structured event streaming for AI agent tmux sessions
ARG PLUK_VERSION=0.8.0
RUN npm install -g @kubestellar/pluk@${PLUK_VERSION} && npm cache clean --force
RUN mkdir -p /var/run/pluk/logs /var/run/pluk/commands && \
    chmod 1777 /var/run/pluk/logs /var/run/pluk/commands
RUN (which git && git clone -b feat/cli-agent-mode https://github.com/clubanderson/agentic-strategy-evolution /opt/nous && \
    python3 -m venv /opt/nous/venv && \
    /opt/nous/venv/bin/pip install --no-cache-dir -e /opt/nous 2>&1) || \
    echo "WARN: Nous install failed — run_campaign.py may not work"

# --- LiteLLM proxy (pinned; optional local Anthropic-compat translator) ---
# Used only when governor.litellm.local_proxy is true; the default flow
# forwards to the remote LiteLLM endpoint via the Go inference translator.
# All heavy deps ship manylinux aarch64 wheels, so multi-arch builds work
# without compilation. || echo matches the Nous/Goose optional-layer convention.
ARG LITELLM_VERSION=1.74.3
RUN (python3 -m venv /opt/litellm/venv && \
    /opt/litellm/venv/bin/pip install --no-cache-dir "litellm[proxy]==${LITELLM_VERSION}" && \
    ln -s /opt/litellm/venv/bin/litellm /usr/local/bin/litellm) || \
    echo "WARN: litellm install failed — local litellm proxy unavailable"
RUN mkdir -p /data/litellm

EXPOSE 3001 3002 7681
VOLUME ["/data", "/secrets"]

COPY v2/deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN set -eu; \
    test -x /usr/bin/ps; \
    runtime_scripts="/usr/local/bin/entrypoint.sh /usr/local/bin/ttyd-tmux.sh /usr/local/bin/hive-panes \
      /usr/local/bin/gh-app-token.sh /usr/local/bin/hive-config.sh /usr/local/bin/agent-launch.sh \
      /usr/local/bin/git-credential-hive.sh /usr/local/bin/gh \
      $(find /opt/hive/dashboard /tmp/hive/bin -type f -name '*.sh' -print)"; \
    runtime_shebang_files="$runtime_scripts /tmp/hive/bin/nous-sync.py /tmp/hive/bin/nous-hive-gate.py \
      /opt/hive/dashboard/agent-activity.py /opt/hive/dashboard/build-snapshot.mjs"; \
    runtime_text_files="$runtime_shebang_files /usr/local/etc/hive/backends.conf"; \
    for runtime_file in $runtime_text_files; do \
      sed -i 's/\r$//' "$runtime_file"; \
      if grep -q "$(printf '\r')" "$runtime_file"; then \
        echo "carriage return remains in runtime text file: $runtime_file" >&2; \
        exit 1; \
      fi; \
    done; \
    for script in $runtime_shebang_files; do \
      test "$(head -c 2 "$script")" = '#!'; \
    done; \
    chmod 0555 $runtime_shebang_files

# Pre-create /data/home/ subdirectories with correct ownership so the
# permissions watcher doesn't have to create them at runtime.
RUN mkdir -p /data/home/.copilot /data/home/.cache /data/home/.config /data/home/.local && \
    chown -R dev:node /data/home/

RUN chown -R dev:node /data /home/dev 2>/dev/null || true

ENTRYPOINT ["entrypoint.sh"]
CMD ["--config", "/etc/hive/hive.yaml"]
