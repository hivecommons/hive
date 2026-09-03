# Pinned to the golang:1.27-alpine multi-arch index digest to prevent
# floating-tag supply-chain risk. Refresh deliberately:
#   TOKEN=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/golang:pull" | jq -r .token)
#   curl -sI -H "Authorization: Bearer $TOKEN" \
#     -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json" \
#     https://registry-1.docker.io/v2/library/golang/manifests/1.27-alpine | grep -i docker-content-digest
# and update both the tag and the @sha256 digest together.
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
RUN apk add --no-cache git
WORKDIR /src

# Layer 1: cache Go module downloads (only re-runs when go.mod/go.sum change).
# The cache MOUNT (id=hive-go-mod) makes the always-re-executed download (see
# no-cache-filters in docker.yml) a no-op when the module cache is already
# populated. Mount contents never become image layers, so nothing here can
# ship a stale artifact — the mount is an input cache for `go`, which
# verifies modules against go.sum regardless of where they come from.
COPY src/go.mod src/go.sum ./
RUN --mount=type=cache,id=hive-go-mod,target=/go/pkg/mod \
    go mod download

# Layer 2: copy source and build (re-runs on any source change)
COPY .git /repo/.git
COPY src/ .

ARG GIT_HASH=unknown
ARG GIT_BRANCH=unknown
# VERSION is the semver Hive reports (`hive --version`, heartbeats,
# registration), wired the same way as GIT_HASH/GIT_BRANCH: an -X ldflag that
# takes over from the Go linker default (cmd/hive/main.go, "0.0.0-dev") only
# when a caller passes it. NO current workflow passes it — docker.yml never
# does, and .github/workflows/tagged-release.yml deliberately does not
# rebuild at all; it retags the image docker.yml already published for that
# commit rather than invoking this Dockerfile a second time. This ARG exists
# so that gap can be closed later (teaching a build to embed a real semver)
# without a second source change. See src/docs/releases.md, "What still
# blocks cutting a real release", for why that is not done in this PR.
ARG VERSION=""
# Cache mounts for GOCACHE (/tmp/gobuild, matching the GOCACHE= below) and the
# module cache. docker.yml's `no-cache-filters: builder,runtime` still forces
# this RUN to re-execute on every build — that is the #3760 stale-binary
# defense and it stays — the mounts only make the re-executed compile
# INCREMENTAL. Correctness is `go`'s own: the build cache is keyed by content
# hash of every input, so a stale or corrupted cache entry can never produce a
# binary that differs from a cold build. The linker always writes /hive fresh.
RUN --mount=type=cache,id=hive-go-build,target=/tmp/gobuild \
    --mount=type=cache,id=hive-go-mod,target=/go/pkg/mod \
    GIT_HASH=$(git -C /repo rev-parse HEAD 2>/dev/null || echo "unknown") && \
    GIT_SHORT=$(git -C /repo rev-parse --short=7 HEAD 2>/dev/null || echo "unknown") && \
    if [ "$GIT_BRANCH" = "unknown" ] || [ -z "$GIT_BRANCH" ]; then GIT_BRANCH=$(git -C /repo rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown"); fi && \
    VERSION_LDFLAG="" && \
    if [ -n "$VERSION" ]; then VERSION_LDFLAG="-X main.version=${VERSION}"; fi && \
    GOCACHE=/tmp/gobuild CGO_ENABLED=0 go build -ldflags "-X main.gitHash=${GIT_HASH} -X main.gitShort=${GIT_SHORT} -X main.gitBranch=${GIT_BRANCH} ${VERSION_LDFLAG}" -o /hive ./cmd/hive && \
    GOCACHE=/tmp/gobuild CGO_ENABLED=0 go build -o /bd ./cmd/bd && \
    GOCACHE=/tmp/gobuild CGO_ENABLED=0 go build -o /hive-backup ./cmd/hive-backup


# --- tmux from source (cached until TMUX_VERSION changes) ---
# Pinned to the node:26-slim multi-arch index digest to prevent floating-tag
# supply-chain risk. Refresh deliberately (same technique as golang above,
# repo library/node, tag 26-slim) and update both the tag and the @sha256
# digest together in BOTH FROM node:26-slim lines in this file.
FROM node:26-slim@sha256:c0753125a3789977aefe869cbebccf70e3cfd7ea84ca48547458f02e4f1d7146 AS tmux-builder
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

# Pinned to the same node:26-slim digest as the tmux-builder stage above —
# refresh both FROM lines together.
#
# The final stage is NAMED `runtime` so the CI build steps can list it in
# `no-cache-filters` alongside `builder`. Busting ONLY `builder` rebuilt a fresh
# /hive but let BuildKit serve the final-stage `COPY --from=builder /hive` layer
# (and the rewrite RUN below) from the branch-scoped GHA cache — so a fresh
# binary was rebuilt and then discarded, and the OLD binary shipped. See the
# stale-binary note in .github/workflows/docker.yml.
FROM node:26-slim@sha256:c0753125a3789977aefe869cbebccf70e3cfd7ea84ca48547458f02e4f1d7146 AS runtime

# --- Layer 1: system packages (rarely changes) ---
# util-linux provides `setpriv`, used by the entrypoint to drop to `dev` with an
# AMBIENT CAP_NET_ADMIN (bounding-set gated) so the proxy's SO_MARK egress-gate
# works without a file capability on the binary (#3760). gosu is the fallback
# when setpriv is unavailable or the bounding set lacks NET_ADMIN.
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates bash curl bzip2 gosu util-linux iptables inotify-tools libevent-core-2.1-7 \
    gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*

COPY --from=tmux-builder /usr/local/bin/tmux /usr/local/bin/tmux

# su-exec: lightweight setuid exec for UID switching from non-root.
# gosu rejects SUID mode, so we use su-exec (which is designed for it)
# for the manager's dev→agent UID switches (pkg/agent/manager.go,
# pkg/dashboard/workspace_cleanup.go, deploy/ttyd-tmux.sh — all invoked by the
# non-root `dev` process AFTER the entrypoint's `exec gosu dev`, so a SUID
# helper or CAP_SETUID is genuinely required to switch to agent UIDs at runtime).
#
# SECURITY (C6, CWE-250/269). Two hardening measures over the old
# `curl master | gcc | chmod u+s` (which produced a world-executable
# setuid-root binary, mode 4755, that ANY agent UID could run as
# `su-exec root sh` → root in the pod holding the org App key + all agent
# tokens + iptables control):
#
#  1. SUPPLY CHAIN: pin su-exec.c by COMMIT SHA (not the moving `master` ref)
#     and verify its SHA256 before compiling. An upstream force-push or a
#     MITM'd raw.githubusercontent.com can no longer swap the source under us.
#
#  2. PRIVILEGE ISOLATION: the binary is owned root:hive-launch mode 4750, NOT
#     world-exec 4755. Only root and members of the `hive-launch` group may
#     exec it. The sole member of hive-launch is `dev` (UID 1001), the process
#     that legitimately launches agents. Agent UIDs (2001+) are in group `node`
#     — NOT hive-launch — so an agent trying `su-exec root sh` now gets EACCES
#     at exec, restoring the per-UID isolation the world-exec bit collapsed.
#     Note: root:node 4750 would NOT be enough — `dev` AND every agent share
#     group node, so a node-group SUID is still agent-executable. A dedicated
#     launcher group is what actually excludes the agents.
#
# Full removal of SUID (make dev switch UIDs via CAP_SETUID/CAP_SETGID instead)
# was considered and rejected — see PR body: it needs ambient/inheritable caps
# that fight `allowPrivilegeEscalation: false`, and the hub's netadmin_reconcile
# JSON-Patches the whole securityContext to `{NET_ADMIN}` on drift, silently
# wiping any SETUID/SETGID grant on live hives.
ARG SU_EXEC_COMMIT=89c016e6e08749d583efdeda04b9f73e1218e253
ARG SU_EXEC_SHA256=0c90fe15804f2670037b90e72eb496579fe00d2c15765e5fe2ddec0f5afa35fa
RUN curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors -o /tmp/su-exec.c "https://raw.githubusercontent.com/ncopa/su-exec/${SU_EXEC_COMMIT}/su-exec.c" \
 && echo "${SU_EXEC_SHA256}  /tmp/su-exec.c" | sha256sum -c - \
 && gcc -Wall -o /usr/local/bin/su-exec /tmp/su-exec.c \
 && rm /tmp/su-exec.c \
 && apt-get purge -y gcc libc6-dev && apt-get autoremove -y

# Non-root user for agent processes (Claude Code refuses --dangerously-skip-permissions as root)
# node:24-slim already has group 1000 (node), so reuse it as dev's primary group.
# `hive-launch` is a dedicated supplementary group whose ONLY member is dev; it
# gates exec of the SUID su-exec helper (chmod 4750 below) so agent UIDs — which
# are in group node but NOT hive-launch — cannot run it. See the su-exec C6 note.
#
# The GID is PINNED (not left to groupadd --system's dynamic allocation) because
# Kubernetes `fsGroup` and Secret `defaultMode: 0440` are what keep the projected
# GitHub App PEMs readable by dev while excluding agent UIDs (audit N5, CWE-732).
# fsGroup takes a numeric GID — it cannot name a group — so this value is part of
# the deployment contract and must stay in sync with src/deploy/k8s/*.yaml and the
# SaaS provisioning template in src/pkg/hub/saas_provision.go.
ARG HIVE_LAUNCH_GID=1002
RUN if getent group "${HIVE_LAUNCH_GID}" >/dev/null; then \
      echo "FATAL: GID ${HIVE_LAUNCH_GID} is already taken by '$(getent group "${HIVE_LAUNCH_GID}" | cut -d: -f1)'." >&2; \
      echo "       fsGroup in the deployment manifests pins this GID; pick a free one and update" >&2; \
      echo "       src/deploy/k8s/*.yaml and saas_provision.go together." >&2; \
      exit 1; \
    fi \
 && groupadd --system --gid "${HIVE_LAUNCH_GID}" hive-launch \
 && useradd -m -u 1001 -g node -G hive-launch -s /bin/bash dev \
 && chown root:hive-launch /usr/local/bin/su-exec \
 && chmod 4750 /usr/local/bin/su-exec \
 && [ "$(stat -c '%G' /usr/local/bin/su-exec)" = "hive-launch" ]

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
# Pinned for MITM-proxy compatibility. History: 1.0.6x went native-only with
# a rustls/webpki-roots TLS stack that rejected the proxy's forged certs, so
# the hive sat on 1.0.59 (last version whose Node.js fallback honored
# NODE_EXTRA_CA_CERTS) with the native binary deleted. As of 1.0.78 the
# platform package is a Node SEA again with an explicit NODE_EXTRA_CA_CERTS
# handler (plus SSL_CERT_FILE/SSL_CERT_DIR), so the proxy CA is trusted
# without any binary surgery. Do NOT re-add the old rm-the-binary hack: the
# 1.0.78 npm wrapper only spawns the platform binary — deleting it now
# breaks the CLI outright instead of triggering a fallback.
ARG COPILOT_VERSION=1.0.78
RUN npm install -g --ignore-scripts --no-fund --no-audit \
    "@github/copilot@${COPILOT_VERSION}" && npm cache clean --force || \
    echo "WARN: GitHub Copilot CLI install failed — skipping"

# --- Layer 3b: GitHub Copilot SDK (pinned; powers SDK-based model discovery) ---
# The dashboard's copilot model discovery prefers a probe through the official
# SDK (bin/copilot-models.mjs → /usr/local/bin/copilot-models.mjs) because it
# rides the CLI's own auth and TLS handling. The helper pins the SDK's runtime
# connection to the Layer 3 CLI entry above, so the newer @github/copilot the
# SDK nests as its own dependency is never executed (its native binary would
# reject the proxy CA). Tolerant of failure like the copilot layer: discovery
# falls back to the raw HTTP probe when the SDK is absent.
ARG COPILOT_SDK_VERSION=1.0.8
RUN npm install -g --ignore-scripts --no-fund --no-audit \
    "@github/copilot-sdk@${COPILOT_SDK_VERSION}" && npm cache clean --force || \
    echo "WARN: GitHub Copilot SDK install failed — skipping"

# --- Layer 4: OpenAI Codex CLI (pinned; backend: codex + live model discovery) ---
# Pinned for reproducibility; version matches src/Dockerfile.contributor —
# keep the two pins in sync. @openai/codex resolves a platform-specific
# native binary (@openai/codex-<platform>) via optionalDependencies, so a
# plain npm install serves both linux/amd64 and linux/arm64.
# Tolerant of failure like the copilot layer: if codex is absent, the
# dashboard's codex model discovery (`codex app-server` model/list — pure
# local stdio, no network, unaffected by the proxy CA) falls back to the
# static list, and non-codex backends are unaffected.
ARG CODEX_VERSION=0.146.0
RUN npm install -g --ignore-scripts --no-fund --no-audit @openai/codex@${CODEX_VERSION} && npm cache clean --force || \
    echo "WARN: OpenAI Codex CLI install failed — skipping"

# --- Layer 7: Claude Code (most frequent updates) ---
ARG CLAUDE_CODE_VERSION=2.1.226
# --ignore-scripts (kept, deliberately: no arbitrary postinstall runs during the
# build) skips claude-code's install.cjs, which is what LINKS the platform
# native binary into the package's bin/. The binary itself ships in the
# platform sub-package, so nothing is downloaded here — but without the link
# every invocation dies with:
#
#     Error: claude native binary not installed.
#
# i.e. the `claude` backend is unusable in the image. Run the vendor's own
# install step explicitly: still auditable (one named script, not "whatever any
# dependency wants"), and it writes into the global package dir, so the fix
# applies to every agent UID rather than one $HOME.
RUN npm install -g --ignore-scripts --no-fund --no-audit @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force && \
    node /usr/local/lib/node_modules/@anthropic-ai/claude-code/install.cjs && \
    claude --version

# --- Layer 8: Goose CLI (official block/goose upstream) ---
# Installs from the OFFICIAL block/goose repo, pinned to a fixed release and
# SHA-256-verified before extraction. Download failures are tolerated because
# Goose is an optional backend; checksum mismatches must hard-fail the build.
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

# --- Layer 8.25: Google Antigravity CLI (backend: agy) ---
# The vendor distributes a self-contained Linux executable, not an npm package.
# Pin the version, build ID, and SHA-512 from its public updater manifest; do
# not use the mutable manifest at image-build time. The CLI requires a
# pre-authenticated Google OAuth session in $HOME/.gemini: the image supplies
# only the binary, while an operator seeds that persistent state (a pod cannot
# complete the interactive OAuth flow itself). This enables a self-hosted Hive
# controller to use its configured agy fallback rung without pretending that
# generic contributor containers can bootstrap Google login.
ARG AGY_VERSION=1.1.19
ARG AGY_BUILD=4894004681244672
ARG AGY_SHA512_AMD64=7c3b310c80685adcba714994207eb870fb4817403975da46555b7d9fade446487da3d8f897aa220dfbc30f602ab461a0147a6f7b3c8228fdebd35951e3f250fd
ARG AGY_SHA512_ARM64=488c3dac1c7ca866a9da990f9a88648bfb7176992a0a2d27605e3bc10143e5b17af552ed99f0bf4250e2d69fed6d0d5f50684c729bf03f269f2b52b44503558d
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    case "$ARCH" in \
      amd64|x86_64) AGY_URL_ARCH=x64; AGY_FILE_ARCH=x64; AGY_SHA512="$AGY_SHA512_AMD64" ;; \
      arm64|aarch64) AGY_URL_ARCH=arm; AGY_FILE_ARCH=arm64; AGY_SHA512="$AGY_SHA512_ARM64" ;; \
      *) echo "unsupported arch for agy: $ARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors \
      -o /tmp/agy.tar.gz "https://storage.googleapis.com/antigravity-public/antigravity-cli/${AGY_VERSION}-${AGY_BUILD}/linux-${AGY_URL_ARCH}/cli_linux_${AGY_FILE_ARCH}.tar.gz" && \
    echo "${AGY_SHA512}  /tmp/agy.tar.gz" | sha512sum -c - && \
    tar xzf /tmp/agy.tar.gz -C /tmp && \
    install -m 0755 /tmp/antigravity /usr/local/bin/agy && \
    rm -f /tmp/agy.tar.gz /tmp/antigravity && \
    agy --version

# --- Layer 8.5: pi CLI (@earendil-works/pi-coding-agent — backend: pi) ---
# Tolerant of failure like the copilot/codex layers: agents configured for
# backend: pi simply won't launch if this is absent, same degradation as
# any other missing backend binary.
ARG PI_VERSION=0.84.1
RUN npm install -g --ignore-scripts --no-fund --no-audit \
    "@earendil-works/pi-coding-agent@${PI_VERSION}" && npm cache clean --force || \
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
# Tolerant of download failure now, like the copilot/codex/goose/pi layers
# above (issue #5203) — IBM COS (s3.us-south.cloud-object-storage.appdomain.cloud)
# has gone unreachable for multi-minute windows three times in one day
# (#4941, then recurred post-fix; CI runs 32990236986, 33072306789,
# 33132187354, 33322522386), and no retry ladder can ride out an outage that
# outlasts it — every one of #4941's added attempts hit
# "Connection timed out after 15002 ms" across ~175s, i.e. the endpoint was
# down for the whole window, not flaking.
#
# This used to be deliberately hard-fail-only, because a hive configured with
# backend "bob" passed config validation (validBackends in pkg/config) and
# then failed at launch with no honest signal — a silent trap that cost real
# debugging time. That trap is now closed at the launch site instead of at
# image-build time: pkg/agent/manager.go's launchInTmux resolves the backend
# binary via exec.LookPath BEFORE any bob-specific logic runs, and on a miss
# marks the agent StateFailed, records LastError, logs a warning, writes a
# banner into the agent's own tmux pane, and emits an AuditAgentStartFailed
# event — all before the agent could ever appear to be silently stuck. A
# soft-failed bobshell layer now degrades exactly the same way a soft-failed
# Goose layer already does: the backend is honestly reported unavailable per
# agent, at launch, not silently absent with no signal anywhere (the failure
# mode #5048 describes for a different backend, agy).
#
# Checksum mismatches are still never tolerated — the || only covers the
# curl download step, not the verify/install steps, so a tampered or
# corrupted tarball still hard-fails the build exactly as before.
# Per-attempt bounds (--connect-timeout/--speed-time/--max-time) still kill
# stalled IBM COS connections so a single hung attempt cannot consume the
# whole retry budget on its own (issue #4941).
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    case "$ARCH" in \
      amd64) BOBSHELL_SHA256="$BOBSHELL_SHA256_AMD64" ;; \
      arm64) BOBSHELL_SHA256="$BOBSHELL_SHA256_ARM64" ;; \
      *) echo "unsupported arch for bobshell: $ARCH" >&2; exit 1 ;; \
    esac && \
    if curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 600 --retry-connrefused --retry-all-errors \
      --connect-timeout 15 --speed-limit 1024 --speed-time 30 --max-time 180 \
      -o /tmp/bobshell.tgz "${BOBSHELL_BASE_URL}/bobshell-${BOBSHELL_VERSION}.tgz"; then \
      echo "${BOBSHELL_SHA256}  /tmp/bobshell.tgz" | sha256sum -c - && \
      npm install -g /tmp/bobshell.tgz && \
      npm cache clean --force && \
      which bob && \
      rm -f /tmp/bobshell.tgz; \
    else \
      echo "WARN: Bob CLI download failed — skipping (backend: bob will be unavailable)"; \
    fi

# --- Application layers ---
ENV HOME=/home/dev

# Proxy dependencies (only re-runs when package.json changes)
COPY src/proxy/package.json src/proxy/package-lock.json* /opt/hive/proxy/
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

# #3760 — defeat the BuildKit/overlayfs metacopy redirect on the cross-stage
# COPY above. When BuildKit builds these images it can emit a layer where a
# file brought in by `COPY --from` is stored as an overlay *metacopy redirect*
# (a size-0 upper entry with a trusted.overlay.redirect / metacopy xattr that
# points at the lower data). On the CI runner's docker/runc this execs fine,
# but on the fleet's runtimes — containerd/k3s and rootless podman, both on the
# overlayfs snapshotter — the kernel presents the redirected file as
# NON-EXECUTABLE at its image path (bare EPERM at execve) until it is copied up
# to the writable upper layer. That is precisely why the reporter saw
# `cp /usr/local/bin/hive /tmp/hx && /tmp/hx --version` succeed while running it
# in place failed: the `cp` forces a copy-up that materializes the real bytes.
#
# Rewriting the binary here — a plain read+write to a NEW inode in THIS stage —
# lands the full bytes freshly on this stage's own top layer, so the shipped
# layer contains real file data with no cross-stage overlay redirect. The exec
# bit is reasserted for good measure. This is robust regardless of buildx
# attestation/OCI-index flags; the workflow-side `provenance:false / sbom:false`
# change (which stops the OCI-index form that enables the metacopy layout in the
# first place) and this rewrite are independent, belt-and-suspenders defenses.
RUN for b in hive bd hive-backup; do \
      cp --remove-destination "/usr/local/bin/$b" "/usr/local/bin/$b.fresh" && \
      mv -f "/usr/local/bin/$b.fresh" "/usr/local/bin/$b" && \
      chmod 0755 "/usr/local/bin/$b"; \
    done

COPY src/proxy/ /opt/hive/proxy/

COPY src/pkg/dashboard/static/index.html /opt/hive/proxy/public/

COPY dashboard/ /opt/hive/dashboard/
RUN chmod +x /opt/hive/dashboard/publish-snapshot*.sh

COPY examples/kubestellar/hive-project.yaml /etc/hive/hive-project.yaml
COPY examples/acmm/ /opt/hive/examples/acmm/
COPY examples/agents/ /opt/hive/examples/agents/
COPY src/pkg/config/packs/ /opt/hive/packs/
COPY src/deploy/data/ /opt/hive/seed-data/
COPY src/deploy/ttyd-tmux.sh /usr/local/bin/ttyd-tmux.sh
COPY src/deploy/hive-panes.sh /usr/local/bin/hive-panes
COPY bin/gh-app-token.sh bin/hive-config.sh bin/agent-launch.sh bin/git-credential-hive.sh /usr/local/bin/
# SECURITY (#4045): sourced (not executed) by every shell in an agent's process
# tree — via BASH_ENV/ENV from agent-launch.sh for non-interactive tool shells,
# and via the /etc/bash.bashrc guard below for interactive ones — to unset the
# GitHub credentials backend CLIs re-export into the shells they spawn. No exec
# bit on purpose: it must only ever be sourced.
COPY bin/agent-env-scrub.sh /usr/local/bin/agent-env-scrub.sh
COPY bin/hive-open-pr.sh /usr/local/bin/hive-open-pr
COPY bin/hive-open-issue.sh /usr/local/bin/hive-open-issue
COPY bin/hive-merge.sh /usr/local/bin/hive-merge
COPY bin/hive-review.sh /usr/local/bin/hive-review
COPY bin/hive-baseline-check.sh /usr/local/bin/hive-baseline-check.sh
COPY bin/gh-wrapper.sh /usr/local/bin/gh
# SDK-based copilot model discovery helper (see Layer 3b). Invoked as
# `node /usr/local/bin/copilot-models.mjs` by the dashboard, so no exec bit.
COPY bin/copilot-models.mjs /usr/local/bin/copilot-models.mjs
COPY config/backends.conf /usr/local/etc/hive/backends.conf
RUN chmod +x /usr/local/bin/ttyd-tmux.sh /usr/local/bin/hive-panes /usr/local/bin/gh-app-token.sh \
    /usr/local/bin/hive-config.sh /usr/local/bin/agent-launch.sh /usr/local/bin/gh \
    /usr/local/bin/git-credential-hive.sh /usr/local/bin/hive-open-pr /usr/local/bin/hive-open-issue /usr/local/bin/hive-merge /usr/local/bin/hive-review \
    /usr/local/bin/hive-baseline-check.sh

# SECURITY (#4045): interactive-shell arm of the agent credential scrub.
# BASH_ENV (exported by agent-launch.sh) only reaches NON-interactive shells;
# an interactive bash a backend CLI spawns for the agent reads
# /etc/bash.bashrc instead. Gated on the agent-identity env agent-launch.sh
# exports, so an operator's own `kubectl exec` shell is untouched.
RUN printf '\n# hive #4045: scrub backend-exported GitHub credentials from agent shells\n[ -n "${HIVE_AGENT:-}${HIVE_AGENT_ID:-}" ] && . /usr/local/bin/agent-env-scrub.sh || true\n' >> /etc/bash.bashrc

# SECURITY (audit F5-egress / refs #2674, #2678, #3760): the hive process needs
# CAP_NET_ADMIN in its EFFECTIVE set so the MITM proxy can stamp SO_MARK on its
# own upstream dials and be exempted from the forced-egress iptables REDIRECT
# (on OpenShift/OVN xt_owner is ABSENT, so the owner-UID exemptions silently fail
# to load and SO_MARK is the ONLY remaining exemption — observed live on the heartbeat-only cluster,
# where the proxy's own :443 dials looped into the REDIRECT and 502'd every forge
# request). setsockopt(SO_MARK) requires CAP_NET_ADMIN in the caller's EFFECTIVE
# set, and the hive process is dropped to non-root `dev`.
#
# WHY NOT A FILE CAPABILITY (the previous approach — deliberately removed)
# A `setcap cap_net_admin+ep /usr/local/bin/hive` file capability is recomputed
# by the kernel AT execve, and the kernel REFUSES to exec a binary whose file
# caps it cannot fully grant from the current BOUNDING set. NET_ADMIN is NOT in
# the default docker/podman/containerd bounding set, so on any self-hosted spoke
# (k3s, rootless podman) execve of the capped binary fails with a bare EPERM —
# the crash-loop diagnosed in #3760 (the overlayfs metacopy diagnosis was a red
# herring for THIS symptom: even after the metacopy rewrite above, the file cap
# alone EPERMs wherever the bounding set lacks NET_ADMIN). A file capability and
# "execs everywhere" are fundamentally incompatible, so the file cap is GONE and
# the binary ships with NO security.capability xattr.
#
# HOW NET_ADMIN REACHES hive INSTEAD (see src/deploy/entrypoint.sh)
# The entrypoint reads its own CapBnd (bounding set) at the privilege-drop point
# and, ONLY when CAP_NET_ADMIN is present (managed OKE/OpenShift spokes grant it
# via SCC/PSA), drops to `dev` with
# `setpriv --inh-caps +net_admin --ambient-caps +net_admin` so hive inherits
# NET_ADMIN in its EFFECTIVE+AMBIENT set — SO_MARK works exactly as before.
# BOTH flags are required (#3874): the kernel only permits an ambient bit that
# is also in the INHERITABLE set, and setpriv applies --ambient-caps AFTER the
# UID change (when pI is already zeroed), so `--ambient-caps` alone exits 0
# while silently leaving CapAmb=0x0. When the bounding set LACKS it (self-hosted/rootless), the entrypoint
# does NOT attempt the raise (setpriv would error) and falls back to plain
# `gosu dev`: the binary execs fine (no file cap to block it) and the proxy's
# SO_MARK path degrades gracefully (src/pkg/proxy/github_proxy.go logs once and
# dials unmarked). setpriv comes from util-linux, installed in Layer 1.

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
RUN (npm install -g --ignore-scripts --no-fund --no-audit "@hivecommons/pluk@${PLUK_VERSION}" || \
    npm install -g --ignore-scripts --no-fund --no-audit "@kubestellar/pluk@${PLUK_VERSION}") && npm cache clean --force
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

COPY src/deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Pre-create /data/home/ subdirectories with correct ownership so the
# permissions watcher doesn't have to create them at runtime.
RUN mkdir -p /data/home/.copilot /data/home/.cache /data/home/.config /data/home/.local && \
    chown -R dev:node /data/home/

RUN chown -R dev:node /data /home/dev 2>/dev/null || true

# --- SUID/SGID inventory (#3866, CWE-250/CWE-732) ---
# Strip every setuid/setgid bit in the image except the one su-exec helper the
# launcher needs, and FAIL THE BUILD if the inventory is anything else.
#
# The C6 fix above hardened su-exec to 4750 root:hive-launch so agent UIDs
# cannot exec it. It did not touch what the BASE IMAGE ships. node:26-slim
# (Debian 13) plus the passwd/util-linux packages installed at the top of this
# stage leave eleven setuid/setgid binaries behind, and every one of them is
# world-executable — so reachable by any agent UID (2001+), which is exactly the
# population the su-exec 4750 mode exists to exclude:
#
#   4755 root:root    /usr/bin/{chfn,chsh,gpasswd,mount,newgrp,passwd,su,umount}
#   2755 root:shadow  /usr/bin/{chage,expiry}  /usr/sbin/unix_chkpwd
#
# Nothing in Hive execs any of them. The entrypoint creates the per-agent
# accounts with useradd/groupadd/usermod while it is still root, and every
# privilege hop goes through gosu (as root) or su-exec (as dev) — none of which
# needs a setuid bit on a Debian tool.
#
# They matter because the pod cannot neutralise them the usual way. Setting
# `allowPrivilegeEscalation: false` would set no_new_privs and disable ALL SUID
# binaries — including su-exec, which would break agent launch (see the C6 note
# in src/deploy/k8s/deployment.yaml). So no_new_privs stays off, and a SUID-root
# exec from an agent UID would land in a bounding set that still holds SETUID,
# SETGID and DAC_OVERRIDE: enough to read the MITM CA private key whatever mode
# the 0700 directory #4341 moved it into carries.
#
# Stripping is safe precisely because only NON-root callers need a setuid bit.
# root keeps full use of mount/su/passwd, and the accounts those tools would
# edit are locked system accounts (root's shadow entry is `*`).
#
# This runs LAST, after every apt/npm/pip layer, so a package installed anywhere
# above is covered. The assertion is what makes it durable: a future layer that
# reintroduces a setuid binary fails the build here rather than shipping.
RUN find / -xdev -type f \( -perm -4000 -o -perm -2000 \) \
      ! -path /usr/local/bin/su-exec -print -exec chmod -s '{}' + \
 && remaining="$(find / -xdev -type f \( -perm -4000 -o -perm -2000 \) -printf '%M %u:%g %p\n' | sort)" \
 && if [ "$remaining" != "-rwsr-x--- root:hive-launch /usr/local/bin/su-exec" ]; then \
      echo "FATAL: image SUID/SGID inventory is not the single expected helper." >&2; \
      echo "       want: -rwsr-x--- root:hive-launch /usr/local/bin/su-exec" >&2; \
      echo "       got:" >&2; echo "$remaining" | sed 's/^/         /' >&2; \
      exit 1; \
    fi


# --- Layer 8.26: shared-$HOME state hygiene (umask 007 wrappers) ---
# Agent CLIs rewrite their credential/settings state as mode 600 owned by the
# writing agent (agy: antigravity-cli/*.json + oauth token; claude:
# .credentials.json). With a shared $HOME (/data/home) that breaks every other
# agent on the same provider (EACCES -> login/onboarding screens). Launch
# through wrappers that set umask 007 so shared state stays group-readable.
# See RFC #4665.
RUN for b in agy claude; do \
      [ -e "/usr/local/bin/$b" ] || continue; \
      mv "/usr/local/bin/$b" "/usr/local/bin/$b.real"; \
      printf '#!/bin/sh\numask 007\nexec /usr/local/bin/%s.real "$@"\n' "$b" > "/usr/local/bin/$b"; \
      chmod 755 "/usr/local/bin/$b"; \
    done

ENTRYPOINT ["entrypoint.sh"]
CMD ["--config", "/etc/hive/hive.yaml"]
