#!/usr/bin/env bash
# Advisor tool-less probe launcher (hivecommons/hive#18421).
#
# Builds a disposable image with the Codex and Copilot CLIs at the versions
# src/Dockerfile pins, then runs probe.mjs in a capped container to prove
# whether each CLI can run a turn with no tool a prompt injection could use.
# See probe.mjs for the cases and the pass rule.
#
#   probe.sh            fake mode: no network, no login. A fake model inside
#                       the container calls every tool; proves what the CLI
#                       itself lets run. Safe anywhere a container runs.
#   probe.sh --login    login mode: the real model on a subscription login.
#                       Needs egress to the providers. Run it only on a
#                       disposable machine, never on a hive host, with a
#                       login made for this test:
#                         PROBE_CODEX_AUTH=<path to a Codex auth.json>
#                         PROBE_COPILOT_TOKEN=<Copilot token>
#                       Either may be omitted to probe only the other CLI.
#
# Environment: PROBE_ENGINE (podman|docker, default: whichever is found),
# PROBE_OUT (results directory, default: a new temp dir), CODEX_VERSION /
# COPILOT_VERSION (override the pins), PROBE_CODEX_MODEL /
# PROBE_COPILOT_MODEL (login mode model).
#
# Exit 0 only if every locked-down case passed (and, in fake mode, every
# negative control failed). The report is printed and kept in PROBE_OUT.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
dockerfile="$here/../../Dockerfile"
mode=fake
case "${1:-}" in
  "") ;;
  --login) mode=login ;;
  -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
  *) echo "usage: $0 [--login]" >&2; exit 2 ;;
esac

engine=${PROBE_ENGINE:-$(command -v podman || command -v docker || true)}
[ -n "$engine" ] || { echo "probe: needs podman or docker" >&2; exit 2; }
pin() { sed -n "s/^ARG $1=//p" "$dockerfile" | head -n1; }
codex_version=${CODEX_VERSION:-$(pin CODEX_VERSION)}
copilot_version=${COPILOT_VERSION:-$(pin COPILOT_VERSION)}
[ -n "$codex_version" ] && [ -n "$copilot_version" ] ||
  { echo "probe: could not read the CLI pins from $dockerfile" >&2; exit 2; }
out=${PROBE_OUT:-$(mktemp -d "${TMPDIR:-/tmp}/advisor-toolless-probe.XXXXXX")}
mkdir -p "$out"

image="hive-advisor-toolless-probe:codex-$codex_version-copilot-$copilot_version"
"$engine" build -q -t "$image" \
  --build-arg "CODEX_VERSION=$codex_version" --build-arg "COPILOT_VERSION=$copilot_version" \
  -f "$here/Containerfile" "$here" >/dev/null

run=(run --rm --memory=2g --pids-limit=512 --cap-drop=ALL
  --security-opt=no-new-privileges -v "$out:/probe/out:Z" -e "PROBE_MODE=$mode")
clis=()
if [ "$mode" = fake ]; then
  run+=(--network=none)
  clis=(codex copilot)
else
  if [ -e /data/agents ] || pgrep -x hive >/dev/null 2>&1; then
    echo "probe: this looks like a hive host (/data/agents or a hive process); login mode refuses to run here" >&2
    exit 2
  fi
  if [ -n "${PROBE_CODEX_AUTH:-}" ]; then
    run+=(-v "$PROBE_CODEX_AUTH:/login/codex-auth.json:ro,Z")
    clis+=(codex)
  fi
  if [ -n "${PROBE_COPILOT_TOKEN:-}" ]; then
    # Passed by name so the token is not on the engine's argv.
    export COPILOT_GITHUB_TOKEN=$PROBE_COPILOT_TOKEN
    run+=(-e COPILOT_GITHUB_TOKEN)
    clis+=(copilot)
  fi
  [ "${#clis[@]}" -gt 0 ] || { echo "probe: login mode needs PROBE_CODEX_AUTH and/or PROBE_COPILOT_TOKEN" >&2; exit 2; }
  for m in PROBE_CODEX_MODEL PROBE_COPILOT_MODEL; do
    [ -z "${!m:-}" ] || run+=(-e "$m=${!m}")
  done
fi
run+=(-e "PROBE_CLIS=$(IFS=,; echo "${clis[*]}")")

echo "probe: $mode mode, codex $codex_version, copilot $copilot_version, results in $out" >&2
"$engine" "${run[@]}" "$image"
