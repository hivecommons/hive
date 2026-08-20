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
pod, volume, or network cannot be reached from here.
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
