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

if printf '%s\n' "$output" | grep -qF 'DRIFT doc:5 GET /api/widgets -> pkg/widget/routes.go:2'; then
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

if [ "$fail" -ne 0 ]; then
  echo "api-reference-citations checker tests: FAIL"
  exit 1
fi

echo "api-reference-citations checker tests: PASS"
