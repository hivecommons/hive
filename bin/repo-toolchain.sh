#!/usr/bin/env bash
# repo-toolchain.sh — install a repository's declared Python toolchain into
# the throwaway contributor container before the task prompt is typed
# (hivecommons/hive#7925, second half).
#
# The contributor image ships a small check baseline (#7936: pip, pytest,
# pyyaml, jsonschema, requests, make, just, shellcheck, yq) so an agent can run
# most repositories' own checks instead of citing a proxy that looks green.
# The image cannot grow forever, though, and repositories already document
# what they need. This reads ONE thing — `<checkout>/.hive/tools` — and
# installs the `pip` lines it declares into whatever `python3` is first on
# PATH (in the container, the dev-owned /opt/hive/pyenv virtualenv).
#
#   # .hive/tools — one directive per line, '#' comments
#   pip ruff==0.6.9
#   pip "pytest-cov>=5,<6"
#   apt libfoo-dev            # recorded, not installed: see below
#
# Bounded on purpose. The file comes from the repository under work, i.e.
# from whoever last pushed to it, so nothing here executes text from it:
#   - only `pip <requirement>` is acted on, and each requirement must match a
#     PEP 508 name[extras] with an optional version specifier. URLs, paths,
#     `-r`, `--index-url`, `-e` and anything else pip would treat as an option
#     or a location are rejected and named, never passed through.
#   - `apt` lines are recorded and skipped: the container runs as the
#     unprivileged `dev` user and has no sudo. The line is still useful — the
#     log tells an operator what the image is missing.
#   - `.devcontainer/devcontainer.json` `postCreateCommand` is NOT honoured: it
#     is an arbitrary shell command from the checkout.
#   - the whole install is time-boxed; pip's own network failure (a container
#     without egress) is reported and the task proceeds without the tools.
# The script never fails the task: exit 0 always, findings on stdout.
set -uo pipefail

CHECKOUT="${1:-}"
MANIFEST_REL=".hive/tools"
PIP_TIMEOUT_SECONDS="${HIVE_REPO_TOOLCHAIN_PIP_TIMEOUT:-120}"
PIP_MAX_REQUIREMENTS="${HIVE_REPO_TOOLCHAIN_MAX_PIP:-32}"

# PEP 508-shaped, no URLs/paths/options: name, optional [extras], optional
# version specifier built only from comparison operators, dots, digits,
# letters, '*', ',' and whitespace.
REQ_RE='^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?(\[[A-Za-z0-9._,[:space:]-]+\])?([[:space:]]*(===|==|!=|~=|<=|>=|<|>)[[:space:]]*[A-Za-z0-9.*+!-]+([[:space:]]*,[[:space:]]*(===|==|!=|~=|<=|>=|<|>)[[:space:]]*[A-Za-z0-9.*+!-]+)*)?$'

log() { printf 'repo-toolchain: %s\n' "$*"; }

if [ -z "$CHECKOUT" ] || [ ! -d "$CHECKOUT" ]; then
  log "usage: repo-toolchain.sh <checkout-dir> (got ${CHECKOUT:-nothing})"
  exit 0
fi
MANIFEST="$CHECKOUT/$MANIFEST_REL"
if [ ! -f "$MANIFEST" ]; then
  log "no $MANIFEST_REL in $CHECKOUT; nothing declared"
  exit 0
fi

pip_reqs=()
rejected=0
apt_lines=0
lineno=0
while IFS= read -r raw || [ -n "$raw" ]; do
  lineno=$((lineno + 1))
  line="${raw%%#*}"
  line="$(printf '%s' "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  [ -z "$line" ] && continue
  directive="${line%%[[:space:]]*}"
  rest="$(printf '%s' "${line#"$directive"}" | sed -e 's/^[[:space:]]*//')"
  # Allow the requirement to be quoted, as shells make people do for '>'.
  rest="${rest#\"}"; rest="${rest%\"}"; rest="${rest#\'}"; rest="${rest%\'}"
  case "$directive" in
    pip)
      if [ -z "$rest" ]; then
        log "line $lineno: 'pip' with no requirement — skipped"
        rejected=$((rejected + 1))
      elif [[ "$rest" =~ $REQ_RE ]]; then
        pip_reqs+=("$rest")
      else
        log "line $lineno: rejected pip requirement (must be name[extras][version-spec]; no URLs, paths or options): ${rest:0:80}"
        rejected=$((rejected + 1))
      fi
      ;;
    apt)
      apt_lines=$((apt_lines + 1))
      log "line $lineno: apt '${rest:0:80}' recorded, not installed — the container has no root; ask for it in the image (#7925)"
      ;;
    *)
      log "line $lineno: unknown directive '${directive:0:40}' — skipped"
      rejected=$((rejected + 1))
      ;;
  esac
done < "$MANIFEST"

if [ "${#pip_reqs[@]}" -gt "$PIP_MAX_REQUIREMENTS" ]; then
  log "$MANIFEST_REL declares ${#pip_reqs[@]} pip requirements, more than the $PIP_MAX_REQUIREMENTS this installs — none installed"
  exit 0
fi
if [ "${#pip_reqs[@]}" -eq 0 ]; then
  log "no installable pip requirement in $MANIFEST_REL (apt lines: $apt_lines, rejected: $rejected)"
  exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
  log "python3 is not on PATH; cannot install ${#pip_reqs[@]} pip requirement(s)"
  exit 0
fi

log "installing ${#pip_reqs[@]} pip requirement(s) from $MANIFEST_REL: ${pip_reqs[*]}"
timeout_cmd=()
if command -v timeout >/dev/null 2>&1; then timeout_cmd=(timeout "$PIP_TIMEOUT_SECONDS"); fi
# `--` ends option parsing, so a requirement can never be read as a pip flag
# even if the regex above is ever loosened.
if "${timeout_cmd[@]}" python3 -m pip install --no-input --disable-pip-version-check --quiet -- "${pip_reqs[@]}" 2>&1 | tail -n 20; then
  log "installed ${#pip_reqs[@]} pip requirement(s)"
else
  status=${PIPESTATUS[0]}
  if [ "$status" = "124" ]; then
    log "pip install timed out after ${PIP_TIMEOUT_SECONDS}s — the task proceeds without the declared tools (no network in the container?)"
  else
    log "pip install failed (exit $status) — the task proceeds without the declared tools"
  fi
fi
exit 0
