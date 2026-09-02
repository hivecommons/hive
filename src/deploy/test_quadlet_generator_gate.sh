#!/usr/bin/env bash
# Quadlet generator syntax gate (kubestellar/hive#4338).
#
# ADR-0017 chose Quadlet `.container`/`.pod` units as the Podman persistent
# lifecycle. A unit that fails to generate should fail CI, not surface at
# deploy time — so this gate runs the Podman systemd generator over whatever
# Quadlet units the repository holds, in BOTH modes the ADR installs into:
# rootful (system manager search path) and `--user` (per-user manager).
#
# WHAT THIS GATE IS NOT. `quadlet --dryrun` is a syntax and reference check,
# never a semantic one. ADR-0017 records three things it exits 0 on:
# `Notify=healthy` with no `HealthCmd`, `PublishPort=` on a pod member, and a
# short-name image. The first two need Hive's own assertions and are out of
# this gate's scope; the third IS caught here, because the generator emits a
# `Warning:` for it even while exiting 0 — see the warning rule below.
#
# THE TRAP THIS GATE IS BUILT AROUND (#4211). The generator is a separate
# binary from `podman`, and a runner can have podman without it — the hosted
# GitHub runners are exactly that case, because they carry a hand-installed
# /usr/local/bin/podman rather than the distribution package. A gate that
# merely skips when the generator is absent reports green while testing
# nothing. So there is NO skip path in this script: a missing generator is a
# failure, not a skip.
#
# EXIT STATUS IS NOT THE ONLY SIGNAL. The generator exits 0 while printing
# `Warning: ... not a fully qualified image name`, and ADR-0017 requires fully
# qualified references because `podman auto-update` cannot resolve a short
# name — a deploy-time failure the exit status would wave through. This gate
# therefore fails on ANY generator diagnostic, not just a non-zero exit: after
# the always-present "Loading source unit file" debug lines are filtered out,
# `--dryrun` writes to stderr only when it has something to complain about.
# The rule is deliberately conservative in the failing direction; a new benign
# stderr line upstream would show up as a red build rather than as silently
# reduced coverage, which is the correct way round for a gate.
#
# NO UNITS YET. At the time of writing the repository holds no Quadlet units —
# the deployment asset is a separate slice downstream of ADR-0017. Per #4338's
# stop condition the gate then covers the generator's PRESENCE and DRY-RUN
# CAPABILITY only, and says so out loud rather than reporting a vacuous pass.
# The self-tests below are what make that meaningful: they run known-good and
# known-bad units through the same code path the real units will take, so the
# gate proves it can FAIL today, before there is anything real to catch. When
# units land anywhere in the tree, they are picked up with no change here.
#
# Usage:
#   src/deploy/test_quadlet_generator_gate.sh [options]
#
#   --quadlet PATH  generator binary (default: search the usual locations)
#   --units DIR     gate this directory instead of scanning the repository
#
# Exit codes: 0 the gate passed, 1 the gate failed. There is no skip code.

set -uo pipefail

QUADLET_BIN="${QUADLET_BIN:-}"
UNITS_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --quadlet) QUADLET_BIN="${2:?--quadlet needs a value}"; shift 2 ;;
    --units)   UNITS_DIR="${2:?--units needs a value}"; shift 2 ;;
    -h|--help) sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'ERROR: unknown argument %q\n' "$1" >&2; exit 1 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d)"
# shellcheck disable=SC2329 # invoked through the EXIT trap below
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

failures=0
fail() { printf '  FAIL: %s\n' "$1"; [[ $# -gt 1 ]] && printf '        %s\n' "$2"; failures=$((failures + 1)); }
pass() { printf '  PASS: %s\n' "$1"; }

printf '=== Quadlet generator syntax gate (#4338) ===\n\n'

# ── 1. The generator must be present. A missing one fails. ──────────────────
printf -- '--- generator ---\n'
if [[ -z "$QUADLET_BIN" ]]; then
  for candidate in \
    /usr/libexec/podman/quadlet \
    /usr/lib/podman/quadlet \
    /usr/lib/systemd/system-generators/podman-system-generator \
    /usr/local/libexec/podman/quadlet; do
    if [[ -x "$candidate" ]]; then QUADLET_BIN="$candidate"; break; fi
  done
fi
if [[ -z "$QUADLET_BIN" ]] && command -v quadlet >/dev/null 2>&1; then
  QUADLET_BIN="$(command -v quadlet)"
fi

if [[ -z "$QUADLET_BIN" || ! -x "$QUADLET_BIN" ]]; then
  fail "the Quadlet generator is installed" \
       "not found in the usual locations. #4211: podman can be present without it — the runner image carries a hand-installed podman and no generator. A gate that skips here tests nothing, so this is a failure."
  printf '\nSUMMARY: %d failure(s)\n' "$failures"
  exit 1
fi
pass "generator found at ${QUADLET_BIN}"

version="$("$QUADLET_BIN" -version 2>/dev/null | tr -d '[:space:]')"
if [[ -z "$version" ]]; then
  fail "generator reports a version" "\`${QUADLET_BIN} -version\` printed nothing"
else
  pass "generator version ${version}"
fi

# ADR-0017's binding constraint: `.pod` units and `Notify=healthy` both first
# shipped in 5.0.0, so a pod-based layout with a readiness gate is not
# expressible below it. 5.6.0 is the ADR's recommended floor (the #26105
# wrong-`--pod`-name fix); below that is reported, not failed, because Hive's
# layout sets PodName= explicitly and is outside that regression window.
q_major="${version%%.*}"; q_rest="${version#*.}"; q_minor="${q_rest%%.*}"
if [[ "${q_major:-0}" =~ ^[0-9]+$ ]]; then
  if (( q_major < 5 )); then
    fail "generator meets ADR-0017's 5.0.0 requirement" \
         "found ${version}; .pod units and Notify=healthy both need 5.0.0, so this generator cannot express the chosen layout"
  else
    pass "generator meets ADR-0017's 5.0.0 requirement"
    if (( q_major == 5 && ${q_minor:-0} < 6 )); then
      printf '  NOTE: %s is below ADR-0017'"'"'s recommended 5.6.0 floor (podman#26105).\n' "$version"
    fi
  fi
fi

# ── 2. Dry-run capability, in both modes, proven able to fail ───────────────
# Every fixture is generated here rather than committed: #4338's boundary is a
# syntax gate and explicitly not a deployment unit, and a unit file living in
# the tree is exactly the thing that could later be mistaken for one.
mkfixture() {
  local name="$1"; shift
  local dir="${WORK}/fx-${name}"
  mkdir -p "$dir"
  cat >"${dir}/gate-fixture.container"
  printf '%s' "$dir"
}

# Sets RC and DIAG (the generator's stderr with its unconditional
# "Loading source unit file" debug lines removed).
run_quadlet() {
  local mode="$1" dirs="$2"
  local errf="${WORK}/stderr"
  if [[ "$mode" == "user" ]]; then
    QUADLET_UNIT_DIRS="$dirs" "$QUADLET_BIN" --user --dryrun >"${WORK}/stdout" 2>"$errf"
  else
    QUADLET_UNIT_DIRS="$dirs" "$QUADLET_BIN" --dryrun >"${WORK}/stdout" 2>"$errf"
  fi
  RC=$?
  DIAG="$(grep -v 'Loading source unit file' "$errf" | grep -v '^[[:space:]]*$' || true)"
}

# A unit is clean only when the generator both exits 0 AND says nothing.
quadlet_clean() { [[ "$RC" -eq 0 && -z "$DIAG" ]]; }

good_dir="$(mkfixture good <<'EOF'
[Unit]
Description=Quadlet gate fixture — known good

[Container]
Image=ghcr.io/hivecommons/hive:v4-latest
ContainerName=hive-quadlet-gate-fixture
PublishPort=127.0.0.1:13001:3001

[Install]
WantedBy=default.target
EOF
)"

# Rejected outright by the generator (non-zero exit).
badkey_dir="$(mkfixture unsupported-key <<'EOF'
[Container]
Image=ghcr.io/hivecommons/hive:v4-latest
ThisKeyDoesNotExist=true
EOF
)"

dangling_dir="$(mkfixture dangling-pod <<'EOF'
[Container]
Image=ghcr.io/hivecommons/hive:v4-latest
Pod=no-such-unit.pod
EOF
)"

# The one that matters most: the generator exits 0 here and only WARNS. If this
# gate ever regressed to trusting the exit status, this is the case that would
# start sailing through — and a short name is what breaks `podman auto-update`,
# which ADR-0017 depends on.
shortname_dir="$(mkfixture short-name <<'EOF'
[Container]
Image=hive:v4-latest
EOF
)"

for mode in system user; do
  printf -- '\n--- dry-run capability: %s mode ---\n' "$mode"

  run_quadlet "$mode" "$good_dir"
  if quadlet_clean; then
    pass "${mode}: a well-formed unit generates cleanly"
  else
    fail "${mode}: a well-formed unit generates cleanly" \
         "exit ${RC}${DIAG:+; }${DIAG}"
  fi

  # The generated unit must actually be a systemd service, not empty output.
  if ! grep -q '^ExecStart=' "${WORK}/stdout"; then
    fail "${mode}: the dry-run emitted a service unit" \
         "no ExecStart= in the generated output — the generator ran but produced nothing usable"
  else
    pass "${mode}: the dry-run emitted a service unit with an ExecStart"
  fi

  run_quadlet "$mode" "$badkey_dir"
  if quadlet_clean; then
    fail "${mode}: an unsupported key is rejected" \
         "the generator accepted ThisKeyDoesNotExist= — this gate cannot catch a malformed unit"
  else
    pass "${mode}: an unsupported key is rejected (exit ${RC})"
  fi

  run_quadlet "$mode" "$dangling_dir"
  if quadlet_clean; then
    fail "${mode}: a dangling .pod reference is rejected" \
         "the generator accepted Pod=no-such-unit.pod — a unit that cannot start would reach deploy"
  else
    pass "${mode}: a dangling .pod reference is rejected (exit ${RC})"
  fi

  run_quadlet "$mode" "$shortname_dir"
  if [[ "$RC" -eq 0 && -z "$DIAG" ]]; then
    fail "${mode}: a short-name image is caught" \
         "the generator neither failed nor warned. ADR-0017 requires fully qualified references for podman auto-update, so this gate would let a deploy-time failure through."
  elif [[ "$RC" -eq 0 ]]; then
    pass "${mode}: a short-name image is caught by the warning rule, not the exit status"
  else
    pass "${mode}: a short-name image is rejected (exit ${RC})"
  fi
done

# ── 3. The repository's own Quadlet units ───────────────────────────────────
printf -- '\n--- repository units ---\n'
if [[ -n "$UNITS_DIR" ]]; then
  unit_dirs="$UNITS_DIR"
  mapfile -t units < <(find "$UNITS_DIR" -maxdepth 1 -type f \
    \( -name '*.container' -o -name '*.pod' -o -name '*.volume' \
       -o -name '*.network' -o -name '*.kube' -o -name '*.build' \) | sort)
else
  # Scanned repo-wide on purpose. Pinning one directory would mean units that
  # land somewhere else are silently ungated — the same vacuous-pass failure
  # mode as skipping on a missing generator.
  mapfile -t units < <(find "$REPO_ROOT" \
    \( -name .git -o -name node_modules -o -name vendor \) -prune -o \
    -type f \( -name '*.container' -o -name '*.pod' -o -name '*.volume' \
       -o -name '*.network' -o -name '*.kube' -o -name '*.build' \) -print | sort)
  unit_dirs=""
  for u in "${units[@]}"; do
    d="$(dirname "$u")"
    case ":${unit_dirs}:" in *":${d}:"*) ;; *) unit_dirs="${unit_dirs:+${unit_dirs}:}${d}" ;; esac
  done
fi

if [[ "${#units[@]}" -eq 0 ]]; then
  printf '  none found.\n'
  printf '  This gate therefore covers the generator'"'"'s PRESENCE and DRY-RUN CAPABILITY\n'
  printf '  only — #4338'"'"'s stop condition — and not any Hive unit, because the\n'
  printf '  deployment asset is a separate slice downstream of ADR-0017. The\n'
  printf '  self-tests above are what keep that from being a vacuous pass.\n'
else
  printf '  %d unit(s) found:\n' "${#units[@]}"
  printf '    %s\n' "${units[@]#"${REPO_ROOT}/"}"
  # All unit directories are passed together so a .container joining a .pod
  # resolves its reference the way it will on a real host.
  for mode in system user; do
    run_quadlet "$mode" "$unit_dirs"
    if quadlet_clean; then
      pass "${mode}: every repository unit generates cleanly"
    else
      fail "${mode}: every repository unit generates cleanly" \
           "exit ${RC}${DIAG:+; }${DIAG}"
    fi
  done
fi

printf '\nSUMMARY: %d failure(s)\n' "$failures"
[[ "$failures" -eq 0 ]] || exit 1
exit 0
