#!/usr/bin/env bash
# Install a build/test tool a CI job needs, with a BOUNDED network budget and a
# failure that says what actually went wrong.
#
# Usage:
#   ci-install-tool.sh --require <cmd> --for <what-needs-it> \
#                      [--apt "<pkgs>"] [--apk "<pkgs>"] [--verify "<cmd>"]
#
# Example:
#   ci-install-tool.sh --require gcc --for '-race tests' \
#     --apt 'gcc libc6-dev' --apk 'gcc musl-dev' --verify 'gcc --version'
#
# ── Why this exists (kubestellar/hive#6648) ──────────────────────────────────
#
# The self-hosted `hive` runners lost egress to the Ubuntu mirrors at ~07:00Z on
# 2026-09-11. Every -race shard then failed in "Prepare cgo toolchain", and the
# six inline apt blocks these calls replace each burned 2+ MINUTES first:
#
#   W: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble/InRelease
#      Cannot initiate the connection to archive.ubuntu.com:80 (2620:2d:...).
#      - connect (101: Network is unreachable) ... Could not connect to
#      archive.ubuntu.com:80 (185.125.190.81), connection timed out
#
# apt's defaults are tuned for a flaky link, not a dead one: it retries each
# mirror with a long connect timeout, so a cluster with no route spends minutes
# per shard proving it. Across five cgo shards plus two tmux steps that is the
# difference between a run that fails in seconds and one that fails in ten
# minutes — multiplied by every queued PR.
#
# This script does not try to FIX egress; it cannot. It does make the
# workflow resilient to short mirror/egress drops without hiding a real loss of
# coverage:
#
#   1. If the tool is already present, do nothing. On GitHub-hosted runners gcc
#      and tmux are preinstalled, so the common path touches no network at all.
#   2. Bound each apt fetch and retry the update/download network phase with
#      backoff inside a single hard deadline. A transient mirror loss can heal;
#      a dead route still fails loudly instead of burning minutes per shard.
#   3. On failure emit ONE ::error:: annotation naming runner egress, the mirror
#      hosts attempted, and the remediations, so the answer is on the job
#      summary instead of buried in a wall of apt warnings.
#
# The remediations are the ones from #6648, in the order the issue ranks them:
# bake the tool into the runner image (best — removes the apt dependency), fix
# the cluster's egress, or point HIVE_RUNNER_LABELS at GitHub-hosted runners
# (the mitigation applied on the day).
set -uo pipefail

require=""
purpose=""
apt_pkgs=""
apk_pkgs=""
verify=""

while [ $# -gt 0 ]; do
  case "$1" in
    --require) require="${2:?--require needs a command name}"; shift 2 ;;
    --for)     purpose="${2:?--for needs a description}"; shift 2 ;;
    --apt)     apt_pkgs="${2:-}"; shift 2 ;;
    --apk)     apk_pkgs="${2:-}"; shift 2 ;;
    --verify)  verify="${2:-}"; shift 2 ;;
    *) echo "::error::ci-install-tool.sh: unknown argument '$1'" >&2; exit 2 ;;
  esac
done

[ -n "$require" ] || { echo "::error::ci-install-tool.sh: --require is mandatory" >&2; exit 2; }
purpose=${purpose:-this job}

# Per-attempt network budget. Deliberately small: on a healthy runner the
# mirror answers in well under a second, so anything near this ceiling is a
# broken route. Retries below handle short intermittent drops; longer individual
# hangs only delay the signal.
APT_TIMEOUT_SECONDS="${HIVE_CI_APT_TIMEOUT_SECONDS:-10}"
# Retry the NETWORK phase (update + package download). #6648 made dead egress
# fail fast; #6870 shows the failure is now intermittent, so a small number of
# bounded retries is the repo-side resilience we can add without pretending to
# fix the runner network.
APT_ATTEMPTS="${HIVE_CI_APT_ATTEMPTS:-3}"
APT_BACKOFF_SECONDS="${HIVE_CI_APT_BACKOFF_SECONDS:-5}"
# END-TO-END ceiling for the NETWORK phase (update + download), not per
# invocation. Deliberately so: a per-call ceiling of N gives a worst case of 2N
# (update + download), which for any N large enough to be safe on a healthy
# runner is no better than the 2+ minutes #6648 is about. Budgeting once and
# handing each call what is LEFT makes this number mean what it says — "the
# network cannot stall this step for longer than this".
#
# This ceiling covers ONLY the parts that touch the mirrors. It must NOT cover
# dpkg's local unpack/configure work: installing gcc+libc6-dev pulls ~100
# packages and configuring them routinely takes over a minute on the hive
# runners, which is legitimate CPU/disk time, not a dead route. Wrapping the
# whole `apt-get install` in this budget killed healthy installs mid-configure
# (four consecutive v2 Tests failures on v4, 2026-09-12 08:17–11:00Z).
APT_DEADLINE_SECONDS="${HIVE_CI_APT_DEADLINE_SECONDS:-60}"
# Separate, generous ceiling for the LOCAL phase (dpkg unpack/configure from
# already-downloaded .debs). No network involved: this only guards against a
# wedged dpkg, so it can be long without weakening the fail-fast promise above.
APT_LOCAL_DEADLINE_SECONDS="${HIVE_CI_APT_LOCAL_DEADLINE_SECONDS:-300}"
deadline_at=$(( $(date +%s) + APT_DEADLINE_SECONDS ))
budget_remaining() {
  local left=$(( deadline_at - $(date +%s) ))
  [ "$left" -gt 0 ] && echo "$left" || echo 0
}

# Emit the remediation block once, as a single ::error:: annotation (so it lands
# on the job summary) followed by plain lines for the log.
fail_with_remediation() {
  local what="$1"
  # One line: GitHub renders an annotation up to the first newline, so the
  # summary has to carry the whole diagnosis. The detail block below is for the
  # log.
  echo "::error::${require} is required for ${purpose} and could not be installed: ${what}." \
"Tried apt network operations ${APT_ATTEMPTS} time(s) with ${APT_TIMEOUT_SECONDS}s per fetch against: $(apt_source_hosts)." \
"On the self-hosted 'hive' runners this is normally lost egress to the Ubuntu mirrors (kubestellar/hive#6648/#6870)," \
"not a fault in this workflow. See the log for remediations." >&2
  {
    echo ""
    echo "  ${require} is missing and the package manager could not fetch it."
    echo ""
    echo "  Most likely cause: the runner cannot reach the distro mirrors."
    echo "  Remediations, best first:"
    echo "    1. Bake ${require} into the runner image, so this job needs no network at all."
    echo "    2. Fix egress from the runner cluster (port 80/443 to the distro mirrors)."
    echo "    3. Point the repo variable HIVE_RUNNER_LABELS at '[\"ubuntu-latest\"]' to"
    echo "       run on GitHub-hosted runners, which ship ${require} preinstalled."
    echo ""
    echo "  Apt source hosts: $(apt_source_hosts)"
    echo "  Bounded at ${APT_TIMEOUT_SECONDS}s per fetch, ${APT_ATTEMPTS} network attempt(s),"
    echo "  ${APT_BACKOFF_SECONDS}s backoff, and ${APT_DEADLINE_SECONDS}s for the network phase,"
    echo "  so transient egress can heal but a dead mirror still fails loudly (kubestellar/hive#6648/#6870)."
    echo ""
  } >&2
  exit 1
}

# ── 1. Already present? Then this step is a no-op. ───────────────────────────
if command -v "$require" >/dev/null 2>&1; then
  if [ -n "$verify" ]; then
    # Best-effort: a version banner is nice to have in the log, never a gate.
    $verify 2>/dev/null | head -n1 || true
  fi
  exit 0
fi

# ── 2. Install, with the budget bounded. ─────────────────────────────────────
# `timeout` is not guaranteed on every image; fall back to running bare rather
# than refusing to install. The per-fetch apt options still apply either way.
run_bounded() {
  if command -v timeout >/dev/null 2>&1; then
    local left
    left=$(budget_remaining)
    # Budget already spent: do not start another call that can only run the step
    # further past the ceiling it promises.
    [ "$left" -gt 0 ] || return 124
    timeout "$left" "$@"
  else
    # No `timeout` on this image. The per-fetch apt options still bound each
    # transfer, which is the bulk of the cost; run bare rather than refusing to
    # install over a missing coreutils binary.
    "$@"
  fi
}

# For the LOCAL (no-network) phase: a flat generous ceiling, independent of the
# network budget above, so slow-but-healthy dpkg configure work is never killed
# by a deadline that exists to catch dead mirrors.
run_local_bounded() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "$APT_LOCAL_DEADLINE_SECONDS" "$@"
  else
    "$@"
  fi
}


apt_source_hosts() {
  if [ -d /etc/apt ]; then
    {
      grep -RhoE 'https?://[^/ ]+' /etc/apt/sources.list /etc/apt/sources.list.d 2>/dev/null || true
    } | sed -E 's#https?://##' | sort -u | awk 'NR > 1 { printf ", " } { printf "%s", $0 } END { print "" }'
  fi
}

retry_network_phase() {
  local label="$1"
  shift
  local attempt=1
  local status=0

  while [ "$attempt" -le "$APT_ATTEMPTS" ]; do
    echo "ci-install-tool: ${label} attempt ${attempt}/${APT_ATTEMPTS} (apt hosts: $(apt_source_hosts))" >&2
    "$@"
    status=$?
    if [ "$status" -eq 0 ]; then
      return 0
    fi

    echo "ci-install-tool: ${label} attempt ${attempt}/${APT_ATTEMPTS} failed with exit ${status}" >&2
    if [ "$attempt" -ge "$APT_ATTEMPTS" ]; then
      return "$status"
    fi

    local left
    left=$(budget_remaining)
    if [ "$left" -le "$APT_BACKOFF_SECONDS" ]; then
      echo "ci-install-tool: ${label} has ${left}s of network budget left; not starting another retry" >&2
      return "$status"
    fi

    echo "ci-install-tool: waiting ${APT_BACKOFF_SECONDS}s before retrying ${label}" >&2
    sleep "$APT_BACKOFF_SECONDS"
    attempt=$(( attempt + 1 ))
  done

  return "$status"
}

apt_opts=(
  -o "Acquire::http::Timeout=${APT_TIMEOUT_SECONDS}"
  -o "Acquire::https::Timeout=${APT_TIMEOUT_SECONDS}"
  -o "Acquire::ftp::Timeout=${APT_TIMEOUT_SECONDS}"
  # One retry, not apt's default three: a route that is down does not come back
  # within a single job step, and each extra attempt costs another full timeout.
  -o "Acquire::Retries=1"
)

apt_network_fetch_once() {
  local sudo_prefix=("$@")
  # `apt-get update` EXITS 0 when every mirror fails — it downgrades unreachable
  # sources to `W:` warnings. So its status cannot be the only gate; the
  # download below is what actually proves a usable index/package path exists.
  # Still run update on every retry so an initially-empty or stale index can
  # recover when egress returns.
  run_bounded "${sudo_prefix[@]}" apt-get "${apt_opts[@]}" update -qq \
    || echo "ci-install-tool: apt-get update did not complete cleanly; attempting package download anyway" >&2
  # shellcheck disable=SC2086 # apt_pkgs is a deliberate space-separated list
  run_bounded "${sudo_prefix[@]}" apt-get "${apt_opts[@]}" install --download-only -y -qq $apt_pkgs
}

apt_install() {
  local sudo_prefix=("$@")
  # NETWORK phase: refresh package indexes and fetch the .debs under one
  # bounded retry loop. This is the part a dead mirror can stall, so it is the
  # only part the #6648/#6870 deadline covers.
  retry_network_phase "apt network fetch" apt_network_fetch_once "${sudo_prefix[@]}" \
    || return 1
  # LOCAL phase: unpack/configure from the cache just fetched. No network here —
  # configuring gcc's ~100-package closure legitimately takes >60s on the hive
  # runners, so this runs under its own generous ceiling instead of whatever
  # scraps remain of the network budget.
  # shellcheck disable=SC2086 # apt_pkgs is a deliberate space-separated list
  run_local_bounded "${sudo_prefix[@]}" apt-get "${apt_opts[@]}" install -y -qq $apt_pkgs
}

if [ -n "$apt_pkgs" ] && command -v apt-get >/dev/null 2>&1; then
  if command -v sudo >/dev/null 2>&1; then
    apt_install sudo || fail_with_remediation "apt-get could not install '${apt_pkgs}'"
  else
    apt_install || fail_with_remediation "apt-get could not install '${apt_pkgs}'"
  fi
elif [ -n "$apk_pkgs" ] && command -v apk >/dev/null 2>&1; then
  # shellcheck disable=SC2086 # apk_pkgs is a deliberate space-separated list
  run_bounded apk add --no-cache $apk_pkgs \
    || fail_with_remediation "apk could not install '${apk_pkgs}'"
else
  echo "::error::${require} is required for ${purpose} but no supported package manager is available" >&2
  exit 1
fi

# ── 3. Prove it. ─────────────────────────────────────────────────────────────
# The package manager reporting success is not the same as the command being on
# PATH — and a job that proceeds without it fails later, somewhere less obvious.
if ! command -v "$require" >/dev/null 2>&1; then
  fail_with_remediation "the package manager reported success but '${require}' is still not on PATH"
fi
if [ -n "$verify" ]; then
  $verify 2>/dev/null | head -n1 || true
fi
