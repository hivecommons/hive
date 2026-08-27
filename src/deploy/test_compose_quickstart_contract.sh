#!/usr/bin/env bash
# The documented Docker Compose quick start must actually work (#4477).
# Run: bash src/deploy/test_compose_quickstart_contract.sh
#
# WHY THIS EXISTS
# ---------------
# The README's default quick start was broken twice over, on its own terms, and
# both faults were invisible to every existing gate:
#
#   1. It wrote `.env` at the repository root. `-f src/docker-compose.yaml` sets
#      the project directory to `src/`, and that is where Compose reads `.env`
#      from -- the same place the compose file's own `./hive.yaml` and
#      `./secrets` mounts already resolve against. The root file was read by
#      nothing. Both paths are gitignored, so neither git nor Compose ever said
#      so: the operator pasted a real PAT into an ignored file, the hive came up,
#      and every GitHub call 401'd -- which reads like a bad token, not an unread
#      file.
#
#   2. It never set HIVE_DASHBOARD_TOKEN. The dashboard's auth proxy refuses to
#      start without one (src/proxy/server.js), and `nginx.conf` points the
#      gateway at that proxy deliberately (3d1e6077), so `http://localhost:3001`
#      -- the address the quick start advertises -- served 503 from an upstream
#      that never bound.
#
# Neither is visible to a reader, both are mechanical, and the repo already
# knows the answer to both: `bin/hive-setup.sh` passes `--env-file` explicitly
# AND generates a dashboard token, and the PODMAN quick start in the same README
# sets the token as a numbered step. Only the default Compose path, which is the
# first thing a new operator runs, omitted them.
#
# WHAT IS ASSERTED, AND AGAINST WHAT
# ----------------------------------
# Nothing here hardcodes a variable name. The required set is read from
# src/deploy/quadlet/hive.env.example, which is the tracked environment contract
# for BOTH runtimes and already marks HIVE_DASHBOARD_TOKEN as REQUIRED in prose.
# test_standalone_runtime_parity.sh holds that file's names equal to the compose
# file's, so a variable that becomes required on one runtime cannot quietly not
# be required on the other.
#
# The documents under test are DISCOVERED, not listed: any file carrying a
# `docker compose -f src/docker-compose.yaml` recipe is covered, so a fourth
# copy of the quick start is gated the day it is written. There were three.
#
# Runs without starting a container and without Docker installed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENV_CONTRACT="src/deploy/quadlet/hive.env.example"
COMPOSE_REL="src/docker-compose.yaml"

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== the documented Compose quick start (#4477) ==="

# --- 1. The required set, read from the contract rather than restated --------
#
# A variable whose own comment block says REQUIRED. The block is the run of
# comment lines above the assignment, reset at each blank line, so the file's
# header cannot leak into the first variable.

if [ ! -f "${ROOT}/${ENV_CONTRACT}" ]; then
  fail "${ENV_CONTRACT} exists" "not found -- the required set has no source of truth"
  echo; echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
fi

REQUIRED_VARS="$(
  awk '
    /^[[:space:]]*$/            { block = ""; next }
    /^#?[A-Z][A-Z0-9_]*=/       {
                                   name = $0
                                   sub(/^#/, "", name)
                                   sub(/=.*/, "", name)
                                   if (block ~ /REQUIRED/) print name
                                   block = ""
                                   next
                                }
    /^#/                        { block = block " " $0; next }
  ' "${ROOT}/${ENV_CONTRACT}"
)"

if [ -z "$REQUIRED_VARS" ]; then
  fail "${ENV_CONTRACT} marks at least one variable REQUIRED" \
       "parsed none -- either the contract changed shape or this parser is broken, and a green run would mean nothing"
  echo; echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
fi
pass "required variables read from ${ENV_CONTRACT}: $(echo $REQUIRED_VARS)"

# --- 2. The documents that carry a quick start ------------------------------

DOCS="$(
  git -C "$ROOT" grep -l -F -e "docker compose -f ${COMPOSE_REL}" -- '*.md' 2>/dev/null \
    || grep -rl -F "docker compose -f ${COMPOSE_REL}" --include='*.md' "$ROOT" 2>/dev/null
)"

if [ -z "$DOCS" ]; then
  fail "at least one document runs \`docker compose -f ${COMPOSE_REL}\`" \
       "found none -- the quick start has moved and this gate is watching nothing"
  echo; echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
fi
pass "$(printf '%s\n' "$DOCS" | wc -l) document(s) carry a Compose recipe"

# --- 3. Each recipe block, checked where it is written ----------------------
#
# Scoped to fenced code blocks, not whole files. The README also documents the
# PODMAN path, which writes a different env file at a different path, and a
# file-wide grep would confuse the two.
#
# An INSTALL block is one that runs Compose and bootstraps the config from
# `hive.yaml.example`. That is what separates a quick start from an operational
# recipe: `docker compose ... up -d` after restoring a volume, or after a
# `down -v`, is a restart of a stack whose env file already exists, and holding
# it to "you must also mint a token" would be wrong. An install block must write
# an env file and must set every required variable.
#
# The env-file PATH check is wider than that: ANY block that writes a .env
# beside a Compose command is checked, install or not, because the fault was
# writing it where Compose does not read.

check_doc() {
  local doc="$1" rel
  rel="${doc#"${ROOT}/"}"

  local report
  report="$(
    REQUIRED_VARS="$REQUIRED_VARS" COMPOSE_REL="$COMPOSE_REL" awk '
      function evaluate(   i, n, lines, line, target, targets, ntargets,
                           has_compose, is_install, stated) {
        has_compose = (index(body, compose) > 0)
        if (!has_compose) return
        is_install = (index(body, "hive.yaml.example") > 0)
        stated = (index(body, "--project-directory") > 0 || index(body, "--env-file") > 0)

        ntargets = 0
        n = split(body, lines, "\n")
        for (i = 1; i <= n; i++) {
          line = lines[i]
          if (match(line, />>?[[:space:]]*(\.\/)?[A-Za-z0-9_.\/-]*\.env([[:space:]]|$)/)) {
            target = substr(line, RSTART, RLENGTH)
            sub(/^>>?[[:space:]]*/, "", target)
            sub(/^\.\//, "", target)
            gsub(/[[:space:]]/, "", target)
            targets[++ntargets] = target
            if (!stated && target != "src/.env") printf "PATH %d %s\n", start, target
          }
        }

        if (!is_install) {
          if (ntargets > 0) checked++
          return
        }
        checked++
        if (ntargets == 0) { printf "NOENV %d -\n", start; return }
        for (i in req) {
          if (req[i] == "") continue
          if (body !~ (req[i] "[[:space:]]*=")) printf "VAR %d %s\n", start, req[i]
        }
      }
      BEGIN {
        split(ENVIRON["REQUIRED_VARS"], req, /[[:space:]]+/)
        compose = "docker compose -f " ENVIRON["COMPOSE_REL"]
        in_block = 0; checked = 0
      }
      /^[[:space:]]*```/ {
        if (in_block) { evaluate(); in_block = 0 }
        else { in_block = 1; body = ""; start = NR }
        next
      }
      in_block { body = body $0 "\n"; next }
      END { printf "CHECKED %d -\n", checked }
    ' "$doc"
  )"

  local checked=0 findings=0
  while IFS=' ' read -r kind line detail; do
    [ -z "$kind" ] && continue
    case "$kind" in
      CHECKED) checked="$line" ;;
      PATH)
        findings=$((findings + 1))
        fail "${rel}: the env file is written where Compose reads it" \
             "block at line ${line} writes \`${detail}\`; \`-f ${COMPOSE_REL}\` makes src/ the project directory, so Compose reads src/.env and never that. Use src/.env, or state --project-directory." ;;
      VAR)
        findings=$((findings + 1))
        fail "${rel}: the install recipe sets ${detail}" \
             "block at line ${line} bootstraps a stack without it. ${ENV_CONTRACT} marks it REQUIRED: the deployment does not start without it." ;;
      NOENV)
        findings=$((findings + 1))
        fail "${rel}: the install recipe writes an env file" \
             "block at line ${line} bootstraps a stack and never writes one, so nothing sets the required variables." ;;
    esac
  done <<<"$report"

  if [ "$findings" -gt 0 ]; then return; fi
  if [ "$checked" -gt 0 ]; then
    pass "${rel}: ${checked} recipe block(s) write src/.env and set every required variable"
  else
    echo "  note: ${rel}: Compose commands only, no install recipe and no env file written"
  fi
}

while IFS= read -r doc; do
  [ -z "$doc" ] && continue
  case "$doc" in /*) ;; *) doc="${ROOT}/${doc}" ;; esac
  check_doc "$doc"
done <<<"$DOCS"

# --- 4. A required variable must not carry a default in the compose file -----
#
# The obvious way to silence Compose's "variable is not set" warning is to write
# `${HIVE_DASHBOARD_TOKEN:-}`. That would substitute an EMPTY token, which the
# auth proxy treats exactly as an absent one -- it refuses to start either way
# -- with the warning that was the only remaining evidence now gone. The two
# genuinely optional variables carry `:-` for that reason and the tokens must
# not.

if [ -f "${ROOT}/${COMPOSE_REL}" ]; then
  defaulted=""
  for var in $REQUIRED_VARS; do
    if grep -qE "\\$\\{${var}:-" "${ROOT}/${COMPOSE_REL}"; then
      defaulted="${defaulted} ${var}"
    fi
  done
  if [ -n "$defaulted" ]; then
    fail "${COMPOSE_REL}: no required variable carries a \`:-\` default" \
         "${defaulted# } would substitute empty and start without the warning that says so"
  else
    pass "${COMPOSE_REL}: required variables have no default, so an unset one still warns"
  fi
fi

# --- 5. The documented path stays untracked ---------------------------------
#
# It holds a PAT. Asserted by asking git, not by reading .gitignore: the answer
# that matters is whether git would take the file, and more than one ignore file
# can decide that.

if git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  if git -C "$ROOT" check-ignore -q src/.env; then
    pass "src/.env is ignored by git -- the documented path cannot be committed"
  else
    fail "src/.env is ignored by git" \
         "the quick start tells the operator to put a PAT there and git would take it"
  fi
else
  echo "  SKIP: not a git checkout, cannot ask git whether src/.env is ignored"
fi

echo
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
