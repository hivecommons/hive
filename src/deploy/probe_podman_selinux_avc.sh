#!/usr/bin/env bash
# probe_podman_selinux_avc.sh — AVC evidence for SELinux-enforcing Podman, and
# the hive-launch group-secret case (kubestellar/hive#4188).
#
# This is the follow-up to src/deploy/qualify_podman_selinux.sh (#4337), not a
# replacement for it. That suite establishes the :z/:Z/unlabeled matrix, but it
# decides "denied" from the container's exit status: it never reads the audit
# log, so a denial it reports is INFERRED rather than observed, and a failure
# that had nothing to do with SELinux would look identical. This probe records
# the kernel's own AVC records per case, and adds the secret case the release
# image actually depends on — 0440 owned by the hive-launch group, read through
# a supplementary group rather than by the file's owner.
#
# Cases:
#   control     bind mount, no relabel flag  -> expect DENIED, with an AVC
#   Z           :Z private relabel           -> expect granted, no new AVC
#   Zisolation  second container, no flag    -> expect DENIED, with an AVC
#   z           :z shared relabel            -> expect granted, no new AVC
#   zsharing    second container, no flag    -> expect granted (shared type)
#   gsecret     0440 root:hive-launch(1002), read as dev(1001) via the
#               supplementary group, unlabeled / :Z / :z
#   preflight   bin/hive-podman-preflight-host.sh on this enforcing host
#
# AVC EVIDENCE. A host of this class carries background denials, so a snapshot
# is taken before any container starts and every case is diffed against it.
# Without that, a denial this probe caused is indistinguishable from ambient
# noise. Reading the audit log needs root: the probe uses `sudo -n ausearch`
# when it can, falls back to `journalctl`, and reports UNMEASURED rather than
# claiming a clean result when neither reader is available.
#
# STORAGE SAFETY. Every podman call goes through pod(), which pins
# --root/--runroot to a throwaway store, and the probe refuses to address the
# host's own store. :z and :Z RELABEL the directory they are pointed at and the
# relabel outlives the container, so the only mount source is a fixture this
# probe creates under /var/tmp and removes on exit. No checkout, no real
# secrets directory, nothing under /usr, /var/lib, or $HOME. Containers are
# removed by exact name; nothing is pruned.
#
# NON-GOALS, so the result stays worth having. No setenforce, no
# --security-opt label=disable, no --privileged, no chmod that widens a secret.
# A case that fails under enforcing is recorded as a failure. The same
# constraint #4209's preflight holds to.
#
# Exit codes:
#   0  every case matched what src/docs/podman-selinux-avc-evidence.md records
#   1  at least one case did not match
#  78  not qualifiable here (not enforcing, or a prerequisite is missing)

set -uo pipefail

IMAGE="${IMAGE:-ghcr.io/kubestellar/hive:v4-latest}"
PROBE_PREFIX="hive-selinux-avc-$$"
HIVE_LAUNCH_GID="${HIVE_LAUNCH_GID:-1002}"   # src/Dockerfile pins this
DEV_UID="${DEV_UID:-1001}"                   # src/Dockerfile: useradd -u 1001 dev
STORE="${STORE:-}"
OWN_STORE="false"
KEEP="false"
RUN_PREFLIGHT="true"
failures=0
MCS_AUDITED="unmeasured"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE="$2"; shift 2 ;;
    --store) STORE="$2"; shift 2 ;;
    --keep)  KEEP="true"; shift ;;
    --skip-preflight) RUN_PREFLIGHT="false"; shift ;;
    -h|--help) sed -n '2,50p' "$0"; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 64 ;;
  esac
done

not_qualifiable() {
  printf '\nNOT QUALIFIABLE HERE: %s\n' "$1" >&2
  printf 'No result is reported. See src/docs/podman-selinux-avc-evidence.md.\n' >&2
  exit 78
}
ok()  { printf '  PASS: %s\n' "$1"; }
bad() { printf '  FAIL: %s\n' "$1"; [[ $# -gt 1 ]] && printf '        %s\n' "$2"; failures=$((failures + 1)); }
note(){ printf '  ..    %s\n' "$1"; }

# ── The stop condition, enforced here rather than trusted to the operator ────
command -v podman >/dev/null 2>&1 || not_qualifiable "podman is not installed or not on PATH"
[[ -d /sys/fs/selinux ]] || not_qualifiable "/sys/fs/selinux is absent — this host has no SELinux (see #4211)"
command -v getenforce >/dev/null 2>&1 || not_qualifiable "getenforce is absent — cannot establish the mode"
MODE="$(getenforce 2>/dev/null)"
[[ "$MODE" == "Enforcing" ]] || \
  not_qualifiable "SELinux mode is ${MODE:-unknown}, not Enforcing. Do not setenforce 1 to make this run; record it unexecuted."

# ── A label reader that is actually a label reader ───────────────────────────
# `stat -c '%C'` is the obvious choice and cannot be trusted bare: on a host
# where uutils coreutils shadows GNU coreutils on PATH — the default on
# Bluefin/Universal Blue, i.e. exactly the Fedora-atomic class this lane
# targets — uutils `stat` does not implement %C and prints "unsupported for
# this operating system" to stdout with exit 0. The resolve-by-probing reader
# this probe pioneered is now shared with #4337's suite (#4490).
# shellcheck source=src/deploy/selinux_label_reader.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/selinux_label_reader.sh"
label_of() { hive_selinux_label_of "$1"; }
# Only the LEVEL field decides whether a private MCS category is present. A
# naive *:c* over the whole context matches the ":container_file_t" in the type
# field and reports every label as private (the bug #4337 caught in review).
mcs_of()       { printf '%s' "${1#*:*:*:}"; }
has_category() { [[ "$(mcs_of "$1")" == *:c* ]]; }
mode_of()      { /usr/bin/stat -c '%a' "$1" 2>/dev/null || stat -c '%a' "$1"; }
gid_of()       { /usr/bin/stat -c '%g' "$1" 2>/dev/null || stat -c '%g' "$1"; }

# ── Storage safety ──────────────────────────────────────────────────────────
if [[ -z "$STORE" ]]; then
  STORE="$(mktemp -d -p /var/tmp hive-selinux-avc-store-XXXXXX)" \
    || not_qualifiable "cannot create a throwaway store under /var/tmp"
  OWN_STORE="true"
fi
mkdir -p "${STORE}/graph" "${STORE}/run"
# Refuse the host's own store, whatever was passed in. :z/:Z make this probe a
# writer, not just a reader, so it has no business near the real store.
case "$STORE" in
  /var/lib/containers*|/run/containers*)
    not_qualifiable "refusing to use ${STORE}: that is the host's container store" ;;
esac
host_graph="$(podman info --format '{{.Store.GraphRoot}}' 2>/dev/null || true)"
if [[ -n "$host_graph" && "${STORE}/graph" == "$host_graph" ]]; then
  not_qualifiable "refusing to use ${STORE}/graph: that is the host's graphRoot"
fi
POD=(podman --root "${STORE}/graph" --runroot "${STORE}/run")
pod() { "${POD[@]}" "$@"; }

# ── Audit reader ────────────────────────────────────────────────────────────
# ausearch needs root to read /var/log/audit/audit.log. sudo -n first, then the
# journal, then give up honestly.
AUDIT_READER=""
if [[ "$(id -u)" -eq 0 ]] && command -v ausearch >/dev/null 2>&1; then
  AUDIT_READER="ausearch"
elif command -v ausearch >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  AUDIT_READER="sudo-ausearch"
elif command -v journalctl >/dev/null 2>&1 && journalctl -n1 --no-pager >/dev/null 2>&1; then
  AUDIT_READER="journalctl"
fi

# `ausearch -ts` resolves to whole seconds, so a mark taken in the same second
# as the previous case's denial would sweep that denial into this case's window
# and report it as newly caused. Waiting for the second to turn means every
# record in the marked second happened after this point.
mark_now() {
  local t0 t
  t0="$(date '+%H:%M:%S')"
  while :; do
    t="$(date '+%H:%M:%S')"
    [[ "$t" != "$t0" ]] && break
    sleep 0.1
  done
  printf '%s' "$t"
}

# All denials since $1 (an HH:MM:SS on today's date).
avc_since() {
  case "$AUDIT_READER" in
    ausearch)      ausearch -m AVC -ts "$1" 2>/dev/null | grep 'avc:  denied' ;;
    sudo-ausearch) sudo -n ausearch -m AVC -ts "$1" 2>/dev/null | grep 'avc:  denied' ;;
    journalctl)    journalctl --since "$1" --no-pager 2>/dev/null | grep 'avc:  denied' ;;
    *)             return 0 ;;
  esac
  return 0
}

# Denials attributable to a container: scontext is a container domain. The
# ambient noise on this host class is other domains entirely, which is the
# whole reason for taking a baseline.
container_avcs() { avc_since "$1" | grep -E 'scontext=[^ ]*:container_' || true; }

# Audit records are flushed asynchronously, so a denial is polled for rather
# than read once. An expected-empty case pays the full wait; that is the price
# of being able to say "no denial" and mean it.
AVC_OUT=""
collect_avc() {
  local mark="$1" _want="$2" attempt
  # shellcheck disable=SC2034 # a counter; the body only needs the iteration
  for attempt in 1 2 3 4 5; do
    AVC_OUT="$(container_avcs "$mark")"
    # Found one: no reason to keep waiting. Found none: keep polling to the end,
    # so "no denial" means five seconds of silence rather than one look.
    [[ -n "$AVC_OUT" ]] && return 0
    sleep 1
  done
  return 0
}

# One line per denial, deduplicated on the fields that identify the rule.
summarise_avc() {
  [[ -z "$AVC_OUT" ]] && { printf '        (no container-domain denial recorded)\n'; return 0; }
  printf '%s\n' "$AVC_OUT" | sed -E \
    's/.*avc:  denied  \{ ([^}]*)\}.*comm="([^"]*)".*scontext=([^ ]*) tcontext=([^ ]*) tclass=([^ ]*).*/        denied { \1} comm=\2 tclass=\5\n          scontext=\3\n          tcontext=\4/' \
    | sort -u
}

# ── Fixture. Created here, relabelled here, removed here. ───────────────────
FIXTURE="$(mktemp -d -p /var/tmp hive-selinux-avc-fix-XXXXXX)" \
  || not_qualifiable "cannot create a fixture directory under /var/tmp"
# shellcheck disable=SC2329 # invoked through the EXIT trap below
cleanup() {
  local name
  for name in "${PROBE_PREFIX}-control" "${PROBE_PREFIX}-z" "${PROBE_PREFIX}-Z"; do
    pod rm -f "$name" >/dev/null 2>&1
  done
  if [[ "$KEEP" == "true" ]]; then
    printf '\n--keep: fixture %s and store %s left in place\n' "$FIXTURE" "$STORE"
    return 0
  fi
  # Files chgrp'd through the user namespace are owned by a mapped subgid, so
  # plain rm cannot remove them. unshare re-enters that namespace.
  pod unshare rm -rf "$FIXTURE" 2>/dev/null || rm -rf "$FIXTURE" 2>/dev/null
  if [[ "$OWN_STORE" == "true" && -n "$STORE" ]]; then
    pod unshare rm -rf "$STORE" 2>/dev/null || rm -rf "$STORE" 2>/dev/null
  fi
  return 0
}
trap cleanup EXIT

printf 'qualification fixture\n' >"${FIXTURE}/secret.txt"
chmod 600 "${FIXTURE}/secret.txt"

hive_selinux_resolve_label_reader "${FIXTURE}/secret.txt" \
  || not_qualifiable "no working SELinux label reader (tried ${HIVE_SELINUX_LABEL_READERS})"

START_LABEL="$(label_of "${FIXTURE}/secret.txt")"
START_MODE="$(mode_of "${FIXTURE}/secret.txt")"

printf '== Hive SELinux AVC evidence probe (#4188) ==\n'
printf '  selinux     %s, policy %s\n' "$MODE" "$(sestatus 2>/dev/null | awk -F': *' '/Loaded policy name/{print $2}')"
pod info --format '  podman      {{.Version.Version}} (rootless={{.Host.Security.Rootless}}, selinux={{.Host.Security.SELinuxEnabled}}, runtime={{.Host.OCIRuntime.Name}})'
printf '  store       %s (throwaway=%s)\n' "$STORE" "$OWN_STORE"
printf '  fixture     %s\n' "$FIXTURE"
printf '  label rdr   %s\n' "$HIVE_SELINUX_LABEL_READER"
printf '  audit rdr   %s\n' "${AUDIT_READER:-NONE — AVC evidence will be UNMEASURED}"
printf '  image       %s\n' "$IMAGE"

if [[ -z "$AUDIT_READER" ]]; then
  bad "an audit reader is available" \
      "no readable audit source; this probe exists to record AVCs, so it reports nothing rather than a clean run"
  printf '\nSUMMARY: %d failure(s)\n' "$failures"
  exit 1
fi

if ! pod image exists "$IMAGE" 2>/dev/null; then
  printf 'pulling %s ...\n' "$IMAGE"
  pod pull -q "$IMAGE" >/dev/null 2>&1 || not_qualifiable "cannot pull ${IMAGE}"
fi
printf '  digest      %s\n' "$(pod image inspect "$IMAGE" --format '{{.Digest}}' 2>/dev/null)"
printf '  fixture     mode %s, %s\n\n' "$START_MODE" "$START_LABEL"

# ── AVC baseline, before any container starts ───────────────────────────────

BASELINE_ALL="$(avc_since 'today')"
BASELINE_N="$(printf '%s' "$BASELINE_ALL" | grep -c 'avc:  denied' || true)"
BASELINE_CONTAINER="$(printf '%s\n' "$BASELINE_ALL" | grep -cE 'scontext=[^ ]*:container_' || true)"
printf -- '--- AVC baseline (before any container) ---\n'
printf '  %s denial(s) today, %s of them from a container domain\n' "$BASELINE_N" "$BASELINE_CONTAINER"
printf '  ambient domains: %s\n\n' \
  "$(printf '%s\n' "$BASELINE_ALL" | sed -nE 's/.*scontext=[^:]*:[^:]*:([^:]*):.*/\1/p' | sort -u | tr '\n' ' ')"

# Runs the image and reads the fixture. Sets READ_OUT and READ_RC.
read_fixture() {
  local flag="$1" file="${2:-secret.txt}" user="${3:-}" vol mark
  vol="${FIXTURE}:/mnt:ro"
  [[ -n "$flag" ]] && vol="${vol},${flag}"
  local args=(run --rm -v "$vol")
  [[ -n "$user" ]] && args+=(--user "$user")
  mark="$(mark_now)"
  READ_OUT="$(pod "${args[@]}" --entrypoint /bin/sh "$IMAGE" -c "cat /mnt/${file}" 2>&1)"
  READ_RC=$?
  LAST_MARK="$mark"
}
denied() { [[ "$READ_OUT" == *"Permission denied"* || "$READ_OUT" == *"Operation not permitted"* ]]; }

# ── The control. Every "granted" result below depends on this being denied. ──
printf -- '--- control: bind mount, no relabel flag (expect DENIED + AVC) ---\n'
read_fixture ""
if denied || [[ "$READ_RC" -ne 0 && -z "$READ_OUT" ]]; then
  ok "an unlabeled ${START_LABEL##*:object_r:} mount is denied"
else
  bad "an unlabeled mount is denied" \
      "the container read it anyway (${READ_OUT}). Every 'granted' result below would be meaningless here."
  printf '\nSUMMARY: %d failure(s)\n' "$failures"
  exit 1
fi
collect_avc "$LAST_MARK" some

if [[ -n "$AVC_OUT" ]]; then
  ok "the denial is recorded in the audit log (observed, not inferred)"
else
  bad "the denial is recorded in the audit log" \
      "the read failed but no container-domain AVC appeared; the failure may not be SELinux at all"
fi
summarise_avc
if [[ "$(label_of "${FIXTURE}/secret.txt")" == "$START_LABEL" ]]; then
  ok "a denied mount left the host label untouched"
else
  bad "a denied mount does not relabel" "label changed to $(label_of "${FIXTURE}/secret.txt")"
fi

# ── :Z — private relabel ────────────────────────────────────────────────────
printf -- '\n--- :Z private relabel (expect granted, no new AVC) ---\n'
read_fixture "Z"
Z_LABEL="$(label_of "${FIXTURE}/secret.txt")"
printf '  label after :Z  %s\n' "$Z_LABEL"
if [[ "$READ_OUT" == "qualification fixture" ]]; then
  ok ":Z grants the container access"
else
  bad ":Z grants the container access" "read returned: ${READ_OUT}"
fi
if [[ "$Z_LABEL" == *container_file_t* ]] && has_category "$Z_LABEL"; then
  ok ":Z relabelled to container_file_t with a private MCS category"
else
  bad ":Z relabelled to container_file_t with a private MCS category" "got ${Z_LABEL}"
fi
collect_avc "$LAST_MARK" none
if [[ -z "$AVC_OUT" ]]; then
  ok "no container-domain denial was recorded for the granted read"
else
  note "denials recorded during the granted read (not necessarily this mount):"
  summarise_avc
fi
if [[ "$(mode_of "${FIXTURE}/secret.txt")" == "$START_MODE" ]]; then
  ok ":Z left the 0${START_MODE} secret's mode unchanged"
else
  bad ":Z left the secret's mode unchanged" "mode went ${START_MODE} -> $(mode_of "${FIXTURE}/secret.txt")"
fi

printf -- '\n--- :Z isolation: a second container, no flag (expect DENIED + AVC) ---\n'
read_fixture ""
if denied || [[ "$READ_RC" -ne 0 && -z "$READ_OUT" ]]; then
  ok "the private category denies a container that does not carry it"
else
  bad "the private category denies a container that does not carry it" \
      "the second container read it (${READ_OUT}); :Z is not isolating"
fi
collect_avc "$LAST_MARK" some
# MEASURED, and the reason this probe exists. A TYPE denial (the control case,
# container_t vs user_tmp_t) is audited. An MCS CATEGORY denial is not: the
# read is refused and the audit log stays completely empty — no AVC, no
# SELINUX_ERR, nothing. So the one failure an operator is most likely to hit
# with :Z — a second container needing a file the first one stamped — is the
# one they cannot diagnose from the audit log. Recorded rather than asserted as
# a failure: it is the kernel's behaviour, not a fault in the system under test.
if [[ -n "$AVC_OUT" ]]; then
  note "the isolation denial IS audited on this host:"
  summarise_avc
  MCS_AUDITED="yes"
else
  note "SILENT: the isolation denial produced no audit record of any kind."
  note "  A type denial is audited (see the control case); this category denial is not."
  MCS_AUDITED="no"
fi

# ── :z — shared relabel ─────────────────────────────────────────────────────
printf -- '\n--- :z shared relabel (expect granted, no category) ---\n'
read_fixture "z"
SH_LABEL="$(label_of "${FIXTURE}/secret.txt")"
printf '  label after :z  %s\n' "$SH_LABEL"
if [[ "$READ_OUT" == "qualification fixture" ]]; then
  ok ":z grants the container access"
else
  bad ":z grants the container access" "read returned: ${READ_OUT}"
fi
if [[ "$SH_LABEL" == *container_file_t* ]] && ! has_category "$SH_LABEL"; then
  ok ":z relabelled to container_file_t with no private category"
else
  bad ":z relabelled to container_file_t with no private category" "got ${SH_LABEL}"
fi
if [[ "$(mode_of "${FIXTURE}/secret.txt")" == "$START_MODE" ]]; then
  ok ":z left the 0${START_MODE} secret's mode unchanged"
else
  bad ":z left the secret's mode unchanged" "mode went ${START_MODE} -> $(mode_of "${FIXTURE}/secret.txt")"
fi

printf -- '\n--- :z sharing: a second container, no flag (expect granted) ---\n'
read_fixture ""
if [[ "$READ_OUT" == "qualification fixture" ]]; then
  ok "the shared type is readable by a container that did not relabel it"
else
  bad "the shared type is readable by a second container" "read returned: ${READ_OUT}"
fi

# ── The hive-launch group secret ────────────────────────────────────────────
# The case the release image actually depends on, and the one #4337's suite does
# not cover: not a 0600 file read by its owner, but 0440 owned by the
# hive-launch group (GID 1002, pinned in src/Dockerfile and as fsGroup in
# src/deploy/k8s/deployment.yaml), read by dev (UID 1001) through a
# SUPPLEMENTARY group. DAC and MAC both have to permit it, and only one of them
# is what :z/:Z addresses — so the two are measured apart rather than together.
#
# Rootless: host IDs are mapped, so the fixture is built THROUGH the user
# namespace with `podman unshare`. A plain chgrp would need root and would set
# the wrong GID anyway. Note the consequence that drives the first sub-case:
# the host user maps to container ROOT, not to dev, so a directory this user
# owns at mode 0700 is one dev cannot traverse at all.
printf -- '\n--- hive-launch group secret: 0440, GID %s, read as UID %s ---\n' \
  "$HIVE_LAUNCH_GID" "$DEV_UID"
GDIR="${FIXTURE}/secrets"
GSECRET="${GDIR}/github-app.pem"
mkdir -p "$GDIR"
printf 'not a real key\n' >"$GSECRET"
if pod unshare chgrp "$HIVE_LAUNCH_GID" "$GSECRET" 2>/dev/null && \
   pod unshare chmod 0440 "$GSECRET" 2>/dev/null && \
   pod unshare chgrp "$HIVE_LAUNCH_GID" "$GDIR" 2>/dev/null; then
  G_START_MODE="$(mode_of "$GSECRET")"
  G_START_GID="$(gid_of "$GSECRET")"
  printf '  host view       file mode %s, gid %s (container gid %s)\n' \
    "$G_START_MODE" "$G_START_GID" "$HIVE_LAUNCH_GID"

  ID_OUT="$(pod run --rm -v "${GDIR}:/mnt:ro,z" --user "$DEV_UID" \
    --entrypoint /bin/sh "$IMAGE" -c 'id' 2>&1)"
  printf '  container view  %s\n' "$ID_OUT"
  if printf '%s' "$ID_OUT" | grep -q "$HIVE_LAUNCH_GID"; then
    ok "dev carries the hive-launch group inside the container"
  else
    bad "dev carries the hive-launch group inside the container" "id said: ${ID_OUT}"
  fi

  # Reads $GDIR rather than $FIXTURE, so the directory under test is the one
  # whose mode we are varying.
  read_gsecret() {
    local flag="$1" vol="${GDIR}:/mnt:ro" mark
    [[ -n "$flag" ]] && vol="${vol},${flag}"
    mark="$(mark_now)"
    READ_OUT="$(pod run --rm -v "$vol" --user "$DEV_UID" \
      --entrypoint /bin/sh "$IMAGE" -c 'cat /mnt/github-app.pem' 2>&1)"
    READ_RC=$?
    LAST_MARK="$mark"
  }

  # (a) The directory mode #4209's preflight actually advises for secrets/.
  # SELinux is satisfied here (:z has relabelled), so anything that fails is
  # DAC — and it does fail, because dev is not the owner and 0700 grants the
  # group nothing. This is a real trap in the shipped remediation, not a
  # property of SELinux.
  printf '  -- dir 0700 (the mode the preflight advises), :z, as dev --\n'
  chmod 0700 "$GDIR"
  read_gsecret "z"
  if denied; then
    ok "0700 secrets dir denies dev even with :z — the block is DAC traversal, not SELinux"
  else
    bad "0700 secrets dir denies dev even with :z" "read returned: ${READ_OUT}"
  fi
  collect_avc "$LAST_MARK" none
  if [[ -z "$AVC_OUT" ]]; then
    ok "and no AVC accompanies it, confirming SELinux was not the refuser"
  else
    note "an AVC did appear, so this denial is not purely DAC:"
    summarise_avc
  fi

  # (b) The directory mode fsGroup produces. DAC now permits; SELinux is the
  # only thing left, so this denial is purely MAC.
  printf '  -- dir 0750 group %s (what fsGroup gives), UNLABELED, as dev --\n' "$HIVE_LAUNCH_GID"
  chmod 0750 "$GDIR"
  pod unshare chcon -t user_tmp_t "$GSECRET" >/dev/null 2>&1
  pod unshare chcon -t user_tmp_t "$GDIR" >/dev/null 2>&1
  read_gsecret ""
  if denied; then
    ok "0440 group-readable is denied unlabeled — DAC permitting is not enough"
  else
    bad "0440 group-readable is denied unlabeled" "read returned: ${READ_OUT}"
  fi
  collect_avc "$LAST_MARK" some
  if [[ -n "$AVC_OUT" ]]; then
    ok "the MAC denial is recorded in the audit log"
  else
    bad "the MAC denial is recorded in the audit log" "no container-domain AVC appeared"
  fi
  summarise_avc

  # (c) Both permitting: the combination the deployment needs.
  for flag in z Z; do
    printf '  -- dir 0750 group %s, :%s, as dev --\n' "$HIVE_LAUNCH_GID" "$flag"
    read_gsecret "$flag"
    if [[ "$READ_OUT" == "not a real key" ]]; then
      ok ":${flag} + a traversable dir lets dev read the 0440 secret via the supplementary group"
    else
      bad ":${flag} + a traversable dir lets dev read the 0440 secret" "read returned: ${READ_OUT}"
    fi
    if [[ "$(mode_of "$GSECRET")" == "$G_START_MODE" ]]; then
      ok ":${flag} did not widen the 0440 secret (still 0$(mode_of "$GSECRET"))"
    else
      bad ":${flag} did not widen the 0440 secret" "mode went ${G_START_MODE} -> $(mode_of "$GSECRET")"
    fi
    if [[ "$(gid_of "$GSECRET")" == "$G_START_GID" ]]; then
      ok ":${flag} left the group ownership alone"
    else
      bad ":${flag} left the group ownership alone" "gid went ${G_START_GID} -> $(gid_of "$GSECRET")"
    fi
  done
else
  bad "the 0440 hive-launch fixture could be built" \
      "podman unshare chgrp/chmod failed; the group-secret case is UNMEASURED, not passing"
fi

# ── #4209's preflight, on an enforcing host ─────────────────────────────────
if [[ "$RUN_PREFLIGHT" == "true" ]]; then
  printf -- '\n--- bin/hive-podman-preflight-host.sh under Enforcing ---\n'
  PF="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/bin/hive-podman-preflight-host.sh"
  if [[ ! -x "$PF" ]]; then
    bad "#4209's preflight is present and executable" "not found at ${PF}"
  else

    PF_SRC="$(cd "$(dirname "$PF")/../src" && pwd)"
    PF_BEFORE="$(label_of "${PF_SRC}/hive.yaml" 2>/dev/null)|$(mode_of "${PF_SRC}/hive.yaml" 2>/dev/null)"
    PF_OUT="$(HIVE_DEPLOY_RUNTIME=podman "$PF" 2>&1)"; PF_RC=$?
    printf '%s\n' "$PF_OUT" | sed 's/^/        /'
    printf '  exit %d\n' "$PF_RC"
    PF_AFTER="$(label_of "${PF_SRC}/hive.yaml" 2>/dev/null)|$(mode_of "${PF_SRC}/hive.yaml" 2>/dev/null)"
    if printf '%s' "$PF_OUT" | grep -qi 'enforcing'; then
      ok "the preflight detected the enforcing mode"
    else
      bad "the preflight detected the enforcing mode" "no mention of Enforcing in its output"
    fi
    if [[ "$PF_BEFORE" == "$PF_AFTER" ]]; then
      ok "the preflight changed no label and no mode on its own inputs (it is read-only)"
    else
      bad "the preflight is read-only" "hive.yaml went ${PF_BEFORE} -> ${PF_AFTER}"
    fi
    if printf '%s' "$PF_OUT" | grep -qiE 'setenforce|label=disable|--privileged|permissive'; then
      bad "the preflight never advises disabling SELinux" \
          "its output mentions a mode-weakening remedy: $(printf '%s' "$PF_OUT" | grep -iE 'setenforce|label=disable|--privileged|permissive' | head -2)"
    else
      ok "the preflight advised no SELinux-weakening remedy"
    fi
    if printf '%s' "$PF_OUT" | grep -qE 'chmod (o\+|a\+|[0-7]*[4-7][4-7]?7)'; then
      bad "the preflight never advises widening a secret" \
          "$(printf '%s' "$PF_OUT" | grep -E 'chmod' | head -2)"
    else
      ok "the preflight advised no widening chmod"
    fi
  fi
fi

# ── Summary and the ledger row ──────────────────────────────────────────────
printf -- '\n--- AVC totals ---\n'
FINAL_ALL="$(avc_since 'today')"
FINAL_N="$(printf '%s' "$FINAL_ALL" | grep -c 'avc:  denied' || true)"
FINAL_CONTAINER="$(printf '%s\n' "$FINAL_ALL" | grep -cE 'scontext=[^ ]*:container_' || true)"
printf '  denials today: %s -> %s (container-domain: %s -> %s)\n' \
  "$BASELINE_N" "$FINAL_N" "$BASELINE_CONTAINER" "$FINAL_CONTAINER"
printf '  MCS category denial audited: %s\n' "$MCS_AUDITED"

printf '\nSUMMARY: %d failure(s)\n' "$failures"
printf '\nLedger row for src/docs/podman-selinux-avc-evidence.md:\n'
printf '| <release> | %s | %s | %s | container AVCs %s->%s, MCS audited=%s | %s |\n' \
  "$(date '+%Y-%m-%d')" \
  "$([[ $failures -eq 0 ]] && echo PASS || echo FAIL)" \
  "$(pod info --format 'podman {{.Version.Version}} rootless={{.Host.Security.Rootless}}')" \
  "$BASELINE_CONTAINER" "$FINAL_CONTAINER" "$MCS_AUDITED" \
  "$(pod image inspect "$IMAGE" --format '{{.Digest}}' 2>/dev/null | cut -c1-19)"

[[ $failures -eq 0 ]] || exit 1
exit 0
