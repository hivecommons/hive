#!/usr/bin/env bash
# SELinux-enforcing release qualification for standalone Podman (#4337).
#
# #4211 measured SELinux as ABSENT on every hosted GitHub runner — no
# getenforce, no selinuxfs, on both amd64 and arm64. This is therefore the one
# lane that cannot be hosted CI, and #4209's SELinux/mount/secret preflight
# consequently ships with no coverage at all. This script is the qualification
# that closes that gap: it runs on a Fedora/CentOS Stream-class ENFORCING host,
# before a release, and records what the kernel actually did.
#
# It is deliberately not a workflow. Do not "fix" it by moving it to
# ubuntu-latest — there is no SELinux there to enforce anything, so the whole
# suite would pass while measuring nothing. See
# src/docs/podman-selinux-release-qualification.md.
#
# WHAT IT CHECKS
#   enforcement    an unlabeled bind mount is DENIED       (the control)
#   :Z             grants access, private MCS category
#   :Z isolation   a second container without the flag is DENIED
#   :z             grants access, shared, no category
#   :z sharing     a second container without the flag is ALLOWED
#   secrets        neither flag widens a 0600 file's mode
#   preflight      #4209's host preflight runs on this enforcing host
#
# The enforcement control comes first and is not optional. Every "access was
# granted" result below is only meaningful if the kernel would otherwise have
# said no, and a host that permits the unlabeled mount is a host where the rest
# of this suite proves nothing.
#
# WHAT IT REFUSES TO DO
#   * It never runs on a non-enforcing host. #4337's stop condition is to record
#     the procedure as UNEXECUTED rather than to run it somewhere permissive and
#     report a pass, so a non-enforcing host exits 78 and reports no result.
#   * It never disables SELinux, never runs setenforce, and never suggests
#     --security-opt label=disable as a remedy — the same constraint #4209's
#     preflight holds to.
#   * It never widens a secret. It asserts modes are UNCHANGED; it does not
#     chmod anything.
#   * It relabels ONLY a fixture directory it creates itself, under a path it
#     chose, and removes it on exit. It is never pointed at an operator's
#     checkout or secrets — :Z relabels in place, and doing that to a real
#     directory is a change to the host, not a test of it.
#
# Usage:
#   src/deploy/qualify_podman_selinux.sh [options]
#
#   --image REF   image to test with (default ghcr.io/kubestellar/hive:v4-latest)
#   --fixture DIR parent directory for the throwaway fixture (default $HOME)
#   --store DIR   pull into a throwaway store here instead of the host's own
#   --skip-preflight  do not run bin/hive-podman-preflight-host.sh
#
# Exit codes: 0 every check passed, 1 at least one failed,
#             78 not qualifiable here — record as UNEXECUTED (EX_CONFIG).

set -uo pipefail

IMAGE="ghcr.io/kubestellar/hive:v4-latest"
FIXTURE_PARENT="${HOME:-/root}"
RUN_PREFLIGHT="true"
STORE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE="${2:?--image needs a value}"; shift 2 ;;
    --fixture) FIXTURE_PARENT="${2:?--fixture needs a value}"; shift 2 ;;
    --store) STORE="${2:?--store needs a value}"; shift 2 ;;
    --skip-preflight) RUN_PREFLIGHT="false"; shift ;;
    -h|--help) sed -n '2,52p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'ERROR: unknown argument %q\n' "$1" >&2; exit 78 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

# The label reader is resolved, never assumed (#4490). Bare `stat -c '%C'`
# under uutils coreutils prints "unsupported for this operating system" to
# stdout and exits 0, and this suite compared that sentence against
# container_file_t and emitted FAIL (2) — plus a ledger row — on a healthy
# enforcing host.
# shellcheck source=src/deploy/selinux_label_reader.sh
. "${REPO_ROOT}/src/deploy/selinux_label_reader.sh"

# By default the host's OWN store is used: a release qualification is meant to
# measure the host as it is actually configured. --store is for a run that must
# leave the operator's storage untouched.
POD=(podman)
if [[ -n "$STORE" ]]; then
  mkdir -p "${STORE}/graph" "${STORE}/run"
  POD=(podman --root "${STORE}/graph" --runroot "${STORE}/run")
fi
pod() { "${POD[@]}" "$@"; }

not_qualifiable() {
  printf '\nNOT QUALIFIABLE HERE: %s\n' "$1" >&2
  printf 'Record this release as UNEXECUTED. Do not run the suite on a\n' >&2
  printf 'permissive or SELinux-less host and record a pass — #4337.\n' >&2
  exit 78
}

failures=0
ok()   { printf '  PASS: %s\n' "$1"; }
bad()  { printf '  FAIL: %s\n' "$1"; [[ $# -gt 1 ]] && printf '        %s\n' "$2"; failures=$((failures + 1)); }

printf '=== Hive SELinux-enforcing release qualification (#4337) ===\n\n'

# ── Gate: this host must actually be enforcing ─────────────────────────────
printf -- '--- host ---\n'
command -v podman >/dev/null 2>&1 || not_qualifiable "podman is not installed or not on PATH"

[[ -d /sys/fs/selinux ]] || not_qualifiable "no /sys/fs/selinux — this kernel has no SELinux at all (the hosted-runner case #4211 measured)"
command -v getenforce >/dev/null 2>&1 || not_qualifiable "getenforce is not installed; cannot establish the mode"

mode="$(getenforce 2>/dev/null)"
[[ "$mode" == "Enforcing" ]] || not_qualifiable "SELinux is ${mode:-unknown}, not Enforcing"

policy="$(sestatus 2>/dev/null | sed -n 's/^Loaded policy name: *//p')"
podman_ver="$(pod info --format '{{.Version.Version}}' 2>/dev/null)"
rootless="$(pod info --format '{{.Host.Security.Rootless}}' 2>/dev/null)"
selinux_on="$(pod info --format '{{.Host.Security.SELinuxEnabled}}' 2>/dev/null)"

printf '  os          %s\n' "$(sed -n 's/^PRETTY_NAME="\(.*\)"$/\1/p' /etc/os-release 2>/dev/null)"
printf '  kernel      %s\n' "$(uname -r)"
printf '  selinux     %s, policy=%s\n' "$mode" "${policy:-unknown}"
printf '  podman      %s (rootless=%s, selinux=%s)\n' "$podman_ver" "$rootless" "$selinux_on"
printf '  image       %s\n' "$IMAGE"
printf '  store       %s\n' "${STORE:-<host default>}"
if command -v rpm >/dev/null 2>&1; then
  printf '  policy pkgs %s\n' "$(rpm -q container-selinux selinux-policy 2>/dev/null | tr '\n' ' ')"
fi

if [[ "$selinux_on" != "true" ]]; then
  bad "Podman reports SELinux labeling enabled" \
      "podman info says SELinuxEnabled=${selinux_on:-unknown} on an enforcing kernel; :z/:Z would do nothing"
fi

# ── Fixture. Created here, relabelled here, removed here. ──────────────────
FIXTURE="$(mktemp -d -p "$FIXTURE_PARENT" hive-selinux-qual-XXXXXX)" \
  || not_qualifiable "cannot create a fixture directory under ${FIXTURE_PARENT}"
# shellcheck disable=SC2329 # invoked through the EXIT trap below
cleanup() { rm -rf "$FIXTURE" 2>/dev/null; return 0; }
trap cleanup EXIT

printf 'qualification fixture' >"${FIXTURE}/secret.txt"
chmod 600 "${FIXTURE}/secret.txt"

# Resolve the reader against the fixture itself. No working reader means no
# verdict and no ledger row — "I could not measure this" and "this host
# failed" must not produce the same output (#4490).
hive_selinux_resolve_label_reader "${FIXTURE}/secret.txt" \
  || not_qualifiable "no working SELinux label reader (tried ${HIVE_SELINUX_LABEL_READERS}). Under uutils coreutils, 'stat -c %C' prints an error to stdout and exits 0, so a label read that way is not a label. Install GNU coreutils or attr (getfattr), then re-run."

START_MODE="$(stat -c '%a' "${FIXTURE}/secret.txt")"
START_LABEL="$(hive_selinux_label_of "${FIXTURE}/secret.txt")"
printf '  label rdr   %s\n' "$HIVE_SELINUX_LABEL_READER"
printf '  fixture     %s (mode %s, %s)\n\n' "$FIXTURE" "$START_MODE" "$START_LABEL"

if ! pod image exists "$IMAGE" 2>/dev/null; then
  printf 'pulling %s ...\n' "$IMAGE"
  pod pull -q "$IMAGE" >/dev/null 2>&1 || not_qualifiable "cannot pull ${IMAGE}"
fi

# Reads the fixture from inside a container. Returns the container's exit
# status; output goes to READ_OUT.
read_fixture() {
  local flag="$1" vol
  vol="${FIXTURE}:/mnt:ro"
  [[ -n "$flag" ]] && vol="${vol},${flag}"
  READ_OUT="$(pod run --rm -v "$vol" --entrypoint /bin/sh "$IMAGE" \
    -c 'cat /mnt/secret.txt' 2>&1)"
}

label_of() { hive_selinux_label_of "${FIXTURE}/secret.txt"; }
mode_of()  { stat -c '%a' "${FIXTURE}/secret.txt"; }
# container_file_t at s0 with no cN category is the shared form; with one it is
# private to a single container's MCS pair. Only the LEVEL field is examined —
# a naive *:c* over the whole context matches the ":container_file_t" in the
# type field and reports every label as private.
mcs_of() { printf '%s' "${1#*:*:*:}"; }
has_category() { [[ "$(mcs_of "$1")" == *:c* ]]; }

# ── The control. Everything else depends on this failing. ──────────────────
printf -- '--- enforcement control: bind mount with no relabel flag ---\n'
read_fixture ""
if [[ -n "$READ_OUT" && "$READ_OUT" != *"Permission denied"* ]]; then
  bad "an unlabeled bind mount is denied" \
      "the container read the file anyway (${READ_OUT}). Every 'access granted' result below would be meaningless on this host."
  printf '\nSUMMARY: %d failure(s)\n' "$failures"
  exit 1
fi
ok "an unlabeled ${START_LABEL##*:object_r:} mount is denied (the kernel is enforcing on this path)"
if [[ "$(label_of)" != "$START_LABEL" ]]; then
  bad "a denied mount does not relabel" "label changed to $(label_of) without a relabel flag"
else
  ok "a denied mount left the host label untouched"
fi

# ── :Z — private relabel ───────────────────────────────────────────────────
printf -- '\n--- :Z (private relabel) ---\n'
read_fixture "Z"
z_label="$(label_of)"
printf '  label after :Z  %s\n' "$z_label"
printf '  mode  after :Z  %s\n' "$(mode_of)"
if [[ "$READ_OUT" == "qualification fixture" ]]; then
  ok ":Z grants the container access"
else
  bad ":Z grants the container access" "read returned: ${READ_OUT}"
fi
if [[ "$z_label" == *container_file_t* ]] && has_category "$z_label"; then
  ok ":Z relabelled to container_file_t with a private MCS category"
else
  bad ":Z relabelled to container_file_t with a private MCS category" "got ${z_label}"
fi
if [[ "$(mode_of)" == "$START_MODE" ]]; then
  ok ":Z left the 0${START_MODE} secret's mode unchanged"
else
  bad ":Z left the secret's mode unchanged" "mode went ${START_MODE} -> $(mode_of)"
fi

# The isolation this buys: the category belongs to the container that got it,
# so anything else is denied. This is why #4209's preflight says :Z for the
# secrets directory and :z for a checked-out config file — a private category
# on a shared file locks out every other container that needs it.
printf -- '\n--- :Z isolation: a second container, no relabel flag ---\n'
read_fixture ""
if [[ -z "$READ_OUT" || "$READ_OUT" == *"Permission denied"* ]]; then
  ok "the private category denies a container that does not carry it"
else
  bad "the private category denies a container that does not carry it" \
      "the second container read the file (${READ_OUT}); :Z is not isolating"
fi

# ── :z — shared relabel ────────────────────────────────────────────────────
printf -- '\n--- :z (shared relabel) ---\n'
read_fixture "z"
sh_label="$(label_of)"
printf '  label after :z  %s\n' "$sh_label"
printf '  mode  after :z  %s\n' "$(mode_of)"
if [[ "$READ_OUT" == "qualification fixture" ]]; then
  ok ":z grants the container access"
else
  bad ":z grants the container access" "read returned: ${READ_OUT}"
fi
if [[ "$sh_label" == *container_file_t* ]] && ! has_category "$sh_label"; then
  ok ":z relabelled to container_file_t with no private category"
else
  bad ":z relabelled to container_file_t with no private category" "got ${sh_label}"
fi
if [[ "$(mode_of)" == "$START_MODE" ]]; then
  ok ":z left the 0${START_MODE} secret's mode unchanged"
else
  bad ":z left the secret's mode unchanged" "mode went ${START_MODE} -> $(mode_of)"
fi

printf -- '\n--- :z sharing: a second container, no relabel flag ---\n'
read_fixture ""
if [[ "$READ_OUT" == "qualification fixture" ]]; then
  ok "a shared label is readable by a second container"
else
  bad "a shared label is readable by a second container" "read returned: ${READ_OUT}"
fi

# ── #4209's preflight, on the host it has never been covered on ────────────
if [[ "$RUN_PREFLIGHT" == "true" && -x "${REPO_ROOT}/bin/hive-podman-preflight-host.sh" ]]; then
  printf -- '\n--- #4209 preflight on this enforcing host ---\n'
  pf_out="$(HIVE_DEPLOY_RUNTIME=podman bash "${REPO_ROOT}/bin/hive-podman-preflight-host.sh" 2>&1)"
  pf_rc=$?
  printf '%s\n' "$pf_out" | sed 's/^/  | /'
  if printf '%s' "$pf_out" | grep -q 'SELinux: Enforcing'; then
    ok "the preflight detects Enforcing (exit ${pf_rc})"
  else
    bad "the preflight detects Enforcing" "it did not report Enforcing on a host where getenforce says ${mode}"
  fi
  # The preflight is read-only by contract. Verify that on a real enforcing
  # host it did not relabel the fixture behind our back.
  if [[ "$(label_of)" == "$sh_label" && "$(mode_of)" == "$START_MODE" ]]; then
    ok "the preflight changed no label and no mode"
  else
    bad "the preflight changed no label and no mode" \
        "label ${sh_label} -> $(label_of), mode ${START_MODE} -> $(mode_of)"
  fi
fi

printf '\n=== result block (paste into src/docs/podman-selinux-release-qualification.md) ===\n'
# The release cell is left as a placeholder on purpose: this suite qualifies a
# host and an image, and only the person cutting the release knows which
# release that is. Filling it in is part of recording the result.
printf '| <release> | %s | %s | %s | %s | podman %s, rootless=%s | %s |\n' \
  "$(date -u +%Y-%m-%d)" \
  "$(sed -n 's/^PRETTY_NAME="\(.*\)"$/\1/p' /etc/os-release 2>/dev/null)" \
  "${policy:-unknown}" \
  "$(uname -r)" \
  "$podman_ver" "$rootless" \
  "$([[ "$failures" -eq 0 ]] && echo 'PASS' || echo "FAIL (${failures})")"

printf '\nSUMMARY: %d failure(s)\n' "$failures"
[[ "$failures" -eq 0 ]] || exit 1
exit 0
