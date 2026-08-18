#!/usr/bin/env bash
# Behavioural tests for bin/gh-wrapper.sh's enforcement gates.
# Run: bash bin/test_gh_wrapper_gates.sh
#
# Unlike src/deploy/test_entrypoint_*.sh, which grep the script TEXT, this
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
# Every command here is one agents actually run per src/policies/*.md,
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

# ── #4043: label injection is best-effort, never a wall ──────────────────────
# The edit/create arms append the agent/hive provenance labels. When the label
# does not exist on the target repo, gh fails the WHOLE operation with
# "'<label>' not found" — which took down a fleet owner's edit lane. The
# wrapper must retry without labels. This stub simulates gh's behavior by
# failing any invocation that carries a label-injection flag; the wrapper runs
# under `set -e`, so these tests also pin that the retry is not dead code.
echo "-- label injection falls back instead of failing the operation (#4043) --"
LABEL_STUB="${WORK}/gh-stub-labelfail"
cat >"$LABEL_STUB" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}"
case " $* " in
  *" --add-label "*|*" --label "*) exit 1 ;;
esac
exit 0
STUBEOF
chmod +x "$LABEL_STUB"

# run_label_wrapper <args...> — like run_wrapper but with the label-failing
# stub, and it reports the LAST stub invocation so the assertions can prove the
# retry dropped the injected labels. Per-repo ensure caches live in /tmp; clear
# ours so the label-create calls are deterministic.
run_label_wrapper() {
  : >"$STUB_LOG"
  rm -f /tmp/.hive-labels-ensured-owner_repo
  local out rc last
  out="$(
    HIVE_GH_WRAPPER_REAL_GH="$LABEL_STUB" \
    STUB_LOG="$STUB_LOG" \
    HIVE_AGENT="testagent" \
    HIVE_AGENT_ID="testagent" \
    HIVE_AGENT_MODE="ISSUES_PRS_MERGE" \
    HIVE_ACMM_LEVEL=5 \
    HIVE_AGENT_TOKEN_CACHE="$TOKEN_CACHE" \
    HIVE_CONTRIBUTOR_MODE="false" \
    bash "$WRAPPER" "$@" 2>&1
  )"
  rc=$?
  rm -f /tmp/.hive-labels-ensured-owner_repo
  last="$(tail -n 1 "$STUB_LOG" 2>/dev/null)"
  echo "exit=${rc} last=${last}"
}

result="$(run_label_wrapper pr edit 5 --repo owner/repo --title x)"
if [[ "$result" == "exit=0 last=pr edit 5 --repo owner/repo --title x" ]]; then
  echo "  PASS: pr edit retries without injected labels and succeeds"
  PASS=$((PASS + 1))
else
  echo "  FAIL: pr edit retries without injected labels and succeeds"
  echo "        got '$result', want exit=0 with an unlabeled final retry"
  FAIL=$((FAIL + 1))
fi

result="$(run_label_wrapper issue edit 7 --repo owner/repo --add-assignee z)"
if [[ "$result" == "exit=0 last=issue edit 7 --repo owner/repo --add-assignee z" ]]; then
  echo "  PASS: issue edit retries without injected labels and succeeds"
  PASS=$((PASS + 1))
else
  echo "  FAIL: issue edit retries without injected labels and succeeds"
  echo "        got '$result', want exit=0 with an unlabeled final retry"
  FAIL=$((FAIL + 1))
fi

result="$(run_label_wrapper issue create --repo owner/repo --title t --body b)"
if [[ "$result" == exit=0* && "$result" != *"--label"* ]]; then
  echo "  PASS: issue create retries without injected labels and succeeds"
  PASS=$((PASS + 1))
else
  echo "  FAIL: issue create retries without injected labels and succeeds"
  echo "        got '$result', want exit=0 with an unlabeled final retry"
  FAIL=$((FAIL + 1))
fi

# ── #4043: empty per-agent token cache must fail LOUD, not go unauthenticated ─
# A pre-created-but-not-yet-minted (0-byte) cache passed the old -f check and
# exported an EMPTY GH_TOKEN, sending every call out unauthenticated. Direct
# invocation (not run_wrapper) because the harness treats this gate's message
# as a harness error elsewhere.
echo "-- empty token cache fails loud (#4043) --"
EMPTY_CACHE="${WORK}/empty-token"
: >"$EMPTY_CACHE"
out="$(
  HIVE_GH_WRAPPER_REAL_GH="$STUB" \
  STUB_LOG="$STUB_LOG" \
  HIVE_AGENT="testagent" \
  HIVE_AGENT_ID="testagent" \
  HIVE_AGENT_MODE="ISSUES_PRS_MERGE" \
  HIVE_AGENT_TOKEN_CACHE="$EMPTY_CACHE" \
  HIVE_CONTRIBUTOR_MODE="false" \
  bash "$WRAPPER" issue view 1 --repo owner/repo 2>&1
)"
rc=$?
if [[ $rc -eq 1 && "$out" == *"per-agent scoped GitHub token not available"* ]]; then
  echo "  PASS: empty token cache blocks with the fail-loud message"
  PASS=$((PASS + 1))
else
  echo "  FAIL: empty token cache blocks with the fail-loud message"
  echo "        got rc=$rc out='$out'"
  FAIL=$((FAIL + 1))
fi

# ── #4044: author gate must work for App INSTALLATION tokens ─────────────────
# Staff agents hold ghs_ installation tokens, for which `gh api user` 403s
# unconditionally — the #3982 oracle could NEVER resolve for them, so every
# author-gated list failed closed fleet-wide. The fix: the hive (which mints
# the tokens) publishes the bot login to a trusted file in the agent-token dir
# (dev-owned; no agent UID can write it). These tests drive the wrapper with a
# stub that reproduces the 403, exactly like production installation tokens.
#
# HIVE_GH_WRAPPER_BOT_LOGIN_FILE relocates the trusted file for the harness
# only — the wrapper honors it solely under HIVE_GH_WRAPPER_REAL_GH, where the
# caller already substitutes the gh binary itself and the override grants
# nothing new.
echo "-- author gate resolves staff identity from the trusted file (#4044) --"

# Stub reproducing an App installation token: /user is structurally
# unavailable (HTTP 403 "Resource not accessible by integration").
INSTALL_STUB="${WORK}/gh-stub-installation"
cat >"$INSTALL_STUB" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}"
if [ "${1:-}" = "api" ] && [ "${2:-}" = "user" ]; then
  echo "gh: Resource not accessible by integration (HTTP 403)" >&2
  exit 1
fi
exit 0
STUBEOF
chmod +x "$INSTALL_STUB"

# Stub reproducing a USER token (contributor path): /user resolves normally.
USER_STUB="${WORK}/gh-stub-usertoken"
cat >"$USER_STUB" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}"
if [ "${1:-}" = "api" ] && [ "${2:-}" = "user" ]; then
  echo "someuser"
fi
exit 0
STUBEOF
chmod +x "$USER_STUB"

TRUSTED_LOGIN_FILE="${WORK}/gh-bot-login"
printf 'test-app[bot]\n' >"$TRUSTED_LOGIN_FILE"

# run_author_wrapper <stub> <login-file> [extra VAR=val...] -- <args...>
# Reports "exit=<rc> last=<final stub argv> ::: <wrapper output>" so assertions
# can pin both the decision and WHAT was forwarded to gh.
run_author_wrapper() {
  local stub="$1" login_file="$2"; shift 2
  local extra_env=()
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do extra_env+=("$1"); shift; done
  [ "${1:-}" = "--" ] && shift
  : >"$STUB_LOG"
  local out rc
  # ${arr[@]+...} keeps the empty-array case safe under `set -u` on bash 3.x.
  out="$(
    env ${extra_env[@]+"${extra_env[@]}"} \
    HIVE_GH_WRAPPER_REAL_GH="$stub" \
    HIVE_GH_WRAPPER_BOT_LOGIN_FILE="$login_file" \
    STUB_LOG="$STUB_LOG" \
    HIVE_AGENT="testagent" \
    HIVE_AGENT_ID="testagent" \
    HIVE_AGENT_MODE="ISSUES_PRS_MERGE" \
    HIVE_ACMM_LEVEL=5 \
    HIVE_AGENT_TOKEN_CACHE="$TOKEN_CACHE" \
    HIVE_CONTRIBUTOR_MODE="false" \
    bash "$WRAPPER" "$@" 2>&1
  )"
  rc=$?
  local last
  last="$(tail -n 1 "$STUB_LOG" 2>/dev/null)"
  echo "exit=${rc} last=${last} ::: ${out}"
}

assert_author() {
  local label="$1" result="$2" want="$3"
  if [[ "$result" == $want ]]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        got '$result'"
    echo "        want glob '$want'"
    FAIL=$((FAIL + 1))
  fi
}

# THE regression: an installation-token agent with the trusted file present
# must be able to list its own items — and only its own.
assert_author "installation token + trusted file: self-listing by bot login works" \
  "$(run_author_wrapper "$INSTALL_STUB" "$TRUSTED_LOGIN_FILE" -- issue list --repo owner/repo --author 'test-app[bot]')" \
  "exit=0 last=issue list --repo owner/repo --author test-app\[bot\] ::: *"
assert_author "matching is [bot]-suffix-insensitive (bare slug allowed)" \
  "$(run_author_wrapper "$INSTALL_STUB" "$TRUSTED_LOGIN_FILE" -- pr list --repo owner/repo --author test-app)" \
  "exit=0 last=pr list --repo owner/repo --author test-app ::: *"
# @me has no server-side meaning for an installation token — the wrapper must
# substitute the trusted identity, or there is no working self-listing form.
assert_author "--author @me is rewritten to the trusted bot identity" \
  "$(run_author_wrapper "$INSTALL_STUB" "$TRUSTED_LOGIN_FILE" -- pr list --repo owner/repo --author @me)" \
  "exit=0 last=pr list --repo owner/repo --author test-app\[bot\] ::: *"
assert_author "--author=@me equals-form is rewritten too" \
  "$(run_author_wrapper "$INSTALL_STUB" "$TRUSTED_LOGIN_FILE" -- pr list --repo owner/repo --author=@me)" \
  "exit=0 last=pr list --repo owner/repo --author=test-app\[bot\] ::: *"

# The gate itself must keep gating: a foreign author is still refused.
assert_author "trusted file does not open listing OTHER authors" \
  "$(run_author_wrapper "$INSTALL_STUB" "$TRUSTED_LOGIN_FILE" -- issue list --repo owner/repo --author someone-else)" \
  "exit=1 last= ::: *must match the authenticated GitHub identity*"

# Fail-closed is preserved when no trusted identity exists: the installation
# token cannot resolve /user, so with the file missing there is NO oracle.
assert_author "missing trusted file + installation token: explicit author fails closed" \
  "$(run_author_wrapper "$INSTALL_STUB" "${WORK}/no-such-login-file" -- issue list --repo owner/repo --author 'test-app[bot]')" \
  "exit=1 last=api user --jq .login ::: *requires authenticated GitHub identity*"
: >"${WORK}/empty-login-file"
assert_author "EMPTY trusted file + installation token: fails closed (no empty identity)" \
  "$(run_author_wrapper "$INSTALL_STUB" "${WORK}/empty-login-file" -- issue list --repo owner/repo --author 'test-app[bot]')" \
  "exit=1 last=api user --jq .login ::: *requires authenticated GitHub identity*"
assert_author "missing trusted file + installation token: @me fails closed with the #4044 diagnosis" \
  "$(run_author_wrapper "$INSTALL_STUB" "${WORK}/no-such-login-file" -- pr list --repo owner/repo --author @me)" \
  "exit=1 last=api user --jq .login ::: *cannot work with an App installation token*"

# POSITIVE CONTROL (the #3982 invariant): agent-controlled environment must
# never seed the trusted identity. With no trusted file and a 403ing /user,
# these env vars are the ONLY place the claimed identity appears — if any of
# them were consulted, this listing would succeed.
assert_author "env vars cannot seed the identity (#3982 invariant holds)" \
  "$(run_author_wrapper "$INSTALL_STUB" "${WORK}/no-such-login-file" \
      HIVE_AUTH_LOGIN_CACHED="test-app[bot]" HIVE_BOT_LOGIN="test-app[bot]" GITHUB_USER="test-app[bot]" \
      -- issue list --repo owner/repo --author 'test-app[bot]')" \
  "exit=1 last=api user --jq .login ::: *requires authenticated GitHub identity*"

# USER-token path unchanged: with no trusted file, `gh api user` remains the
# oracle — resolves, matches, mismatches refuse. (Contributor mode itself is an
# image marker the harness cannot plant; the oracle chain is what's under test.)
assert_author "user token without trusted file still resolves via gh api user" \
  "$(run_author_wrapper "$USER_STUB" "${WORK}/no-such-login-file" -- issue list --repo owner/repo --author someuser)" \
  "exit=0 last=issue list --repo owner/repo --author someuser ::: *"
assert_author "user token without trusted file still refuses a foreign author" \
  "$(run_author_wrapper "$USER_STUB" "${WORK}/no-such-login-file" -- issue list --repo owner/repo --author someone-else)" \
  "exit=1 last=api user --jq .login ::: *must match the authenticated GitHub identity*"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
