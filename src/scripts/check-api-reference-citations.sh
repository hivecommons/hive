#!/usr/bin/env bash
# check-api-reference-citations.sh — verify every `pkg/<file>:<line>` citation
# in src/docs/api-reference.md still points at the route registration it
# names (hivecommons/hive#6628).
#
# The doc's Source column cites route-registration sites as `pkg/<file>:<line>`
# next to the row's Method/Path. Refactors shift line numbers without anyone
# updating the doc (298/423 citations had drifted by the time this landed),
# so this makes the drift mechanically checkable: for every citation, the
# named line in `src/<file>` must contain the route's `"<METHOD> <path>"`
# registration string.
#
# Two registration shapes are recognized, both seen in
# src/pkg/dashboard/server.go and friends:
#   mux.HandleFunc("GET /api/version", handler)
#   mux.Handle("GET /api/version", handler)
# A third shape concatenates a path constant, e.g.
#   mux.HandleFunc("GET "+openRouterCallbackPath, handler)
# For that shape the checker falls back to confirming the line ends in
# `"<METHOD> "+IDENT` and that IDENT is defined elsewhere in the same file as
# exactly `"<path>"` — the citation is only accepted if both hold.
#
# Source cells that cite a file with no line number (`pkg/dashboard/api.go`,
# used when a route's exact line is not worth tracking) are left alone; only
# `pkg/<file>:<line>` citations are checked or rewritten.
#
# Usage:
#   src/scripts/check-api-reference-citations.sh [--fix] [doc-path]
#
# Without --fix: prints every drifted citation and exits non-zero if any
# citation does not verify.
#
# With --fix: for each drifted citation, if the exact `"<METHOD> <path>"`
# string appears exactly once in the cited file, rewrites the citation's line
# number in place to match. Citations that cannot be resolved this way (route
# moved to a different file, string appears more than once, or the
# concatenated-constant shape does not resolve) are left as-is and reported;
# --fix still exits non-zero if anything remains unresolved.
set -u -o pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SRC_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

FIX=0
DOC=""
for arg in "$@"; do
  case "$arg" in
    --fix) FIX=1 ;;
    -h|--help)
      sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    -*)
      echo "unknown option: $arg" >&2
      exit 2
      ;;
    *)
      DOC="$arg"
      ;;
  esac
done

DOC="${DOC:-${SRC_ROOT}/docs/api-reference.md}"

if [ ! -f "$DOC" ]; then
  echo "cannot find api-reference doc at $DOC" >&2
  exit 2
fi

# Extract one line per `pkg/<file>:<line>` citation found in a table row:
#   <doc-line-number>\t<METHOD>\t</path>\t<cited-file>\t<cited-line>
extract_citations() {
  awk -F'|' '
    function trim(s) {
      gsub(/^[ \t]+|[ \t]+$/, "", s)
      return s
    }
    {
      # Only table rows whose first cell is a backtick-quoted HTTP method.
      if (NF < 3) next
      method = trim($2)
      if (method !~ /^`(GET|POST|PUT|PATCH|DELETE)`$/) next
      gsub(/`/, "", method)
      path = trim($3)
      gsub(/`/, "", path)

      source_cell = trim($(NF - 1))
      if (source_cell == "") source_cell = trim($NF)

      rest = source_cell
      while (match(rest, /`pkg\/[^`]+:[0-9]+`/)) {
        citation = substr(rest, RSTART + 1, RLENGTH - 2)
        split(citation, parts, ":")
        cited_line = parts[length(parts)]
        cited_file = citation
        sub(/:[0-9]+$/, "", cited_file)
        printf "%d\t%s\t%s\t%s\t%s\n", NR, method, path, cited_file, cited_line
        rest = substr(rest, RSTART + RLENGTH)
      }
    }
  ' "$DOC"
}

drifted=0
resolved=0
unresolved=0

while IFS=$'\t' read -r doc_line method path cited_file cited_line; do
  [ -n "${doc_line:-}" ] || continue
  target="${SRC_ROOT}/${cited_file}"
  needle="\"${method} ${path}\""

  if [ ! -f "$target" ]; then
    echo "DRIFT doc:${doc_line} ${method} ${path} -> ${cited_file}:${cited_line} (file does not exist)" >&2
    drifted=$((drifted + 1))
    unresolved=$((unresolved + 1))
    continue
  fi

  actual_line="$(sed -n "${cited_line}p" "$target" 2>/dev/null || true)"
  ok=0
  if [ -n "$actual_line" ] && printf '%s' "$actual_line" | grep -qF -- "$needle"; then
    ok=1
  elif [ -n "$actual_line" ] && printf '%s' "$actual_line" | grep -qE -- "\"${method} \"\+[A-Za-z_][A-Za-z0-9_.]*"; then
    ident="$(printf '%s' "$actual_line" | sed -n "s/.*\"${method} \"+\\([A-Za-z_][A-Za-z0-9_.]*\\).*/\\1/p")"
    if [ -n "$ident" ]; then
      case "$ident" in
        *.*)
          # Package-qualified constant, e.g. delegation.KeysPath: look for its
          # definition under the matching pkg/<package>/ directory rather than
          # just the citing file.
          const_pkg="${ident%%.*}"
          const_name="${ident#*.}"
          if grep -qE -- "\\b${const_name}\\b[[:space:]]*=[[:space:]]*\"${path}\"" "${SRC_ROOT}"/pkg/*/"${const_pkg}"*.go "${SRC_ROOT}/pkg/${const_pkg}"/*.go 2>/dev/null; then
            ok=1
          fi
          ;;
        *)
          if grep -qE -- "\\b${ident}\\b[[:space:]]*=[[:space:]]*\"${path}\"" "$target"; then
            ok=1
          fi
          ;;
      esac
    fi
  fi

  if [ "$ok" -eq 1 ]; then
    continue
  fi

  drifted=$((drifted + 1))

  if [ "$FIX" -eq 0 ]; then
    echo "DRIFT doc:${doc_line} ${method} ${path} -> ${cited_file}:${cited_line} (line does not register it)" >&2
    continue
  fi

  # --fix: only rewrite when the exact registration string appears exactly
  # once in the cited file.
  matches="$(grep -cF -- "$needle" "$target" | tr -d '[:space:]')"
  if [ "$matches" != "1" ]; then
    echo "UNRESOLVED doc:${doc_line} ${method} ${path} -> ${cited_file}:${cited_line} (found ${matches} exact matches in ${cited_file}, need exactly 1)" >&2
    unresolved=$((unresolved + 1))
    continue
  fi

  new_line="$(grep -nF -- "$needle" "$target" | cut -d: -f1)"
  # Rewrite only the specific `pkg/<cited_file>:<cited_line>` citation on this
  # doc line (identified by its line number, not a doc-wide substitution, so a
  # coincidental same file:line citation on a different row is untouched).
  old_ref="${cited_file}:${cited_line}"
  new_ref="${cited_file}:${new_line}"
  awk -v ln="$doc_line" -v old="$old_ref" -v new="$new_ref" '
    BEGIN { old_lit = old; new_lit = new }
    NR == ln {
      idx = index($0, old_lit)
      if (idx > 0) {
        $0 = substr($0, 1, idx - 1) new_lit substr($0, idx + length(old_lit))
      }
    }
    { print }
  ' "$DOC" > "${DOC}.tmp" && mv "${DOC}.tmp" "$DOC"

  echo "FIXED doc:${doc_line} ${method} ${path} -> ${old_ref} => ${new_ref}"
  resolved=$((resolved + 1))
done < <(extract_citations)

if [ "$FIX" -eq 1 ]; then
  echo "api-reference citation fix: ${resolved} rewritten, ${unresolved} left unresolved (${drifted} drifted total)."
  [ "$unresolved" -eq 0 ]
  exit $?
fi

if [ "$drifted" -eq 0 ]; then
  echo "api-reference citations: all pkg/<file>:<line> references verified."
  exit 0
fi

echo "api-reference citations: ${drifted} drifted citation(s). Run with --fix to rewrite resolvable ones." >&2
exit 1
