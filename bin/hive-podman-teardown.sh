#!/usr/bin/env bash
# Teardown for standalone Hive under Podman (#4326).
#
# #4210 defined the ownership contract — the io.kubestellar.hive.* label set,
# the guard that rejects store-wide operations, and the plan that prints the
# only commands Hive is allowed to run. It deliberately stopped there: nothing
# performed a teardown through it. A cleanup path that is defined but not
# implemented is the dangerous state, because the guard only protects
# operations that go through it.
#
# This is that path, and it is built on the contract rather than beside it:
#
#   * Every resource is selected with hive_podman_filters — the ownership
#     label plus the deployment instance, and nothing else.
#   * Every command, read or destructive, is passed through
#     hive_podman_cleanup_check before it runs. A command the guard rejects
#     aborts the teardown; it is never "fixed up" and retried.
#   * Nothing here takes --all, and no prune appears anywhere. The removal
#     commands name the exact IDs the label filter returned.
#
# Consequence worth stating plainly: an unlabelled resource is invisible to
# this script. That is the point. On a workstation the same rootless store
# holds the operator's Distroboxes and unrelated development containers, and
# Hive teardown must not be able to see them, let alone remove them.
#
# Docker teardown is untouched. This script speaks only to Podman, and refuses
# to run when the deployment runtime is explicitly something else.
#
# Usage:
#   bin/hive-podman-teardown.sh plan            # print what would be removed
#   bin/hive-podman-teardown.sh run --yes       # remove it
#   bin/hive-podman-teardown.sh run --yes --images
#
# HIVE_DEPLOY_INSTANCE scopes the teardown to one deployment on the host and
# defaults to "default", exactly as it does for the labels themselves.

HIVE_TEARDOWN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# shellcheck source=bin/hive-podman-cleanup.sh
. "${HIVE_TEARDOWN_ROOT}/bin/hive-podman-cleanup.sh"

# Resource kinds, in removal order. Containers before pods, because removing a
# pod that still holds containers is a wider operation than removing the
# containers it was created with. Volumes and networks follow, once nothing
# references them. Images are last and opt-in.
#
# Fields: kind|list arguments|remove arguments|format
HIVE_TEARDOWN_KINDS=(
  "containers|ps --all|rm --force|{{.ID}}"
  "pods|pod ps|pod rm --force|{{.ID}}"
  "volumes|volume ls|volume rm|{{.Name}}"
  "networks|network ls|network rm|{{.Name}}"
)

# Images are shared by every deployment that pulled the same reference, so
# removing them is a separate decision from tearing down one deployment.
HIVE_TEARDOWN_IMAGE_KIND="images|images|rmi|{{.ID}}"

# Well-known Hive volume names, for the unlabelled-orphan report (#4485).
#
# Selection stays the ownership labels and nothing else — that contract is not
# weakened here. But `podman run -v hive-data:/data` auto-creates a missing
# named volume with NO labels, and a labelling failure used to read as "no
# Hive-owned volumes": the operator is told the deployment is gone while
# audit.jsonl and every byte of Hive state survive on disk. So a volume that
# carries one of Hive's well-known names but no ownership label is REPORTED,
# never removed: the output says it exists and that this teardown will not
# touch it, instead of saying something untrue.
HIVE_TEARDOWN_KNOWN_VOLUMES=(hive-data)

# The Quadlet-generated services that own the resources above, in STOP order —
# the reverse of the dependency order in which setup starts them (#4484).
#
# They must be stopped before their resources are removed, for two measured
# reasons. hive-network.service is a run-once unit that stays `active (exited)`
# after creating the network; removing the network underneath it leaves systemd
# believing the network exists, so the next install's `daemon-reload` + start
# skips creation and `podman run --network hive` fails with `network not
# found`. And the containers carry Restart=always / RestartSec=30, so
# `podman rm --force` on a container whose unit is still running invites
# systemd to recreate it 30 seconds later — or, with the network already gone,
# to retry `network not found` forever, since nothing limits the restarts.
#
# These names exist only for the default instance: the Quadlet units in
# src/deploy/quadlet/ hard-code io.kubestellar.hive.instance=default, so a
# teardown scoped to another instance leaves them alone.
HIVE_TEARDOWN_UNITS=(
  hive-gateway.service
  hive.service
  hive-data-volume.service
  hive-network.service
)

hive_podman_teardown_usage() {
  cat <<'EOF'
Usage: hive-podman-teardown.sh plan [--images]
       hive-podman-teardown.sh run --yes [--images]

plan   Print the scoped commands and the Hive-owned resources they would
       remove. Removes nothing.
run    Perform the teardown. Requires --yes, because it deletes data.

--images  Also remove Hive-labelled images. Off by default: an image is shared
          with every other deployment that pulled the same reference.

HIVE_DEPLOY_INSTANCE selects which deployment on this host to tear down. It
defaults to "default".

Only Hive-owned resources are visible to this command. Selection is the
ownership label set from #4210 and nothing else, so an unlabelled container,
pod, volume, or network cannot be reached from here. A volume carrying one of
Hive's well-known names WITHOUT the labels is reported, never removed (#4485),
so a labelling failure cannot read as "no Hive-owned volumes".

Before removing anything, run stops the Quadlet-generated services that own
the resources (hive-gateway, hive, hive-data-volume, hive-network), in the
reverse of the order setup starts them, and clears their failed state. The
unit FILES and the configuration under ~/.config/hive stay installed: a later
`systemctl daemon-reload` + start (or bin/hive-podman-setup.sh) recreates the
deployment from them.
EOF
}

# The standalone runtime selector (#4205) defaults to Docker. This script is
# the Podman teardown; if an operator has explicitly selected another runtime,
# refuse rather than act on a store they did not mean to touch.
hive_podman_teardown_runtime_guard() {
  local runtime="${HIVE_DEPLOY_RUNTIME:-}"

  if [[ -n "$runtime" && "$runtime" != "podman" ]]; then
    printf 'ERROR: HIVE_DEPLOY_RUNTIME=%q selects a different runtime; this is the Podman teardown.\n' \
      "$runtime" >&2
    printf 'HINT: Docker deployments are torn down through their own Compose path, which this script does not touch.\n' >&2
    return 64
  fi

  if ! command -v podman >/dev/null 2>&1; then
    printf 'ERROR: podman is not installed or not in PATH\n' >&2
    return 127
  fi
}

# Runs one command after the ownership guard has approved it.
#
# The guard is the enforcement, not a second opinion: a command it rejects
# aborts the teardown with 70, because a rejected command means this script
# constructed something outside the contract and the bug is here.
_hive_podman_teardown_exec() {
  local -a command=("$@")

  if ! hive_podman_cleanup_check "${command[@]}"; then
    printf 'ERROR: ownership guard rejected a command this teardown constructed: %s\n' \
      "${command[*]}" >&2
    printf 'ERROR: aborting the teardown rather than running it.\n' >&2
    return 70
  fi

  if [[ "${HIVE_TEARDOWN_APPLY:-no}" != "yes" ]]; then
    printf 'would run: %s\n' "${command[*]}"
    return 0
  fi

  printf 'running: %s\n' "${command[*]}" >&2
  "${command[@]}"
}

# Lists the Hive-owned resources of one kind, one per line.
#
# Listing is a read, but it still goes through the guard: the filters are the
# only thing standing between this and the operator's other containers, and a
# listing that lost its filter would feed unowned IDs straight into removal.
_hive_podman_teardown_list() {
  local format="$1"
  local -a list_words=()
  read -r -a list_words <<<"$2"

  # Command substitution rather than a process substitution, so a failure
  # building the filters is an error here and not an empty, unfiltered list.
  local filter_lines
  filter_lines="$(hive_podman_filters)" || return

  local -a filters=()
  local line
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    filters+=("$line")
  done <<<"$filter_lines"

  local -a command=(podman "${list_words[@]}" "${filters[@]}" --format "$format")

  if ! hive_podman_cleanup_check "${command[@]}"; then
    printf 'ERROR: ownership guard rejected a listing this teardown constructed: %s\n' \
      "${command[*]}" >&2
    return 70
  fi

  "${command[@]}"
}

# Reports well-known Hive volume names that exist WITHOUT the ownership labels
# (#4485). A read of exact names, never a removal: an unlabelled volume is
# outside the #4210 selection contract and stays outside it — but its
# existence is stated rather than folded into "no Hive-owned volumes".
#
# The arguments are the labelled volume names the filter already returned;
# those are Hive's and need no report. A volume that carries the ownership
# marker under a DIFFERENT instance is another deployment's and is skipped
# too: its own teardown selects it.
_hive_podman_teardown_report_unlabelled_volumes() {
  local -a labelled=("$@")
  local name known labelled_name labels
  local -a command=()

  for name in "${HIVE_TEARDOWN_KNOWN_VOLUMES[@]}"; do
    known="no"
    for labelled_name in "${labelled[@]}"; do
      [[ "$labelled_name" == "$name" ]] && known="yes"
    done
    [[ "$known" == "yes" ]] && continue

    command=(podman volume inspect "$name" --format '{{.Labels}}')
    if ! hive_podman_cleanup_check "${command[@]}"; then
      printf 'ERROR: ownership guard rejected an inspection this teardown constructed: %s\n' \
        "${command[*]}" >&2
      return 70
    fi

    # A missing volume is the expected case and not an error.
    labels="$("${command[@]}" 2>/dev/null)" || continue

    if [[ "$labels" == *"${HIVE_PODMAN_OWNED_LABEL_KEY}:${HIVE_PODMAN_OWNED_LABEL_VALUE}"* ]]; then
      continue
    fi

    printf 'WARNING: volume %s exists but carries no Hive ownership labels; this teardown will not touch it.\n' \
      "$name"
    printf 'WARNING: if it is a Hive volume that lost its labels (#4485), back it up first (src/docs/backup-restore.md), then remove it yourself: podman volume rm %s\n' \
      "$name"
  done
}

# Tears down one resource kind. Prints what it found; removes it only when
# HIVE_TEARDOWN_APPLY=yes.
_hive_podman_teardown_kind() {
  local kind="$1"
  local list_spec="$2"
  local remove_spec="$3"
  local format="$4"

  # Same reason as above: a guard rejection or an engine error while listing
  # must stop the teardown, not read as "nothing is labelled".
  local listed
  listed="$(_hive_podman_teardown_list "$format" "$list_spec")" || return

  local -a found=()
  local line
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    found+=("$line")
  done <<<"$listed"

  # Volumes get the unlabelled-orphan report (#4485) whether or not anything
  # labelled was found: the dangerous output is exactly "no Hive-owned
  # volumes" over a store where hive-data exists without its labels.
  if [[ "$kind" == "volumes" ]]; then
    _hive_podman_teardown_report_unlabelled_volumes "${found[@]}" || return
  fi

  if (( ${#found[@]} == 0 )); then
    printf 'no Hive-owned %s\n' "$kind"
    return 0
  fi

  printf '%s (%d): %s\n' "$kind" "${#found[@]}" "${found[*]}"

  local -a remove=()
  read -r -a remove <<<"$remove_spec"

  # The operands are the exact IDs the ownership filter returned. Removal is
  # never expressed as a prune or an --all, so there is no form of this command
  # that can reach a resource the filter did not name.
  _hive_podman_teardown_exec podman "${remove[@]}" "${found[@]}"
}

# systemd scope follows who is running the teardown, exactly as the resources
# do: a rootless deployment lives in the user manager, a rootful one in the
# system manager. Same convention as bin/hive-podman-setup.sh, which the
# operator ran with the matching privilege to install these units.
_hive_podman_teardown_sctl() {
  if [[ "$(id -u)" -eq 0 ]]; then
    systemctl "$@"
  else
    systemctl --user "$@"
  fi
}

_hive_podman_teardown_sctl_label() {
  if [[ "$(id -u)" -eq 0 ]]; then
    printf 'systemctl'
  else
    printf 'systemctl --user'
  fi
}

# Stops the Quadlet-generated services before their resources are removed
# (#4484). See HIVE_TEARDOWN_UNITS for why the order is units-first.
#
# Idempotent by construction: a unit that is not loaded — never installed, or
# already gone from an earlier teardown — is reported and skipped, never an
# error. A unit that IS loaded but refuses to stop aborts the teardown, because
# removing a resource from underneath a still-running unit is exactly the state
# this phase exists to prevent.
hive_podman_teardown_units() {
  local instance="$1"
  local sctl_label unit load_state

  if [[ "$instance" != "default" ]]; then
    printf 'instance %s: the Quadlet units belong to the default instance; leaving them alone\n' \
      "$instance"
    return 0
  fi

  if ! command -v systemctl >/dev/null 2>&1; then
    printf 'systemctl not found: no systemd units to stop on this host\n'
    return 0
  fi

  sctl_label="$(_hive_podman_teardown_sctl_label)"

  if [[ "${HIVE_TEARDOWN_APPLY:-no}" != "yes" ]]; then
    for unit in "${HIVE_TEARDOWN_UNITS[@]}"; do
      printf 'would run: %s stop %s (if loaded), then reset-failed\n' "$sctl_label" "$unit"
    done
    return 0
  fi

  for unit in "${HIVE_TEARDOWN_UNITS[@]}"; do
    load_state="$(_hive_podman_teardown_sctl show -p LoadState --value "$unit" 2>/dev/null)" \
      || load_state=""
    if [[ -z "$load_state" || "$load_state" == "not-found" || "$load_state" == "masked" ]]; then
      printf 'unit %s is not loaded; nothing to stop\n' "$unit"
      continue
    fi

    printf 'running: %s stop %s\n' "$sctl_label" "$unit" >&2
    if ! _hive_podman_teardown_sctl stop "$unit"; then
      printf 'ERROR: %s stop %s failed; refusing to remove resources the unit still owns.\n' \
        "$sctl_label" "$unit" >&2
      printf 'HINT: a resource removed underneath a running unit is how the next install breaks (#4484).\n' >&2
      return 69
    fi

    # A unit that failed while it was being torn down must not be left in the
    # `failed` state (or, with Restart=always, retrying forever against
    # resources this script is about to remove).
    _hive_podman_teardown_sctl reset-failed "$unit" >/dev/null 2>&1 || true
    printf 'stopped %s\n' "$unit"
  done
}

hive_podman_teardown_run() {
  local include_images="${1:-no}"
  local instance
  local entry

  instance="$(hive_podman_instance)" || return
  hive_podman_teardown_runtime_guard || return

  printf 'Hive Podman teardown — instance %s, selecting %s\n' \
    "$instance" "$HIVE_PODMAN_OWNED_LABEL"

  if [[ "${HIVE_TEARDOWN_APPLY:-no}" != "yes" ]]; then
    printf '(plan only — nothing is removed)\n'
  fi

  # Units first: the resources below are owned by these units, and removing a
  # resource from underneath its still-running unit is the #4484 bug.
  hive_podman_teardown_units "$instance" || return

  local -a kinds=("${HIVE_TEARDOWN_KINDS[@]}")
  [[ "$include_images" == "yes" ]] && kinds+=("$HIVE_TEARDOWN_IMAGE_KIND")

  local kind list_spec remove_spec format
  for entry in "${kinds[@]}"; do
    IFS='|' read -r kind list_spec remove_spec format <<<"$entry"
    _hive_podman_teardown_kind "$kind" "$list_spec" "$remove_spec" "$format" || return
  done
}

hive_podman_teardown_main() {
  local action="${1:-help}"
  local include_images="no"
  local confirmed="no"
  local argument

  case "$action" in
    plan|run)
      shift
      ;;
    -h|--help|help)
      hive_podman_teardown_usage
      return 0
      ;;
    *)
      printf 'ERROR: unknown action %q\n' "$action" >&2
      hive_podman_teardown_usage >&2
      return 64
      ;;
  esac

  for argument in "$@"; do
    case "$argument" in
      --images) include_images="yes" ;;
      --yes) confirmed="yes" ;;
      *)
        printf 'ERROR: unknown option %q\n' "$argument" >&2
        hive_podman_teardown_usage >&2
        return 64
        ;;
    esac
  done

  if [[ "$action" == "run" ]]; then
    if [[ "$confirmed" != "yes" ]]; then
      printf 'ERROR: run deletes Hive-owned containers, pods, volumes, and networks; pass --yes to confirm.\n' >&2
      printf 'HINT: bin/hive-podman-teardown.sh plan shows exactly what would go.\n' >&2
      return 64
    fi
    HIVE_TEARDOWN_APPLY="yes"
  else
    HIVE_TEARDOWN_APPLY="no"
  fi

  hive_podman_teardown_run "$include_images"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  hive_podman_teardown_main "$@"
fi
