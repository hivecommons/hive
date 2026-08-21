#!/usr/bin/env bash
# check-release-lines.sh — release-line carry-forward guard (#4405).
#
# Nine workflows pin their push/pull_request triggers to a hand-maintained list
# of version-branch names. When mainline moved from v2 to v4, one of them was
# left naming `v2` alone and stopped running entirely (#4339) — absent, not red.
# This asserts every pinned workflow against the single source of truth in
# .github/release-lines.yml, so cutting a new release line is one edit that CI
# enforces instead of nine edits nobody is reminded to make.
#
# It reads both YAML spellings of a branch filter:
#
#   branches: [v2, v4]          # flow sequence  (v2-ci.yml, v2-tests.yml)
#   branches:                   # block sequence (the podman lanes, scorecard's
#     - v2                      #                 siblings, suid-contract)
#     - v4
#
# A workflow that carries a branch filter must be classified in the manifest as
# either `pinned` (its list must equal release_lines, plus any declared extras,
# minus any declared `-exclusions`) or `unpinned` (it must keep a wildcard, e.g.
# docker.yml's '**'). An unclassified one fails: a new pinned workflow is
# exactly the thing that goes stale next.
#
# Usage: src/scripts/check-release-lines.sh [manifest] [workflows-dir]
#   manifest        default .github/release-lines.yml
#   workflows-dir   default .github/workflows
#
# Exit 0 = in sync, 1 = out of sync (or the inputs are unreadable).
set -uo pipefail

MANIFEST="${1:-.github/release-lines.yml}"
WORKFLOW_DIR="${2:-.github/workflows}"

if [[ ! -f "$MANIFEST" ]]; then
  echo "ERROR: manifest not found at ${MANIFEST}" >&2
  exit 1
fi
if [[ ! -d "$WORKFLOW_DIR" ]]; then
  echo "ERROR: workflow directory not found at ${WORKFLOW_DIR}" >&2
  exit 1
fi

fail=0

note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

# ---------------------------------------------------------------------------
# Manifest
# ---------------------------------------------------------------------------

# Strip a YAML flow sequence "[a, b]" (or "[]") down to a comma-free word list.
flow_items() {
  local raw="$1"
  raw="${raw#*[}"
  raw="${raw%%]*}"
  raw="${raw//,/ }"
  raw="${raw//\"/}"
  raw="${raw//\'/}"
  echo $raw
}

# Normalise a branch set: sorted, de-duplicated, comma-joined.
normalise() {
  if [[ $# -eq 0 ]]; then
    echo ""
    return
  fi
  printf '%s\n' "$@" | LC_ALL=C sort -u | paste -sd, -
}

RELEASE_LINES=()
declare -A PINNED_EXTRAS=()
UNPINNED=()

section=""
while IFS= read -r line; do
  # Comments are stripped only where they cannot be inside a value; every value
  # in this manifest is a flow sequence or a quoted scalar on one line.
  case "$line" in
    \#*|"") continue ;;
  esac
  if [[ "$line" =~ ^release_lines:[[:space:]]*(.*)$ ]]; then
    read -r -a RELEASE_LINES <<< "$(flow_items "${BASH_REMATCH[1]}")"
    section=""
    continue
  fi
  if [[ "$line" =~ ^pinned:[[:space:]]*$ ]];   then section="pinned";   continue; fi
  if [[ "$line" =~ ^unpinned:[[:space:]]*$ ]]; then section="unpinned"; continue; fi
  if [[ "$line" =~ ^[^[:space:]] ]]; then section=""; continue; fi

  # Indented entry: "  name.yml: value"
  [[ "$line" =~ ^[[:space:]]+([^[:space:]#][^:]*):[[:space:]]*(.*)$ ]] || continue
  entry="${BASH_REMATCH[1]}"
  value="${BASH_REMATCH[2]}"
  case "$section" in
    pinned)   PINNED_EXTRAS["$entry"]="$(flow_items "$value")" ;;
    unpinned) UNPINNED+=("$entry") ;;
  esac
done < "$MANIFEST"

if [[ ${#RELEASE_LINES[@]} -eq 0 ]]; then
  echo "ERROR: ${MANIFEST} declares no release_lines" >&2
  exit 1
fi

echo "== Release-line carry-forward guard =="
echo "manifest:  ${MANIFEST}"
echo "workflows: ${WORKFLOW_DIR}"
echo "release lines: $(normalise "${RELEASE_LINES[@]}")"
echo

# ---------------------------------------------------------------------------
# Workflow branch filters
# ---------------------------------------------------------------------------

# Emits one TSV record per branch filter found:
#   <file>\t<line-number>\t<key>\t<comma-joined branch entries>
# An empty final field means an empty list, which is itself a finding.
extract_branch_filters() {
  awk '
    function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
    function unquote(s,  c) {
      s = trim(s)
      if (length(s) >= 2) {
        c = substr(s, 1, 1)
        if ((c == "\"" || c == "'"'"'") && substr(s, length(s), 1) == c)
          return substr(s, 2, length(s) - 2)
      }
      return s
    }
    # Drop a trailing YAML comment without touching a "#" inside quotes.
    function strip_comment(s,  i, c, q, out) {
      q = ""; out = ""
      for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (q != "") { out = out c; if (c == q) q = ""; continue }
        if (c == "\"" || c == "'"'"'") { q = c; out = out c; continue }
        if (c == "#") break
        out = out c
      }
      return out
    }
    function indent_of(s,  i, c) {
      for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (c != " " && c != "\t") return i - 1
      }
      return length(s)
    }
    function flush(  i, joined) {
      if (!pending) return
      joined = ""
      for (i = 1; i <= n; i++) joined = joined (i > 1 ? "," : "") items[i]
      printf "%s\t%d\t%s\t%s\n", fname, startline, key, joined
      pending = 0; n = 0; inblock = 0
    }
    FNR == 1 { flush() }
    {
      raw = strip_comment($0)
      if (pending && inblock) {
        if (raw ~ /^[[:space:]]*$/) next
        if (indent_of(raw) > blockind && match(raw, /^[[:space:]]*-[[:space:]]*/)) {
          items[++n] = unquote(substr(raw, RSTART + RLENGTH))
          next
        }
        flush()
      }
      if (match(raw, /^[[:space:]]*(branches|branches-ignore):[[:space:]]*/)) {
        key = raw; sub(/:.*$/, "", key); key = trim(key)
        rest = trim(substr(raw, RSTART + RLENGTH))
        fname = FILENAME; startline = FNR; pending = 1; n = 0
        if (rest == "") { inblock = 1; blockind = indent_of(raw); next }
        if (substr(rest, 1, 1) == "[") {
          inner = substr(rest, 2, index(rest, "]") - 2)
          cnt = split(inner, parts, ",")
          for (i = 1; i <= cnt; i++) if (trim(parts[i]) != "") items[++n] = unquote(parts[i])
        } else {
          items[++n] = unquote(rest)   # single scalar, e.g. `branches: v4`
        }
        flush()
      }
    }
    END { flush() }
  ' "$@"
}

shopt -s nullglob
workflow_files=("$WORKFLOW_DIR"/*.yml "$WORKFLOW_DIR"/*.yaml)
shopt -u nullglob
if [[ ${#workflow_files[@]} -eq 0 ]]; then
  echo "ERROR: no workflow files under ${WORKFLOW_DIR}" >&2
  exit 1
fi

records="$(extract_branch_filters "${workflow_files[@]}")"

declare -A SEEN_FILTERS=()
while IFS=$'\t' read -r path lineno key branches; do
  [[ -n "$path" ]] || continue
  base="${path##*/}"
  SEEN_FILTERS["$base"]=1

  expected_extras="${PINNED_EXTRAS[$base]-__unset__}"
  is_unpinned=0
  for u in "${UNPINNED[@]}"; do [[ "$u" == "$base" ]] && is_unpinned=1; done

  if [[ "$expected_extras" != "__unset__" && $is_unpinned -eq 1 ]]; then
    note_fail "${base} is listed under BOTH pinned and unpinned in ${MANIFEST}"
    continue
  fi

  IFS=',' read -r -a actual <<< "$branches"
  actual_set="$(normalise "${actual[@]}")"

  if [[ $is_unpinned -eq 1 ]]; then
    if [[ "$branches" == *'*'* ]]; then
      note_ok "${base}:${lineno} ${key}: wildcard kept (${actual_set}) — deliberately unpinned"
    else
      note_fail "${base}:${lineno} ${key}: declared unpinned in ${MANIFEST} but the filter is now a fixed list (${actual_set:-<empty>}). Reclassify it under 'pinned', or restore the wildcard."
    fi
    continue
  fi

  if [[ "$expected_extras" == "__unset__" ]]; then
    note_fail "${base}:${lineno} ${key}: workflow has a branch filter (${actual_set:-<empty>}) but is not classified in ${MANIFEST}. Add it under 'pinned' (with any extra branches) or 'unpinned'."
    continue
  fi

  # A manifest value may carry `-name` entries: release lines this workflow
  # deliberately does not cover. Expected = release_lines + extras - exclusions.
  extras=()
  excluded=()
  for token in $expected_extras; do
    if [[ "$token" == -* ]]; then excluded+=("${token#-}"); else extras+=("$token"); fi
  done

  expected=()
  for rl in "${RELEASE_LINES[@]}"; do
    skip=0
    for ex in ${excluded[@]+"${excluded[@]}"}; do [[ "$ex" == "$rl" ]] && skip=1; done
    [[ $skip -eq 0 ]] && expected+=("$rl")
  done
  expected+=(${extras[@]+"${extras[@]}"})

  bad_exclusion=0
  for ex in ${excluded[@]+"${excluded[@]}"}; do
    found=0
    for rl in "${RELEASE_LINES[@]}"; do [[ "$ex" == "$rl" ]] && found=1; done
    if [[ $found -eq 0 ]]; then
      note_fail "${base}: ${MANIFEST} excludes '-${ex}', which is not a declared release line. Drop the stale exclusion."
      bad_exclusion=1
    fi
  done
  [[ $bad_exclusion -eq 1 ]] && continue

  if [[ ${#expected[@]} -eq 0 ]]; then
    note_fail "${base}:${lineno} ${key}: ${MANIFEST} excludes every release line and declares no extras, so nothing is asserted. Move it to 'unpinned' or fix the entry."
    continue
  fi

  expected_set="$(normalise "${expected[@]}")"

  if [[ "$actual_set" == "$expected_set" ]]; then
    note_ok "${base}:${lineno} ${key}: ${actual_set}"
    continue
  fi

  missing="$(comm -23 \
    <(printf '%s\n' "${expected[@]}" | LC_ALL=C sort -u) \
    <(printf '%s\n' "${actual[@]}" | LC_ALL=C sort -u) | paste -sd, -)"
  unexpected="$(comm -13 \
    <(printf '%s\n' "${expected[@]}" | LC_ALL=C sort -u) \
    <(printf '%s\n' "${actual[@]}" | LC_ALL=C sort -u) | paste -sd, -)"

  detail=""
  [[ -n "$missing" ]] && detail="missing: ${missing}"
  [[ -n "$unexpected" ]] && detail="${detail:+${detail}; }unexpected: ${unexpected}"
  note_fail "${base}:${lineno} ${key}: has [${actual_set:-<empty>}], expected [${expected_set}] — ${detail}. A release line named here but absent from the workflow means that workflow does not run on it at all."
done <<< "$records"

# A manifest entry whose workflow lost its branch filter (or its file) is just
# as dangerous as a stale list: it means the guard silently checks nothing.
for base in "${!PINNED_EXTRAS[@]}"; do
  [[ -n "${SEEN_FILTERS[$base]-}" ]] && continue
  if [[ -f "${WORKFLOW_DIR}/${base}" ]]; then
    note_fail "${base}: listed as 'pinned' in ${MANIFEST} but no branch filter was found in it."
  else
    note_fail "${base}: listed as 'pinned' in ${MANIFEST} but ${WORKFLOW_DIR}/${base} does not exist."
  fi
done
for base in "${UNPINNED[@]}"; do
  [[ -n "${SEEN_FILTERS[$base]-}" ]] && continue
  if [[ -f "${WORKFLOW_DIR}/${base}" ]]; then
    note_fail "${base}: listed as 'unpinned' in ${MANIFEST} but no branch filter was found in it."
  else
    note_fail "${base}: listed as 'unpinned' in ${MANIFEST} but ${WORKFLOW_DIR}/${base} does not exist."
  fi
done

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL — workflow branch filters are out of sync with ${MANIFEST}."
  echo "Cutting a release line takes two edits: the manifest AND every workflow"
  echo "listed under 'pinned'. Fixing only the manifest leaves those workflows"
  echo "not running on the new branch at all (#4339)."
  exit 1
fi
echo "RESULT: PASS — every pinned workflow covers the declared release lines."
