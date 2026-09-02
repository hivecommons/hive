#!/usr/bin/env bash
# probe_podman_volume_persistence.sh — what the hive-data named volume actually
# guarantees under SELinux enforcing (kubestellar/hive#4376).
#
# #4354 shipped src/deploy/quadlet/hive-data.volume and said persistence and
# SELinux volume semantics were "deliberately NOT characterised here — that is
# its own slice". This is that slice.
#
# It is NOT a re-run of #4337/#4357. Those established the :z/:Z/unlabeled
# matrix for BIND MOUNTS and recorded the kernel's own AVC records per case;
# src/docs/podman-selinux-avc-evidence.md is the evidence and is not superseded.
# What was never measured is the NAMED VOLUME: podman labels it itself, without
# being asked, and the label it chooses is what decides whether a recreated
# container can still read the data. The bind cases here exist only as the
# contrast that makes the volume result meaningful.
#
# Cases:
#   static     the shipped units carry no relabel suffix on the volume line and
#              DO carry :Z on the config/secret bind mounts   (runs anywhere)
#   label      a fresh named volume's _data label, and whether an MCS category
#              is applied                                     -> expect none
#   copyup     first use chowns the volume root to the image's /data owner and
#              copies the image's directory into it; label unchanged
#   persist    data written through the volume survives the container being
#              REMOVED, and a second, differently-categorised container reads it
#   Zpoison    :Z on the NAMED VOLUME stamps a private category; a later mount
#              WITHOUT the flag is then denied. The footgun this probe exists
#              to make visible. Restored with :z before the case ends.
#   bind       control: a fresh directory is user_tmp_t and is denied with no
#              flag; :z grants with no category; :Z grants with one
#   envfile    EnvironmentFile= is read by podman on the HOST and is never
#              relabeled — it is not a mount and needs no flag
#
# STORAGE SAFETY. This probe deliberately does NOT pin --root/--runroot the way
# the #4357 probe does, and the reason is worth stating rather than looking like
# an omission: the thing being characterised is what podman does to a volume in
# a real store, and a throwaway store would also mean re-pulling a 3.8 GB image
# to say anything. The safety property is name discipline instead — every object
# is prefixed `hive-volprobe-<pid>-`, the probe REFUSES TO START if any of those
# names already exists, and cleanup removes exactly the names it created.
# Nothing is pruned and no pre-existing volume is read, written, or removed.
# Pass --store to run against an isolated store anyway.
#
# :z and :Z relabel the host directory they point at and the relabel outlives
# the container, so every bind-mount source is a fixture created under /var/tmp
# by this probe and removed on exit. No checkout, no real secrets directory,
# nothing under $HOME.
#
# NON-GOALS. No setenforce, no --security-opt label=disable, no --privileged, no
# chmod that widens anything. A case that fails under enforcing is recorded as a
# failure. Not in scope: backup/restore and migration (#4188 bullet 8), reboot
# persistence (bullet 6), and rootful — see the doc's "not executed" list.
#
# Exit codes:
#   0  every case matched what src/docs/podman-volume-persistence.md records
#   1  at least one case did not match
#  78  not qualifiable here (not enforcing, or a prerequisite is missing)

set -uo pipefail

IMAGE="${IMAGE:-ghcr.io/hivecommons/hive:stable}"
PREFIX="hive-volprobe-$$"
STORE="${STORE:-}"
KEEP="false"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
failures=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE="${2:?--image needs a value}"; shift 2 ;;
    --store) STORE="${2:?--store needs a value}"; shift 2 ;;
    --keep)  KEEP="true"; shift ;;
    -h|--help) sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 64 ;;
  esac
done

not_qualifiable() {
  printf '\nNOT QUALIFIABLE HERE: %s\n' "$1" >&2
  printf 'No result is reported. See src/docs/podman-volume-persistence.md.\n' >&2
  exit 78
}
ok()   { printf '  PASS: %s\n' "$1"; }
bad()  { printf '  FAIL: %s\n' "$1"; [[ $# -gt 1 ]] && printf '        %s\n' "$2"; failures=$((failures + 1)); }
note() { printf '  ..    %s\n' "$1"; }
head2(){ printf -- '\n--- %s ---\n' "$1"; }

# ── The one section that runs anywhere ──────────────────────────────────────
# Everything below this needs an enforcing host with podman. This does not, and
# it is the assertion an operator is most likely to break by hand, so it runs
# before the stop condition rather than after it.
UNIT_DIR="${REPO_ROOT}/src/deploy/quadlet"
printf '=== Podman volume persistence and SELinux behaviour (#4376) ===\n'
head2 "static: the shipped units (no podman required)"

unit_values() {
  local file="$1" key="$2"
  [[ -f "$file" ]] || return 0
  grep -v '^[[:space:]]*[#;]' "$file" \
    | grep -E "^[[:space:]]*${key}[[:space:]]*=" \
    | sed -E "s/^[[:space:]]*${key}[[:space:]]*=[[:space:]]*//"
}

HIVE_UNIT="${UNIT_DIR}/hive.container"
if [[ ! -f "$HIVE_UNIT" ]]; then
  bad "src/deploy/quadlet/hive.container exists" "not found at ${HIVE_UNIT}"
else
  # The named-volume line must carry NO relabel suffix. :Z stamps a private MCS
  # category on the volume's contents, and the Zpoison case below shows what
  # that costs the moment anything mounts it without the flag.
  vol_line="$(unit_values "$HIVE_UNIT" "Volume" | grep '^hive-data.volume:' || true)"
  if [[ -z "$vol_line" ]]; then
    bad "hive.container mounts the hive-data volume by unit name" "no 'Volume=hive-data.volume:...' line found"
  elif [[ "$vol_line" == *:z* || "$vol_line" == *:Z* ]]; then
    bad "the hive-data volume line carries no :z/:Z suffix" \
        "got '${vol_line}' — podman already labels a named volume container_file_t:s0 with no category. Adding :Z stamps a private category and the next container that mounts it without the flag is denied, silently (#4357 finding 3)."
  else
    ok "the hive-data volume line carries no :z/:Z suffix"
  fi

  # The bind mounts are the opposite case: a host directory keeps whatever
  # label it had, and the container cannot read it without one of the flags.
  for target in "/etc/hive/hive.yaml" "/secrets"; do
    line="$(unit_values "$HIVE_UNIT" "Volume" | grep ":${target}:" || true)"
    if [[ -z "$line" ]]; then
      bad "hive.container bind-mounts ${target}" "no Volume= line with that target"
    elif [[ "$line" == *,Z* || "$line" == *:Z* ]]; then
      ok "the ${target} bind mount carries :Z (private), not :z"
    else
      bad "the ${target} bind mount carries :Z (private), not :z" \
          "got '${line}' — :z is the SHARED label; these are one container's config and secrets (#4357)"
    fi
  done
fi

# ── Stop condition ──────────────────────────────────────────────────────────
head2 "prerequisites"
command -v podman >/dev/null 2>&1 || not_qualifiable "podman is not installed or not on PATH"
[[ -d /sys/fs/selinux ]] || not_qualifiable "/sys/fs/selinux is absent — this host has no SELinux (see #4211)"
command -v getenforce >/dev/null 2>&1 || not_qualifiable "getenforce is absent — cannot establish the mode"
MODE="$(getenforce 2>/dev/null)"
[[ "$MODE" == "Enforcing" ]] || \
  not_qualifiable "SELinux mode is ${MODE:-unknown}, not Enforcing. Do not setenforce 1 to make this run; record it unexecuted."
ok "SELinux is Enforcing"

# ── A label reader that is actually a label reader (#4357 finding 1) ────────
# uutils `stat` prints "unsupported for this operating system" to STDOUT and
# exits 0 for -c '%C', and uutils `ls -Z` prints no context at all. That string
# would then be compared against container_file_t as if it were a label. The
# reader is resolved by trying it on a known file rather than assumed.
LABEL_READER=""
for cand in "/usr/bin/stat -c %C" "stat -c %C" "getfattr -n security.selinux --only-values --absolute-names"; do
  out="$($cand /etc/passwd 2>/dev/null)" || continue
  [[ "$out" == *:object_r:* ]] || continue
  LABEL_READER="$cand"; break
done
[[ -n "$LABEL_READER" ]] || not_qualifiable "no working SELinux label reader (tried stat -c %C and getfattr) — see #4357 finding 1"
label_of() { $LABEL_READER "$1" 2>/dev/null; }
# Only the LEVEL field decides whether a private MCS category is present. A
# naive *:c* over the whole context matches the ":container_file_t" in the type
# field and calls every label private.
mcs_of()       { printf '%s' "${1#*:*:*:}"; }
type_of()      { local c="${1#*:}"; c="${c#*:}"; printf '%s' "${c%%:*}"; }
has_category() { [[ "$(mcs_of "$1")" == *:c* ]]; }
ok "label reader: ${LABEL_READER}"

# ── Storage: name discipline, not store isolation. See the header. ──────────
POD=(podman)
if [[ -n "$STORE" ]]; then
  case "$STORE" in
    /var/lib/containers*|/run/containers*)
      not_qualifiable "refusing --store ${STORE}: that is the host's system container store" ;;
  esac
  mkdir -p "${STORE}/graph" "${STORE}/run"
  POD=(podman --root "${STORE}/graph" --runroot "${STORE}/run")
  note "isolated store: ${STORE}"
fi
pod() { "${POD[@]}" "$@"; }

VOL="${PREFIX}-data"
BINDVOL="${PREFIX}-bind"
# Refuse to start rather than risk addressing something that is not ours.
if pod volume exists "$VOL" 2>/dev/null; then
  not_qualifiable "a volume named ${VOL} already exists — this probe only ever addresses names it created"
fi
for n in "${PREFIX}-c1" "${PREFIX}-c2" "${PREFIX}-c3"; do
  pod container exists "$n" 2>/dev/null && not_qualifiable "a container named ${n} already exists"
done
pod image exists "$IMAGE" >/dev/null 2>&1 || not_qualifiable "image ${IMAGE} is not present in this store; pull it first (this probe does not pull)"

FIXTURE="$(mktemp -d -p /var/tmp "${PREFIX}-fixture-XXXXXX")" \
  || not_qualifiable "cannot create a fixture directory under /var/tmp"

# shellcheck disable=SC2329 # invoked through the EXIT trap below
cleanup() {
  [[ "$KEEP" == "true" ]] && { printf '\n--keep: left %s and volume %s in place\n' "$FIXTURE" "$VOL"; return; }
  local n
  for n in "${PREFIX}-c1" "${PREFIX}-c2" "${PREFIX}-c3"; do
    pod rm -f "$n" >/dev/null 2>&1
  done
  pod volume rm "$VOL" >/dev/null 2>&1
  pod volume rm "$BINDVOL" >/dev/null 2>&1
  rm -rf "$FIXTURE" "${FIXTURE}-env" 2>/dev/null || podman unshare rm -rf "$FIXTURE" "${FIXTURE}-env" 2>/dev/null
}
trap cleanup EXIT

# Runs a throwaway container over the volume. Every invocation gets a FRESH MCS
# category from podman, which is the whole point: a recreated Hive container is
# a differently-categorised container reading the same bytes.
in_vol() { pod run --rm -v "${VOL}:/data" --entrypoint /bin/sh "$IMAGE" -c "$1" 2>&1; }

# ── label ───────────────────────────────────────────────────────────────────
head2 "label: what podman puts on a fresh named volume"
pod volume create "$VOL" >/dev/null || { bad "create the probe volume"; exit 1; }
MP="$(pod volume inspect "$VOL" --format '{{.Mountpoint}}')"
vol_label="$(label_of "$MP")"
note "${LABEL_READER} ${MP}"
note "  -> ${vol_label}"

if [[ "$(type_of "$vol_label")" == "container_file_t" ]]; then
  ok "the volume's _data directory is container_file_t"
else
  bad "the volume's _data directory is container_file_t" "got type '$(type_of "$vol_label")' in ${vol_label}"
fi
if has_category "$vol_label"; then
  bad "no MCS category is applied to the volume" \
      "got level '$(mcs_of "$vol_label")' — a category here means only the container that stamped it can read the data"
else
  ok "no MCS category is applied to the volume (level $(mcs_of "$vol_label"))"
fi

# It is podman that does this, not inheritance: the parent directory carries the
# home-directory type, and a directory created by hand beside the volume keeps
# it. Without this the result reads as an accident of where volumes happen to
# live.
parent_label="$(label_of "$(dirname "$MP")")"
if [[ "$(type_of "$parent_label")" == "container_file_t" ]]; then
  note "parent is also container_file_t (${parent_label}) — provenance not separable on this host"
else
  ok "the label is applied by podman, not inherited: parent is $(type_of "$parent_label")"
fi

# ── copyup ──────────────────────────────────────────────────────────────────
head2 "copyup: first use"
before_owner="$(/usr/bin/stat -c '%u:%g' "$MP" 2>/dev/null)"
in_vol 'true' >/dev/null
after_owner="$(/usr/bin/stat -c '%u:%g' "$MP" 2>/dev/null)"
after_label="$(label_of "$MP")"
img_owner="$(pod run --rm --entrypoint /bin/sh "$IMAGE" -c 'stat -c "%u:%g" /data' 2>/dev/null | tr -d '\r\n')"
note "volume root owner before first use: ${before_owner}; after: ${after_owner}"
note "image /data owner (container view): ${img_owner}"
if [[ "$before_owner" != "$after_owner" ]]; then
  ok "first use chowned the volume root to match the image's /data (copy-up)"
else
  note "volume root owner unchanged by first use — the image's /data may already match"
fi
if [[ "$after_label" == "$vol_label" ]]; then
  ok "first use did not change the volume's label"
else
  bad "first use did not change the volume's label" "before ${vol_label}, after ${after_label}"
fi

# ── persist ─────────────────────────────────────────────────────────────────
head2 "persist: across container removal and recreation"
PAYLOAD="persisted-4376-$(date -u +%s)"
d1="$(pod run --name "${PREFIX}-c1" -v "${VOL}:/data" --entrypoint /bin/sh "$IMAGE" \
        -c "cat /proc/self/attr/current; printf '%s\n' '${PAYLOAD}' > /data/probe.txt" 2>&1 | tr -d '\0\n')"
note "writer domain:  ${d1}"
pod rm -f "${PREFIX}-c1" >/dev/null 2>&1
if pod container exists "${PREFIX}-c1" 2>/dev/null; then
  bad "the writing container was removed" "${PREFIX}-c1 still exists"
else
  ok "the writing container was removed (podman rm)"
fi
if pod volume exists "$VOL" 2>/dev/null; then
  ok "the named volume survived the container's removal"
else
  bad "the named volume survived the container's removal" "volume ${VOL} is gone"
fi

# /proc/self/attr/current is NUL-terminated and carries no newline, so the two
# values are separated explicitly rather than by line position.
readback="$(in_vol 'tr -d "\0" < /proc/self/attr/current; printf "\n@@\n"; cat /data/probe.txt')"
d2="$(printf '%s' "$readback" | sed -n '1p' | tr -d '\r')"
got="$(printf '%s' "$readback" | sed -n '/^@@$/,$p' | tail -n1 | tr -d '\r')"
note "reader domain:  ${d2}"
if [[ "$got" == "$PAYLOAD" ]]; then
  ok "a NEW container read the data back byte-for-byte"
else
  bad "a NEW container read the data back byte-for-byte" "expected '${PAYLOAD}', got '${got}'"
fi
if [[ "$d1" != "$d2" ]]; then
  ok "the two containers had DIFFERENT MCS categories — the read is not category-matched luck"
else
  note "the two containers happened to share a category; the sharing property is untested by this case"
fi

# ── Zpoison ─────────────────────────────────────────────────────────────────
head2 "Zpoison: what :Z on a NAMED volume costs"
pod run --rm -v "${VOL}:/data:Z" --entrypoint /bin/sh "$IMAGE" -c 'true' >/dev/null 2>&1
z_label="$(label_of "$MP")"
note "after one :Z mount: ${z_label}"
if has_category "$z_label"; then
  ok ":Z stamped a private MCS category on the volume ($(mcs_of "$z_label"))"
else
  bad ":Z stamped a private MCS category on the volume" "level is still $(mcs_of "$z_label")"
fi
denied="$(in_vol 'cat /data/probe.txt')"
if [[ "$denied" == *"Permission denied"* ]]; then
  ok "a later mount WITHOUT the flag is then denied: ${denied##*: }"
  note "this is the silent one — #4357 finding 3 measured that a category denial records NO AVC"
else
  bad "a later mount WITHOUT the flag is then denied" "got '${denied}' — expected Permission denied"
fi
# Restore the shared label, so the volume this probe created is left in the
# state the shipped unit produces rather than in the poisoned one.
pod run --rm -v "${VOL}:/data:z" --entrypoint /bin/sh "$IMAGE" -c 'true' >/dev/null 2>&1
restored="$(label_of "$MP")"
if has_category "$restored"; then
  bad ":z restored the shared label" "still ${restored}"
else
  ok ":z restored the shared label (${restored}) and the no-flag read works again"
fi
if [[ "$(in_vol 'cat /data/probe.txt' | tail -n1 | tr -d '\r')" == "$PAYLOAD" ]]; then
  ok "the data itself was never at risk — only its label changed"
else
  bad "the data itself was never at risk — only its label changed" "read-back failed after the relabel round trip"
fi

# ── bind ────────────────────────────────────────────────────────────────────
head2 "bind: the contrast (#4357 measured this matrix; repeated here only as the contrast)"
printf 'bind-fixture\n' > "${FIXTURE}/f"
fresh_label="$(label_of "${FIXTURE}/f")"
note "a fresh /var/tmp file: ${fresh_label}"
if [[ "$(type_of "$fresh_label")" != "container_file_t" ]]; then
  ok "a bind source is NOT container_file_t by default (it is $(type_of "$fresh_label"))"
else
  note "this fixture is already container_file_t; the no-flag case below will not be a control"
fi
out="$(pod run --rm -v "${FIXTURE}:/mnt:ro" --entrypoint /bin/sh "$IMAGE" -c 'cat /mnt/f' 2>&1)"
if [[ "$out" == *"Permission denied"* ]]; then
  ok "no flag: denied — a bind mount needs one, where the named volume needed none"
else
  bad "no flag: denied" "got '${out}'"
fi
pod run --rm -v "${FIXTURE}:/mnt:ro,z" --entrypoint /bin/sh "$IMAGE" -c 'cat /mnt/f' >/dev/null 2>&1
z_bind="$(label_of "${FIXTURE}/f")"
if [[ "$(type_of "$z_bind")" == "container_file_t" ]] && ! has_category "$z_bind"; then
  ok ":z relabels the bind source to the SHARED label (${z_bind})"
else
  bad ":z relabels the bind source to the SHARED label" "got ${z_bind}"
fi
pod run --rm -v "${FIXTURE}:/mnt:ro,Z" --entrypoint /bin/sh "$IMAGE" -c 'cat /mnt/f' >/dev/null 2>&1
Z_bind="$(label_of "${FIXTURE}/f")"
if has_category "$Z_bind"; then
  ok ":Z relabels the bind source PRIVATE to one container ($(mcs_of "$Z_bind"))"
else
  bad ":Z relabels the bind source PRIVATE to one container" "got ${Z_bind}"
fi
mode_now="$(/usr/bin/stat -c '%a' "${FIXTURE}/f" 2>/dev/null)"
if [[ "$mode_now" == "644" ]]; then
  ok "neither flag widened the file's mode (still ${mode_now})"
else
  bad "neither flag widened the file's mode" "mode is now ${mode_now}"
fi

# ── envfile ─────────────────────────────────────────────────────────────────
head2 "envfile: EnvironmentFile= is not a mount"
# Its own directory, deliberately: the bind cases above relabeled $FIXTURE, and a
# file created inside a relabeled directory inherits container_file_t from it.
# The point of this case is that an env file NEVER needs a container label, so it
# has to start without one.
ENVDIR="${FIXTURE}-env"
mkdir -p "$ENVDIR"
printf 'PROBE_KEY=probe-value\n' > "${ENVDIR}/probe.env"
env_before="$(label_of "${ENVDIR}/probe.env")"
note "the env file starts as: ${env_before}"
# shellcheck disable=SC2016 # $PROBE_KEY must expand INSIDE the container, not here
env_out="$(pod run --rm --env-file "${ENVDIR}/probe.env" --entrypoint /bin/sh "$IMAGE" -c 'printf "%s" "$PROBE_KEY"' 2>&1)"
env_after="$(label_of "${ENVDIR}/probe.env")"
if [[ "$env_out" == "probe-value" ]]; then
  ok "the container received the variable"
else
  bad "the container received the variable" "got '${env_out}'"
fi
if [[ "$env_before" == "$env_after" ]]; then
  ok "the env file's label was untouched (${env_after}) — podman reads it on the HOST, so it is not a mount and needs no flag"
else
  bad "the env file's label was untouched" "before ${env_before}, after ${env_after}"
fi

printf '\nSUMMARY: %d failure(s)\n' "$failures"
[[ "$failures" -eq 0 ]] || exit 1
exit 0
