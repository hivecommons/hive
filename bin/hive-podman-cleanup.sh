#!/usr/bin/env bash
# Hive-owned Podman resources and the safe cleanup contract (#4210).
#
# Rootless Podman and Buildah share one image/container store with whatever
# else the operator runs on the host: unrelated development containers,
# Distroboxes, and local builds. Hive lifecycle tooling therefore must never
# translate broad Docker cleanup into a global Podman operation. This file is
# the single source of truth for two things:
#
#   1. The stable labels that mark a resource as Hive-owned.
#   2. A guard that rejects any Podman/Buildah cleanup command that is not
#      restricted to those labels or to explicit Hive-owned operands.
#
# Scope: ownership and the cleanup contract only. Actually tearing resources
# down belongs to the later Podman lifecycle work, which must call
# hive_podman_cleanup_check before running anything destructive. Docker
# cleanup behavior is deliberately untouched by this contract.

# --- Ownership labels -------------------------------------------------------
#
# Applied at create/build time to every Hive-owned container, pod, network,
# volume, and image. Selected at cleanup time as filters. Reverse-DNS keys keep
# them from colliding with labels other tools put in the same shared store.

HIVE_PODMAN_OWNED_LABEL_KEY="io.kubestellar.hive.owned"
HIVE_PODMAN_OWNED_LABEL_VALUE="true"
HIVE_PODMAN_COMPONENT_LABEL_KEY="io.kubestellar.hive.component"
HIVE_PODMAN_INSTANCE_LABEL_KEY="io.kubestellar.hive.instance"
HIVE_PODMAN_RUNTIME_LABEL_KEY="io.kubestellar.hive.runtime"

# The one selector every cleanup command must carry.
HIVE_PODMAN_OWNED_LABEL="${HIVE_PODMAN_OWNED_LABEL_KEY}=${HIVE_PODMAN_OWNED_LABEL_VALUE}"

HIVE_PODMAN_INSTANCE_DEFAULT="default"

# Podman/Buildah global options that consume a separate value. They must be
# skipped when locating the subcommand, or `podman --root /tmp/x rm c1` parses
# as if `/tmp/x` were the verb.
HIVE_PODMAN_VALUE_OPTIONS=(
  --cgroup-manager --cni-config-dir --conmon --connection --events-backend
  --hooks-dir --identity --log-level --module --namespace --network-cmd-path
  --registries-conf --root --runroot --ssh --storage-driver --storage-opt
  --tmpdir --url -c -r
)

hive_podman_instance() {
  local instance="${HIVE_DEPLOY_INSTANCE:-$HIVE_PODMAN_INSTANCE_DEFAULT}"

  # The instance name lands inside label filters, so keep it to characters that
  # cannot smuggle a second filter term or a shell metacharacter.
  if [[ ! "$instance" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    printf 'ERROR: invalid HIVE_DEPLOY_INSTANCE=%q; expected [A-Za-z0-9][A-Za-z0-9._-]*\n' \
      "$instance" >&2
    return 64
  fi

  printf '%s\n' "$instance"
}

# Create/build-time labels, one CLI argument per line.
# Usage: hive_podman_labels <component>
hive_podman_labels() {
  local component="${1:-}"
  local instance

  if [[ -z "$component" ]]; then
    printf 'ERROR: labels requires a component name\n' >&2
    return 64
  fi

  if [[ ! "$component" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    printf 'ERROR: invalid component %q; expected [A-Za-z0-9][A-Za-z0-9._-]*\n' "$component" >&2
    return 64
  fi

  instance="$(hive_podman_instance)" || return

  printf -- '--label\n%s\n' "$HIVE_PODMAN_OWNED_LABEL"
  printf -- '--label\n%s=%s\n' "$HIVE_PODMAN_COMPONENT_LABEL_KEY" "$component"
  printf -- '--label\n%s=%s\n' "$HIVE_PODMAN_INSTANCE_LABEL_KEY" "$instance"
  printf -- '--label\n%s=%s\n' "$HIVE_PODMAN_RUNTIME_LABEL_KEY" "podman"
}

# Cleanup-time selectors, one CLI argument per line.
hive_podman_filters() {
  local instance
  instance="$(hive_podman_instance)" || return

  printf -- '--filter\nlabel=%s\n' "$HIVE_PODMAN_OWNED_LABEL"
  printf -- '--filter\nlabel=%s=%s\n' "$HIVE_PODMAN_INSTANCE_LABEL_KEY" "$instance"
}

# --- Cleanup guard ----------------------------------------------------------

_hive_podman_is_value_option() {
  local candidate="$1"
  local option

  for option in "${HIVE_PODMAN_VALUE_OPTIONS[@]}"; do
    [[ "$candidate" == "$option" ]] && return 0
  done

  return 1
}

# Splits a Podman/Buildah command into tool, resource noun, verb, and the
# remaining arguments. Sets HIVE_GUARD_TOOL, HIVE_GUARD_NOUN, HIVE_GUARD_VERB,
# and the HIVE_GUARD_REST array.
_hive_podman_parse_command() {
  local -a args=("$@")
  local nouns=" container containers image images volume volumes network networks pod pods system builder secret manifest machine farm "
  local index=0
  local token

  HIVE_GUARD_TOOL="${args[0]:-}"
  HIVE_GUARD_NOUN=""
  HIVE_GUARD_VERB=""
  HIVE_GUARD_REST=()

  index=1
  while (( index < ${#args[@]} )); do
    token="${args[index]}"

    if [[ "$token" == "--" ]]; then
      index=$((index + 1))
      continue
    fi

    if _hive_podman_is_value_option "$token"; then
      index=$((index + 2))
      continue
    fi

    if [[ "$token" == -* ]]; then
      index=$((index + 1))
      continue
    fi

    break
  done

  (( index < ${#args[@]} )) || return 0
  token="${args[index]}"

  if [[ "$nouns" == *" $token "* ]]; then
    HIVE_GUARD_NOUN="$token"
    index=$((index + 1))

    while (( index < ${#args[@]} )); do
      token="${args[index]}"

      if _hive_podman_is_value_option "$token"; then
        index=$((index + 2))
        continue
      fi

      if [[ "$token" == -* ]]; then
        index=$((index + 1))
        continue
      fi

      break
    done

    (( index < ${#args[@]} )) || return 0
    token="${args[index]}"
  fi

  HIVE_GUARD_VERB="$token"
  index=$((index + 1))
  HIVE_GUARD_REST=("${args[@]:index}")
}

# True when the argument list selects every resource of a kind.
_hive_podman_selects_all() {
  local token

  for token in "$@"; do
    case "$token" in
      --all|--all=true)
        return 0
        ;;
      -[A-Za-z]*)
        # Short flags may be bundled: -af is --all --force.
        [[ "$token" != --* && "$token" == *a* ]] && return 0
        ;;
    esac
  done

  return 1
}

# True when the argument list carries the Hive ownership label filter.
_hive_podman_has_owner_filter() {
  local -a args=("$@")
  local index=0
  local token

  while (( index < ${#args[@]} )); do
    token="${args[index]}"

    case "$token" in
      --filter=label="$HIVE_PODMAN_OWNED_LABEL"|-f=label="$HIVE_PODMAN_OWNED_LABEL")
        return 0
        ;;
      --filter)
        [[ "${args[index + 1]:-}" == "label=$HIVE_PODMAN_OWNED_LABEL" ]] && return 0
        ;;
    esac

    index=$((index + 1))
  done

  return 1
}

# True when at least one explicit operand (a name or ID) is present.
_hive_podman_has_operand() {
  local -a args=("$@")
  local index=0
  local token

  while (( index < ${#args[@]} )); do
    token="${args[index]}"

    if [[ "$token" == "--filter" || "$token" == "--format" ]]; then
      index=$((index + 2))
      continue
    fi

    if [[ "$token" == -* ]]; then
      index=$((index + 1))
      continue
    fi

    return 0
  done

  return 1
}

# The guard. Exits 0 when the command is scoped to Hive-owned resources, 65
# when the ownership contract rejects it, and 64 on unusable input.
hive_podman_cleanup_check() {
  if (( $# == 0 )); then
    printf 'ERROR: check requires a command to inspect\n' >&2
    return 64
  fi

  _hive_podman_parse_command "$@"

  case "$HIVE_GUARD_TOOL" in
    podman|buildah) ;;
    *)
      printf 'ERROR: the Hive cleanup contract covers podman and buildah only; got %q\n' \
        "$HIVE_GUARD_TOOL" >&2
      return 64
      ;;
  esac

  local subject="$HIVE_GUARD_TOOL${HIVE_GUARD_NOUN:+ $HIVE_GUARD_NOUN}${HIVE_GUARD_VERB:+ $HIVE_GUARD_VERB}"

  # Whole-store operations cannot be narrowed to Hive-owned resources at all.
  if [[ "$HIVE_GUARD_NOUN" == "system" || "$HIVE_GUARD_NOUN" == "machine" ]]; then
    case "$HIVE_GUARD_VERB" in
      prune|reset)
        printf 'REJECTED: %s operates on the whole shared store; it cannot be scoped to Hive-owned resources.\n' \
          "$subject" >&2
        printf 'HINT: select Hive resources with --filter label=%s instead.\n' \
          "$HIVE_PODMAN_OWNED_LABEL" >&2
        return 65
        ;;
    esac
  fi

  # Build-cache pruning is shared with every other build on the host and
  # carries no per-image ownership label.
  if [[ "$HIVE_GUARD_NOUN" == "builder" && "$HIVE_GUARD_VERB" == "prune" ]]; then
    printf 'REJECTED: %s prunes build cache shared with unrelated builds and cannot be label-scoped.\n' \
      "$subject" >&2
    return 65
  fi

  case "$HIVE_GUARD_VERB" in
    prune)
      if _hive_podman_selects_all "${HIVE_GUARD_REST[@]}"; then
        printf 'REJECTED: %s with --all reaches resources Hive does not own.\n' "$subject" >&2
        return 65
      fi

      if ! _hive_podman_has_owner_filter "${HIVE_GUARD_REST[@]}"; then
        printf 'REJECTED: %s must carry --filter label=%s.\n' \
          "$subject" "$HIVE_PODMAN_OWNED_LABEL" >&2
        return 65
      fi
      ;;
    rm|rmi|remove)
      if _hive_podman_selects_all "${HIVE_GUARD_REST[@]}"; then
        printf 'REJECTED: %s with --all removes resources Hive does not own.\n' "$subject" >&2
        return 65
      fi

      if _hive_podman_has_owner_filter "${HIVE_GUARD_REST[@]}"; then
        :
      elif _hive_podman_has_operand "${HIVE_GUARD_REST[@]}"; then
        :
      else
        printf 'REJECTED: %s must name Hive-owned resources or carry --filter label=%s.\n' \
          "$subject" "$HIVE_PODMAN_OWNED_LABEL" >&2
        return 65
      fi
      ;;
    reset)
      printf 'REJECTED: %s discards the whole shared store.\n' "$subject" >&2
      return 65
      ;;
  esac

  return 0
}

# --- Cleanup plan -----------------------------------------------------------
#
# The scoped commands a Podman lifecycle implementation is allowed to run.
# Printing them is not running them: this command never contacts an engine.

hive_podman_cleanup_plan() {
  local -a filters=()
  local line

  while IFS= read -r line; do
    filters+=("$line")
  done < <(hive_podman_filters) || return

  (( ${#filters[@]} > 0 )) || return 1

  local -a commands=(
    "podman ps --all ${filters[*]} --format {{.ID}}"
    "podman rm --force ${filters[*]}"
    "podman pod ps ${filters[*]} --format {{.ID}}"
    "podman pod rm --force POD_ID"
    "podman volume ls ${filters[*]} --format {{.Name}}"
    "podman volume rm VOLUME_NAME"
    "podman network ls ${filters[*]} --format {{.Name}}"
    "podman network rm NETWORK_NAME"
    "podman images ${filters[*]} --format {{.ID}}"
    "podman rmi IMAGE_ID"
  )

  local command
  for command in "${commands[@]}"; do
    # Self-check: the plan must never emit anything the guard would reject.
    # shellcheck disable=SC2086 # the plan lines are built here, word-splitting is intended
    if ! hive_podman_cleanup_check $command 2>/dev/null; then
      printf 'ERROR: planned command failed its own ownership guard: %s\n' "$command" >&2
      return 70
    fi

    printf '%s\n' "$command"
  done
}

hive_podman_cleanup_usage() {
  cat <<'EOF'
Usage: hive-podman-cleanup.sh labels <component>
       hive-podman-cleanup.sh filters
       hive-podman-cleanup.sh plan
       hive-podman-cleanup.sh check [--] <podman|buildah> <arguments...>

labels   Print the create/build-time ownership labels, one argument per line.
filters  Print the cleanup-time ownership selectors, one argument per line.
plan     Print the scoped cleanup commands. It never contacts a container engine.
check    Exit 0 when a command is scoped to Hive-owned resources, 65 when the
         ownership contract rejects it.

HIVE_DEPLOY_INSTANCE names the deployment being labeled or selected. It
defaults to "default".
EOF
}

hive_podman_cleanup_main() {
  local action="${1:-help}"

  case "$action" in
    labels)
      shift
      hive_podman_labels "$@"
      ;;
    filters)
      shift
      [[ $# -eq 0 ]] || {
        hive_podman_cleanup_usage >&2
        return 64
      }
      hive_podman_filters
      ;;
    plan)
      shift
      [[ $# -eq 0 ]] || {
        hive_podman_cleanup_usage >&2
        return 64
      }
      hive_podman_cleanup_plan
      ;;
    check)
      shift
      [[ "${1:-}" == "--" ]] && shift
      hive_podman_cleanup_check "$@"
      ;;
    -h|--help|help)
      hive_podman_cleanup_usage
      ;;
    *)
      printf 'ERROR: unknown action %q\n' "$action" >&2
      hive_podman_cleanup_usage >&2
      return 64
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  hive_podman_cleanup_main "$@"
fi
