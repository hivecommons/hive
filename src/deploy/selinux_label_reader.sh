# shellcheck shell=bash
# selinux_label_reader.sh — a label reader that is actually a label reader.
#
# Sourced, not executed. Shared by src/deploy/qualify_podman_selinux.sh and
# src/deploy/probe_podman_selinux_avc.sh (#4490); bin/hive-podman-preflight-host.sh
# carries its own copy of the same idea because its readers are contract-tested
# through HIVE_PFH_LABEL_READERS (#4359).
#
# `stat -c '%C'` is the obvious way to read an SELinux label and it cannot be
# trusted bare. On a host where uutils coreutils shadows GNU coreutils on PATH
# — the default on Bluefin/Universal Blue, i.e. exactly the Fedora-atomic class
# the SELinux lanes target — uutils `stat` does not implement %C. It prints
# "unsupported for this operating system" to STDOUT and exits 0, so neither the
# exit status nor a non-empty test detects it, and that sentence gets compared
# against container_file_t as if it were a label (#4490's FAIL (2) on a healthy
# enforcing host). So the reader is resolved by running each candidate against
# a known-labelled file and requiring the answer to be SHAPED like a context,
# never assumed from what `command -v` finds first.
#
# Overridable only so the contract test can present a host carrying broken or
# missing readers; operators have no reason to set it. Candidates are
# comma-separated because the commands themselves contain spaces.
HIVE_SELINUX_LABEL_READERS="${HIVE_SELINUX_LABEL_READERS:-stat -c %C,/usr/bin/stat -c %C,getfattr -n security.selinux --only-values --absolute-names}"

# Resolved by hive_selinux_resolve_label_reader: a reader command, or empty.
HIVE_SELINUX_LABEL_READER=""

# An SELinux context is user:role:type:level. Requiring the role field is what
# makes this a validator rather than a non-empty test — it is exactly the check
# the uutils sentence fails.
hive_selinux_looks_like_context() {
  [[ "$1" == *:object_r:* ]]
}

# Resolve a working reader against $1, a path known to carry a label on any
# SELinux host (the caller's own fixture). Sets HIVE_SELINUX_LABEL_READER and
# returns 0, or returns 1 when no candidate produces a context-shaped answer —
# in which case the caller must refuse to produce a verdict, because "I could
# not measure this" and "this host failed" must not read the same.
hive_selinux_resolve_label_reader() {
  local probe="$1" cand out candidates=() words=()
  IFS=',' read -r -a candidates <<<"$HIVE_SELINUX_LABEL_READERS"
  for cand in "${candidates[@]}"; do
    read -r -a words <<<"$cand"
    command -v "${words[0]}" >/dev/null 2>&1 || continue
    out="$("${words[@]}" "$probe" 2>/dev/null)" || continue
    hive_selinux_looks_like_context "$out" || continue
    HIVE_SELINUX_LABEL_READER="$cand"
    return 0
  done
  HIVE_SELINUX_LABEL_READER=""
  return 1
}

# The label of $1, read with the resolved reader. Empty until
# hive_selinux_resolve_label_reader has succeeded.
hive_selinux_label_of() {
  local words=()
  read -r -a words <<<"$HIVE_SELINUX_LABEL_READER"
  [[ "${#words[@]}" -gt 0 ]] || return 1
  "${words[@]}" "$1" 2>/dev/null
}
