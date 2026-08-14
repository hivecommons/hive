#!/usr/bin/env bash
# Behavioural tests for bin/gh-wrapper.sh's enforcement gates.
# Run: bash bin/test_gh_wrapper_gates.sh
#
# Unlike v2/deploy/test_entrypoint_*.sh, which grep the script TEXT, this
# harness EXECUTES the wrapper against a stub gh and asserts on exit codes and
# on whether the stub was reached. A parser bug cannot be caught by grepping —
# the audit's N7 bypass (`gh pr --repo o/r merge 123`) looks perfectly normal in
# source and only misbehaves when run.
#
# The stub records its argv to $STUB_LOG. "Reached the stub" means every gate
# allowed the command through — for a merge that is the failure condition unless
# the PR is genuinely merge-eligible.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WRAPPER="${REPO_ROOT}/bin/gh-wrapper.sh"

PASS=0
FAIL=0

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

STUB="${WORK}/gh-stub"
STUB_LOG="${WORK}/stub.log"
cat >"$STUB" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}"
exit 0
STUBEOF
chmod +x "$STUB"

# A scoped-token file, so the H3 token gate near the top of the wrapper does not
# short-circuit every case before we reach the gates under test.
TOKEN_CACHE="${WORK}/token"
echo "ghs_stubtoken" >"$TOKEN_CACHE"

# run_wrapper <mode> <acmm> -- <args...>
# Echoes "exit=<code> stub=<yes|no>" for the assertions below.
run_wrapper() {
  local mode="$1" acmm="$2"; shift 2
  [ "${1:-}" = "--" ] && shift
  : >"$STUB_LOG"
  local out rc
  out="$(
    HIVE_GH_WRAPPER_REAL_GH="$STUB" \
    STUB_LOG="$STUB_LOG" \
    HIVE_AGENT="testagent" \
    HIVE_AGENT_ID="testagent" \
    HIVE_AGENT_MODE="$mode" \
    HIVE_ACMM_LEVEL="$acmm" \
    HIVE_AGENT_TOKEN_CACHE="$TOKEN_CACHE" \
    HIVE_CONTRIBUTOR_MODE="false" \
    bash "$WRAPPER" "$@" 2>&1
  )"
  rc=$?
  local reached=no
  [ -s "$STUB_LOG" ] && reached=yes

  # A wrapper that cannot find the stub exits 1 from the availability guard near
  # the top, which looks EXACTLY like "a gate blocked this". That would make
  # every assert_blocked pass for the wrong reason and hide a real regression —
  # it did, the first time this harness was run. Treat it as a harness error,
  # not a result.
  if [[ "$out" == *"gh CLI is not available"* ]]; then
    echo "harness-error: wrapper could not find the stub gh"
    return 0
  fi
  # Same for the H3 scoped-token gate, which also exits 1 before any mode gate.
  if [[ "$out" == *"per-agent scoped GitHub token not available"* ]]; then
    echo "harness-error: scoped-token gate fired before the gates under test"
    return 0
  fi

  # Tag WHICH gate blocked, so a test cannot pass because some unrelated gate
  # happened to fire. `pr merge` is gated by both the mode case and the
  # merge-eligibility check; without this tag an unknown-mode merge test would
  # go green off the eligibility gate even with the mode arm deleted.
  local gate=other
  [[ "$out" == *"unrecognized agent mode"* ]] && gate=mode-default
  [[ "$out" == *"NOT in merge-eligible.json"* || "$out" == *"cannot verify merge eligibility"* ]] && gate=eligibility
  [[ "$out" == *"needs an explicit PR number or URL"* ]] && gate=no-identifier
  [[ "$out" == *"is not on the hive's allowlist"* ]] && gate=surface

  echo "exit=${rc} stub=${reached} gate=${gate}"
  # Surface wrapper output only when debugging a failure.
  [ -n "${GH_WRAPPER_TEST_VERBOSE:-}" ] && printf '    wrapper said: %s\n' "$out" >&2
  return 0
}

# assert_blocked <label> <result> [expected-gate]
# Passing the gate name pins WHICH check did the blocking.
assert_blocked() {
  local label="$1" result="$2" want_gate="${3:-}"
  local want="exit=1 stub=no"
  [ -n "$want_gate" ] && want="exit=1 stub=no gate=${want_gate}"
  if [[ "$result" == "$want"* && ( -z "$want_gate" || "$result" == *"gate=${want_gate}" ) ]]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        got '$result', want '${want}' (blocked before reaching gh)"
    FAIL=$((FAIL + 1))
  fi
}

assert_reached() {
  local label="$1" result="$2"
  if [[ "$result" == exit=0*stub=yes* ]]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        got '$result', want 'exit=0 stub=yes' (allowed through to gh)"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== gh-wrapper gate tests ==="

# ── N15 chain A: unknown mode must fail CLOSED ────────────────────────────────
# Before the fix an unrecognized mode matched no case arm and fell through with
# ZERO enforcement. The mode is read from a world-writable /tmp path whose name
# derives from agent-controlled env, so this was plantable and durable.
echo "-- unknown mode fails closed (N15 chain A) --"
assert_blocked "garbage mode blocks pr merge" \
  "$(run_wrapper "TOTALLY_BOGUS" 5 -- pr merge 123)" mode-default
assert_blocked "garbage mode blocks issue create" \
  "$(run_wrapper "TOTALLY_BOGUS" 5 -- issue create --title x --body y)" mode-default
assert_blocked "lowercase variant of a real mode is still unknown" \
  "$(run_wrapper "issues_only" 5 -- pr merge 123)" mode-default
assert_blocked "mode with trailing junk is unknown" \
  "$(run_wrapper "ISSUES_ONLY_EXTRA" 5 -- pr merge 123)" mode-default

# ── Known modes keep working (no over-correction) ─────────────────────────────
echo "-- known modes still enforce as before --"
assert_blocked "NO_GITHUB blocks pr" \
  "$(run_wrapper "NO_GITHUB" 5 -- pr view 1)"
assert_blocked "NO_GITHUB blocks issue" \
  "$(run_wrapper "NO_GITHUB" 5 -- issue view 1)"
assert_blocked "ISSUES_ONLY blocks pr merge" \
  "$(run_wrapper "ISSUES_ONLY" 5 -- pr merge 123)"
assert_blocked "ISSUES_AND_PRS blocks pr merge" \
  "$(run_wrapper "ISSUES_AND_PRS" 5 -- pr merge 123)"

# A read-only command under a permissive mode must still reach gh, or the
# wrapper would be useless.
assert_reached "ISSUES_PRS_MERGE allows a read (pr view)" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr view 1)"
assert_reached "NO_GITHUB allows a non-issue/pr subcommand (repo view)" \
  "$(run_wrapper "NO_GITHUB" 5 -- repo view)"

# ── N7: the flag-reorder bypass (documents the CURRENT state) ─────────────────
# `gh pr --repo o/r merge 123` makes the subcmd/action extractor read --repo's
# VALUE as the action, so action != "merge" and every gate is skipped. The fix
# is a real flag parser; until it lands these cases record the live bypass so
# the harness fails loudly the moment behaviour changes in either direction.
# ── N7: the flag-reorder bypass ───────────────────────────────────────────────
# `gh pr --repo o/r merge 123` used to make the subcmd/action extractor read
# --repo's VALUE as the action, so action != "merge" and every gate was skipped.
# Each of these must be gated for the SAME reason the un-reordered form is.
echo "-- N7 flag-reorder must not skip the gates --"
assert_blocked "reordered merge is gated under ISSUES_AND_PRS" \
  "$(run_wrapper "ISSUES_AND_PRS" 5 -- pr --repo owner/repo merge 123)"
assert_blocked "reordered merge is gated under ISSUES_ONLY" \
  "$(run_wrapper "ISSUES_ONLY" 5 -- pr --repo owner/repo merge 123)"
assert_blocked "reordered merge is gated under ADVISORY" \
  "$(run_wrapper "ADVISORY" 5 -- pr --repo owner/repo merge 123)"
assert_blocked "-R short form is gated too" \
  "$(run_wrapper "ISSUES_AND_PRS" 5 -- pr -R owner/repo merge 123)"
assert_blocked "a value-taking flag before the action does not hide it (--title)" \
  "$(run_wrapper "ISSUES_ONLY" 5 -- pr --title 'merge me' merge 123)"
# --flag=value is self-contained and must NOT consume the next token.
assert_blocked "--repo=value form is gated" \
  "$(run_wrapper "ISSUES_AND_PRS" 5 -- pr --repo=owner/repo merge 123)"

# At the most permissive mode the merge-eligibility gate is the wall. A PR that
# is not in merge-eligible.json must be refused however the args are arranged.
echo "-- N7 eligibility gate applies to every merge form --"
assert_blocked "reordered merge still hits eligibility at ISSUES_PRS_MERGE" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr --repo owner/repo merge 123)" eligibility
assert_blocked "plain merge hits eligibility at ISSUES_PRS_MERGE" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr merge 123)" eligibility
# No identifier => cannot be checked against merge-eligible.json => denied.
# The wrapper cannot resolve the current branch's PR without calling gh, which
# is the very thing under gate, so requiring an explicit number is the only
# fail-closed answer.
assert_blocked "bare 'pr merge' (current branch, no number) is not a free pass" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr merge)" no-identifier
assert_blocked "'pr merge --repo o/r' with no number is not a free pass" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr merge --repo owner/repo)" no-identifier

# Availability: forms that gh accepts must reach the eligibility check rather
# than being wrongly denied. A PR URL previously landed in pr_num verbatim and
# never matched a number, so a legitimate merge was refused.
assert_blocked "PR URL is normalized and reaches the eligibility gate" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr merge https://github.com/owner/repo/pull/123)" eligibility
assert_blocked "-R form reaches the eligibility gate (not a parse denial)" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr merge 123 -R owner/repo)" eligibility

# ── #3840: general command surface is an ALLOWLIST, not a denylist ───────────
# Before the fix the script ended in a bare `exec "$REAL_GH" "$@"`, so any
# subcommand no gate named reached GitHub with the App token attached. Each of
# these was verified reaching gh with rc=0 on v4 @ c9ea2cc8 — several in
# NO_GITHUB mode, because the mode gates only ever inspect `issue`/`pr`.
#
# These assert on `gate=surface` specifically. Without that pin a test could go
# green off some unrelated gate (NO_GITHUB blocks pr/issue, ADVISORY blocks
# creates) and keep passing even if the allowlist were deleted entirely.
echo "-- general surface denies by default (#3840) --"
assert_blocked "gh auth token cannot exfiltrate the App token (NO_GITHUB)" \
  "$(run_wrapper "NO_GITHUB" 5 -- auth token)" surface
assert_blocked "gh secret set is denied (NO_GITHUB)" \
  "$(run_wrapper "NO_GITHUB" 5 -- secret set FOO --body bar)" surface
assert_blocked "gh ssh-key add is denied (NO_GITHUB)" \
  "$(run_wrapper "NO_GITHUB" 5 -- ssh-key add /tmp/k.pub)" surface
assert_blocked "gh variable set is denied (NO_GITHUB)" \
  "$(run_wrapper "NO_GITHUB" 5 -- variable set X --body y)" surface
assert_blocked "gh repo delete is denied (ISSUES_ONLY)" \
  "$(run_wrapper "ISSUES_ONLY" 5 -- repo delete owner/repo --yes)" surface
assert_blocked "gh release create is denied (ISSUES_ONLY)" \
  "$(run_wrapper "ISSUES_ONLY" 5 -- release create v9.9.9)" surface
assert_blocked "gh gist create is denied (ISSUES_ONLY)" \
  "$(run_wrapper "ISSUES_ONLY" 5 -- gist create /etc/passwd)" surface
assert_blocked "gh workflow run is denied (ADVISORY)" \
  "$(run_wrapper "ADVISORY" 5 -- workflow run deploy.yml)" surface
# Denial is per-ACTION, not per-subcommand: an allowed subcommand must not carry
# a destructive action through with it.
assert_blocked "an unlisted action on an allowed subcommand is denied (repo archive)" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- repo archive owner/repo)" surface
assert_blocked "an unlisted action on an allowed subcommand is denied (release delete)" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- release delete v1)" surface
# The N7 flag parser feeds this gate, so the reorder trick must not smuggle a
# denied verb past it either.
assert_blocked "flag-reordered denied verb is still denied" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- secret --repo owner/repo set FOO)" surface

# ── POSITIVE CONTROL: the allowlist must not be "deny everything" ────────────
# This is the control that makes the block-assertions above meaningful. If these
# fail, the fix has broken real agent workflows — which is a worse outcome than
# the vulnerability, and exactly how a "security fix" gets reverted wholesale.
# Every command here is one agents actually run per v2/policies/*.md,
# examples/kubestellar/agents/** and bin/*.sh.
echo "-- allowlist preserves the operations agents actually perform --"
assert_reached "issue view reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- issue view 1 --repo owner/repo)"
assert_reached "pr view reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr view 1 --repo owner/repo)"
assert_reached "pr diff reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr diff 1 --repo owner/repo)"
assert_reached "pr checks reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr checks 1 --repo owner/repo)"
assert_reached "search issues reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- search issues --repo owner/repo)"
assert_reached "search prs reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- search prs --repo owner/repo)"
assert_reached "run list reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- run list --repo owner/repo)"
assert_reached "run view reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- run view 123 --repo owner/repo)"
assert_reached "release list reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- release list --repo owner/repo)"
assert_reached "repo view reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- repo view owner/repo)"
assert_reached "repo fork (contributor flow) reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- repo fork owner/repo --clone=false)"
assert_reached "read-only gh api reaches gh" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- api repos/owner/repo/pulls)"

# The allowlist admits `pr merge` as a VERB; the merge-eligibility allowlist is
# what actually decides it. Asserting gate=eligibility here proves the surface
# gate did not swallow the merge path — if it had, the eligibility gate would
# never run and this would report gate=surface.
assert_blocked "pr merge passes the surface gate and is held by eligibility" \
  "$(run_wrapper "ISSUES_PRS_MERGE" 5 -- pr merge 123)" eligibility

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
