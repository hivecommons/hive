#!/usr/bin/env bash
# test-check-docs-citations.sh — exercises check-docs-citations.py against
# fixture Markdown trees with valid, missing-file, and out-of-range citations,
# plus the --fix Go-declaration re-anchoring and its fail-closed rules (#14713).
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

func spacerA() {}
func spacerB() {}
func spacerC() {}

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

cat > "${DOCS}/bad-anchor.md" <<'EOF'
# Bad anchor

This helper moved away from `pkg/widget/routes.go:2`, where `two()` should appear.
EOF
set +e
output="$(python3 "$CHECKER" "$DOCS" 2>&1)"
rc=$?
set -e
if [ "$rc" -ne 0 ] && printf '%s\n' "$output" | grep -qF 'lacks anchor(s) near cited range: two()'; then
  pass "stale symbol anchor is reported"
else
  bad "expected stale-anchor failure"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi
rm -f "${DOCS}/bad-anchor.md"

cat > "${DOCS}/good-anchor.md" <<'EOF'
# Good anchor

This helper is cited at `pkg/widget/routes.go:8`, where `two()` appears.
EOF
if python3 "$CHECKER" "$DOCS" >/dev/null 2>&1; then
  pass "matching symbol anchor passes"
else
  bad "matching symbol anchor should pass"
fi
rm -f "${DOCS}/good-anchor.md"

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
rm -f "${DOCS}/fenced.md"

# --fix: re-anchor drifted citations by unique top-level Go declaration.
cat > "${FIXTURE_SRC}/pkg/widget/moved.go" <<'EOF'
package widget

// padding
// padding
// padding
// padding
// padding
// padding

func Moved(n int) int {
	total := 0
	for i := 0; i < n; i++ {
		total += i
	}
	return total
}

type Grown struct {
	A    int
	B    int
	C    int
	D    int
	E    int
	F    int
	Tail string
}

func Dup() {}

type Widget struct{}

func (w *Widget) Dup() {}
EOF
cat > "${FIXTURE_SRC}/pkg/widget/run.sh" <<'EOF'
#!/bin/sh
# padding
# padding
# padding
# padding
# padding

helper() {
  :
}
EOF

# run_fix <doc-basename> <body-line>: writes the doc, runs --fix, and leaves
# rc/output in globals for the caller.
run_fix() {
  printf '# Fix\n\n%s\n' "$2" > "${DOCS}/$1"
  set +e
  output="$(python3 "$CHECKER" --fix "$DOCS" 2>&1)"
  rc=$?
  set -e
}

# expect_untouched <doc-basename> <body-line> <label>
expect_untouched() {
  local doc="$1" body="$2" label="$3"
  run_fix "$doc" "$body"
  if [ "$rc" -ne 0 ] && printf '%s\n' "$output" | grep -qF 'DRIFT ' \
      && [ "$(sed -n 3p "${DOCS}/${doc}")" = "$body" ]; then
    pass "$label"
  else
    bad "$label"
    printf '%s\n' "$output" | sed 's/^/      | /'
    sed 's/^/      doc| /' "${DOCS}/${doc}"
  fi
  rm -f "${DOCS}/${doc}"
}

run_fix fix-moved.md 'Moved lives at `pkg/widget/moved.go:3`, where `Moved` is declared.'
if [ "$rc" -eq 0 ] && grep -qF '`pkg/widget/moved.go:10`, where `Moved`' "${DOCS}/fix-moved.md" \
    && printf '%s\n' "$output" | grep -qF 'FIXED '; then
  pass "--fix re-anchors a moved function to its declaration"
else
  bad "--fix should re-anchor a moved function"
  printf '%s\n' "$output" | sed 's/^/      | /'
  sed 's/^/      doc| /' "${DOCS}/fix-moved.md"
fi
cp "${DOCS}/fix-moved.md" "${TMP}/fix-moved.first"
set +e
output="$(python3 "$CHECKER" --fix "$DOCS" 2>&1)"
rc=$?
set -e
if [ "$rc" -eq 0 ] && cmp -s "${DOCS}/fix-moved.md" "${TMP}/fix-moved.first" \
    && ! printf '%s\n' "$output" | grep -qF 'FIXED '; then
  pass "--fix is idempotent (second run makes no change)"
else
  bad "--fix second run should be a no-op"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi
if python3 "$CHECKER" "$DOCS" >/dev/null 2>&1; then
  pass "re-anchored citation verifies clean under the checker"
else
  bad "re-anchored citation should verify clean"
fi
rm -f "${DOCS}/fix-moved.md"

run_fix fix-grown.md 'The struct is at `pkg/widget/moved.go:10-12`, where `Grown` holds `Tail`.'
if [ "$rc" -eq 0 ] && grep -qF '`pkg/widget/moved.go:18-25`, where `Grown`' "${DOCS}/fix-grown.md"; then
  pass "--fix widens the range to cover every anchor after a declaration grew"
else
  bad "--fix should widen to 18-25"
  printf '%s\n' "$output" | sed 's/^/      | /'
  sed 's/^/      doc| /' "${DOCS}/fix-grown.md"
fi
rm -f "${DOCS}/fix-grown.md"

expect_untouched fix-dup.md 'Dup is at `pkg/widget/moved.go:3`, where `Dup` is defined.' \
  "--fix leaves a citation whose anchor matches two declarations untouched + DRIFT"
expect_untouched fix-absent.md 'Gone is at `pkg/widget/moved.go:3`, where `Vanished` is defined.' \
  "--fix leaves a citation whose anchor is absent untouched + DRIFT"
expect_untouched fix-outside.md 'See `pkg/widget/moved.go:3`, where `Moved` reads `Tail`.' \
  "--fix leaves a citation untouched when an anchor is outside the declaration"
expect_untouched fix-no-anchor.md 'No anchors here: `pkg/widget/moved.go:99`.' \
  "--fix leaves a citation with no anchors untouched + DRIFT"
expect_untouched fix-non-go.md 'The helper is at `pkg/widget/run.sh:1`, where `helper` is defined.' \
  "--fix leaves a non-.go citation untouched + DRIFT"

if [ "$fail" -ne 0 ]; then
  echo "docs-citations checker tests: FAIL"
  exit 1
fi

echo "docs-citations checker tests: PASS"
