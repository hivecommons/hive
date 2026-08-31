#!/usr/bin/env bash
# Contract test for the Quadlet unit's config coupling (#4367).
# Run: bash src/deploy/test_quadlet_config_contract.sh
#
# The unit's HealthCmd= probes two ports, and both are coupled to something the
# unit cannot see: the Go API port lives in the operator's hive.yaml (#4367) and
# the auth-proxy port is what nginx.conf dials (#4476).
#
# The first coupling is the original one:
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

# --- 1. The ports the unit probes ------------------------------------------
#
# There are TWO, and there is an order to them: the Go API first, the auth proxy
# second (#4476). `health_port` stays the API port, because that is the one
# sections 2-4 and 7 couple to dashboard.port; the proxy port has its own
# sources of truth and is checked in section 8.

health_ports=""
health_port=""
proxy_health_port=""
if need_file "$UNIT"; then
  health_ports="$(
    grep -E '^HealthCmd=' "${ROOT}/${UNIT}" \
      | grep -oE '127\.0\.0\.1:[0-9]+' \
      | cut -d: -f2
  )"
  health_port="$(printf '%s\n' "$health_ports" | sed -n '1p')"
  proxy_health_port="$(printf '%s\n' "$health_ports" | sed -n '2p')"
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
  # EVERY port on the line, in order — one probe per runtime is no longer the
  # shape. If Compose probed the API and the unit probed both, "healthy" would
  # mean two different things on the two runtimes, which is exactly what the
  # comment in each file claims it does not.
  compose_ports="$(
    awk '
      /^  [a-zA-Z0-9_-]+:$/ { svc = $1; sub(/:$/, "", svc) }
      svc == "hive" && /127\.0\.0\.1:[0-9]+\/api\/health/ {
        line = $0
        while (match(line, /127\.0\.0\.1:[0-9]+/)) {
          port = substr(line, RSTART, RLENGTH)
          sub(/.*:/, "", port)
          print port
          line = substr(line, RSTART + RLENGTH)
        }
        exit
      }
    ' "${ROOT}/${COMPOSE}"
  )"
  if [[ -z "$compose_ports" ]]; then
    fail "${COMPOSE}: no /api/health healthcheck found for the hive service"
  elif [[ -n "$health_ports" && "$(echo $compose_ports)" != "$(echo $health_ports)" ]]; then
    fail "${COMPOSE}: the hive healthcheck probes [$(echo $compose_ports)] but ${UNIT} probes [$(echo $health_ports)]. \"healthy\" must mean the same thing on both runtimes."
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

# --- 8. The auth proxy listener is probed too (#4476) -----------------------
#
# The container serves on two ports and the unit used to certify one of them.
# 3002 is the Go API. 3001 is the Node auth proxy, and src/deploy/nginx.conf
# points `upstream hive_api` at it, so 3001 is what the gateway and therefore
# every operator actually reaches — 3d1e6077 moved the upstream off the Go API
# deliberately so nothing can skip the proxy.
#
# Measured consequence of probing only 3002: with HIVE_DASHBOARD_TOKEN absent
# from hive.env the proxy fails closed and never binds 3001 while 3002 answers
# 200 throughout, so hive.service reported active and its container healthy for
# the entire two minutes the deployment was unusable. The only red was
# hive-gateway.service, 120s later, as an nginx connection-refused naming
# neither the port nor the variable.
#
# Asserted against each source of truth rather than against the literal 3001,
# the same way sections 2-4 treat the API port.

PROXY_SRC="src/proxy/server.js"
NGINX_CONF="src/deploy/nginx.conf"

if [[ -f "${ROOT}/${UNIT}" ]]; then
  if [[ -z "$proxy_health_port" ]]; then
    fail "${UNIT}: HealthCmd probes ${health_port:-?} and nothing else. The auth proxy on the port nginx.conf dials is unprobed, so the unit reports healthy while the dashboard is dead (#4476)."
  elif [[ "$proxy_health_port" == "$health_port" ]]; then
    fail "${UNIT}: HealthCmd probes ${health_port} twice. The two listeners are different ports."
  else
    ok
  fi
fi

if need_file "$PROXY_SRC"; then
  proxy_default="$(
    sed -n "s/.*HIVE_PROXY_PORT[[:space:]]*||[[:space:]]*'\([0-9]\{1,\}\)'.*/\1/p" \
      "${ROOT}/${PROXY_SRC}" | head -n1
  )"
  if [[ -z "$proxy_default" ]]; then
    fail "${PROXY_SRC}: could not read the HIVE_PROXY_PORT default"
  elif [[ -n "$proxy_health_port" && "$proxy_default" != "$proxy_health_port" ]]; then
    fail "${PROXY_SRC}: the proxy defaults to ${proxy_default} but ${UNIT} probes ${proxy_health_port}. An operator who does not set HIVE_PROXY_PORT would never reach healthy."
  else
    ok
  fi
fi

if need_file "$NGINX_CONF"; then
  nginx_upstream_port="$(
    awk '
      /upstream[[:space:]]+hive_api/ { in_up = 1 }
      in_up && match($0, /hive:[0-9]+/) {
        port = substr($0, RSTART, RLENGTH)
        sub(/.*:/, "", port)
        print port
        exit
      }
    ' "${ROOT}/${NGINX_CONF}"
  )"
  if [[ -z "$nginx_upstream_port" ]]; then
    fail "${NGINX_CONF}: no \`server hive:<port>\` inside upstream hive_api"
  elif [[ -n "$proxy_health_port" && "$nginx_upstream_port" != "$proxy_health_port" ]]; then
    fail "${NGINX_CONF}: the gateway dials hive:${nginx_upstream_port} but ${UNIT} probes ${proxy_health_port}. The probe is not watching the port the gateway needs."
  else
    ok
  fi
fi

# --- 9. The unit names the second coupling too ------------------------------

if [[ -f "${ROOT}/${UNIT}" ]]; then
  if grep -q 'HIVE_PROXY_PORT' "${ROOT}/${UNIT}"; then
    ok
  else
    fail "${UNIT}: the HealthCmd comment no longer names HIVE_PROXY_PORT. The proxy-port coupling is invisible from the unit."
  fi
fi

# --- 10. hive pulls the gateway up, not just down (#4516) -------------------
#
# hive-gateway.container carries Requires=hive.service, which propagates a STOP
# only. Without a start-direction dependency, any path that restarts hive while
# the gateway is already down leaves :3001 -- the sole published port -- dead.
# `podman auto-update --rollback` is exactly that path and runs unattended, so
# the coupling has to live on the unit rather than in a caller.

GATEWAY_UNIT="src/deploy/quadlet/hive-gateway.container"

if [[ -f "${ROOT}/${UNIT}" ]]; then
  if grep -qE '^Wants=hive-gateway\.service[[:space:]]*$' "${ROOT}/${UNIT}"; then
    ok
  else
    fail "${UNIT}: no 'Wants=hive-gateway.service'. Requires= on the gateway propagates a stop only, so a restart of hive with the gateway already down leaves the published port dead (#4516)."
  fi
fi

# Wants= must NOT be paired with an ordering key here: hive-gateway.container's
# own After=hive.service is what sequences them. An After= in this direction
# would invert that and deadlock the pair.
if [[ -f "${ROOT}/${UNIT}" ]]; then
  if grep -qE '^(After|Before)=hive-gateway\.service' "${ROOT}/${UNIT}"; then
    fail "${UNIT}: orders itself against hive-gateway.service. The gateway's own After=hive.service owns that ordering; adding the reverse here risks a cycle (#4516)."
  else
    ok
  fi
fi

# And the gateway must still carry the stop-direction half, or stopping hive
# would leave an orphaned gateway serving 502s.
#
# need_file, not a bare `[[ -f ]]`: GATEWAY_UNIT is not covered by need_file
# anywhere else in this file (UNIT is, in sections 1-4), so a bare existence
# test made this the one section that could vanish silently. Deleting
# hive-gateway.container dropped the run from 14 assertions to 13 and still
# exited 0 — the guard reported PASS for a repo with no gateway unit at all
# (#5388). need_file turns that into a failure.
if need_file "${GATEWAY_UNIT}"; then
  if grep -qE '^Requires=hive\.service[[:space:]]*$' "${ROOT}/${GATEWAY_UNIT}" \
     && grep -qE '^After=hive\.service[[:space:]]*$' "${ROOT}/${GATEWAY_UNIT}"; then
    ok
  else
    fail "${GATEWAY_UNIT}: lost Requires=/After=hive.service. Wants= in hive.container only pulls the gateway UP; these are what take it down and order it (#4516)."
  fi
fi

# --- Summary ----------------------------------------------------------------

if [[ "$failures" -gt 0 ]]; then
  printf '\nFAILED: %d assertion(s) — see src/docs/podman-standalone-quadlet.md (#4367)\n' "$failures" >&2
  exit 1
fi

printf 'PASS: %d Quadlet config-coupling assertions (health ports %s)\n' \
  "$pass_count" "$(echo ${health_ports:-?})"
