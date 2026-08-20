#!/usr/bin/env bash
# Contract test for the Quadlet unit's config coupling (#4367).
# Run: bash src/deploy/test_quadlet_config_contract.sh
#
# The unit's HealthCmd= probes a port that lives in the operator's hive.yaml,
# and NOTHING at install time notices when the two disagree. The config parses,
# the Quadlet generator exits 0, the container starts and is genuinely healthy —
# on the other port. Notify=healthy then does exactly what it should and holds
# the unit in `activating` until TimeoutStartSec=300 expires, at which point the
# generated `--rm` deletes the container that held the evidence. Five minutes,
# no artifact, one wrong digit.
#
# That is the shape the repo already gates rather than reviews: invisible when
# it breaks, and no dry-run can catch it. So this asserts the coupling directly.
# Pure string analysis: nothing is started, no registry is contacted.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

UNIT="src/deploy/quadlet/hive.container"
DOC="src/docs/podman-standalone-quadlet.md"
COMPOSE="src/docker-compose.yaml"
DEPLOY_CONF="src/deploy/hive.yaml"
EXAMPLE_CONF="src/hive.yaml.example"
GO_CONFIG="src/pkg/config/config.go"

pass_count=0
failures=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

ok() {
  pass_count=$((pass_count + 1))
}

need_file() {
  if [[ ! -f "${ROOT}/$1" ]]; then
    fail "$1: expected file is missing"
    return 1
  fi
  return 0
}

# --- 1. The port the unit probes -------------------------------------------

health_port=""
if need_file "$UNIT"; then
  health_port="$(
    grep -E '^HealthCmd=' "${ROOT}/${UNIT}" \
      | grep -oE '127\.0\.0\.1:[0-9]+' \
      | cut -d: -f2 \
      | head -n1
  )"
  if [[ -z "$health_port" ]]; then
    fail "${UNIT}: no HealthCmd= probing http://127.0.0.1:<port>/api/health. Notify=healthy without a healthcheck reports \"started\" the moment conmon is up."
  else
    ok
  fi
fi

# --- 2. The Go default, which is what an omitted dashboard.port gives -------

go_default=""
if need_file "$GO_CONFIG"; then
  go_default="$(
    grep -oE 'defaultDashboardPort[[:space:]]*=[[:space:]]*[0-9]+' "${ROOT}/${GO_CONFIG}" \
      | grep -oE '[0-9]+$' \
      | head -n1
  )"
  if [[ -z "$go_default" ]]; then
    fail "${GO_CONFIG}: could not read defaultDashboardPort"
  elif [[ -n "$health_port" && "$go_default" != "$health_port" ]]; then
    fail "${GO_CONFIG}: defaultDashboardPort is ${go_default} but ${UNIT} probes ${health_port}. An operator who omits dashboard.port would then never reach healthy."
  else
    ok
  fi
fi

# --- 3. yaml configs that ship a dashboard.port ----------------------------

# Reads the `port:` immediately under the top-level `dashboard:` key. Good
# enough for these files and deliberately not a YAML parser: the gate must run
# with nothing installed but bash.
dashboard_port() {
  awk '
    /^dashboard:/ { in_dash = 1; next }
    in_dash && /^[^[:space:]#]/ { in_dash = 0 }
    in_dash && $1 == "port:" { print $2; exit }
  ' "$1"
}

if need_file "$DEPLOY_CONF"; then
  deploy_port="$(dashboard_port "${ROOT}/${DEPLOY_CONF}")"
  if [[ -z "$deploy_port" ]]; then
    fail "${DEPLOY_CONF}: no dashboard.port found"
  elif [[ -n "$health_port" && "$deploy_port" != "$health_port" ]]; then
    fail "${DEPLOY_CONF}: dashboard.port is ${deploy_port} but ${UNIT} probes ${health_port}"
  else
    ok
  fi
fi

# --- 4. The Compose healthcheck, so all three container paths agree --------

if need_file "$COMPOSE"; then
  # Scoped to the `hive:` service on purpose: the `gateway:` service probes
  # /api/health too, on its own published port, and matching that one instead
  # would make this assertion agree with the wrong thing.
  compose_port="$(
    awk '
      /^  [a-zA-Z0-9_-]+:$/ { svc = $1; sub(/:$/, "", svc) }
      svc == "hive" && /127\.0\.0\.1:[0-9]+\/api\/health/ {
        match($0, /127\.0\.0\.1:[0-9]+/)
        port = substr($0, RSTART, RLENGTH)
        sub(/.*:/, "", port)
        print port
        exit
      }
    ' "${ROOT}/${COMPOSE}"
  )"
  if [[ -z "$compose_port" ]]; then
    fail "${COMPOSE}: no /api/health healthcheck found for the hive service"
  elif [[ -n "$health_port" && "$compose_port" != "$health_port" ]]; then
    fail "${COMPOSE}: the hive healthcheck probes ${compose_port} but ${UNIT} probes ${health_port}"
  else
    ok
  fi
fi

# --- 5. The unit names the config key it is coupled to ---------------------

if [[ -f "${ROOT}/${UNIT}" ]]; then
  if grep -q 'dashboard\.port' "${ROOT}/${UNIT}"; then
    ok
  else
    fail "${UNIT}: the HealthCmd comment no longer names dashboard.port. The coupling is invisible from the unit again."
  fi
fi

# --- 6. Every repo path the doc tells an operator to copy is TRACKED -------
#
# This is the #4367 defect itself: the install step said `cp src/hive.yaml`,
# and src/hive.yaml is gitignored — it is the file the operator creates, not
# one the repo ships.
#
# Tracked, not merely present on disk. A contributor who has followed the
# README has an untracked src/hive.yaml sitting in their checkout, so a plain
# existence test passes for them and fails for the operator who cloned the
# repo and did nothing else — which is precisely the reader this step is for.

tracked_in_repo() {
  git -C "$ROOT" ls-files --error-unmatch -- "$1" >/dev/null 2>&1
}

if need_file "$DOC"; then
  if ! git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    printf 'SKIP: not a git checkout, cannot tell tracked files from local ones\n'
  else
    doc_paths="$(
      grep -oE '(cp|install)[[:space:]]+(-Dm644[[:space:]]+)?src/[A-Za-z0-9._/-]+' "${ROOT}/${DOC}" \
        | grep -oE 'src/[A-Za-z0-9._/-]+' \
        | sort -u
    )"
    missing=0
    while IFS= read -r doc_path; do
      [[ -z "$doc_path" ]] && continue
      if ! tracked_in_repo "$doc_path"; then
        fail "${DOC}: tells the operator to copy ${doc_path}, which the repo does not ship (untracked or absent)"
        missing=1
      fi
    done <<<"$doc_paths"
    [[ "$missing" -eq 0 ]] && ok
  fi
fi

# --- 7. The doc does not hand the operator the example's port unchanged ----
#
# src/hive.yaml.example ships 3001 for source runs and is the file an operator
# reaches for. If the doc installs it, it must also change the port, or the
# start hangs for the full TimeoutStartSec.

if [[ -f "${ROOT}/${DOC}" && -f "${ROOT}/${EXAMPLE_CONF}" && -n "$health_port" ]]; then
  example_port="$(dashboard_port "${ROOT}/${EXAMPLE_CONF}")"
  if [[ -z "$example_port" ]]; then
    fail "${EXAMPLE_CONF}: no dashboard.port found"
  elif [[ "$example_port" == "$health_port" ]]; then
    # The example agrees with the unit; no correction is owed by the doc.
    ok
  elif grep -q "${EXAMPLE_CONF}" "${ROOT}/${DOC}"; then
    if grep -qE "port: ${health_port}" "${ROOT}/${DOC}"; then
      ok
    else
      fail "${DOC}: installs ${EXAMPLE_CONF} (dashboard.port ${example_port}) but never sets port ${health_port}. Following it literally hangs the start for TimeoutStartSec."
    fi
  else
    ok
  fi
fi

# --- Summary ----------------------------------------------------------------

if [[ "$failures" -gt 0 ]]; then
  printf '\nFAILED: %d assertion(s) — see src/docs/podman-standalone-quadlet.md (#4367)\n' "$failures" >&2
  exit 1
fi

printf 'PASS: %d Quadlet config-coupling assertions (health port %s)\n' "$pass_count" "${health_port:-?}"
