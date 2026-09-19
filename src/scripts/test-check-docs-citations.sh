#!/usr/bin/env bash
# test-check-docs-citations.sh — exercises check-docs-citations.py against
# fixture Markdown trees with valid, missing-file, and out-of-range citations.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER_SRC="${HERE}/check-docs-citations.py"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/docs-citations.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

FIXTURE_SRC="${TMP}/src"
mkdir -p "${FIXTURE_SRC}/scripts" "${FIXTURE_SRC}/docs" "${FIXTURE_SRC}/pkg/widget"
cp "$CHECKER_SRC" "${FIXTURE_SRC}/scripts/check-docs-citations.py"
chmod +x "${FIXTURE_SRC}/scripts/check-docs-citations.py"
CHECKER="${FIXTURE_SRC}/scripts/check-docs-citations.py"
DOCS="${FIXTURE_SRC}/docs"
cat > "${FIXTURE_SRC}/pkg/widget/routes.go" <<'EOF'
package widget

func one() {}
func two() {}
func three() {}
EOF

cat > "${DOCS}/clean.md" <<'EOF'
# Clean

The implementation lives at `pkg/widget/routes.go:3-5` and `src/pkg/widget/routes.go:4`.
EOF
if python3 "$CHECKER" "$DOCS" >/dev/null 2>&1; then
  pass "clean repo-local citations pass"
else
  bad "clean repo-local citations should pass"
fi

cat > "${DOCS}/bad-range.md" <<'EOF'
# Bad range

This drifted citation points past EOF: `pkg/widget/routes.go:99`.
EOF
set +e
output="$(python3 "$CHECKER" "$DOCS" 2>&1)"
rc=$?
set -e
if [ "$rc" -ne 0 ] && printf '%s\n' "$output" | grep -qF 'out of range: 99'; then
  pass "out-of-range line is reported"
else
  bad "expected out-of-range failure"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi
rm -f "${DOCS}/bad-range.md"

cat > "${DOCS}/missing.md" <<'EOF'
# Missing

This file moved: `pkg/widget/missing.go:1`.
EOF
set +e
output="$(python3 "$CHECKER" "$DOCS" 2>&1)"
rc=$?
set -e
if [ "$rc" -ne 0 ] && printf '%s\n' "$output" | grep -qF 'file does not exist'; then
  pass "missing cited file is reported"
else
  bad "expected missing-file failure"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi
rm -f "${DOCS}/missing.md"

cat > "${DOCS}/fenced.md" <<'EOF'
# Fenced

```markdown
`pkg/widget/missing.go:1`
```
EOF
if python3 "$CHECKER" "$DOCS" >/dev/null 2>&1; then
  pass "citations inside fenced examples are ignored"
else
  bad "fenced examples should be ignored"
fi

if [ "$fail" -ne 0 ]; then
  echo "docs-citations checker tests: FAIL"
  exit 1
fi

echo "docs-citations checker tests: PASS"
