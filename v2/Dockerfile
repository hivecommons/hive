FROM golang:1.26-alpine AS builder
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
RUN GIT_HASH=$(git -C /repo rev-parse HEAD 2>/dev/null || echo "unknown") && \
    GIT_SHORT=$(git -C /repo rev-parse --short=7 HEAD 2>/dev/null || echo "unknown") && \
    if [ "$GIT_BRANCH" = "unknown" ] || [ -z "$GIT_BRANCH" ]; then GIT_BRANCH=$(git -C /repo rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown"); fi && \
    GOCACHE=/tmp/gobuild CGO_ENABLED=0 go build -ldflags "-X main.gitHash=${GIT_HASH} -X main.gitShort=${GIT_SHORT} -X main.gitBranch=${GIT_BRANCH}" -o /hive ./cmd/hive && \
    GOCACHE=/tmp/gobuild CGO_ENABLED=0 go build -o /bd ./cmd/bd && \
    GOCACHE=/tmp/gobuild CGO_ENABLED=0 go build -o /hive-backup ./cmd/hive-backup


# --- tmux from source (cached until TMUX_VERSION changes) ---
FROM node:26-slim AS tmux-builder
ARG TARGETARCH
ARG TMUX_VERSION=3.5a
ARG TMUX_SHA256_AMD64=16216bd0877170dfcc64157085ba9013610b12b082548c7c9542cc0103198951
ARG TMUX_SHA256_ARM64=16216bd0877170dfcc64157085ba9013610b12b082548c7c9542cc0103198951
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl gcc make pkg-config libevent-dev libncurses-dev bison ca-certificates \
 && ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && case "$ARCH" in \
      amd64) TMUX_SHA256="$TMUX_SHA256_AMD64" ;; \
      arm64) TMUX_SHA256="$TMUX_SHA256_ARM64" ;; \
      *) echo "unsupported arch for tmux: $ARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors -o /tmp/tmux.tar.gz "https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/tmux-${TMUX_VERSION}.tar.gz" \
 && echo "${TMUX_SHA256}  /tmp/tmux.tar.gz" | sha256sum -c - \
 && tar xz -C /tmp -f /tmp/tmux.tar.gz \
 && cd /tmp/tmux-${TMUX_VERSION} && ./configure --prefix=/usr/local && make -j$(nproc) && make install \
 && rm -rf /tmp/tmux-${TMUX_VERSION} /tmp/tmux.tar.gz

FROM node:26-slim

# --- Layer 1: system packages (rarely changes) ---
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates bash curl bzip2 gosu iptables inotify-tools libevent-core-2.1-7 \
    gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*

COPY --from=tmux-builder /usr/local/bin/tmux /usr/local/bin/tmux

# su-exec: lightweight setuid exec for UID switching from non-root.
# gosu rejects SUID mode, so we use su-exec (which is designed for it)
# for the manager's dev→agent UID switches.
ARG TARGETARCH
ARG SU_EXEC_VERSION=0.3
ARG SU_EXEC_SHA256_AMD64=0c90fe15804f2670037b90e72eb496579fe00d2c15765e5fe2ddec0f5afa35fa
ARG SU_EXEC_SHA256_ARM64=0c90fe15804f2670037b90e72eb496579fe00d2c15765e5fe2ddec0f5afa35fa
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && case "$ARCH" in \
      amd64) SU_EXEC_SHA256="$SU_EXEC_SHA256_AMD64" ;; \
      arm64) SU_EXEC_SHA256="$SU_EXEC_SHA256_ARM64" ;; \
      *) echo "unsupported arch for su-exec: $ARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors -o /tmp/su-exec.c "https://raw.githubusercontent.com/ncopa/su-exec/v${SU_EXEC_VERSION}/su-exec.c" \
 && echo "${SU_EXEC_SHA256}  /tmp/su-exec.c" | sha256sum -c - \
 && gcc -Wall -o /usr/local/bin/su-exec /tmp/su-exec.c \
 && rm /tmp/su-exec.c \
 && apt-get purge -y gcc libc6-dev && apt-get autoremove -y

# Non-root user for agent processes (Claude Code refuses --dangerously-skip-permissions as root)
# node:24-slim already has group 1000 (node), so reuse it as dev's primary group.
#
# SECURITY (audit C6, CWE-250/732): `hive-launch` is a dedicated supplementary
# group whose ONLY member is dev. It gates exec of the SUID-root su-exec helper
# (chmod 4750 below).
#
# su-exec was previously `chmod u+s` with the default 755 — SETUID-ROOT AND
# WORLD-EXECUTABLE. Verified live on this branch across all three production
# clusters: any agent UID could run `su-exec root sh` and become root inside its
# own container, defeating the per-agent-UID isolation the whole ACMM capability
# model rests on. That container holds the GitHub App key and every agent's
# credentials.
#
# Only the hive process (running as dev, UID 1001) ever execs su-exec — see the
# four call sites in pkg/agent/manager.go. Agents are launched BY it and never
# invoke it themselves, so restricting exec to dev breaks nothing.
#
# root:node 4750 would NOT be enough: dev AND every agent UID share the primary
# group `node` (agents are created `-g node`), so a node-gated helper is still
# reachable by every agent. The group must contain dev alone.
RUN groupadd --system hive-launch \
 && useradd -m -u 1001 -g node -G hive-launch -s /bin/bash dev \
 && chown root:hive-launch /usr/local/bin/su-exec \
 && chmod 4750 /usr/local/bin/su-exec \
 && [ "$(stat -c '%G' /usr/local/bin/su-exec)" = "hive-launch" ] \
 && [ "$(stat -c '%a' /usr/local/bin/su-exec)" = "4750" ]

# --- Layer 2: ttyd (pinned version for reproducible builds) ---
ARG TTYD_VERSION=1.7.7
ARG TTYD_SHA256_AMD64=8a217c968aba172e0dbf3f34447218dc015bc4d5e59bf51db2f2cd12b7be4f55
ARG TTYD_SHA256_ARM64=b38acadd89d1d396a0f5649aa52c539edbad07f4bc7348b27b4f4b7219dd4165
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    case "$ARCH" in \
      amd64) TTYD_ARCH=x86_64; TTYD_SHA256="$TTYD_SHA256_AMD64" ;; \
      arm64) TTYD_ARCH=aarch64; TTYD_SHA256="$TTYD_SHA256_ARM64" ;; \
      *) echo "unsupported arch for ttyd: $ARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors -o /usr/local/bin/ttyd \
      "https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/ttyd.${TTYD_ARCH}" && \
    echo "${TTYD_SHA256}  /usr/local/bin/ttyd" | sha256sum -c - && \
    chmod +x /usr/local/bin/ttyd

# --- Layer 2b: GitHub CLI (off-PATH — only snapshot publisher uses it) ---
ARG GH_VERSION=2.74.0
ARG GH_SHA256_AMD64=e55c9d49dc49c0b0fef0a9acd3510482fd9e27ff52ae80f8a6e838cd25b4cd89
ARG GH_SHA256_ARM64=221222bd00281ab993f7e2c19b9f3f2482d66fa759baedcd8668785c940c8df7
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    case "$ARCH" in \
      amd64) GH_ARCH=amd64; GH_SHA256="$GH_SHA256_AMD64" ;; \
      arm64) GH_ARCH=arm64; GH_SHA256="$GH_SHA256_ARM64" ;; \
      *) echo "unsupported arch for gh: $ARCH" >&2; exit 1 ;; \
    esac && \
    mkdir -p /opt/hive/bin && \
    curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors -o /tmp/gh.tar.gz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${GH_ARCH}.tar.gz" && \
    echo "${GH_SHA256}  /tmp/gh.tar.gz" | sha256sum -c - && \
    tar xz --strip-components=2 -C /opt/hive/bin -f /tmp/gh.tar.gz "gh_${GH_VERSION}_linux_${GH_ARCH}/bin/gh" && \
    mv /opt/hive/bin/gh /opt/hive/bin/gh-real && \
    chmod +x /opt/hive/bin/gh-real && \
    rm -f /tmp/gh.tar.gz

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

# --- Layer 3b: GitHub Copilot SDK (pinned; powers SDK-based model discovery) ---
# The dashboard's copilot model discovery prefers a probe through the official
# SDK (bin/copilot-models.mjs → /usr/local/bin/copilot-models.mjs) because it
# rides the CLI's own auth and TLS handling. The helper pins the SDK's runtime
# connection to the Layer 3 CLI entry above, so the newer @github/copilot the
# SDK nests as its own dependency is never executed (its native binary would
# reject the proxy CA). Tolerant of failure like the copilot layer: discovery
# falls back to the raw HTTP probe when the SDK is absent.
ARG COPILOT_SDK_VERSION=1.0.8
RUN npm install -g @github/copilot-sdk@${COPILOT_SDK_VERSION} && npm cache clean --force || \
    echo "WARN: GitHub Copilot SDK install failed — skipping"

# --- Layer 4: OpenAI Codex CLI (pinned; backend: codex + live model discovery) ---
# Pinned for reproducibility; version matches v2/Dockerfile.contributor —
# keep the two pins in sync. @openai/codex resolves a platform-specific
# native binary (@openai/codex-<platform>) via optionalDependencies, so a
# plain npm install serves both linux/amd64 and linux/arm64.
# Tolerant of failure like the copilot layer: if codex is absent, the
# dashboard's codex model discovery (`codex app-server` model/list — pure
# local stdio, no network, unaffected by the proxy CA) falls back to the
# static list, and non-codex backends are unaffected.
ARG CODEX_VERSION=0.146.0
RUN npm install -g @openai/codex@${CODEX_VERSION} && npm cache clean --force || \
    echo "WARN: OpenAI Codex CLI install failed — skipping"

# --- Layer 7: Claude Code (most frequent updates) ---
ARG CLAUDE_CODE_VERSION=2.1.226
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force

# --- Layer 8: Goose CLI (official block/goose upstream) ---
# Installs from the OFFICIAL block/goose repo (was the unofficial aaif-goose
# fork, a supply-chain risk — #3417), pinned to a version matching
# Dockerfile.contributor, and SHA-256-verified before extraction so a tampered
# release cannot inject code into the spoke image. Bump GOOSE_SHA256_* whenever
# GOOSE_VERSION changes (compute from the release's linux-gnu tarballs).
# A download/extract failure is still tolerated (agents on backend: goose just
# won't launch, same as other optional backends), but a CHECKSUM MISMATCH is a
# hard failure — a tampered binary must never be installed.
ARG GOOSE_VERSION=1.45.0
ARG GOOSE_SHA256_AMD64=e0db638ac437ca0a60b0c1622f45322608d228d1a285214c3bf48fd9763346a5
ARG GOOSE_SHA256_ARM64=c9894106c90e404ac8b8d67c628aea2943dd6a1bc83bfd8e2171d482fa43d72a
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    case "$ARCH" in \
      amd64) GOOSE_ARCH=x86_64; GOOSE_SHA256="$GOOSE_SHA256_AMD64" ;; \
      arm64) GOOSE_ARCH=aarch64; GOOSE_SHA256="$GOOSE_SHA256_ARM64" ;; \
      *) echo "unsupported arch for goose: $ARCH" >&2; exit 1 ;; \
    esac && \
    if curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors "https://github.com/block/goose/releases/download/v${GOOSE_VERSION}/goose-${GOOSE_ARCH}-unknown-linux-gnu.tar.gz" -o /tmp/goose.tar.gz; then \
      echo "${GOOSE_SHA256}  /tmp/goose.tar.gz" | sha256sum -c - && \
      tar xz -C /usr/local/bin -f /tmp/goose.tar.gz && \
      chmod +x /usr/local/bin/goose && \
      rm -f /tmp/goose.tar.gz; \
    else \
      echo "WARN: Goose CLI download failed — skipping (backend: goose will be unavailable)"; \
    fi

# --- Layer 8.5: pi CLI (@earendil-works/pi-coding-agent — backend: pi) ---
# Tolerant of failure like the copilot/codex layers: agents configured for
# backend: pi simply won't launch if this is absent, same degradation as
# any other missing backend binary.
ARG PI_VERSION=0.84.1
RUN npm install -g @earendil-works/pi-coding-agent@${PI_VERSION} && npm cache clean --force || \
    echo "WARN: pi CLI install failed — skipping"

# --- Layer 9: Bob CLI (IBM bobshell — backend: bob) ---
# bobshell is NOT published to the public npm registry; it is distributed as a
# tarball from IBM Cloud Object Storage. The vendor's documented host install is
# `curl https://bob.ibm.com/download/bobshell.sh | bash`, but that script merely
# resolves the latest version and npm-installs the same tarball. We install the
# tarball directly at a pinned version instead: piping a remote script into the
# build would be unpinned and unauditable, and inconsistent with the copilot /
# claude / goose layers above.
# The package is pure JavaScript (no native .node/.so payloads, no os/cpu
# fields), so a single layer serves both linux/amd64 and linux/arm64.
# package.json declares "bin": {"bob": "bundle/bob.js"} — npm therefore creates
# /usr/local/bin/bob directly, matching the binary name the agent launcher
# invokes (manager.go maps "bob" -> "bob"). No symlink is required.
ARG BOBSHELL_VERSION=1.0.6
ARG BOBSHELL_BASE_URL=https://s3.us-south.cloud-object-storage.appdomain.cloud/bob-shell
ARG BOBSHELL_SHA256_AMD64=6ec51abec4251d41ec45709030988b90baa659f535fc8d14dd003023dd163a5b
ARG BOBSHELL_SHA256_ARM64=6ec51abec4251d41ec45709030988b90baa659f535fc8d14dd003023dd163a5b
# Deliberately NOT tolerant of failure, unlike the copilot/goose layers above.
# A hive configured with backend "bob" passes config validation (validBackends
# in pkg/config) and then fails at launch with "agent scanner not running" —
# a silent trap that cost real debugging time. If this download breaks, the
# build must break loudly rather than ship an image that reproduces that bug.
# The `which bob` check turns a partial install into a build failure too.
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    case "$ARCH" in \
      amd64) BOBSHELL_SHA256="$BOBSHELL_SHA256_AMD64" ;; \
      arm64) BOBSHELL_SHA256="$BOBSHELL_SHA256_ARM64" ;; \
      *) echo "unsupported arch for bobshell: $ARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors -o /tmp/bobshell.tgz "${BOBSHELL_BASE_URL}/bobshell-${BOBSHELL_VERSION}.tgz" && \
    echo "${BOBSHELL_SHA256}  /tmp/bobshell.tgz" | sha256sum -c - && \
    npm install -g /tmp/bobshell.tgz && \
    npm cache clean --force && \
    which bob && \
    rm -f /tmp/bobshell.tgz

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
COPY --from=builder /hive-backup /usr/local/bin/hive-backup

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
COPY bin/hive-open-pr.sh /usr/local/bin/hive-open-pr
COPY bin/gh-wrapper.sh /usr/local/bin/gh
# SDK-based copilot model discovery helper (see Layer 3b). Invoked as
# `node /usr/local/bin/copilot-models.mjs` by the dashboard, so no exec bit.
COPY bin/copilot-models.mjs /usr/local/bin/copilot-models.mjs
COPY config/backends.conf /usr/local/etc/hive/backends.conf
RUN chmod +x /usr/local/bin/ttyd-tmux.sh /usr/local/bin/hive-panes /usr/local/bin/gh-app-token.sh \
    /usr/local/bin/hive-config.sh /usr/local/bin/agent-launch.sh /usr/local/bin/gh \
    /usr/local/bin/git-credential-hive.sh /usr/local/bin/hive-open-pr

# --- Nous framework (strategist experiment engine) ---
RUN (apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-venv python3-pip jq && rm -rf /var/lib/apt/lists/*) || \
    echo "WARN: apt-get failed — Nous framework will not be available"
COPY bin/nous-runner.sh bin/nous-sync.py bin/nous-hive-gate.py bin/nous-install.sh /tmp/hive/bin/
RUN chmod +x /tmp/hive/bin/*.sh
COPY examples/kubestellar/nous-campaign.yaml examples/kubestellar/nous-governor-campaign.yaml examples/kubestellar/nous-repo-campaign.yaml /etc/hive/
RUN mkdir -p /var/run/nous/governor /var/run/nous/repo /data/nous/snapshots /opt/nous
# Pin Python packages to prevent supply-chain injection via PyPI (fixes sec-check #3338).
ARG PYYAML_VERSION=6.0.3
ARG SPECIFY_CLI_VERSION=0.16.2
RUN pip3 install --no-cache-dir --break-system-packages "pyyaml==${PYYAML_VERSION}" 2>/dev/null || \
    pip3 install --no-cache-dir "pyyaml==${PYYAML_VERSION}" 2>/dev/null || \
    echo "WARN: system pyyaml install failed"
RUN pip3 install --no-cache-dir --break-system-packages "specify-cli==${SPECIFY_CLI_VERSION}" 2>/dev/null || \
    pip3 install --no-cache-dir "specify-cli==${SPECIFY_CLI_VERSION}" 2>/dev/null || \
    echo "WARN: spec-kit (specify-cli) install failed — inception will generate facts without it"
# pluk (TypeScript): structured event streaming for AI agent tmux sessions
ARG PLUK_VERSION=0.8.0
RUN npm install -g @kubestellar/pluk@${PLUK_VERSION} && npm cache clean --force
RUN mkdir -p /var/run/pluk/logs /var/run/pluk/commands && \
    chown dev:node /var/run/pluk/logs /var/run/pluk/commands && \
    chmod 2770 /var/run/pluk/logs /var/run/pluk/commands
# Nous framework: pinned to a specific commit SHA to prevent supply-chain
# injection via a push to the mutable branch ref. Update NOUS_COMMIT
# deliberately when pulling upstream changes, after reviewing the diff at
# that SHA. feat/cli-agent-mode HEAD verified 2026-08-08.
ARG NOUS_COMMIT=8de28905b87474b870c56e1ecbd3c970532683b8
RUN (which git && \
    git clone https://github.com/clubanderson/agentic-strategy-evolution /opt/nous && \
    git -C /opt/nous checkout "${NOUS_COMMIT}" && \
    python3 -m venv /opt/nous/venv && \
    /opt/nous/venv/bin/pip install --no-cache-dir -e /opt/nous 2>&1) || \
    echo "WARN: Nous install failed — run_campaign.py may not work"

# --- LiteLLM proxy (pinned; optional local Anthropic-compat translator) ---
# Used only when governor.litellm.local_proxy is true; the default flow
# forwards to the remote LiteLLM endpoint via the Go inference translator.
# All heavy deps ship manylinux aarch64 wheels, so multi-arch builds work
# without compilation. || echo matches the Nous/Goose optional-layer convention.
ARG LITELLM_VERSION=1.96.0
RUN (python3 -m venv /opt/litellm/venv && \
    /opt/litellm/venv/bin/pip install --no-cache-dir "litellm[proxy]==${LITELLM_VERSION}" && \
    ln -s /opt/litellm/venv/bin/litellm /usr/local/bin/litellm) || \
    echo "WARN: litellm install failed — local litellm proxy unavailable"
RUN mkdir -p /data/litellm

EXPOSE 3001 3002 7681
VOLUME ["/data", "/secrets"]

COPY v2/deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Pre-create /data/home/ subdirectories with correct ownership so the
# permissions watcher doesn't have to create them at runtime.
RUN mkdir -p /data/home/.copilot /data/home/.cache /data/home/.config /data/home/.local && \
    chown -R dev:node /data/home/

RUN chown -R dev:node /data /home/dev 2>/dev/null || true

ENTRYPOINT ["entrypoint.sh"]
CMD ["--config", "/etc/hive/hive.yaml"]
