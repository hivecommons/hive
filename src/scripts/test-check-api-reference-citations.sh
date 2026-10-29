#!/usr/bin/env bash
# test-check-api-reference-citations.sh — exercises
# check-api-reference-citations.sh against a small throwaway fixture tree with
# a drifted citation, a fixable citation, and a citation that cannot be
# resolved automatically (hivecommons/hive#6628).
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER_SRC="${HERE}/check-api-reference-citations.sh"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/api-reference-citations.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

# Fixture tree: src/scripts/check-api-reference-citations.sh (a copy of the
# real checker, so it resolves SRC_ROOT the same way it does in the repo)
# alongside src/docs/api-reference.md and src/pkg/widget/routes.go.
FIXTURE_SRC="${TMP}/src"
mkdir -p "${FIXTURE_SRC}/scripts" "${FIXTURE_SRC}/docs" "${FIXTURE_SRC}/pkg/widget"
cp "$CHECKER_SRC" "${FIXTURE_SRC}/scripts/check-api-reference-citations.sh"
chmod +x "${FIXTURE_SRC}/scripts/check-api-reference-citations.sh"
CHECKER="${FIXTURE_SRC}/scripts/check-api-reference-citations.sh"
DOC="${FIXTURE_SRC}/docs/api-reference.md"
ROUTES="${FIXTURE_SRC}/pkg/widget/routes.go"

cat > "$ROUTES" <<'EOF'
package widget

// Legacy route note: "GET /api/widgets/legacy" is kept for backwards compat.
func (s *Server) register() {
	s.mux.HandleFunc("GET /api/widgets", s.handleList)
	s.mux.HandleFunc("POST /api/widgets", s.handleCreate)
	s.mux.Handle("GET /api/widgets/legacy", s.legacyHandler)
}
EOF

write_doc() {
  cat > "$DOC" <<EOF
# Fixture API reference

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| \`GET\` | \`/api/widgets\` | Public | List widgets | \`pkg/widget/routes.go:$1\` |
| \`POST\` | \`/api/widgets\` | Public | Create widget | \`pkg/widget/routes.go:6\` |
| \`GET\` | \`/api/widgets/legacy\` | Public | Legacy widget list | \`pkg/widget/routes.go:9\` |
EOF
}

# Case 1: drifted citation (GET /api/widgets is really on line 4, doc says 2)
# and a citation pointing at a line that does not exist in the file at all.
write_doc 2

set +e
output=$("$CHECKER" "$DOC" 2>&1)
rc=$?
set -e

if [ "$rc" -eq 1 ]; then
  pass "checker exits 1 on drifted citations"
else
  bad "expected exit 1 on drifted citations, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi

if printf '%s\n' "$output" | grep -qF "DRIFT ${DOC}:5 GET /api/widgets -> pkg/widget/routes.go:2"; then
  pass "drifted GET /api/widgets citation is reported"
else
  bad "expected a DRIFT line for the GET /api/widgets citation"
  echo "$output" | sed 's/^/      | /'
fi

# Case 2: --fix rewrites the resolvable drift (exactly one exact match for
# "GET /api/widgets" in the file) in place, leaving the file untouched
# otherwise, and still reports the unresolved one.
set +e
fix_output=$("$CHECKER" --fix "$DOC" 2>&1)
fix_rc=$?
set -e

if grep -qF 'pkg/widget/routes.go:5`' "$DOC"; then
  pass "--fix rewrote the drifted line number to 5"
else
  bad "--fix did not rewrite the citation to line 5"
  cat "$DOC" | sed 's/^/      | /'
fi

if grep -qF 'pkg/widget/routes.go:6`' "$DOC"; then
  pass "--fix left the already-correct POST citation untouched"
else
  bad "the already-correct POST /api/widgets citation changed unexpectedly"
fi

if printf '%s\n' "$fix_output" | grep -qF 'UNRESOLVED'; then
  pass "--fix reports the legacy-route citation as unresolved"
else
  bad "expected an UNRESOLVED line for the legacy widgets route"
  echo "$fix_output" | sed 's/^/      | /'
fi

if [ "$fix_rc" -ne 0 ]; then
  pass "--fix still exits non-zero while an unresolved citation remains"
else
  bad "--fix exited 0 despite an unresolved citation"
fi

# Case 3: once the remaining citation is hand-fixed to the correct line, a
# plain run passes cleanly.
sed -i.bak 's#pkg/widget/routes.go:9#pkg/widget/routes.go:7#' "$DOC" && rm -f "${DOC}.bak"

set +e
clean_output=$("$CHECKER" "$DOC" 2>&1)
clean_rc=$?
set -e

if [ "$clean_rc" -eq 0 ]; then
  pass "checker exits 0 once every citation is correct"
else
  bad "expected exit 0 after fixing every citation, got ${clean_rc}"
  echo "$clean_output" | sed 's/^/      | /'
fi

if printf '%s\n' "$clean_output" | grep -qF 'all pkg/<file>:<line> references verified'; then
  pass "success message is printed"
else
  bad "expected the success message on a clean doc"
fi

# Case 4: the split layout is discovered by default and when the index is
# explicitly supplied (as check-docs-citations.py --fix does). Keep an
# unresolved area alphabetically first to exercise failure aggregation and
# continuing repairs after errors. Also exercise nested directory traversal.
AREAS="${FIXTURE_SRC}/docs/api-reference"
mkdir -p "${AREAS}/nested"
mv "$DOC" "${AREAS}/widgets.md"
cat > "$DOC" <<'EOF'
# API reference

## Widgets

[Routes](api-reference/widgets.md).
EOF
cat > "${AREAS}/a-unresolved.md" <<'EOF'
| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/missing` | Public | Missing | `pkg/widget/routes.go:99` |
EOF
cat > "${AREAS}/nested/drifted.md" <<'EOF'
| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/widgets` | Public | List | `pkg/widget/routes.go:99` |
EOF
# Deliberately old timestamps distinguish an untouched file from one that
# was rewritten with identical contents, which would still cause churn.
touch -t 200001010000 "$DOC" "${AREAS}/widgets.md"
cp -p "$DOC" "${TMP}/index-before.md"
cp -p "${AREAS}/widgets.md" "${TMP}/widgets-before.md"

set +e
split_output=$("$CHECKER" 2>&1)
split_rc=$?
set -e
if [ "$split_rc" -ne 0 ] && printf '%s\n' "$split_output" | grep -qF 'nested/drifted.md:3 GET /api/widgets'; then
  pass "default run finds drift in split and nested area files"
else
  bad "default run missed split reference drift"
fi

set +e
split_fix_output=$("$CHECKER" --fix "$DOC" 2>&1)
split_fix_rc=$?
set -e
if [ "$split_fix_rc" -ne 0 ] && printf '%s\n' "$split_fix_output" | grep -qF 'UNRESOLVED'; then
  pass "explicit index preserves failures from unresolved areas"
else
  bad "explicit index lost unresolved area failures"
fi
if grep -qF 'pkg/widget/routes.go:5`' "${AREAS}/nested/drifted.md"; then
  pass "--fix continues repairing later areas after an unresolved area"
else
  bad "--fix failed to repair a later area"
fi
if cmp -s "$DOC" "${TMP}/index-before.md" && [ ! "$DOC" -nt "${TMP}/index-before.md" ] &&
   cmp -s "${AREAS}/widgets.md" "${TMP}/widgets-before.md" && [ ! "${AREAS}/widgets.md" -nt "${TMP}/widgets-before.md" ]; then
  pass "--fix does not rewrite the index or an unaffected area"
else
  bad "--fix rewrote an unaffected file"
fi

# Explicit area scope ignores drift in unrelated siblings.
if "$CHECKER" "${AREAS}/widgets.md" >/dev/null 2>&1; then
  pass "explicit area scope checks only that file"
else
  bad "explicit area scope checked unrelated files"
fi
rm "${AREAS}/a-unresolved.md"
if "$CHECKER" "$AREAS" >/dev/null 2>&1; then
  pass "directory argument checks all remaining repaired areas"
else
  bad "directory argument failed on repaired areas"
fi
# Default --fix must reach area files too, while retaining the short index.
sed -i.bak 's#pkg/widget/routes.go:5#pkg/widget/routes.go:99#' "${AREAS}/nested/drifted.md"
rm -f "${AREAS}/nested/drifted.md.bak"
if "$CHECKER" --fix >/dev/null 2>&1 && grep -qF 'pkg/widget/routes.go:5`' "${AREAS}/nested/drifted.md"; then
  pass "default --fix repairs area citations"
else
  bad "default --fix missed area citations"
fi
mkdir "${TMP}/empty"
if "$CHECKER" "${TMP}/empty" >/dev/null 2>&1; then
  bad "empty reference directory must fail rather than silently pass"
else
  pass "empty reference directory fails"
fi

if [ "$fail" -ne 0 ]; then
  echo "api-reference-citations checker tests: FAIL"
  exit 1
fi

echo "api-reference-citations checker tests: PASS"
