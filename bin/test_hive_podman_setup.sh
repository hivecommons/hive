#!/usr/bin/env bash
# Contract tests for bin/hive-podman-setup.sh (#4470).
# Run: bash bin/test_hive_podman_setup.sh
#
# The installer is the one script in the Podman family that WRITES to the host,
# so the questions this suite has to answer are not "does it install" but:
#
#   * does it run the preflights in a state where they check anything at all
#     (they exit 0 having checked NOTHING unless HIVE_DEPLOY_RUNTIME=podman —
#     the most expensive way to be reassured by an installer);
#   * does the port it writes come from the unit that will enforce it, rather
#     than from a constant that can drift (#4367);
#   * does it use the RIGHT secrets command for the root mode (#4359);
#   * can a second run destroy an operator's edited config or their keys;
#   * does it ever claim healthy without evidence;
#   * does it touch a package manager, clone anything, or install a unit that
#     names a container socket.
#
# Every input is mocked. No podman, no systemd, no privileges, and no network:
# the fakes record what they were asked to do, and apt-get/dnf/git/docker sit on
# the test PATH as tripwires that fail the run if the installer ever calls them.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SETUP="${ROOT}/bin/hive-podman-setup.sh"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/fakebin"
FIXTURE="${TEST_TMP}/fixture"
mkdir -p "$FAKE_BIN"

pass_count=0
fail_count=0
CASE=""

fail() {
  printf '  FAIL: [%s] %s\n' "$CASE" "$1" >&2
  fail_count=$((fail_count + 1))
}
pass() {
  printf '  ok: [%s] %s\n' "$CASE" "$1"
  pass_count=$((pass_count + 1))
}

assert_eq() {
  local want="$1" got="$2" ctx="$3"
  if [[ "$got" == "$want" ]]; then pass "$ctx"; else fail "${ctx}: got '${got}', want '${want}'"; fi
}
assert_contains() {
  local hay="$1" needle="$2" ctx="$3"
  if [[ "$hay" == *"$needle"* ]]; then pass "$ctx"; else fail "${ctx}: missing '${needle}'"; fi
}
assert_lacks() {
  local hay="$1" needle="$2" ctx="$3"
  if [[ "$hay" != *"$needle"* ]]; then pass "$ctx"; else fail "${ctx}: unexpectedly found '${needle}'"; fi
}
assert_file_contains() {
  local file="$1" needle="$2" ctx="$3"
  if [[ -f "$file" ]] && grep -qF -- "$needle" "$file"; then
    pass "$ctx"
  else
    fail "${ctx}: '${needle}' not in ${file}"
  fi
}

# --- fakes ------------------------------------------------------------------

# podman: records every call. `unshare chown` and `pull` succeed without doing
# anything — the point of the assertion is WHICH command was chosen, not its
# effect, and an unprivileged test cannot perform either for real.
cat >"${FAKE_BIN}/podman" <<'FAKE'
#!/usr/bin/env bash
printf 'podman %s\n' "$*" >>"$FAKE_CALL_LOG"
exit 0
FAKE

# systemctl: records, and answers is-active from FAKE_ACTIVE_STATE so the
# "started is not healthy" case can be driven.
cat >"${FAKE_BIN}/systemctl" <<'FAKE'
#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >>"$FAKE_CALL_LOG"
args=()
for a in "$@"; do [ "$a" = "--user" ] || args+=("$a"); done
case "${args[0]:-}" in
  is-active) printf '%s\n' "${FAKE_ACTIVE_STATE:-active}"
             [ "${FAKE_ACTIVE_STATE:-active}" = "active" ] || exit 3 ;;
esac
exit 0
FAKE

# sudo: logs that it was used, then runs the rest. Rootful mode is asserted by
# the presence of these lines, not by actually escalating anything.
cat >"${FAKE_BIN}/sudo" <<'FAKE'
#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >>"$FAKE_CALL_LOG"
exec "$@"
FAKE

cat >"${FAKE_BIN}/curl" <<'FAKE'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"$FAKE_CALL_LOG"
[ "${FAKE_CURL_FAIL:-0}" = "1" ] && exit 7
exit 0
FAKE

cat >"${FAKE_BIN}/openssl" <<'FAKE'
#!/usr/bin/env bash
printf 'openssl %s\n' "$*" >>"$FAKE_CALL_LOG"
printf 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n'
FAKE

# chgrp: the rootful secrets command. Faked because an unprivileged test user is
# not in group 1002 and the real call would fail for a reason unrelated to what
# is being tested.
cat >"${FAKE_BIN}/chgrp" <<'FAKE'
#!/usr/bin/env bash
printf 'chgrp %s\n' "$*" >>"$FAKE_CALL_LOG"
exit 0
FAKE

# Tripwires. #4470 is explicit that this must not install packages, clone
# anything, or speak to Docker; each of these fails the run loudly if called.
for tripwire in apt-get dnf yum apk zypper git docker; do
  cat >"${FAKE_BIN}/${tripwire}" <<FAKE
#!/usr/bin/env bash
printf '${tripwire} %s\n' "\$*" >>"\$FAKE_TRIPWIRE_LOG"
exit 1
FAKE
done

chmod +x "${FAKE_BIN}"/*

# --- fixture: a copy of the real assets, so it can be mutated per case -------
#
# Copied from the repository rather than invented: the port parsing has to work
# against the units that actually ship, and a hand-written stand-in would let
# them drift apart without this suite noticing.
build_fixture() {
  rm -rf "$FIXTURE"
  mkdir -p "${FIXTURE}/src/deploy/quadlet" "${FIXTURE}/preflight"
  cp "${ROOT}/src/hive.yaml.example"   "${FIXTURE}/src/hive.yaml.example"
  cp "${ROOT}/src/deploy/nginx.conf"   "${FIXTURE}/src/deploy/nginx.conf"
  cp "${ROOT}"/src/deploy/quadlet/hive.container \
     "${ROOT}"/src/deploy/quadlet/hive-gateway.container \
     "${ROOT}"/src/deploy/quadlet/hive.network \
     "${ROOT}"/src/deploy/quadlet/hive-data.volume \
     "${ROOT}"/src/deploy/quadlet/hive.env.example \
     "${FIXTURE}/src/deploy/quadlet/"

  # Preflight stubs. Each records the runtime selector it was invoked with —
  # that recording is the whole point of one of the cases below — and exits with
  # whatever the case asked for.
  local name
  for name in hive-podman-preflight hive-podman-preflight-ids hive-podman-preflight-host; do
    cat >"${FIXTURE}/preflight/${name}.sh" <<FAKE
#!/usr/bin/env bash
printf '${name} runtime=%s src=%s\n' "\${HIVE_DEPLOY_RUNTIME:-<unset>}" "\${HIVE_SRC_DIR:-<unset>}" >>"\$FAKE_PREFLIGHT_LOG"
printf 'PREFLIGHT-OUTPUT-${name}: remediation lives here\n'
exit "\${FAKE_PREFLIGHT_RC_${name//-/_}:-0}"
FAKE
  done
  chmod +x "${FIXTURE}"/preflight/*.sh
}

# --- runner -----------------------------------------------------------------
#
# Each run gets a fresh conf/units pair unless the caller reuses one, which is
# how the idempotency cases are expressed.
RUN_OUT=""
RUN_RC=0
CONF=""
UNITS_DIR=""
CALL_LOG=""
PREFLIGHT_LOG=""
TRIPWIRE_LOG=""

new_case() {
  CASE="$1"
  local dir
  dir="${TEST_TMP}/case-$(printf '%s' "$1" | tr -c 'a-zA-Z0-9' '-')"
  rm -rf "$dir"
  mkdir -p "$dir"
  CONF="${dir}/conf"
  UNITS_DIR="${dir}/units"
  CALL_LOG="${dir}/calls.log"
  PREFLIGHT_LOG="${dir}/preflight.log"
  TRIPWIRE_LOG="${dir}/tripwire.log"
  : >"$CALL_LOG"; : >"$PREFLIGHT_LOG"; : >"$TRIPWIRE_LOG"
  build_fixture
}

# Extra per-case environment is passed as VAR=VALUE arguments. It goes through
# `env` on purpose: assignments that arrive by expanding "$@" are ordinary words
# to the shell, so a bare prefix would try to EXECUTE "FAKE_CURL_FAIL=1".
run_setup() {
  RUN_OUT="$(
    PATH="${FAKE_BIN}:${PATH}" \
    NO_COLOR=1 \
    HOME="${TEST_TMP}/home" \
    FAKE_CALL_LOG="$CALL_LOG" \
    FAKE_PREFLIGHT_LOG="$PREFLIGHT_LOG" \
    FAKE_TRIPWIRE_LOG="$TRIPWIRE_LOG" \
    HIVE_SETUP_SRC_DIR="${FIXTURE}/src" \
    HIVE_SETUP_PREFLIGHT_DIR="${FIXTURE}/preflight" \
    HIVE_SETUP_CONF_DIR="$CONF" \
    HIVE_SETUP_UNIT_DIR="$UNITS_DIR" \
    HIVE_SETUP_HEALTH_RETRIES=2 \
    HIVE_SETUP_HEALTH_DELAY=0 \
    env "$@" \
    bash "$SETUP" ${SETUP_ARGS:-} 2>&1
  )"
  RUN_RC=$?
}

# dashboard.port as the file actually holds it, read the same way the installer
# reads it: inside the `dashboard:` block only.
conf_dashboard_port() {
  awk '
    /^[^[:space:]#]/ { in_dash = ($0 ~ /^dashboard:[[:space:]]*$/) }
    in_dash && $0 ~ /^[[:space:]]+port:[[:space:]]*[0-9]+/ {
      v = $0; sub(/^[[:space:]]+port:[[:space:]]*/, "", v); sub(/[^0-9].*$/, "", v); print v; exit
    }
  ' "${CONF}/hive.yaml"
}

printf '=== bin/hive-podman-setup.sh contract (#4470) ===\n\n'

# ---------------------------------------------------------------------------
# 1. The happy path, rootless.
# ---------------------------------------------------------------------------
new_case "rootless happy path"
SETUP_ARGS="" run_setup
assert_eq 0 "$RUN_RC" "exits 0"
assert_contains "$RUN_OUT" "Hive is running" "reports the deployment is up"

for unit in hive.container hive-gateway.container hive.network hive-data.volume; do
  if [[ -f "${UNITS_DIR}/${unit}" ]]; then pass "installed ${unit}"; else fail "did not install ${unit}"; fi
done
# Installed VERBATIM: the rationale in the unit headers has to travel to the
# host, and a generated stand-in would not carry it (#4470 forbids generating a
# deployment description).
if cmp -s "${FIXTURE}/src/deploy/quadlet/hive.container" "${UNITS_DIR}/hive.container"; then
  pass "hive.container installed byte-for-byte, not generated"
else
  fail "hive.container was modified on the way to the host"
fi

for f in hive.yaml nginx.conf hive.env; do
  if [[ -f "${CONF}/${f}" ]]; then pass "wrote ${f}"; else fail "did not write ${f}"; fi
done
if [[ -d "${CONF}/secrets" ]]; then pass "created secrets/"; else fail "did not create secrets/"; fi
assert_eq "750" "$(stat -c '%a' "${CONF}/secrets")" "secrets/ is mode 750 (#4359)"
assert_eq "600" "$(stat -c '%a' "${CONF}/hive.env")" "hive.env is mode 600"

calls="$(cat "$CALL_LOG")"
assert_contains "$calls" "systemctl --user daemon-reload" "ran daemon-reload on the user manager"
assert_contains "$calls" "systemctl --user start hive-gateway.service" "started the gateway"
assert_contains "$calls" "curl " "checked the gateway over HTTP"
assert_lacks "$calls" "sudo " "rootless never reaches for sudo"

assert_eq "" "$(cat "$TRIPWIRE_LOG")" "no package manager, git clone, or docker call (#4470)"

# ---------------------------------------------------------------------------
# 2. The preflights must run in a state where they check something.
#
# All three exit 0 having checked NOTHING unless Podman is explicitly selected.
# An installer that invokes them without the selector is worse than one that
# skips them: it prints three green reports about a host nobody looked at.
# ---------------------------------------------------------------------------
new_case "preflights see HIVE_DEPLOY_RUNTIME=podman"
SETUP_ARGS="" run_setup
pflog="$(cat "$PREFLIGHT_LOG")"
assert_contains "$pflog" "hive-podman-preflight runtime=podman" "engine preflight ran with podman selected"
assert_contains "$pflog" "hive-podman-preflight-ids runtime=podman" "ids preflight ran with podman selected"
assert_contains "$pflog" "hive-podman-preflight-host runtime=podman" "host preflight ran with podman selected"
assert_lacks "$pflog" "runtime=<unset>" "no preflight was invoked without the selector"
# The host preflight is the one that checks what was just written, so it must be
# pointed at the config directory rather than the source tree.
assert_contains "$pflog" "hive-podman-preflight-host runtime=podman src=${CONF}" \
  "host preflight was pointed at the installed config, not the checkout"

# ---------------------------------------------------------------------------
# 3. A failing preflight stops the run, surfaces ITS guidance, and writes
#    nothing.
# ---------------------------------------------------------------------------
new_case "engine preflight failure refuses to proceed"
SETUP_ARGS="" run_setup FAKE_PREFLIGHT_RC_hive_podman_preflight=78
assert_eq 78 "$RUN_RC" "exits 78 (EX_CONFIG)"
assert_contains "$RUN_OUT" "PREFLIGHT-OUTPUT-hive-podman-preflight" "surfaces the preflight's own output"
assert_contains "$RUN_OUT" "FAILED at step" "says which step failed"
assert_contains "$RUN_OUT" "does not repeat it" "defers to the preflight's remediation rather than restating it"
if [[ ! -e "${CONF}/hive.yaml" ]]; then pass "wrote no config"; else fail "wrote config despite a failed preflight"; fi
if [[ ! -e "${UNITS_DIR}/hive.container" ]]; then pass "installed no unit"; else fail "installed a unit despite a failed preflight"; fi

new_case "host preflight failure stops before the units"
SETUP_ARGS="" run_setup FAKE_PREFLIGHT_RC_hive_podman_preflight_host=78
assert_eq 78 "$RUN_RC" "exits 78"
if [[ -f "${CONF}/hive.yaml" ]]; then pass "leaves the config it wrote, for inspection"; else fail "config was rolled back"; fi
if [[ ! -e "${UNITS_DIR}/hive.container" ]]; then pass "installed no unit"; else fail "installed a unit anyway"; fi
assert_lacks "$(cat "$CALL_LOG")" "daemon-reload" "never reached daemon-reload"

# ---------------------------------------------------------------------------
# 4. #4367 — dashboard.port comes from the UNIT, not from a constant.
#
# The example config ships 3001 and the unit probes 3002, which is the trap.
# Driving it with a unit that probes an arbitrary port is what proves the value
# is read rather than hardcoded: a script with `3002` written in it passes the
# default case and fails this one.
# ---------------------------------------------------------------------------
new_case "dashboard.port is read out of the unit"
build_fixture
sed -i 's|^HealthCmd=curl -sf http://127.0.0.1:[0-9]*/api/health|HealthCmd=curl -sf http://127.0.0.1:3999/api/health|' \
  "${FIXTURE}/src/deploy/quadlet/hive.container"
SETUP_ARGS="" run_setup
assert_eq 0 "$RUN_RC" "exits 0"
assert_eq "3999" "$(conf_dashboard_port)" "wrote the unit's HealthCmd port into hive.yaml"
assert_contains "$RUN_OUT" "verified from the file that will be mounted" "verified by reading the file back"

new_case "the example's 3001 is corrected to the unit's port"
SETUP_ARGS="" run_setup
assert_eq "3001" "$(awk '/^dashboard:/{f=1} f&&/port:/{print $2; exit}' "${FIXTURE}/src/hive.yaml.example")" \
  "the shipped example really does say 3001 (the trap this guards)"
assert_eq "3002" "$(conf_dashboard_port)" "installed config says 3002, matching the shipped unit"

new_case "a hand-edited wrong port is corrected on a re-run"
SETUP_ARGS="" run_setup
assert_eq 0 "$RUN_RC" "first run succeeds"
# The operator "fixes" the port back, the way the example ships it.
sed -i '/^dashboard:/,/^[^[:space:]#]/ s/^\(\s*port:\s*\)[0-9]*$/\13001/' "${CONF}/hive.yaml"
assert_eq "3001" "$(conf_dashboard_port)" "port was edited back to 3001"
SETUP_ARGS="" run_setup
assert_eq 0 "$RUN_RC" "re-run succeeds"
assert_eq "3002" "$(conf_dashboard_port)" "re-run corrected it rather than starting a 300-second hang"

new_case "a config with no dashboard.port is refused"
build_fixture
# Strip the dashboard block's port line from the example.
sed -i '/^dashboard:/,/^[^[:space:]#]/ { /^[[:space:]]*port:[[:space:]]*[0-9]*$/d }' "${FIXTURE}/src/hive.yaml.example"
SETUP_ARGS="" run_setup
assert_eq 78 "$RUN_RC" "exits 78 rather than installing a config whose health probe cannot answer"
assert_contains "$RUN_OUT" "no dashboard.port found" "says what is missing"
if [[ ! -e "${UNITS_DIR}/hive.container" ]]; then pass "installed no unit"; else fail "installed a unit anyway"; fi

# ---------------------------------------------------------------------------
# 5. #4359 — the secrets command differs by root mode, and the wrong one is
#    silently wrong.
# ---------------------------------------------------------------------------
new_case "rootless uses podman unshare chown"
SETUP_ARGS="" run_setup
calls="$(cat "$CALL_LOG")"
assert_contains "$calls" "podman unshare chown -R 0:1002" "translated the container GID through the user namespace"
assert_lacks "$calls" "chgrp" "did not use the rootful command"

new_case "rootful uses chgrp, through sudo"
SETUP_ARGS="--rootful" run_setup
assert_eq 0 "$RUN_RC" "exits 0"
calls="$(cat "$CALL_LOG")"
assert_contains "$calls" "chgrp -R 1002" "rootful maps identity, so chgrp is the right command"
assert_lacks "$calls" "podman unshare" "no user-namespace translation where identity is mapped"
assert_contains "$calls" "sudo systemctl daemon-reload" "drives the SYSTEM manager through sudo"
assert_lacks "$calls" "systemctl --user" "never touches the user manager in rootful mode"

# ---------------------------------------------------------------------------
# 6. Idempotency. A second run must not cost an operator their config or keys.
# ---------------------------------------------------------------------------
new_case "a re-run keeps an edited config and existing secrets"
SETUP_ARGS="" run_setup
assert_eq 0 "$RUN_RC" "first run succeeds"
printf '\n# operator edit, must survive\n' >>"${CONF}/hive.yaml"
printf 'HIVE_GITHUB_TOKEN=ghp_operators_own\n' >>"${CONF}/hive.env"
printf 'private-key-material\n' >"${CONF}/secrets/app.pem"
SETUP_ARGS="" run_setup
assert_eq 0 "$RUN_RC" "second run succeeds"
assert_file_contains "${CONF}/hive.yaml" "operator edit, must survive" "kept the edited hive.yaml"
assert_file_contains "${CONF}/hive.env" "ghp_operators_own" "kept the operator's token"
assert_file_contains "${CONF}/secrets/app.pem" "private-key-material" "never touched anything under secrets/"
assert_contains "$RUN_OUT" "keep    Hive config" "reports the keep rather than doing it silently"
# The generated dashboard token is generated ONCE.
assert_eq "1" "$(grep -c '^HIVE_DASHBOARD_TOKEN=' "${CONF}/hive.env")" "did not append a second dashboard token"

new_case "--force re-copies the config but still not the secrets"
SETUP_ARGS="" run_setup
printf '\n# operator edit\n' >>"${CONF}/hive.yaml"
printf 'private-key-material\n' >"${CONF}/secrets/app.pem"
SETUP_ARGS="--force" run_setup
assert_eq 0 "$RUN_RC" "forced run succeeds"
if grep -qF "operator edit" "${CONF}/hive.yaml"; then
  fail "--force did not re-copy hive.yaml"
else
  pass "--force re-copied hive.yaml from the example"
fi
assert_file_contains "${CONF}/secrets/app.pem" "private-key-material" "--force still never touches secrets/"
assert_eq "3002" "$(conf_dashboard_port)" "the re-copied config is port-corrected too"

# ---------------------------------------------------------------------------
# 7. Socket isolation (#4188). The installer is what puts units on the host, so
#    it is where the rule can be enforced rather than merely followed.
# ---------------------------------------------------------------------------
new_case "a unit naming a container socket is refused"
build_fixture
printf 'Volume=/run/user/1000/podman/podman.sock:/run/podman.sock\n' \
  >>"${FIXTURE}/src/deploy/quadlet/hive.container"
SETUP_ARGS="" run_setup
assert_eq 70 "$RUN_RC" "exits 70 (EX_SOFTWARE)"
assert_contains "$RUN_OUT" "container socket" "says what is wrong"
if [[ ! -e "${UNITS_DIR}/hive.container" ]]; then pass "installed nothing"; else fail "installed the socket-mounting unit"; fi

new_case "the units that actually ship name no socket"
SETUP_ARGS="" run_setup
assert_eq 0 "$RUN_RC" "the repository's own units pass the socket guard"

# ---------------------------------------------------------------------------
# 8. Started is not healthy. Notify=healthy is the reason that distinction is
#    real, and an installer that ends on `systemctl start` returning has not
#    used it.
# ---------------------------------------------------------------------------
new_case "a unit stuck activating is not reported as healthy"
SETUP_ARGS="" run_setup FAKE_ACTIVE_STATE=activating
assert_eq 78 "$RUN_RC" "exits 78"
assert_contains "$RUN_OUT" "not active" "says the unit is not active"
assert_contains "$RUN_OUT" "has not answered /api/health" "explains what activating means under Notify=healthy"
assert_lacks "$RUN_OUT" "Hive is running" "does not claim success"

new_case "units active but the gateway silent is not reported as healthy"
SETUP_ARGS="" run_setup FAKE_CURL_FAIL=1
assert_eq 78 "$RUN_RC" "exits 78"
assert_contains "$RUN_OUT" "did not answer" "says the gateway never answered"
assert_contains "$RUN_OUT" "aardvark-dns" "points at the usual cause of a 502 there"
assert_lacks "$RUN_OUT" "Hive is running" "does not claim success"
assert_contains "$RUN_OUT" "left running for you to inspect" "leaves the deployment up rather than rolling back"

# ---------------------------------------------------------------------------
# 9. The runtime guard: this script speaks only to Podman.
# ---------------------------------------------------------------------------
new_case "an explicit Docker selection is refused"
SETUP_ARGS="" run_setup HIVE_DEPLOY_RUNTIME=docker
assert_eq 64 "$RUN_RC" "exits 64 (EX_USAGE)"
assert_contains "$RUN_OUT" "this is the Podman installer" "says which installer this is"
assert_eq "" "$(cat "$PREFLIGHT_LOG")" "ran no preflight"
if [[ ! -e "${CONF}/hive.yaml" ]]; then pass "wrote nothing"; else fail "wrote config for a Docker deployment"; fi

# ---------------------------------------------------------------------------
# 10. --skip-pull is the only way the image is not pre-pulled, and the default
#     really does pull. The pull exists so it is not spent inside
#     TimeoutStartSec.
# ---------------------------------------------------------------------------
new_case "the image is pre-pulled by default"
SETUP_ARGS="" run_setup
assert_contains "$(cat "$CALL_LOG")" "podman pull ghcr.io/kubestellar/hive" "pulled the image named by the unit"

new_case "--skip-pull skips it, and says what that costs"
SETUP_ARGS="--skip-pull" run_setup
assert_lacks "$(cat "$CALL_LOG")" "podman pull" "did not pull"
assert_contains "$RUN_OUT" "TimeoutStartSec" "says the first start pays for it instead"

# ---------------------------------------------------------------------------
# 11. Usage.
# ---------------------------------------------------------------------------
new_case "an unknown argument is a usage error"
SETUP_ARGS="--wat" run_setup
assert_eq 64 "$RUN_RC" "exits 64"
assert_contains "$RUN_OUT" "Usage: hive-podman-setup.sh" "prints usage"

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[[ "$fail_count" -eq 0 ]] || exit 1
exit 0
