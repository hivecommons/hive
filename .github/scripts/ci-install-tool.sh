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
# This script does not try to FIX egress; it cannot, and pretending otherwise
# (forcing IPv4, swapping in a mirror) would paper over a cluster fault. It
# makes the failure fast and legible:
#
#   1. If the tool is already present, do nothing. On GitHub-hosted runners gcc
#      and tmux are preinstalled, so the common path touches no network at all.
#   2. Bound apt's budget explicitly — short per-attempt timeout, one retry, and
#      a hard `timeout` ceiling around the whole call.
#   3. On failure emit ONE ::error:: annotation naming runner egress and the
#      three remediations, so the answer is on the job summary instead of
#      buried in a wall of apt warnings.
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
# broken route, and waiting longer only makes the report slower — it never
# makes it succeed.
APT_TIMEOUT_SECONDS="${HIVE_CI_APT_TIMEOUT_SECONDS:-10}"
# END-TO-END ceiling for the whole install, not per invocation. Deliberately so:
# a per-call ceiling of N gives a worst case of 2N (update + install), which for
# any N large enough to be safe on a healthy runner is no better than the 2+
# minutes #6648 is about. Budgeting once and handing each call what is LEFT
# makes this number mean what it says — "this step cannot take longer than this
# to fail".
APT_DEADLINE_SECONDS="${HIVE_CI_APT_DEADLINE_SECONDS:-60}"
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
"On the self-hosted 'hive' runners this is normally lost egress to the Ubuntu mirrors (kubestellar/hive#6648)," \
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
    echo "  Bounded at ${APT_TIMEOUT_SECONDS}s per fetch and ${APT_DEADLINE_SECONDS}s for the whole step,"
    echo "  so a dead mirror fails in seconds instead of minutes (kubestellar/hive#6648)."
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

apt_opts=(
  -o "Acquire::http::Timeout=${APT_TIMEOUT_SECONDS}"
  -o "Acquire::https::Timeout=${APT_TIMEOUT_SECONDS}"
  -o "Acquire::ftp::Timeout=${APT_TIMEOUT_SECONDS}"
  # One retry, not apt's default three: a route that is down does not come back
  # within a single job step, and each extra attempt costs another full timeout.
  -o "Acquire::Retries=1"
)

apt_install() {
  local sudo_prefix=("$@")
  # `apt-get update` EXITS 0 when every mirror fails — it downgrades unreachable
  # sources to `W:` warnings. So its status cannot be the gate; the install
  # below is what actually proves an index was fetched. Its failure is reported,
  # not swallowed, but it is the install that decides.
  run_bounded "${sudo_prefix[@]}" apt-get "${apt_opts[@]}" update -qq \
    || echo "ci-install-tool: apt-get update did not complete cleanly; attempting the install anyway" >&2
  # shellcheck disable=SC2086 # apt_pkgs is a deliberate space-separated list
  run_bounded "${sudo_prefix[@]}" apt-get "${apt_opts[@]}" install -y -qq $apt_pkgs
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
