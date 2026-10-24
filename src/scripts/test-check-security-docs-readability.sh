#!/usr/bin/env bash
# test-check-security-docs-readability.sh — exercises
# src/scripts/check-security-docs-readability.sh against small fixture
# documents so the guard's pass/fail behaviour is proven, not just its
# verdict on today's security doc set.
#
# The checker appends any extra positional arguments to its fixed file list,
# so each case hands it one fixture by absolute path and asserts on that
# fixture's own line. The fixed list still runs in every case; those files
# are required to pass on their own in CI, so a failure there surfaces as a
# "FAIL" line for a repo path, never for a fixture.
#
# Usage: src/scripts/test-check-security-docs-readability.sh
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/check-security-docs-readability.sh"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/sec-readability.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
out="$tmp/out"

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

# --- Case 1: the committed security doc set passes at the default grade --
if bash "$checker" >"$out" 2>&1; then
  grep -q "^all [0-9]* security docs read at grade 11 or below$" "$out" \
    && note_ok "committed security docs pass at grade 11" \
    || { note_fail "expected the summary line for the committed set:"; cat "$out"; }
else
  note_fail "committed security docs should exit 0:"; cat "$out"
fi

# --- Case 2: plain prose passes and --verbose prints its score -----------
easy="$tmp/easy.md"
cat > "$easy" <<'MD'
# Easy

The hub runs the agents. Each agent has one job. It reads the task and does
the work. If a step fails, the run stops and the log says why.

## Keys

Keys live in the vault. The hub reads them at start. No agent can see a key.
MD
if bash "$checker" --verbose "$easy" >"$out" 2>&1; then
  if grep -qE "^ok   $easy: grade -?[0-9.]+ \(max 11\), [0-9.]+ words/sentence, [0-9]+ prose words$" "$out"; then
    note_ok "plain prose passes and --verbose reports its grade"
  else
    note_fail "expected a verbose ok line for $easy:"; cat "$out"
  fi
else
  note_fail "plain prose fixture should exit 0:"; cat "$out"
fi

# --- Case 3: without --verbose a passing file prints no per-file line ----
if bash "$checker" "$easy" >"$out" 2>&1; then
  grep -q "$easy" "$out" \
    && { note_fail "passing file must be silent without --verbose:"; cat "$out"; } \
    || note_ok "passing file is silent without --verbose"
else
  note_fail "plain prose fixture should exit 0 without --verbose:"; cat "$out"
fi

# --- Case 4: dense prose is caught ---------------------------------------
dense="$tmp/dense.md"
cat > "$dense" <<'MD'
# Dense

Notwithstanding the aforementioned architectural considerations, the
orchestration infrastructure systematically operationalises heterogeneous
authentication methodologies, simultaneously necessitating comprehensive
cryptographic verification of every intermediate representation, thereby
guaranteeing deterministic reproducibility across geographically distributed
computational environments irrespective of organisational boundaries.
Furthermore, the implementation philosophy prioritises extensibility and
configurability over immediate comprehensibility, inevitably complicating
the onboarding experience for inexperienced administrators unfamiliar with
contemporary containerisation paradigms and their associated terminology.
MD
if bash "$checker" "$dense" >"$out" 2>&1; then
  note_fail "dense prose fixture should exit non-zero:"; cat "$out"
else
  grep -qE "^FAIL $dense: grade [0-9.]+ \(max 11\)" "$out" \
    && grep -q "security doc(s) score above grade 11" "$out" \
    && note_ok "dense prose is reported with its grade and the remediation hint" \
    || { note_fail "expected a FAIL line for $dense plus the summary:"; cat "$out"; }
fi

# --- Case 5: MAX_GRADE is honoured ---------------------------------------
# Short, plain sentences can score below grade 0, so the override has to sit
# under any grade real prose can reach.
if MAX_GRADE=-50 bash "$checker" "$easy" >"$out" 2>&1; then
  note_fail "MAX_GRADE=-50 should fail the plain prose fixture:"; cat "$out"
else
  grep -qE "^FAIL $easy: grade -?[0-9.]+ \(max -50\)" "$out" \
    && note_ok "MAX_GRADE overrides the threshold" \
    || { note_fail "expected a FAIL line at max -50 for $easy:"; cat "$out"; }
fi

# --- Case 6: a file that is all code, tables and headings has no prose ---
noprose="$tmp/noprose.md"
cat > "$noprose" <<'MD'
# Only structure

| column | value |
| --- | --- |
| a | 1 |

```sh
echo "this sentence is inside a fence and must not count as prose."
```

## Another heading
MD
if bash "$checker" "$noprose" >"$out" 2>&1; then
  note_fail "prose-free fixture should exit non-zero:"; cat "$out"
else
  grep -q "^FAIL $noprose: no prose found$" "$out" \
    && note_ok "code fences, tables and headings are stripped before scoring" \
    || { note_fail "expected 'no prose found' for $noprose:"; cat "$out"; }
fi

# --- Case 7: a missing file is reported, not swallowed -------------------
missing="$tmp/does-not-exist.md"
if bash "$checker" "$missing" >"$out" 2>&1; then
  note_fail "missing file should exit non-zero:"; cat "$out"
else
  grep -q "^FAIL $missing: " "$out" \
    && note_ok "missing file is reported as a failure" \
    || { note_fail "expected a FAIL line for $missing:"; cat "$out"; }
fi

# --- Case 8: URLs and inline code do not inflate the score ---------------
# Same easy prose, but every sentence carries a long URL and an inline code
# span. If either leaked into the word count the grade would jump well past
# 11; it must stay a pass. --verbose pins the exact prose word count: each URL
# vanishes and each inline-code span collapses to one word (the spans hold several), so a leaked URL
# or span changes the count even when the grade stays low.
noisy="$tmp/noisy.md"
cat > "$noisy" <<'MD'
# Noisy

The hub runs the agents; see <https://example.com/an/extraordinarily/long/documentation/path/for/orchestration>.
Each agent has one job, set in `hive agent orchestration responsibility identifier`.
It reads the task and does the work, logged to https://example.com/incomprehensibly/verbose/observability/endpoint.
If a step fails, the run stops and `hive diagnostics comprehensive report` says why.
MD
if bash "$checker" --verbose "$noisy" >"$out" 2>&1; then
  grep -qE "^ok   $noisy: grade .*, 35 prose words$" "$out" \
    && note_ok "URLs and inline code are dropped before scoring" \
    || { note_fail "expected exactly 35 prose words for $noisy:"; cat "$out"; }
else
  note_fail "URL/inline-code fixture should still pass:"; cat "$out"
fi

if [ "$fail" -ne 0 ]; then
  echo "test-check-security-docs-readability FAILED"
  exit 1
fi
echo "test-check-security-docs-readability OK"
