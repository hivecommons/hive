# syntax=docker/dockerfile:1

# hivecommons/hive#7206 — bake the CI toolchain into the runner image.
#
# ── Why this exists ─────────────────────────────────────────────────────────
#
# Every `-race` shard needs a C toolchain (CGO_ENABLED=1), and the stock ARC
# runner image does not carry one. So each shard ran `apt-get install gcc
# libc6-dev` against the Ubuntu mirrors, which made a green test run depend on
# runner egress being healthy at that moment. It repeatedly was not:
# #6648 → #6870 → #6935 → #7124 → #7201, closed four times and back every time.
#
# Each previous fix softened the symptom rather than removing the dependency:
#
#   #6648  fail fast with one clear annotation  → still a failure, just sooner
#   #6870  bounded retry with backoff           → cannot conjure a route
#   #7009  offline .deb cache                   → the cache entry gets evicted
#   #7124  prune dead-branch caches             → 0 GB left to reclaim
#
# #7009 is sound, but it cannot win where it was placed. Measured 2026-09-16:
# the repo sits at 9.99 GB of its 10 GB Actions cache ceiling, buildkit blobs
# are 96.6% of that, and 6.57 GB of blobs were written in a single day. A
# few-tens-of-MB apt entry is always what LRU evicts, so race shards are
# permanently forced back onto the network.
#
# Baking the toolchain in takes it out of that competition entirely.
# `.github/scripts/ci-install-tool.sh` already no-ops when the tool is present:
#
#   1. If the tool is already present, do nothing. On GitHub-hosted runners gcc
#      and tmux are preinstalled, so the common path touches no network at all.
#
# So no workflow change is needed. The moment pods run this image, every race
# shard takes the zero-network path and the whole failure class goes away.
#
# ── Base image ──────────────────────────────────────────────────────────────
#
# Default matches the live runners: job logs on 2026-09-16 report
# `Current runner version: '2.337.0'` and a noble (Ubuntu 24.04) userspace,
# which is also what `apt-mirror-configmap.yaml` pins its suites to.
#
# Override if the deployment has moved on:
#   docker build --build-arg RUNNER_BASE_IMAGE=... -f src/deploy/ci-runners/Dockerfile .
#
# Keep this in step with the runner version ARC is deploying. A mismatch is not
# dangerous — ARC replaces the runner binary at registration when it is behind —
# but drifting far enough that the Ubuntu RELEASE changes would invalidate the
# `noble` suites written below, exactly as the README's #6648 caveat warns.
ARG RUNNER_BASE_IMAGE=summerwind/actions-runner:v2.337.0-ubuntu-24.04
FROM ${RUNNER_BASE_IMAGE}

# The base image runs as uid 1001 (`runner`), which does not own /etc/apt.
USER root

# Write the #6648 apt repair into the image BEFORE installing anything.
#
# The runner pods get this same configuration mounted from ConfigMap
# `hive-runner-apt-mirror`, but that only helps at RUN time, not BUILD time. If
# this image is built from inside the cluster — the obvious place to build it —
# the build itself hits the exact dead route #6648 is about: no IPv6 egress at
# all, and IPv4 to archive.ubuntu.com blocked. azure.archive.ubuntu.com carries
# the same archive, security suite included, and is reachable over IPv4.
#
# Built from outside the cluster this is harmless: azure.archive.ubuntu.com is
# a public mirror, so the build works in both places.
#
# These two files are kept byte-identical to the ConfigMap's copies, and
# TestCIRunnerDockerfileAptConfigMatchesConfigMap fails if they drift.
RUN set -eux; \
    printf '%s\n' \
      'Types: deb' \
      'URIs: http://azure.archive.ubuntu.com/ubuntu/' \
      'Suites: noble noble-updates noble-backports' \
      'Components: main universe restricted multiverse' \
      'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' \
      '' \
      'Types: deb' \
      'URIs: http://azure.archive.ubuntu.com/ubuntu/' \
      'Suites: noble-security' \
      'Components: main universe restricted multiverse' \
      'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' \
      > /etc/apt/sources.list.d/ubuntu.sources; \
    printf '%s\n' 'Acquire::ForceIPv4 "true";' \
      > /etc/apt/apt.conf.d/99-force-ipv4

# The toolchain itself.
#
#   gcc, libc6-dev  — cgo, required by every `go test -race` shard
#   tmux            — pkg/agent's hermetic terminal tests
#
# These are precisely the three packages ci-install-tool.sh is asked to install
# across .github/workflows/*, so this list is what makes every one of those
# call sites take the no-op path.
#
# Verification runs in the build, not just in the cluster: if a future base
# image drops one of these or the mirror goes stale, the build fails here
# rather than producing an image that silently sends CI back to the mirrors.
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      gcc \
      libc6-dev \
      tmux; \
    rm -rf /var/lib/apt/lists/*; \
    gcc --version; \
    tmux -V; \
    command -v gcc; \
    command -v tmux

# Back to the unprivileged runner user the base image expects. ARC's entrypoint
# and the runner binary both assume uid 1001; leaving this as root would change
# the security posture of every job.
USER runner
