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
# It also asserts the release-line lists that are NOT trigger filters, declared
# under `env_lists` (#4462). docker.yml hand-writes the branch names a second
# time in its gate job's `LONG_LIVED` env var — the GHCR push policy — and no
# branch-filter check can see it. That list going stale is worse than a skipped
# workflow: on the day `v5` is cut, docker.yml's `'**'` trigger keeps BUILDING
# the image on v5 while the policy never publishes `v5-latest`, so no hive can
# be assigned to the new line and the image looks healthy throughout. Green
# rather than absent.
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

# Assert one hand-written release-line list — a workflow's branch filter, or an
# env list that is not a trigger at all — against the manifest. A manifest value
# may carry `-name` entries: release lines this list deliberately does not
# cover. Expected = release_lines + extras - exclusions, exactly.
#
#   $1 entry        the manifest key ("v2-ci.yml", "docker.yml LONG_LIVED")
#   $2 label        what a finding names, e.g. "v2-ci.yml:5 branches:"
#   $3 spec         the manifest value: extras, and `-name` exclusions
#   $4 consequence  appended to a mismatch — what actually breaks
#   $5.. actual     the branch names found in the file
assert_release_line_set() {
  local entry="$1" label="$2" spec="$3" consequence="$4"
  shift 4
  local actual=("$@")
  local actual_set
  actual_set="$(normalise ${actual[@]+"${actual[@]}"})"

  local extras=() excluded=() token
  for token in $spec; do
    if [[ "$token" == -* ]]; then excluded+=("${token#-}"); else extras+=("$token"); fi
  done

  local ex rl found bad_exclusion=0
  for ex in ${excluded[@]+"${excluded[@]}"}; do
    found=0
    for rl in "${RELEASE_LINES[@]}"; do [[ "$ex" == "$rl" ]] && found=1; done
    if [[ $found -eq 0 ]]; then
      note_fail "${entry}: ${MANIFEST} excludes '-${ex}', which is not a declared release line. Drop the stale exclusion."
      bad_exclusion=1
    fi
  done
  [[ $bad_exclusion -eq 1 ]] && return

  local expected=() skip
  for rl in "${RELEASE_LINES[@]}"; do
    skip=0
    for ex in ${excluded[@]+"${excluded[@]}"}; do [[ "$ex" == "$rl" ]] && skip=1; done
    [[ $skip -eq 0 ]] && expected+=("$rl")
  done
  expected+=(${extras[@]+"${extras[@]}"})

  if [[ ${#expected[@]} -eq 0 ]]; then
    note_fail "${label} ${MANIFEST} excludes every release line for '${entry}' and declares no extras, so nothing is asserted. Fix the entry."
    return
  fi

  local expected_set
  expected_set="$(normalise "${expected[@]}")"
  if [[ "$actual_set" == "$expected_set" ]]; then
    note_ok "${label} ${actual_set}"
    return
  fi

  local missing unexpected detail
  missing="$(comm -23 \
    <(printf '%s\n' "${expected[@]}" | LC_ALL=C sort -u) \
    <(printf '%s\n' ${actual[@]+"${actual[@]}"} | LC_ALL=C sort -u) | paste -sd, -)"
  unexpected="$(comm -13 \
    <(printf '%s\n' "${expected[@]}" | LC_ALL=C sort -u) \
    <(printf '%s\n' ${actual[@]+"${actual[@]}"} | LC_ALL=C sort -u) | paste -sd, -)"

  detail=""
  [[ -n "$missing" ]] && detail="missing: ${missing}"
  [[ -n "$unexpected" ]] && detail="${detail:+${detail}; }unexpected: ${unexpected}"
  note_fail "${label} has [${actual_set:-<empty>}], expected [${expected_set}] — ${detail}. ${consequence}"
}

RELEASE_LINES=()
declare -A PINNED_EXTRAS=()
declare -A ENV_LISTS=()
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
  if [[ "$line" =~ ^pinned:[[:space:]]*$ ]];    then section="pinned";    continue; fi
  if [[ "$line" =~ ^unpinned:[[:space:]]*$ ]];  then section="unpinned";  continue; fi
  if [[ "$line" =~ ^env_lists:[[:space:]]*$ ]]; then section="env_lists"; continue; fi
  if [[ "$line" =~ ^[^[:space:]] ]]; then section=""; continue; fi

  # Indented entry: "  name.yml: value"
  [[ "$line" =~ ^[[:space:]]+([^[:space:]#][^:]*):[[:space:]]*(.*)$ ]] || continue
  entry="${BASH_REMATCH[1]}"
  value="${BASH_REMATCH[2]}"
  case "$section" in
    pinned)    PINNED_EXTRAS["$entry"]="$(flow_items "$value")" ;;
    unpinned)  UNPINNED+=("$entry") ;;
    env_lists) ENV_LISTS["$entry"]="$(flow_items "$value")" ;;
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

  assert_release_line_set "$base" "${base}:${lineno} ${key}:" "$expected_extras" \
    "A release line named here but absent from the workflow means that workflow does not run on it at all." \
    ${actual[@]+"${actual[@]}"}
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

# ---------------------------------------------------------------------------
# Hand-written branch lists that are not trigger filters (#4462)
# ---------------------------------------------------------------------------

# Print every assignment of <var> in a workflow, one per occurrence:
#   <line-number><TAB><space-separated branch names>
# Accepts the spellings a workflow env value can take: `VAR: "v2 v4"`, the same
# unquoted, and `VAR: [v2, v4]`. Comments are dropped outside quotes, so the
# prose that names the variable is not mistaken for a second definition.
extract_env_list() {
  awk -v var="$2" '
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
    BEGIN { pat = "^[[:space:]]*" var ":[[:space:]]*" }
    {
      raw = strip_comment($0)
      if (!match(raw, pat)) next
      rest = unquote(trim(substr(raw, RSTART + RLENGTH)))
      if (substr(rest, 1, 1) == "[") rest = substr(rest, 2, index(rest, "]") - 2)
      gsub(/,/, " ", rest)
      gsub(/["'"'"']/, "", rest)
      printf "%d\t%s\n", FNR, trim(rest)
    }
  ' "$1"
}

if [[ ${#ENV_LISTS[@]} -gt 0 ]]; then
  while IFS= read -r entry; do
    [[ -n "$entry" ]] || continue
    file="${entry%% *}"
    var="${entry##* }"
    if [[ "$file" == "$entry" || -z "$var" ]]; then
      note_fail "${entry}: an 'env_lists' key in ${MANIFEST} must be '<workflow.yml> <ENV_NAME>'."
      continue
    fi
    if [[ ! -f "${WORKFLOW_DIR}/${file}" ]]; then
      note_fail "${entry}: listed under 'env_lists' in ${MANIFEST} but ${WORKFLOW_DIR}/${file} does not exist."
      continue
    fi

    mapfile -t hits < <(extract_env_list "${WORKFLOW_DIR}/${file}" "$var")
    if [[ ${#hits[@]} -eq 0 ]]; then
      note_fail "${entry}: no '${var}:' assignment found in ${file}. It was renamed or removed, so this entry asserts nothing. Update ${MANIFEST} to name the list as it is spelled today."
      continue
    fi
    if [[ ${#hits[@]} -gt 1 ]]; then
      lines="$(printf '%s\n' "${hits[@]}" | cut -f1 | paste -sd, -)"
      note_fail "${entry}: '${var}:' is assigned ${#hits[@]} times in ${file} (lines ${lines}), so the guard cannot tell which one is the policy. Define the list once."
      continue
    fi

    lineno="${hits[0]%%$'\t'*}"
    value="${hits[0]#*$'\t'}"
    read -r -a items <<< "$value"
    assert_release_line_set "$entry" "${file}:${lineno} env ${var}:" "${ENV_LISTS[$entry]}" \
      "This list is not a trigger, so a release line missing from it fails GREEN: the image still builds on that branch and simply never publishes its <branch>-latest tag, leaving no image for a hive to be assigned to." \
      ${items[@]+"${items[@]}"}
  done < <(printf '%s\n' "${!ENV_LISTS[@]}" | LC_ALL=C sort)
fi

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL — hand-written branch lists are out of sync with ${MANIFEST}."
  echo "Cutting a release line takes three edits: the manifest, every workflow"
  echo "listed under 'pinned', and every list under 'env_lists'. Fixing only the"
  echo "manifest leaves those workflows not running on the new branch at all"
  echo "(#4339), and leaves the image built on it but never published (#4462)."
  exit 1
fi
echo "RESULT: PASS — every pinned workflow and hand-written list covers the declared release lines."
