#!/bin/bash
# gh wrapper — enforces per-agent + global restrictions and injects App token.
# Installed at /usr/local/bin/gh (ahead of /usr/bin/gh in PATH).
#
# Per-agent restrictions live at /etc/hive/restrictions/<agent-id>.json.
# The wrapper reads HIVE_AGENT_ID to find the right file.
#
# Restriction file format:
#   { "rules": [
#       { "pattern": "gh issue list*", "reason": "Use actionable.json", "enabled": true },
#       { "pattern": "gh api repos/*/issues*", "reason": "Enumeration disabled", "enabled": true }
#   ]}
#
# Pattern matching: the full command ("gh issue list --repo foo") is checked
# against each pattern using bash glob matching. Patterns support * wildcards.

set -euo pipefail

# The real gh binary is installed at /opt/hive/bin/gh-real (src/Dockerfile:74),
# NOT /usr/bin/gh — which does not exist in the image. A stale /usr/bin/gh path
# made the guard below fire "gh CLI is not available" for every agent gh call,
# silently breaking the CLI GitHub workflow (issue/PR view, create, merge). Keep
# the legacy path as a fallback for any environment that does ship it there.
# HIVE_GH_WRAPPER_REAL_GH lets the test harness point the wrapper at a stub so
# the gates can be exercised as a black box (bin/test_gh_wrapper_gates.sh). It is
# NOT a security control and grants nothing: an agent that could set it could
# equally invoke its own binary directly, and every gate below runs before this
# path is ever exec'd. Unset in production, where the image ships the real gh.
REAL_GH="${HIVE_GH_WRAPPER_REAL_GH:-/opt/hive/bin/gh-real}"
[[ -x "$REAL_GH" ]] || REAL_GH="/usr/bin/gh"
RESTRICTIONS_DIR="/etc/hive/restrictions"
CONTRIBUTOR_MODE_MARKER="/etc/hive/contributor-mode"

# Trusted bot-identity file (#4044). Staff agents authenticate with GitHub App
# INSTALLATION tokens (ghs_…), for which `gh api user` structurally 403s —
# there is no user identity behind a server-to-server token — so the #3982
# oracle can never resolve for them. The hive process, which MINTS every tier
# token and therefore knows the App bot login ("<app-slug>[bot]"), writes that
# login here alongside the per-agent token caches. The directory is owned by
# dev and not writable by any agent UID, so — like CONTRIBUTOR_MODE_MARKER —
# this is an image/runtime property the caller cannot forge. The path is a
# CONSTANT for the same reason as #3249: an env-selected path would let an
# agent point the identity check at a file it controls.
#
# HIVE_GH_WRAPPER_BOT_LOGIN_FILE is honored ONLY while the test harness's
# HIVE_GH_WRAPPER_REAL_GH override is active, where it grants nothing: a caller
# who can substitute the gh binary itself has already bypassed every gate this
# identity feeds (see the REAL_GH comment above). Unset in production.
BOT_LOGIN_FILE="/var/run/hive-metrics/agent-tokens/gh-bot-login"
if [[ -n "${HIVE_GH_WRAPPER_REAL_GH:-}" && -n "${HIVE_GH_WRAPPER_BOT_LOGIN_FILE:-}" ]]; then
  BOT_LOGIN_FILE="$HIVE_GH_WRAPPER_BOT_LOGIN_FILE"
fi

# Contributor mode is an image property, not a caller-controlled environment
# toggle. Keep this path constant: an agent can set its own environment and must
# not be able to redirect the trust check to an agent-writable marker (#3249).
# The env var HIVE_CONTRIBUTOR_MODE is equally caller-controlled and must never
# switch token injection or PR routing.
_contributor_mode() {
  [[ -f "$CONTRIBUTOR_MODE_MARKER" ]]
}

# _deny <code> <message> — the ONE denial format every gate below uses (#12127).
#
# Every blocked call used to print a 2–8 line explanation, and every one of
# those lines landed in the agent's context window. A denial now costs one
# line: what was blocked, why, and what to do instead, followed by a
# machine-readable `reason=<code>` so a policy, a test or a log scraper can
# key on the gate without parsing prose. The codes are stable identifiers
# (snake_case, no spaces); the prose may be reworded, the code may not.
#
# Set HIVE_GH_DENY_VERBOSE=1 to append a second line pointing at the docs —
# never printed by default, because the one line already says what to do.
_deny() {
  local code="$1" msg="$2"
  echo "⛔ BLOCKED: ${msg} reason=${code}" >&2
  [ -n "${HIVE_GH_DENY_VERBOSE:-}" ] && echo "   See src/docs/github-write-surface.md and src/docs/troubleshooting.md (reason=${code})." >&2
  exit 1
}

# Guard: if the real gh binary is not installed, tell the agent to use MCP instead.
if [[ ! -x "$REAL_GH" ]]; then
  _deny gh_unavailable "gh CLI is not available in this environment; use the GitHub MCP server (get_pull_request, get_issue, get_file_contents, search_code) and do NOT install, symlink or download a gh binary."
fi

# Inject GitHub App token for agent gh calls (15k/hr vs PAT's 5k/hr).
# Contributors keep their personal token — they fork+PR with their own identity.
# Token-access audit events (#6287). The durable log the dashboard serves
# (GET /api/token-access) is owned by the hive UID, mode 0600, and NO agent
# can open it: this wrapper runs as the agent UID, and an
# audit log the audited party can write is not an audit log. Events are
# dropped here as one file each and the hive ingests them, attributing each
# to the uid that OWNS the file rather than the uid the event claims. This
# path deliberately has no environment override (an override would let an
# agent redirect its own audit events into the void); it must match
# TokenAccessSpoolDir in pkg/github/token_access_audit.go.
TOKEN_ACCESS_SPOOL="/var/run/hive-metrics/token-access-events"
emit_token_access_event() {
  local _evt op agent uid
  op="$1"
  agent="$2"
  uid="$3"
  shift 3
  _evt="${TOKEN_ACCESS_SPOOL}/$(date -u +%s%N)-$$-${RANDOM}.json"
  (
    umask 027
    python3 - "$_evt.tmp" "$op" "$agent" "$uid" "$@" <<'PY'
import datetime
import json
import shlex
import sys

path, op, agent, uid_s, *args = sys.argv[1:]
event = {
    "ts": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "agent": agent or "unknown",
    "uid": int(uid_s),
    "op": op,
}
if op == "gh":
    event["cmd"] = "gh " + shlex.join(args)
else:
    raise SystemExit(f"unsupported token-access op: {op}")
with open(path, "w", encoding="utf-8") as f:
    f.write(json.dumps(event, separators=(",", ":")) + "\n")
PY
  ) && mv -f "${_evt}.tmp" "$_evt"
}
if ! _contributor_mode; then
  # Per-agent scoped token (Phase 4) — 0640 dev:hive-<agent>, least-privilege,
  # readable ONLY by the owning agent's private group. This is the ONLY token an
  # agent may use.
  #
  # SECURITY (audit H3, CWE-522/732): there is deliberately NO fallback to the
  # shared full-privilege installation-token cache
  # (/var/run/hive-metrics/gh-app-token.cache) nor to the full-token-derived
  # HIVE_GITHUB_TOKEN env var. Both carry the FULL installation token; falling
  # back to either would silently escalate every agent to full privilege and
  # defeat per-agent tier scoping. A missing scoped token must FAIL LOUD so the
  # operator fixes token delivery — it must never quietly escalate.
  # -r and -s as well as -f (#4043): an unreadable cache made the `cat` inside
  # the `export` assignment fail with its status MASKED by export's own exit 0
  # (so even `set -e` never saw it), and an empty pre-created cache (pod rolled,
  # token not yet re-minted) passed -f outright — both exported an EMPTY
  # GH_TOKEN and sent every gh call out UNAUTHENTICATED ("please run gh auth
  # login"), instead of the fail-loud this gate promises.
  if [[ -n "${HIVE_AGENT_TOKEN_CACHE:-}" && -f "${HIVE_AGENT_TOKEN_CACHE}" && -r "${HIVE_AGENT_TOKEN_CACHE}" && -s "${HIVE_AGENT_TOKEN_CACHE}" ]]; then
    export GH_TOKEN="$(cat "$HIVE_AGENT_TOKEN_CACHE")"
    # GHE spokes (github.ibm.com etc.): gh only reads GH_TOKEN for github.com;
    # any other GH_HOST authenticates from GH_ENTERPRISE_TOKEN. Exporting both
    # is harmless on github.com and makes the scoped token work on GHE — the
    # missing export left every GHE gh call unauthenticated (401) even though
    # token delivery itself worked (root-caused live 2026-08-20).
    export GH_ENTERPRISE_TOKEN="$GH_TOKEN"
    # Agents are non-interactive by definition: a gh command that would prompt
    # (e.g. `gh issue create` with no --title after the agent's shell tool
    # mangled a multiline command) hung until the tool timeout and read as a
    # network failure. Fail loud and instant instead.
    export GH_PROMPT_DISABLED=1
  else
    _deny token_cache_unavailable "per-agent scoped GitHub token not available, unreadable, or empty (${HIVE_AGENT_TOKEN_CACHE:-HIVE_AGENT_TOKEN_CACHE unset}); not falling back to the shared App token (audit H3) — report to the operator: on per-UID deployments repair token delivery, on native/shared-UID GitHub Enterprise set GH_HOST (e.g. github.ibm.com) before launch; see src/docs/troubleshooting.md#hive_agent_token_cache-unset-no-read-credential-available."
  fi
  # Write-then-rename so the ingester never sees a half-written event. The
  # subshell pins umask 027: the spool is setgid to the hive's group, so
  # 0640 is exactly "the hive can read it, nobody else"; without the pin an
  # agent umask of 077 would hand the hive an event it cannot open. The group
  # wraps everything so a failed REDIRECTION is silenced too: `> f 2>/dev/null`
  # only mutes the printf, and when the spool is not writable by the agent
  # UID the shell's own "Permission denied" line leaked into stderr on EVERY
  # gh call, priming agents to read later denials as permission errors
  # (#4043).
  {
    emit_token_access_event "gh" "${HIVE_AGENT:-unknown}" "$(id -u)" "$@"
  } 2>/dev/null || true
fi

# NOTE: the contributor-mode `gh auth` gate USED to live here, matching
# `case "$*" in *"auth "*)`. It now lives below the subcommand parser and keys
# on `$subcmd` — see "CONTRIBUTOR MODE — gh auth" after that block (#6659).

# Build the full command string for pattern matching
FULL_CMD="gh $*"

# Check per-agent restrictions
AGENT_ID="${HIVE_AGENT_ID:-}"
if [[ -n "$AGENT_ID" ]]; then
  RESTRICTION_FILE="${RESTRICTIONS_DIR}/${AGENT_ID}.json"
  if [[ -f "$RESTRICTION_FILE" ]]; then
    while IFS='|' read -r pattern reason; do
      [[ -z "$pattern" ]] && continue
      # Use bash extglob for pattern matching
      # shellcheck disable=SC2254
      case "$FULL_CMD" in
        $pattern)
          _deny agent_restriction "${reason:-command not allowed for ${AGENT_ID}} (per-agent restriction on '${pattern}')."
          ;;
      esac
    done < <(python3 -c "
import json, sys
try:
    with open(sys.argv[1]) as f:
        data = json.load(f)
    for r in data.get('rules', []):
        if r.get('enabled', True):
            print(r.get('pattern','') + '|' + r.get('reason',''))
except Exception:
    pass
" "$RESTRICTION_FILE" 2>/dev/null)
  fi
fi

# Global defaults — always enforced for all agents regardless of restriction file
args=("$@")
subcmd=""
action=""

# SECURITY (audit N7/H5, CWE-863): extracting the subcommand and action needs
# REAL flag parsing, not "skip anything starting with -".
#
# The old loop skipped only tokens beginning with '-'. A SEPARATED flag VALUE
# does not begin with '-', so it was consumed as a positional:
#
#   gh pr --repo owner/repo merge 123
#          ^^^^^^ skipped   ^^^^^^^^^^ read as the action
#
# giving action="owner/repo" instead of "merge" — so EVERY gate keyed on
# `action = "merge"` silently skipped: the ADVISORY / ISSUES_ONLY /
# ISSUES_AND_PRS mode arms, the level-based fallback, AND the merge-eligibility
# check. Any value-taking flag placed before the action worked: --repo, -R,
# --title, --body, --label, --head, --base. The audit reproduced this against
# real gh v2.47.0: an agent at ISSUES_PRS_MERGE (or below) could merge unvetted
# code by reordering two flags.
#
# gh_flag_takes_value: does this flag consume the NEXT argv element?
# `--flag=value` is self-contained and never does. Long forms are matched by
# name; short forms are enumerated because gh's short flags are not uniform.
gh_flag_takes_value() {
  case "$1" in
    *=*) return 1 ;;  # --flag=value carries its own value
    # Long flags that take a value. Listed explicitly: a value-taking flag we
    # fail to list would let its value be misread as the action again, so new
    # flags must be added here rather than inferred.
    --repo|--title|--body|--body-file|--label|--head|--base|--assignee|--reviewer| \
    --milestone|--project|--template|--field|--raw-field|--method|--header| \
    --hostname|--jq|--template-file|--search|--state|--author|--mention|--app| \
    --branch|--subject|--match-body|--match-comments|--limit|--sort|--direction| \
    --add-label|--remove-label|--add-assignee|--remove-assignee|--add-reviewer| \
    --remove-reviewer|--add-project|--remove-project|--input|--cache|--comment| \
    --file|--key|--value|--env|--org|--user|--owner|--commit-title|--commit-body| \
    --match|--paginate-limit)
      return 0 ;;
    # Short flags that take a value.
    -R|-t|-b|-B|-F|-f|-H|-l|-a|-A|-m|-p|-q|-c|-s|-L|-e|-d|-i|-k|-o|-u|-w|-x)
      return 0 ;;
    -*) return 1 ;;   # any other flag is boolean
    *)  return 1 ;;
  esac
}

_skip_next_gw=false
for arg in "${args[@]}"; do
  if $_skip_next_gw; then _skip_next_gw=false; continue; fi
  case "$arg" in
    --) # everything after -- is positional-only; stop flag interpretation
        continue ;;
    -*) if gh_flag_takes_value "$arg"; then _skip_next_gw=true; fi
        continue ;;
    *)
      if [ -z "$subcmd" ]; then
        subcmd="$arg"
      elif [ -z "$action" ]; then
        action="$arg"
        break
      fi
      ;;
  esac
done

# ── CONTRIBUTOR MODE — gh auth (kubestellar/hive#6659) ───────────────────────
#
# Contributor agents must not manage the gh credential store: they run on a
# volunteer's own machine with that person's own `gh` login, and re-authing,
# switching accounts or printing the token is none of an assigned task's
# business.
#
# WHAT THIS GATE MUST MATCH IS THE SUBCOMMAND, NOT THE TEXT. It used to run
# ~100 lines above this, before any parsing existed, as a glob over the
# FLATTENED argument string:
#
#   case "$*" in *"auth "*) ... ;; esac
#
# so ANY gh invocation whose arguments merely MENTIONED authentication was
# refused — PR bodies, issue titles, commit messages. The trailing space was
# load-bearing in the worst way: `authfile` passed, `auth file` did not.
#
# The cost was not cosmetic. A codex contributor finished an assigned
# `[sec-check]` task whose whole subject was keeping a registry token out of
# skopeo's argv — the fix replaces `--creds` with an auth FILE — and could not
# open the PR, because the body said "auth file". Security fixes are precisely
# the PRs whose bodies discuss authentication, and `[sec-check]` issues are a
# category the hive assigns routinely, so this systematically killed the last
# step of the tasks it could least afford to lose: work done, tested, committed
# and pushed, slot held, nothing landed.
#
# It also did not hold. The agent, refused on both `gh pr create` and `gh api`,
# reached the REST endpoint with curl and the GH_TOKEN it already holds by
# design. Nothing was escalated — but an over-broad refusal is what taught it to
# leave the sanctioned path, and that is a lesson it carries into refusals that
# mean something.
#
# Keyed on the parsed `$subcmd`, so it is exactly as strict as it was meant to
# be and not one argument wider. Placed immediately after the parser and before
# the surface allowlist, keeping the precedence the old gate had over it: a
# contributor typing `gh auth login` gets this specific message rather than a
# generic "not on the allowlist".
if _contributor_mode && [ "$subcmd" = "auth" ]; then
  _deny contributor_auth "gh auth is disabled for contributor agents (matches the 'auth' SUBCOMMAND only, #6659); your personal token is already configured — do not re-authenticate."
fi

# ── GENERAL COMMAND-SURFACE ALLOWLIST (#3840 F6/F7 residual; added in #3854) ─
#
# WHAT THIS GATE IS (read this before touching `_gh_surface_allowed`):
# `_gh_surface_allowed` below is a deny-by-default ALLOWLIST, not a denylist.
# A subcommand/action pair reaches real `gh` only if it is EXPLICITLY named in
# the `case` arms; anything unenumerated is rejected. Removing an arm therefore
# BLOCKS a verb, and the way to permit a new verb is to ADD an arm — never to
# "add it to a deny list". Do not restructure this into a denylist: that is the
# exact bug #3854 fixed, and audit finding L1 (2026-08-17 security review)
# flagged the stale wording here for misleading a future maintainer into
# weakening the gate.
#
# WHY (historical, pre-#3854): every gate FURTHER BELOW this block is still a
# denylist — each names a specific thing an agent must not do (`pr merge`,
# `issue create`, mutating `gh api`, ...) and the script ends in a bare
# `exec "$REAL_GH" "$@"`. Before this allowlist existed, any subcommand nobody
# thought to enumerate reached real GitHub with the App token attached. That is
# the same failure shape as the mode `case` with no default arm (fixed in
# ce9d19aa): unenumerated input takes the permissive branch.
#
# This was not theoretical. Against the stub harness, on v4 @ c9ea2cc8 (i.e.
# BEFORE the allowlist below landed in #3854), EVERY one of these reached gh
# with rc=0 — including in NO_GITHUB mode, the most restrictive mode there is,
# because the mode gates only ever inspect `subcmd = issue|pr`. All of them are
# rejected today because none is on the allowlist:
#
#   NO_GITHUB     gh auth token                 → reached gh  (exfiltrates the token)
#   NO_GITHUB     gh secret set FOO --body bar  → reached gh  (writes Actions secrets)
#   NO_GITHUB     gh ssh-key add /tmp/k.pub     → reached gh  (persistent account access)
#   NO_GITHUB     gh variable set X --body y    → reached gh
#   ISSUES_ONLY   gh repo delete owner/repo     → reached gh  (destroys the repo)
#   ISSUES_ONLY   gh release create v9.9.9      → reached gh  (publishes artifacts)
#   ISSUES_ONLY   gh gist create /etc/passwd    → reached gh  (exfiltrates file content)
#   ADVISORY      gh workflow run deploy.yml    → reached gh  (arbitrary CI execution)
#
# So we deny by default and enumerate what agents legitimately do. The permitted
# set below was derived from actual usage in this repo — the agent policies
# (src/policies/*.md, examples/kubestellar/agents/**) and bin/*.sh — NOT invented,
# so the allowlist cannot quietly break the fleet. Ordering matters: this runs
# BEFORE the mode/ACMM gates, so it only decides whether a verb is on the map at
# all. Everything it admits is still subject to every gate below — `pr merge`
# passes here and is then held by the merge-eligibility allowlist.
#
# Adding a verb here is a deliberate security decision: it must be something an
# agent genuinely needs, and it must be safe for a prompt-injected agent to run.
_gh_surface_allowed() {
  local s="$1" a="$2"
  case "$s" in
    # Core work surface. Per-action so a new destructive action on an existing
    # subcommand (e.g. a future `gh pr delete`) is denied until reviewed, rather
    # than inherited for free by allowing the whole subcommand.
    issue)
      case "$a" in
        list|view|create|edit|comment|close|reopen|status|develop) return 0 ;;
      esac ;;
    pr)
      case "$a" in
        list|view|create|edit|comment|close|reopen|status|diff|checks| \
        merge|review|ready|checkout) return 0 ;;
      esac ;;
    # Read-only discovery.
    search)
      case "$a" in issues|prs|repos|code|commits) return 0 ;; esac ;;
    run)
      # `watch` is deliberately NOT here: it blocks for the whole CI wait
      # (#13587/#13594). The tailored ci_watch_blocked gate above this
      # allowlist refuses it with a specific message before we get here.
      case "$a" in list|view) return 0 ;; esac ;;
    release)
      case "$a" in list|view) return 0 ;; esac ;;
    cache)
      case "$a" in list) return 0 ;; esac ;;
    label)
      # The wrapper itself calls `gh label create` to ensure agent/hive labels
      # exist before tagging (see _ensure_labels below).
      case "$a" in list|create) return 0 ;; esac ;;
    repo)
      # fork/clone/view are the contributor flow (contribute_ws.go:3207 issues
      # `gh repo fork <r> --clone=true`). `delete`, `archive`, `rename`,
      # `edit` and `deploy-key` are deliberately NOT here.
      case "$a" in view|list|fork|clone) return 0 ;; esac ;;
    # `gh api` has its own read/write split gate further down, which is finer
    # grained than anything expressible here (it distinguishes GET from an
    # implicit POST). Admit the subcommand and let that gate do the work.
    api) return 0 ;;
    # No-network local helpers.
    help|version|--version|--help|status) return 0 ;;
  esac
  return 1
}

# ── Blocking CI waits are refused (#13587, #13594, #13596) ───────────────────
#
# `gh run watch` and `gh pr checks --watch` block for the whole CI wait and
# every refresh is an API call; the stop-polling nudge (ci_poll_nudge) only
# advised, so agents kept looping. Refuse both outright. ORDERING: this gate
# runs BEFORE the surface allowlist on purpose — `watch` is not in the `run`
# arm below, so without this gate `gh run watch` would fall through to the
# generic surface_allowlist denial and lose the tailored reason code/message.
# This does NOT turn the allowlist into a denylist: the allowlist still decides
# what is on the map; this only gives one refused verb a better explanation.
_gh_is_blocking_ci_wait() {
  if [ "$subcmd" = "run" ] && [ "$action" = "watch" ]; then
    return 0
  fi
  if [ "$subcmd" = "pr" ] && [ "$action" = "checks" ]; then
    local _a
    for _a in "${args[@]}"; do
      case "$_a" in --watch|--watch=*|-w) return 0 ;; esac
    done
  fi
  return 1
}
if _gh_is_blocking_ci_wait; then
  _ci_wait_cmd="gh ${subcmd} ${action}"
  [ "$action" = "checks" ] && _ci_wait_cmd="${_ci_wait_cmd} --watch"
  _deny ci_watch_blocked "'${_ci_wait_cmd}' is not allowed: stop polling CI. Do not run gh run watch/view, gh pr checks, or gh pr view --json statusCheckRollup: leave the PR as is (awaiting-ci) and move to the next work-list item. The automerge sweep handles green PRs."
fi

# `gh` with no arguments prints help — harmless, and denying it produces a
# confusing error for an agent that is just probing.
if [ -n "$subcmd" ] && ! _gh_surface_allowed "$subcmd" "$action"; then
  _deny surface_allowlist "'gh ${subcmd}${action:+ $action}' is not on the hive's allowlist of permitted gh commands (deny-by-default, #3840); permitted: issue/pr, search, run list|view, release list|view, label list|create, repo view|list|fork|clone, api — if genuinely needed, ask the operator to add it to _gh_surface_allowed in bin/gh-wrapper.sh."
fi

# ── Write-surface enforcement in the sandbox (hivecommons/hive#9587, #9772) ──
#
# A lane the operator lists under write_surface.enforce has the audited relays
# as its ONLY write path: the GitHub proxy refuses its direct REST writes,
# GraphQL mutations and git pushes. That refusal covers traffic that goes
# THROUGH the proxy — a sandbox with forced egress disabled never reaches it.
# This gate is the same decision applied one layer earlier, in the sandbox,
# where a direct `gh` write is refused before it is sent at all.
#
# It is a second lock on the same door, never a new policy: the enforced lanes
# are published by the hive from write_surface.enforce (pkg/github's
# PublishWriteSurfaceEnforce), and a hive that lists no lane — the default —
# publishes an empty list, so nothing here changes what any agent can do.
#
# The path is a CONSTANT, like CONTRIBUTOR_MODE_MARKER and for the same reason
# (#3249): an env-selected path would let an enforced agent point the check at
# a file it writes. The file lives in the hive-owned metrics directory, not in
# world-writable /tmp where the mode file sits, so a peer agent cannot plant a
# list that un-enforces a lane.
WRITE_SURFACE_ENFORCE_FILE="/var/run/hive-metrics/write-surface-enforce.json"

# _write_surface_enforced reports whether THIS lane is enforced. Parsed with
# bash builtins only: every external command here is reachable through the
# agent's own PATH, so a check that shelled out to python3/grep could be
# answered by a binary the enforced party controls.
#
# An absent, unreadable, unparseable or unknown-version file answers NO. That
# is the documented default (enforcement is opt-in per lane) and it keeps a
# hive that cannot publish the file working exactly as before; the proxy
# refusal is the enforcement this feature rests on, and it is unaffected.
_write_surface_enforced() {
  if [ -n "${_WS_ENFORCED:-}" ]; then
    if [ "$_WS_ENFORCED" = "yes" ]; then
      return 0
    fi
    return 1
  fi
  _WS_ENFORCED="no"
  local lane content lanes entry
  lane="${HIVE_AGENT:-${HIVE_AGENT_ID:-}}"
  lane="${lane,,}"
  [ -n "$lane" ] || return 1
  [ -f "$WRITE_SURFACE_ENFORCE_FILE" ] && [ -r "$WRITE_SURFACE_ENFORCE_FILE" ] || return 1
  content="$(<"$WRITE_SURFACE_ENFORCE_FILE")" || return 1
  case "$content" in
    *'"version":1,'*|*'"version": 1,'*|*'"version":1}'*|*'"version": 1}'*) : ;;
    *) return 1 ;;
  esac
  case "$content" in
    *'"lanes":'*) : ;;
    *) return 1 ;;
  esac
  lanes="${content#*\"lanes\":}"
  case "$lanes" in
    *']'*) lanes="${lanes%%]*}" ;;
    *) return 1 ;;
  esac
  lanes="${lanes#*[}"
  lanes="${lanes//\"/}"
  lanes="${lanes// /}"
  # Split on commas with read -a, never an unquoted expansion: a "*" entry
  # would otherwise glob against the caller's cwd instead of staying literal.
  local -a lane_entries=()
  IFS=, read -r -a lane_entries <<<"$lanes"
  for entry in "${lane_entries[@]}"; do
    entry="${entry,,}"
    if [ "$entry" = "*" ] || [ "$entry" = "$lane" ]; then
      _WS_ENFORCED="yes"
      return 0
    fi
  done
  return 1
}

# _write_surface_refuse_direct refuses a DIRECT GitHub write for an enforced
# lane and names the relay that performs the same write under audit. It is
# called at each point where the wrapper would otherwise hand the command to
# the real gh; a relayed write (hive-open-issue, hive-review, …) never reaches
# it, because the relay runs in the hive with the App token, not here.
#
# $1: what is being refused, $2: the relay to use instead.
_write_surface_refuse_direct() {
  local what="$1" instead="$2"
  _write_surface_enforced || return 0
  _deny write_surface_enforce "${HIVE_AGENT:-${HIVE_AGENT_ID:-this agent}} is under write_surface.enforce — direct GitHub write '${what}' refused (reads still work); use instead: ${instead}. Do NOT retry or route around via another CLI, curl or the GitHub MCP — the proxy refuses and records the same write."
}

# The relays, for a refusal that is not tied to one operation.
WRITE_SURFACE_RELAYS="hive-open-pr (open a PR), hive-open-issue (issue, comment, claim, label, close, request-review), hive-review, hive-merge, hive-push-branch (push a branch)"

# ── Helpers: author validation for the list gate ──

# Extract the effective --author/-A value from the args array. GitHub CLI uses
# the last repeated value, so this deliberately scans the whole argv instead of
# returning the first match. Returns the value on stdout on success (exit 0) or
# nothing on failure (exit 1).
_extract_author() {
  local i author_value="" found=false
  for ((i=0; i<${#args[@]}; i++)); do
    if [[ "${args[$i]}" = --author=* || "${args[$i]}" = -A=* ]]; then
      author_value="${args[$i]#*=}"
      found=true
      continue
    fi
    if [[ "${args[$i]}" = -A?* ]]; then
      author_value="${args[$i]#-A}"
      found=true
      continue
    fi
    if [[ "${args[$i]}" = --author || "${args[$i]}" = -A ]]; then
      if [[ $((i+1)) -lt ${#args[@]} ]] && [[ "${args[$((i+1))]}" != -* ]]; then
        author_value="${args[$((i+1))]}"
        found=true
      fi
    fi
  done
  if $found; then
    printf '%s\n' "$author_value"
    return 0
  fi
  return 1
}

# Resolve the authenticated GitHub login. Initialize the cache internally so a
# caller-controlled environment cannot seed a trusted identity (#3982).
#
# Two oracles, both unspoofable by the agent (#4044):
#   1. The trusted bot-identity file — written by the hive process (which mints
#      the tier tokens) into a directory no agent UID can write. This is the
#      ONLY oracle that can work for staff agents: their App installation
#      tokens have no /user identity, so `gh api user` 403s unconditionally.
#   2. `gh api user` — the server-side identity of the token itself. Works for
#      user tokens (contributor mode) and remains the fallback whenever the
#      file is absent, so the contributor path is unchanged.
# A caller-writable path or environment variable must never be consulted:
# fail-closed is preserved when NEITHER oracle resolves.
HIVE_AUTH_LOGIN_CACHED=""
_resolve_self_login() {
  if [[ -n "$HIVE_AUTH_LOGIN_CACHED" ]]; then
    printf '%s\n' "$HIVE_AUTH_LOGIN_CACHED"
    return 0
  fi

  local login=""
  if ! _contributor_mode && [[ -f "$BOT_LOGIN_FILE" && -r "$BOT_LOGIN_FILE" ]]; then
    # Single-line file; strip whitespace/newline. An empty or unreadable file
    # falls through to the API oracle (and from there to fail-closed).
    login="$(tr -d '[:space:]' < "$BOT_LOGIN_FILE" 2>/dev/null || true)"
  fi
  if [[ -z "$login" ]]; then
    if ! login="$("$REAL_GH" api user --jq '.login' 2>/dev/null)"; then
      return 1
    fi
  fi
  if [[ -z "$login" ]]; then
    return 1
  fi

  HIVE_AUTH_LOGIN_CACHED="$login"
  printf '%s\n' "$HIVE_AUTH_LOGIN_CACHED"
}

_lower() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

_author_matches_login() {
  local requested_lc login_lc requested_base login_base
  requested_lc="$(_lower "$1")"
  login_lc="$(_lower "$2")"
  requested_base="${requested_lc%\[bot\]}"
  login_base="${login_lc%\[bot\]}"

  [[ "$requested_lc" = "$login_lc" || "$requested_base" = "$login_base" ]]
}

# ── READ/WRITE SPLIT for GitHub lookups (fixes #2356; addresses #2393 item 6) ──
# Contributor agents MUST be able to do READ-ONLY lookups before starting work so
# they can check whether a PR already exists for their assigned issue — otherwise
# they send DUPLICATE PRs (#2356). We therefore ALLOW read-only enumeration
# (`gh pr list`, `gh issue list`, `gh search prs|issues`, `gh pr/issue view`,
# `gh pr diff`) and GET-only `gh api repos/*/{issues,pulls}` reads, while keeping
# EVERY write/destructive path blocked (create/merge/close/edit/delete, `gh auth`,
# and any mutating `gh api` -X/--method POST|PATCH|PUT|DELETE — enforced below).
#
# Block gh issue list and gh pr list for NON-contributor hive agents (they consume
# assigned work from actionable.json). Contributors are exempt so they can look
# before they leap. `--author` self-listing is allowed only when the author value
# matches the authenticated token identity (fixes #3072 and #3096).
# `search prs|issues` is covered alongside `list` (#6908). It was not, and the
# gap was invisible at the call site: this check gated on action=list, while
# `gh search prs` is subcmd=search/action=prs and matched only the read-only
# discovery allowlist above. So moving a caller from `gh pr list --author @me`
# to `gh search prs --author @me` silently moved it off the identity check
# #3072/#3096 added — which is exactly what the contributor relay's PR review
# cycle did, and how it came to enumerate PRs belonging to the token's human
# owner in repositories no hub manages.
#
# Only the --author VALIDATION is extended. An authorless `gh search prs` is
# still read-only discovery and still allowed for every agent; see the
# search-specific branch below.
_gh_author_scoped=false
if { [ "$subcmd" = "issue" ] || [ "$subcmd" = "pr" ]; } && [ "$action" = "list" ]; then
  _gh_author_scoped=true
elif [ "$subcmd" = "search" ] && { [ "$action" = "prs" ] || [ "$action" = "issues" ]; }; then
  _gh_author_scoped=true
fi
if [ "$_gh_author_scoped" = true ]; then
  if author_value="$(_extract_author)" && [[ -n "$author_value" ]]; then
    if [[ "$author_value" = "@me" ]]; then
      # GitHub resolves @me server-side ONLY for user tokens. An App
      # installation token has no user identity, so for staff agents @me is
      # rejected by the API itself and there would be NO working self-listing
      # form at all (#4044). Map @me to the trusted resolved identity for
      # staff agents; contributor mode keeps the server-side resolution.
      if ! _contributor_mode; then
        if ! _resolve_self_login >/dev/null; then
          _deny author_identity_unresolved "gh $subcmd $action --author @me cannot work with an App installation token (no /user identity, #4044) and no trusted identity is available to substitute (${BOT_LOGIN_FILE} missing/empty, 'gh api user' did not resolve); report to the operator so identity delivery is repaired."
        fi
        _atme_rewritten=()
        _atme_expect=false
        for _atme_arg in "${args[@]}"; do
          if [[ "$_atme_expect" = true ]]; then
            _atme_expect=false
            [[ "$_atme_arg" = "@me" ]] && _atme_arg="$HIVE_AUTH_LOGIN_CACHED"
          else
            case "$_atme_arg" in
              --author|-A) _atme_expect=true ;;
              --author=@me) _atme_arg="--author=${HIVE_AUTH_LOGIN_CACHED}" ;;
              -A=@me) _atme_arg="-A=${HIVE_AUTH_LOGIN_CACHED}" ;;
              -A@me) _atme_arg="-A${HIVE_AUTH_LOGIN_CACHED}" ;;
            esac
          fi
          _atme_rewritten+=("$_atme_arg")
        done
        args=("${_atme_rewritten[@]}")
        # The wrapper's tail executes `exec "$REAL_GH" "$@"` — rewrite the
        # positional parameters too, or the substitution would never ship.
        set -- "${args[@]}"
      fi
    elif ! _resolve_self_login >/dev/null; then
      _deny author_identity_unresolved "gh $subcmd $action --author requires authenticated GitHub identity and none could be resolved (no trusted identity file at ${BOT_LOGIN_FILE}, 'gh api user' did not resolve); report to the operator so identity delivery is repaired."
    elif _author_matches_login "$author_value" "$HIVE_AUTH_LOGIN_CACHED"; then
      : # Match: authenticated login, case-insensitive, with optional [bot] suffix.
    else
      _deny author_mismatch "gh $subcmd $action --author must match the authenticated GitHub identity ('$author_value' is not token identity '$HIVE_AUTH_LOGIN_CACHED'); use --author @me or your authenticated login to list your own items."
    fi
  elif _contributor_mode; then
    : # Allow contributor agents read-only list/search to avoid duplicate PRs (#2356).
  elif [ "$subcmd" = "search" ]; then
    # An authorless search names no identity to validate, and blocking it here
    # would be a policy change riding in on an identity fix (#6908). The
    # enumeration ban for staff agents is stated in their prompt, not enforced
    # by this branch, and widening the --author check must not quietly move it.
    :
  else
    # Root-caused in a live hive (2026-08-20): agents read this two-line
    # message as "all gh $subcmd commands are blocked" and silently skipped
    # `gh issue create` / PR creation for confirmed findings, and the single
    # hardcoded path pointed at a file that (a) does not exist on the
    # container-hosted model (which writes /data/last-actionable.json) and
    # (b) sits outside the agent CLI's workspace sandbox for file-read tools.
    # Be explicit: only LIST/enumeration is blocked, writes remain allowed,
    # and point at whichever work-queue snapshot actually exists here.
    _actionable_hint="/var/run/hive-metrics/actionable.json"
    [ -f /data/last-actionable.json ] && _actionable_hint="/data/last-actionable.json"
    _deny enumeration_disabled "gh $subcmd list is disabled for agents — ONLY listing/enumeration is blocked, writes like 'gh issue create' and 'hive-open-pr' are still ALLOWED; for the pre-filtered queue run 'cat ${_actionable_hint}' (a shell command, not a workspace file-read tool)."
  fi
fi

_clanker_pr_create_autoclose() {
  _contributor_mode || return 0
  [[ "$subcmd" = "pr" && "$action" = "create" ]] || return 0
  command -v python3 >/dev/null 2>&1 || return 0

  local task_file="${HIVE_TASK_FILE:-/tmp/contributor-task.json}"
  [[ -r "$task_file" ]] || return 0

  local repo="" base="" body="" body_file="" have_body=false next=""
  local i arg
  for ((i=0; i<${#args[@]}; i++)); do
    arg="${args[$i]}"
    if [[ -n "$next" ]]; then
      case "$next" in
        repo) repo="$arg" ;;
        base) base="$arg" ;;
        body) body="$arg"; have_body=true ;;
        body_file) body_file="$arg" ;;
      esac
      next=""
      continue
    fi
    case "$arg" in
      --repo|-R) next="repo" ;;
      --base|-B) next="base" ;;
      --body|-b) next="body" ;;
      --body-file|-F) next="body_file" ;;
      --repo=*) repo="${arg#--repo=}" ;;
      --base=*) base="${arg#--base=}" ;;
      --body=*) body="${arg#--body=}"; have_body=true ;;
      --body-file=*) body_file="${arg#--body-file=}" ;;
    esac
  done
  [[ -n "$repo" ]] || return 0
  if [[ -n "$body_file" ]]; then
    $have_body && return 0
    if [[ "$body_file" = "-" ]]; then
      body="$(cat)"
    elif [[ -r "$body_file" ]]; then
      body="$(cat -- "$body_file")"
    else
      return 0
    fi
    have_body=true
  fi
  local default_target=true
  if [[ -n "$base" ]]; then
    local default_branch
    default_branch="$("$REAL_GH" repo view "$repo" --json defaultBranchRef --jq '.defaultBranchRef.name' 2>/dev/null || true)"
    [[ -n "$default_branch" && "$base" = "$default_branch" ]] || default_target=false
  fi

  local new_body="$body"
  if $default_target; then
    new_body="$(python3 - "$task_file" "$repo" "$body" <<'PY'
import json
import re
import sys

task_file, repo, body = sys.argv[1:4]
try:
    with open(task_file, encoding="utf-8") as f:
        task = json.load(f)
except Exception:
    print(body, end="")
    raise SystemExit
if task.get("kind") != "issue" or str(task.get("repo", "")).strip().lower() != repo.strip().lower():
    print(body, end="")
    raise SystemExit
try:
    issue = int(task.get("number") or 0)
except Exception:
    issue = 0
if issue <= 0:
    print(body, end="")
    raise SystemExit
closing = re.compile(
    r"(?im)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b[^\n#]*(?:[\w.-]+/[\w.-]+)?#"
    + re.escape(str(issue))
    + r"\b"
)
if closing.search(body):
    print(body, end="")
    raise SystemExit
# A body that says this PR leaves the issue open must not gain a closing line:
# GitHub would close the issue on merge with the rest of the work undone
# (projectbluefin/testsuite#1057: "Refs #1023 — partial … remain open", then
# the appended "Closes #1023"). A bare "Refs #N" still gets one (#10826).
num = re.escape(str(issue))
stays_open = re.compile(
    r"(?im)(?:#" + num + r"\b[^\n]*\b(?:remains?|stays?)\s+open"
    + r"|\b(?:leaves?|leaving|leave)\s+#" + num + r"\s+open"
    + r"|\b(?:does\s*n[o']t|do\s+not|not)\s+close\s+#" + num + r"\b"
    + r"|^[^\n]*\b(?:refs?|references?|part\s+of)\b[^\n]*#" + num
    + r"\b[^\n]*\b(?:partial(?:ly)?|remains?|remaining|left\s+open|deferred|follow-?up|not\s+(?:yet\s+)?addressed))"
)
if stays_open.search(body):
    print(body, end="")
    raise SystemExit
trimmed = body.rstrip(" \t\r\n")
line = f"Closes #{issue}"
print((trimmed + "\n\n" + line) if trimmed else line, end="")
PY
)"
  fi
  # A body read from a file or stdin is always handed on as --body, changed or
  # not: stdin is spent now, and _inject_identity would read it again, find
  # nothing and open the PR with the footer alone (projectbluefin/testsuite
  # #1036, #1054, #1056).
  [[ "$new_body" != "$body" || -n "$body_file" ]] || return 0

  local -a new_args=()
  next=""
  for ((i=0; i<${#args[@]}; i++)); do
    arg="${args[$i]}"
    if [[ -n "$next" ]]; then
      case "$next" in
        body|body_file) ;;
        *) new_args+=("$arg") ;;
      esac
      next=""
      continue
    fi
    case "$arg" in
      --body|-b) next="body" ;;
      --body-file|-F) next="body_file" ;;
      --body=*|--body-file=*) ;;
      *) new_args+=("$arg") ;;
    esac
  done
  new_args+=("--body" "$new_body")
  args=("${new_args[@]}")
}

# ── Mode-based enforcement (hot-reloadable via mode file) ──
# Read mode from file first (updated by Manager on mode change), fallback to env var.
# -r as well as -f: this script runs under `set -e`, so a mode file that exists
# but is unreadable by the agent UID (owner-only perms, #3679) would kill the
# wrapper before any mode gate or repo restriction ran, failing every gh call
# with exit 1 and no output. Fall back to the env var instead — the same mode
# value the Manager exported.
AGENT_NAME_GW="${HIVE_AGENT:-${HIVE_AGENT_ID:-unknown}}"
MODE_FILE="/tmp/.hive-mode-${AGENT_NAME_GW}"
if [ -f "$MODE_FILE" ] && [ -r "$MODE_FILE" ]; then
  AGENT_MODE="$(cat "$MODE_FILE" 2>/dev/null || true)"
else
  AGENT_MODE="${HIVE_AGENT_MODE:-}"
fi
ACMM_LEVEL="${HIVE_ACMM_LEVEL:-0}"
ADVISORY_ISSUE="${HIVE_ADVISORY_ISSUE:-}"

# ── Route `gh pr create` through the hive so the PR is App-bot-authored ──
# An agent running `gh pr create` would author the PR as whatever identity the gh
# token / Copilot login resolves to (the login USER, not the App bot). Redirect
# to hive-open-pr, which drops a request file the hive's watcher opens with the
# App installation token → authored by "<slug>[bot]". The watcher enforces the
# SAME ACMM write-gate + forge-resistance, so this changes WHO opens the PR, not
# WHAT an agent is allowed to do. Contributors are EXEMPT: they fork and PR under
# their OWN identity by design, so their gh pr create must pass through unchanged.
_clanker_pr_create_autoclose

if [ "$subcmd" = "pr" ] && [ "$action" = "create" ] && ! _contributor_mode; then
  if command -v hive-open-pr >/dev/null 2>&1; then
    # Pass the original gh-pr-create flags straight through — hive-open-pr accepts
    # the same --repo/--head/--base/--title/--body shape and ignores the rest.
    exec hive-open-pr "$@"
  fi
  # If the wrapper somehow isn't installed, fail loud rather than silently
  # opening the PR as the wrong identity (hard switch: no gh-pr-create fallback).
  _deny relay_missing "hive-open-pr not found — cannot open a PR as the App bot; do NOT fall back to gh pr create (would author as the login user), report this to the operator."
fi

# Helper: capture advisory finding to JSONL for governor digest
_capture_advisory_finding() {
  local _adv_title="" _adv_body="" _next_is_title=false _next_is_body=false
  for arg in "${args[@]}"; do
    if $_next_is_title; then _adv_title="$arg"; _next_is_title=false; continue; fi
    if $_next_is_body;  then _adv_body="$arg";  _next_is_body=false;  continue; fi
    case "$arg" in
      --title)   _next_is_title=true ;;
      --title=*) _adv_title="${arg#--title=}" ;;
      --body)    _next_is_body=true ;;
      --body=*)  _adv_body="${arg#--body=}" ;;
      -t)        _next_is_title=true ;;
      -b)        _next_is_body=true ;;
    esac
  done
  local ADVISORY_DIR="/data/advisory"
  mkdir -p "$ADVISORY_DIR"
  python3 -c "
import json, datetime, sys, os
agent = sys.argv[1]
adv_dir = sys.argv[2]
f = {
    'agent': agent,
    'timestamp': datetime.datetime.utcnow().isoformat() + 'Z',
    'type': 'issue',
    'severity': 'medium',
    'title': sys.argv[3],
    'detail': sys.argv[4][:500] if len(sys.argv[4]) > 500 else sys.argv[4]
}
path = os.path.join(adv_dir, agent + '.jsonl')
with open(path, 'a') as fh:
    fh.write(json.dumps(f) + '\n')
" "$AGENT_NAME_GW" "$ADVISORY_DIR" "${_adv_title:-untitled}" "${_adv_body:-}" 2>/dev/null || true
}

if [ -n "$AGENT_MODE" ]; then
  # Mode-based enforcement (preferred path)
  case "$AGENT_MODE" in
    NO_GITHUB)
      if [ "$subcmd" = "issue" ] || [ "$subcmd" = "pr" ]; then
        _deny mode_no_github "${AGENT_NAME_GW} is in NO_GITHUB mode — no GitHub interaction allowed; finish with local work."
      fi
      ;;
    ADVISORY)
      if [ "$subcmd" = "issue" ] && [ "$action" = "create" ]; then
        _capture_advisory_finding
        _deny mode_advisory "${AGENT_NAME_GW} is in ADVISORY mode — finding saved to the digest${ADVISORY_ISSUE:+ (advisory issue #${ADVISORY_ISSUE}, next governor cycle)}; do not retry the create."
      fi
      if [ "$subcmd" = "pr" ] && { [ "$action" = "create" ] || [ "$action" = "merge" ]; }; then
        _deny mode_advisory "${AGENT_NAME_GW} is in ADVISORY mode — no PRs allowed; record the finding instead."
      fi
      ;;
    ISSUES_ONLY)
      if [ "$subcmd" = "pr" ] && { [ "$action" = "create" ] || [ "$action" = "merge" ]; }; then
        _deny mode_issues_only "${AGENT_NAME_GW} is in ISSUES_ONLY mode — no PRs allowed; file an issue instead."
      fi
      ;;
    ISSUES_AND_PRS)
      if [ "$subcmd" = "pr" ] && [ "$action" = "merge" ]; then
        _deny mode_issues_and_prs "${AGENT_NAME_GW} is in ISSUES_AND_PRS mode — merging requires human approval; leave the PR for a maintainer."
      fi
      # NOTE (F6): This hold-label block is DEAD for `pr create`. A non-contributor
      # `gh pr create` is redirected far above via `exec hive-open-pr "$@"` (~line
      # 160), which REPLACES this process — execution never reaches here for the
      # create path. The hold label is now applied AUTHORITATIVELY server-side, in
      # src/pkg/github/pr_request_watcher.go, after the hive's App-bot opens the PR,
      # keyed on the real hive ACMM level (L3/L4/L5). Do NOT rely on this line to
      # gate anything; it is retained only so `args` stays well-formed for any
      # non-create pr subcommand that still falls through.
      if [ "$ACMM_LEVEL" = "5" ] && [ "$subcmd" = "pr" ] && [ "$action" = "create" ]; then
        args+=("--label" "hold")
      fi
      ;;
    ISSUES_PRS_MERGE)
      # Everything allowed — merge-eligible gate still checked below
      ;;
    *)
      # SECURITY (N15 chain A, CWE-20/732): fail CLOSED on an unrecognized mode.
      #
      # Without this arm an unknown non-empty mode matched nothing and fell
      # through with ZERO enforcement — every gate above silently skipped. That
      # is reachable: AGENT_MODE is read from /tmp/.hive-mode-<name> (see the
      # block near :155), a world-writable path whose name comes from
      # agent-controlled env (HIVE_AGENT / HIVE_AGENT_ID). Because the file wins
      # over the env var, anyone who can write /tmp — any agent UID in the pod,
      # or a managed-repo PR that lands a file there — could plant a garbage
      # value and permanently un-gate a victim agent, durable across restarts
      # and `hive agent reset`.
      #
      # Note an EMPTY mode is different and still safe: it leaves AGENT_MODE=""
      # so the `if [ -n "$AGENT_MODE" ]` above is false and the level-based
      # fallback below applies. Only a non-empty unknown value lands here.
      _deny mode_unrecognized "unrecognized agent mode '${AGENT_MODE}' for ${AGENT_NAME_GW} — refusing to run gh (valid: NO_GITHUB, ADVISORY, ISSUES_ONLY, ISSUES_AND_PRS, ISSUES_PRS_MERGE); if ${MODE_FILE} has unexpected contents it may have been tampered with — report to the operator."
      ;;
  esac
else
  # Fallback: level-based enforcement (for containers without HIVE_AGENT_MODE)
  if [ "$subcmd" = "issue" ] && [ "$action" = "create" ]; then
    if [[ "${AGENT_ID:-}" == "supervisor" || "${HIVE_AGENT:-}" == "supervisor" ]]; then
      _deny supervisor_no_issues "supervisor cannot create issues; delegate the finding to a worker lane."
    fi
  fi
  if [ "$ACMM_LEVEL" -gt 0 ] && [ "$ACMM_LEVEL" -lt 3 ]; then
    if [ "$subcmd" = "issue" ] && [ "$action" = "create" ]; then
      _capture_advisory_finding
      _deny acmm_level "gh issue create is not allowed at ACMM L${ACMM_LEVEL} — finding saved to the digest; do not retry."
    fi
    if [ "$subcmd" = "pr" ] && { [ "$action" = "create" ] || [ "$action" = "merge" ]; }; then
      _deny acmm_level "gh pr ${action} is not allowed at ACMM L${ACMM_LEVEL}; record the finding instead."
    fi
  fi
  if [ "$ACMM_LEVEL" -eq 3 ]; then
    if [ "$subcmd" = "pr" ] && [ "$action" = "merge" ]; then
      _deny acmm_level "gh pr merge is not allowed at ACMM L3; leave the PR for a maintainer."
    fi
    if [ "$subcmd" = "issue" ] && [ "$action" = "create" ] && [ "$AGENT_NAME_GW" != "quality" ]; then
      _capture_advisory_finding
      _deny acmm_level "only quality can create issues at ACMM L3 — finding saved to the digest; do not retry."
    fi
    if [ "$subcmd" = "pr" ] && [ "$action" = "create" ] && [ "$AGENT_NAME_GW" != "quality" ]; then
      _deny acmm_level "only quality can create PRs at ACMM L3; record the finding instead."
    fi
  fi
fi

# Enforce merge gate — only PRs in merge-eligible.json can be merged
MERGE_ELIGIBLE_FILE="/var/run/hive-metrics/merge-eligible.json"
if [ "$subcmd" = "pr" ] && [ "$action" = "merge" ]; then
  # SECURITY (audit N7): parse the PR identifier with the SAME flag table the
  # subcommand extractor uses, so a value can never be mistaken for the PR
  # number (or vice versa). The previous loop special-cased only --repo, so
  # `-R owner/repo` left "owner/repo" to be read as pr_num — which then failed
  # the eligibility lookup and DENIED a legitimate merge (fail-closed, but a
  # real availability bug), while other value-taking flags could feed it junk.
  #
  # gh accepts the PR as a number, a URL, or a branch name; all three are
  # positionals after the `merge` action.
  pr_num=""
  pr_repo=""
  skip_next=false
  seen_pr=false
  seen_merge=false
  for arg in "${args[@]}"; do
    if $skip_next; then skip_next=false; continue; fi
    case "$arg" in
      --repo=*|-R=*) pr_repo="${arg#*=}"; continue ;;
      --repo|-R)     skip_next=true; continue ;;
      -*)
        if gh_flag_takes_value "$arg"; then skip_next=true; fi
        continue ;;
      *)
        # Consume the subcommand and action positionals first, then take the
        # next positional as the identifier.
        if ! $seen_pr; then seen_pr=true; continue; fi
        if ! $seen_merge; then seen_merge=true; continue; fi
        [ -z "$pr_num" ] && pr_num="$arg"
        ;;
    esac
  done

  # Recover a separated --repo/-R value (the loop above skips it by design).
  if [ -z "$pr_repo" ]; then
    for i in "${!args[@]}"; do
      case "${args[$i]}" in
        --repo|-R)
          if [ -n "${args[$((i+1))]:-}" ]; then pr_repo="${args[$((i+1))]}"; fi
          break ;;
      esac
    done
  fi

  # Normalize a PR URL to its number so the eligibility lookup can match it.
  # `gh pr merge https://github.com/o/r/pull/123` is a legitimate form that the
  # old parser passed through verbatim and therefore always denied.
  case "$pr_num" in
    http://*|https://*)
      url_repo="$(printf '%s' "$pr_num" | sed -n 's#^https\{0,1\}://[^/]*/\([^/]*/[^/]*\)/pull/.*#\1#p')"
      url_num="$(printf '%s' "$pr_num" | sed -n 's#.*/pull/\([0-9][0-9]*\).*#\1#p')"
      if [ -n "$url_num" ]; then
        pr_num="$url_num"
        [ -z "$pr_repo" ] && pr_repo="$url_repo"
      fi
      ;;
  esac

  # SECURITY (audit N7): fail CLOSED when no PR identifier is present.
  #
  # `gh pr merge` (merge whatever PR belongs to the current branch) and
  # `gh pr merge --repo owner/repo` carry no number, so pr_num stayed empty —
  # and BOTH eligibility branches below were guarded on `[ -n "$pr_num" ]` with
  # no else. Execution fell straight through to `exec "$REAL_GH"`, so the most
  # permissive mode had no wall at all: an agent could merge the current
  # branch's PR without it ever appearing in merge-eligible.json.
  #
  # The wrapper cannot resolve the current branch's PR without calling gh (which
  # is the thing being gated), so the only safe answer is to require an explicit,
  # checkable identifier.
  if [ -z "$pr_num" ]; then
    _deny merge_no_identifier "'gh pr merge' needs an explicit PR number or URL (the merge gate verifies against ${MERGE_ELIGIBLE_FILE}, impossible for an implicit current-branch merge); re-run as: gh pr merge <number> [--repo owner/repo]."
  fi

  if [ -n "$pr_num" ] && [ -f "$MERGE_ELIGIBLE_FILE" ]; then
    is_eligible=$(python3 -c "
import json, sys
try:
    pr_num_arg = sys.argv[1]
    repo_filter = sys.argv[2]
    merge_file = sys.argv[3]
    with open(merge_file) as f:
        data = json.load(f)
    for pr in data.get('merge_eligible', []):
        if str(pr.get('number')) == pr_num_arg:
            if not repo_filter or pr.get('repo','') == repo_filter:
                print('yes')
                sys.exit(0)
    print('no')
except Exception as e:
    print('error:' + str(e), file=sys.stderr)
    print('no')
" "$pr_num" "$pr_repo" "$MERGE_ELIGIBLE_FILE" 2>/dev/null)

    if [ "$is_eligible" != "yes" ]; then
      _deny merge_not_eligible "PR #${pr_num} is NOT in merge-eligible.json (all CI checks must pass first); run 'cat ${MERGE_ELIGIBLE_FILE}' to see eligible PRs."
    fi
  elif [ -n "$pr_num" ] && [ ! -f "$MERGE_ELIGIBLE_FILE" ]; then
    _deny merge_gate_unavailable "${MERGE_ELIGIBLE_FILE} not found — cannot verify merge eligibility; run merge-gate.sh first or wait for the next pipeline cycle."
  fi
fi

# ── gh api read/write split (fixes #2356; addresses #2393 item 6) ──
# GET requests to repos/*/{issues,pulls} are READ-ONLY lookups an agent needs to
# check for an existing PR/issue before starting → ALLOWED. Any MUTATING method
# (POST/PATCH/PUT/DELETE, via -X/--method, or an implicit POST when -f/--field/
# --input is present with no explicit GET) stays BLOCKED. `gh api` defaults to GET
# only when no fields are supplied; supplying fields makes it a POST, so we treat
# "fields present without an explicit GET" as a write and deny it.
if [ "$subcmd" = "api" ]; then
  api_method="GET"        # gh api default
  api_method_explicit=false
  api_has_fields=false
  _next_is_method=false
  for arg in "${args[@]}"; do
    if $_next_is_method; then
      api_method="$(printf '%s' "$arg" | tr '[:lower:]' '[:upper:]')"
      api_method_explicit=true
      _next_is_method=false
      continue
    fi
    case "$arg" in
      -X|--method)          _next_is_method=true ;;
      -X*)                  api_method="$(printf '%s' "${arg#-X}" | tr '[:lower:]' '[:upper:]')"; api_method_explicit=true ;;
      --method=*)           api_method="$(printf '%s' "${arg#--method=}" | tr '[:lower:]' '[:upper:]')"; api_method_explicit=true ;;
      -f|-F|--field|--raw-field|--input) api_has_fields=true ;;
      -f*|-F*)              api_has_fields=true ;;
      --field=*|--raw-field=*|--input=*) api_has_fields=true ;;
    esac
  done
  # Fields without an explicit method mean gh sends a POST → treat as a write.
  if ! $api_method_explicit && $api_has_fields; then
    api_method="POST"
  fi

  # Any mutating gh api (POST/PATCH/PUT/DELETE) stays blocked for contributor
  # agents — the read/write split opens READS only, never writes. Non-contributor
  # hive agents keep their existing write paths (governed by mode/ACMM gates above).
  if _contributor_mode && [ "$api_method" != "GET" ]; then
    _deny contributor_api_write "mutating gh api (${api_method}) is disabled for contributor agents; use gh issue/pr subcommands for writes."
  fi

  # An enforced lane's direct writes are refused here as well as at the proxy
  # (#9587). `gh api` is the most direct write path in the sandbox: it names a
  # method and a path with nothing in between.
  if [ "$api_method" != "GET" ]; then
    _write_surface_refuse_direct "gh api ${api_method}" "$WRITE_SURFACE_RELAYS"
  fi

  for arg in "${args[@]}"; do
    case "$arg" in
      repos/*/issues\?*|repos/*/issues|repos/*/pulls\?*|repos/*/pulls)
        if [ "$api_method" != "GET" ]; then
          _deny api_write_issues_pulls "mutating gh api (${api_method}) to issues/pulls is disabled for agents; use hive-open-issue / hive-open-pr / hive-review instead."
        fi
        # GET is a read-only lookup — allowed so agents can check for existing PRs (#2356).
        ;;
    esac
  done
fi

# Auto-label issues and PRs with agent identity + hive instance ID.
# HIVE_AGENT is set by the Go binary (e.g. "scanner").
# HIVE_ID is the unique hive instance ID (e.g. "hive-bold-fox").
AGENT_NAME="${HIVE_AGENT:-$AGENT_ID}"
AGENT_DISPLAY_NAME="${HIVE_AGENT_DISPLAY_NAME:-$AGENT_NAME}"
HIVE_INSTANCE_ID="${HIVE_ID:-}"
HIVE_SHA="${HIVE_SHA:-$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')}"

# Build identity footer for injection into issue/comment bodies.
#
# The trailing `<!-- hive:origin hive=<id> at=<ts> -->` marker is the
# machine-readable "which hive wrote this" record that replaced the
# hive/<id> label (hivecommons/hive#12539); pkg/github.OriginHive reads it.
# It stays last so pkg/github's attributionIdentityFooter regex keeps
# matching the whole block.
_identity_footer() {
  local parts="---\n🐝 **Hive Agent**:"
  [[ -n "$AGENT_DISPLAY_NAME" ]] && parts="${parts} \`${AGENT_DISPLAY_NAME}\`"
  [[ -n "$HIVE_INSTANCE_ID" ]] && parts="${parts} | **Instance:** \`${HIVE_INSTANCE_ID}\`"
  parts="${parts} | **SHA:** \`${HIVE_SHA}\`"
  [[ -n "$HIVE_INSTANCE_ID" ]] && parts="${parts}\n<!-- hive:origin hive=${HIVE_INSTANCE_ID} at=$(date -u +%Y-%m-%dT%H:%M:%SZ) -->"
  echo -e "$parts"
}

# Structured blocker marker for needs-human reports (hivecommons/hive#15099).
#
# A report that says the App / token / hive *cannot* do something (push to
# .github/workflows without the `workflows` permission, change branch
# protection, set a secret, sign a CLA) describes a capability gap no
# "Let hive decide" can clear. pkg/blocker reads `<!-- hive:blocker kind=… -->`
# and parks the item on that blocker instead of a bare needs-human, so the
# dashboard says what is missing and the delegated decision answers
# "no-effect" with the real options. Prose is classified here with the same
# narrow rules as pkg/blocker.Infer (mirrored, not shared: this file runs
# where no Go binary is guaranteed). Only bodies that cite needs-human are
# marked, a body that already carries a marker is left alone, and anything
# the rules do not match stays unmarked — a wrong kind is worse than none.
_blocker_kind() {
  local body
  body="$(printf '%s' "$1" | tr '\n' ' ' | tr '[:upper:]' '[:lower:]')"
  [[ "$body" == *"hive:blocker"* ]] && return 1
  [[ "$body" =~ needs[-\ ]human|needs\ a\ (human|person|maintainer)|a\ maintainer\ must|a\ person\ must ]] || return 1
  local cannot="(cannot|can\'t|can not|unable to|not able to|no permission to|not permitted to|is not allowed to)"
  if [[ "$body" =~ (no|lacks?|lacking|missing|without)\ (the\ )?\`?workflows?\`?(:\ ?write)?\ permission ]] \
     || [[ "$body" =~ $cannot\ (push|modify|change|edit|write|update|touch|commit)[^.]{0,80}(\.github/workflows|workflow\ files?|workflows?/) ]] \
     || [[ "$body" =~ workflows?\ permission[^.]{0,40}(missing|lacking|not\ granted|needed|required) ]]; then
    echo "capability:workflows-permission"; return 0
  fi
  if [[ "$body" =~ $cannot[^.]{0,60}(branch[\ -]protection|rulesets?|protected\ branch) ]]; then
    echo "capability:branch-protection"; return 0
  fi
  if [[ "$body" =~ $cannot\ (set|add|create|read|rotate|update|configure)[^.]{0,60}(secrets?|credentials?) ]]; then
    echo "capability:secret"; return 0
  fi
  if [[ "$body" =~ $cannot\ (push|write|commit)[^.]{0,60}(repository|repo|branch|fork) ]] \
     || [[ "$body" =~ (no|lacks?|missing|without)\ (the\ )?\`?(contents|write|push)\`?(:\ ?write)?\ (permission|access) ]]; then
    echo "capability:repo-write"; return 0
  fi
  if [[ "$body" =~ (sign|signing|signed)[^.]{0,30}(cla|contributor\ license\ agreement) ]]; then
    echo "human-only:cla"; return 0
  fi
  return 1
}

# Guard Markdown thematic breaks against becoming setext heading underlines.
#
# A line of `---` placed DIRECTLY under a paragraph is not a rule in Markdown —
# it is a setext underline that turns the paragraph above into an <h2>. Agents write
# `Filed by …\n---` and `Resolved — outage cleared\n---` all the time, and the
# identity footer below opens with `---` too, so the last paragraph of nearly
# every agent comment rendered as a large bold heading (hivecommons/hive#9454).
# Inserting a blank line before every rule that follows text makes each one a
# real thematic break. Fenced code blocks are left untouched: `---` inside a
# fence is literal content (YAML front matter, diffs, tables of dashes).
_guard_markdown_rules() {
  # Portable awk only (mawk on the CI runners has no {n,m} intervals): a rule
  # is a line of three or more dashes with optional spaces between them.
  awk '
    function is_blank(s) { return s ~ /^[ \t]*$/ }
    function is_dash_rule(s) {
      gsub(/[ \t]/, "", s)
      return s ~ /^---+$/
    }
    function fence_marker(s) {
      sub(/^ */, "", s)
      if (s ~ /^```/) { match(s, /^`+/); return substr(s, 1, RLENGTH) }
      if (s ~ /^~~~/) { match(s, /^~+/); return substr(s, 1, RLENGTH) }
      return ""
    }
    BEGIN { prev_blank = 1; fence = "" }
    {
      line = $0
      if (fence == "") {
        m = fence_marker(line)
        if (m != "") {
          fence = m
        } else if (!prev_blank && is_dash_rule(line)) {
          print ""
        }
      } else {
        m = fence_marker(line)
        closing = line; sub(/^ */, "", closing); sub(/[ \t]*$/, "", closing)
        if (m != "" && substr(m, 1, 1) == substr(fence, 1, 1) && length(m) >= length(fence) && closing == m) {
          fence = ""
        }
      }
      print line
      prev_blank = is_blank(line)
    }
  '
}

# Inject the identity footer into whichever body flag the caller used, and
# append `--body <footer>` only when there is genuinely no body at all.
#
# EVERY SPELLING gh ACCEPTS MUST BE LISTED HERE. On the commands this runs for
# (issue/pr create, issue/pr comment, pr review) gh spells the body two ways,
# each in three forms: `--body`/`-b` and `--body-file`/`-F`, separated,
# attached, or `=`-joined — plus `-` for "read the body from stdin".
# Recognising only `--body` and `--body=` was silently wrong in BOTH directions
# (hivecommons/hive#7937), because an unrecognised body flag falls through to
# the "no body found" branch below, which ADDS A SECOND BODY FLAG:
#
#   gh pr comment -F notes.md  → gh received --body-file AND --body and refused
#                                the call ("specify only one of --body or
#                                --body-file"): the comment was never posted.
#                                Observed live, 2026-09-21.
#   gh pr comment -b 'text'    → gh received two --body flags and keeps the
#                                LAST, so the comment posted as the footer
#                                ALONE and the agent's text was dropped with no
#                                error at all.
#
# `-F` is precisely what an agent reaches for when the body is long or contains
# quotes and backticks — i.e. the comments most worth posting correctly.
#
# A file body is read here and re-emitted as `--body` rather than rewritten to
# a temporary `--body-file`: most call sites below end in `exec`, so no EXIT
# trap could ever clean such a temp file up, and argv size is not the binding
# constraint — GitHub caps a body at 65536 characters, and the relay this hands
# off to already passes the whole body through argv itself (hive-open-issue.sh
# does `BODY="$(cat "$BODY_FILE")"`). Reading stdin here is also what makes
# `-F -` work at all: the body must be consumed exactly once, and the process
# that would otherwise read it is replaced by that same `exec`.
# _guard_agent_body refuses a body that would post nothing a reader can use:
# empty once trimmed (the footer alone would land), or only an `@path` (gh does
# not expand `@file` for --body, so the literal path is posted —
# projectbluefin/testsuite#908 got "@/tmp/issue_comment_908.txt"). It runs on
# the agent's own text, before the footer, so the footer cannot hide it.
# require_body=false (reviews) lets an empty body through: `gh pr review
# --approve` needs none.
_guard_agent_body() {
  local body="$1" require_body="$2" verb="$3"
  local trimmed="${body#"${body%%[![:space:]]*}"}"
  trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
  if [[ -z "$trimmed" ]]; then
    $require_body || return 0
    _deny empty_body "gh ${verb}: the body is empty, so only the hive footer would be posted. Write the text, or pass it with --body-file <path>."
  fi
  if [[ "$trimmed" =~ ^@[^[:space:]]+$ ]]; then
    _deny body_file_reference "gh ${verb}: the body is only '${trimmed}'. gh posts that text literally; pass the file with --body-file ${trimmed#@} instead."
  fi
}

_inject_identity() {
  # $1 = require_body: true for issue/PR create and comments, false for reviews.
  local require_body="${1:-false}"
  local verb="${subcmd} ${action}"
  local footer
  footer="$(_identity_footer)"
  local new_args=()
  local body_found=false
  local i=0
  local arg body_val body_ref consumed from_file have_body
  while [ $i -lt ${#args[@]} ]; do
    arg="${args[$i]}"
    body_val=""
    body_ref=""
    consumed=1
    from_file=false
    have_body=false
    case "$arg" in
      --body|-b)
        if [ $((i+1)) -lt ${#args[@]} ]; then
          body_val="${args[$((i+1))]}"; have_body=true; consumed=2
        fi ;;
      --body=*) body_val="${arg#--body=}"; have_body=true ;;
      # Attached short form: gh's flag parser reads `-btext` as `-b text`
      # (and `-Fnotes.md` as `-F notes.md`), so this must too.
      -b?*)     body_val="${arg#-b}";      have_body=true ;;
      --body-file|-F)
        if [ $((i+1)) -lt ${#args[@]} ]; then
          body_ref="${args[$((i+1))]}"; have_body=true; from_file=true; consumed=2
        fi ;;
      --body-file=*) body_ref="${arg#--body-file=}"; have_body=true; from_file=true ;;
      -F?*)          body_ref="${arg#-F}";           have_body=true; from_file=true ;;
    esac

    if ! $have_body; then
      new_args+=("$arg")
      i=$((i+1))
      continue
    fi

    if $from_file; then
      if [ "$body_ref" = "-" ]; then
        body_val="$(cat)"
      elif ! body_val="$(cat -- "$body_ref" 2>/dev/null)"; then
        # Unreadable file: pass the caller's flag through untouched so gh (or
        # the relay) reports the missing file in its own words. It still counts
        # as a body, so we do not stack a second body flag on top of the error
        # and turn a clear "file not found" into a confusing flag conflict.
        new_args+=("$arg")
        if [ "$consumed" -eq 2 ]; then
          new_args+=("${args[$((i+1))]}")
        fi
        body_found=true
        i=$((i+consumed))
        continue
      fi
    fi

    _guard_agent_body "$body_val" "$require_body" "$verb"

    # Normalized to the separated `--body` form. gh, hive-open-issue and
    # hive-open-pr all accept it, and emitting one shape keeps the six input
    # spellings from multiplying into six output spellings.
    #
    # The footer starts with a `---` rule. A blank line MUST separate it from
    # the body: in Markdown a `---` directly under a paragraph is a setext
    # heading underline, which rendered every agent comment's last paragraph
    # as a large bold H2. Trailing whitespace in the body is trimmed first so
    # the gap is exactly one blank line regardless of how the file ended.
    body_val="$(printf '%s\n' "${body_val}" | _guard_markdown_rules)"
    body_val="${body_val%"${body_val##*[![:space:]]}"}"
    local blocker_kind
    if blocker_kind="$(_blocker_kind "$body_val")"; then
      body_val="${body_val}
<!-- hive:blocker kind=${blocker_kind} -->"
    fi
    new_args+=("--body" "${body_val}

${footer}")
    body_found=true
    i=$((i+consumed))
  done
  if ! $body_found; then
    # No body flag. A comment would post the footer alone. A contributor's
    # create goes straight to gh, which refuses a body-less create when not
    # interactive; adding the footer turned that refusal into a footer-only
    # issue or PR (projectbluefin/testsuite#838). A lane's create still relays
    # through hive-open-issue / hive-open-pr as before, unless it asked gh to
    # fill the body itself.
    if $require_body && { [[ "$action" = "comment" ]] || _contributor_mode; } && ! _args_fill_body; then
      _deny empty_body "gh ${verb}: no body was given, so only the hive footer would be posted. Pass --body <text> or --body-file <path>."
    fi
    # `--delete-last` removes a comment and posts nothing, and gh refuses any
    # body alongside it ("should not provide comment body when using
    # --delete-last"), so the footer must not be added either (#15031).
    if ! _args_delete_last; then
      new_args+=("--body" "${footer}")
    fi
  fi
  args=("${new_args[@]}")
}

# _settle_edit_body reads an edit's body once and refuses one that would
# blank the issue or PR. A `--body-file -` body is turned into `--body <text>`
# before the first gh call, because the labeled try below consumes stdin and
# its unlabeled retry then sent an empty body: projectbluefin/testsuite#1053
# was blanked that way a second after its author revised it (the injected
# `contributor/<user>` label did not exist in the repository, so the first
# call failed). A file body is read once too, so both calls send the same text.
_settle_edit_body() {
  local -a out=()
  local i=0 arg val ref
  local verb="${subcmd} ${action}"
  while [ $i -lt ${#args[@]} ]; do
    arg="${args[$i]}"
    ref=""
    case "$arg" in
      --body-file|-F)
        if [ $((i+1)) -lt ${#args[@]} ]; then
          ref="${args[$((i+1))]}"; i=$((i+1))
        fi ;;
      --body-file=*) ref="${arg#--body-file=}" ;;
      -F?*)          ref="${arg#-F}" ;;
      --body|-b)
        if [ $((i+1)) -lt ${#args[@]} ]; then
          val="${args[$((i+1))]}"; i=$((i+1))
          _guard_edit_body "$val" "$verb"
          out+=("--body" "$val")
        else
          out+=("$arg")
        fi
        i=$((i+1)); continue ;;
      --body=*) val="${arg#--body=}"; _guard_edit_body "$val" "$verb"; out+=("$arg"); i=$((i+1)); continue ;;
      -b?*)     val="${arg#-b}";      _guard_edit_body "$val" "$verb"; out+=("$arg"); i=$((i+1)); continue ;;
      *) out+=("$arg"); i=$((i+1)); continue ;;
    esac
    if [ "$ref" = "-" ]; then
      val="$(cat)"
    elif ! val="$(cat -- "$ref" 2>/dev/null)"; then
      # Unreadable: let gh report the missing file in its own words.
      out+=("--body-file" "$ref"); i=$((i+1)); continue
    fi
    _guard_edit_body "$val" "$verb"
    out+=("--body" "$val")
    i=$((i+1))
  done
  args=("${out[@]}")
}

# _guard_edit_body refuses an edit body that is empty or only an `@path`.
_guard_edit_body() {
  local body="$1" verb="$2"
  local trimmed="${body#"${body%%[![:space:]]*}"}"
  trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
  if [[ -z "$trimmed" ]]; then
    _deny blanking_edit "gh ${verb}: the new body is empty and would blank the existing one. Pass the full text with --body or --body-file <path>."
  fi
  if [[ "$trimmed" =~ ^@[^[:space:]]+$ ]]; then
    _deny body_file_reference "gh ${verb}: the body is only '${trimmed}'. gh posts that text literally; pass the file with --body-file ${trimmed#@} instead."
  fi
}

# _args_fill_body reports whether gh is asked to build the body itself
# (`--fill*`, `--template`, `--web`, `--recover`) or needs none at all
# (`--delete-last`), so a missing body flag is not an empty body.
_args_fill_body() {
  local a
  for a in "${args[@]}"; do
    case "$a" in
      --fill|--fill-first|--fill-verbose|-f|--template|--template=*|-T|-T?*|--web|-w|--recover|--recover=*|--delete-last) return 0 ;;
    esac
  done
  return 1
}

# _args_delete_last reports whether a comment is being deleted rather than
# posted (`gh issue|pr comment --delete-last`); gh rejects any body there.
_args_delete_last() {
  local a
  for a in "${args[@]}"; do
    [[ "$a" = "--delete-last" ]] && return 0
  done
  return 1
}

if [[ -n "$AGENT_NAME" ]]; then
  # agent/<lane>, never agent/<display name>: the scheduler's PR-ownership
  # check and the classifier's label routing compare this suffix to the lane
  # name (`agents.<lane>`), so a display name such as "Code Scanner" would
  # produce a label no consumer recognises (#8927). The display name is
  # human-facing only — it goes in the label description and the footer.
  LABELS_CSV="agent/${AGENT_NAME}"
  # No hive/<id> label (hivecommons/hive#12539): the origin marker in the
  # identity footer says which hive wrote the item, and the hive strips the
  # retired label from open items at boot. The dashboard hold label is
  # hive-pause/<id>.
  # Contributor labels
  if _contributor_mode; then
    [[ -n "${HIVE_CONTRIBUTOR_USERNAME:-}" ]] && LABELS_CSV="${LABELS_CSV},contributor/${HIVE_CONTRIBUTOR_USERNAME}"
    [[ -n "${HIVE_CONTRIBUTOR_CLI:-}" ]] && LABELS_CSV="${LABELS_CSV},cli/${HIVE_CONTRIBUTOR_CLI}"
  fi

  # Ensure labels exist on the repo (cached per-session to avoid repeated API calls).
  #
  # The cache MUST be keyed per target repo (#4043): a single global flag meant
  # whichever repo an agent touched first got the labels created, and every
  # other repo was skipped for the life of the pod — leaving the injected
  # agent/<lane> label missing there, which made every `gh pr edit`/`gh issue
  # edit` on those repos fail with "'<label>' not found". Verified live on a
  # fleet owner's hive: only the first-touched repos carried the current label.
  LABEL_CACHE_BASE="/tmp/.hive-labels-ensured"
  _ensure_labels() {
    local repo_flag=""
    for arg in "${args[@]}"; do
      case "$arg" in
        --repo) repo_flag="next" ;;
        --repo=*) repo_flag="${arg#--repo=}" ; break ;;
        *) [[ "$repo_flag" = "next" ]] && repo_flag="$arg" && break ;;
      esac
    done
    [[ "$repo_flag" = "next" ]] && repo_flag=""
    # owner/repo → owner_repo; repo names are [A-Za-z0-9_.-] so '/' is the only
    # separator to neutralize. No --repo means "current directory's repo" —
    # cache that under its own key rather than sharing one with named repos.
    local cache="${LABEL_CACHE_BASE}-${repo_flag//\//_}"
    [[ -f "$cache" ]] && return 0
    local rf=""
    [[ -n "$repo_flag" ]] && rf="--repo $repo_flag"
    "$REAL_GH" label create "agent/${AGENT_NAME}" --description "Work by the ${AGENT_DISPLAY_NAME} agent" --color 6f42c1 $rf 2>/dev/null || true
    touch "$cache"
  }

  # Extract issue/PR number and repo from args (for post-action labeling).
  _extract_item() {
    item_num=""
    item_repo=""
    local skip=false
    for arg in "${args[@]}"; do
      if $skip; then skip=false; item_repo="$arg"; continue; fi
      case "$arg" in
        comment|review|"$subcmd"|"$action") continue ;;
        --repo) skip=true; continue ;;
        --repo=*) item_repo="${arg#--repo=}"; continue ;;
        -*) continue ;;
        *) [[ -z "$item_num" ]] && item_num="$arg" ;;
      esac
    done
    # Explicit success: the loop's last iteration is often `[[ -z set ]] && …`
    # (a trailing positional like a --body value), which evaluates false. Under
    # `set -e` that non-zero return killed the whole wrapper — comments with a
    # trailing free-text arg died with a silent exit 1.
    return 0
  }

  # ── PR provenance labels go only on the hive's OWN PRs (#12545) ──
  # `agent/<lane>` is a ROUTING label (the scheduler's PR-ownership check,
  # the classifier and the CI-repair router read it as "this lane owns the
  # work") and `hive/<id>` is "authored by this hive". The pr/edit, pr/comment
  # and pr/merge arms used to stamp both onto WHATEVER PR the agent touched —
  # so a review-fix kick that ran `gh pr edit <n> --body` on a contributor's PR
  # (review.fix_human_prs) re-routed that PR to the scanner and claimed it
  # for the hive. Ownership is decided by authorship: the PR's author must be
  # the identity this wrapper authenticates as (the #4044 trusted bot login,
  # or `gh api user` for a user token — the same oracle the --author gate
  # uses, so an agent cannot forge it). Adoption (stage/074) opens a NEW App
  # PR, which is self-authored and gets the labels by this same rule; the
  # adopted contributor PR keeps its own authorship. Unresolvable author or
  # identity → no labels: provenance is metadata and must fail closed, never
  # mislabel.
  #
  # The cache under LABEL_CACHE_BASE sits in world-writable /tmp (see the
  # mode_unrecognized arm for who can write there: any agent UID in the pod,
  # or a managed-repo PR whose tests run in this sandbox). It therefore only
  # ever records, and only ever honours, the fail-closed answer: a cached
  # "no" skips the lookup, anything else — including a planted "yes" — goes
  # back to the API oracle. A positive verdict is re-verified on every call;
  # the cost is one `gh pr view` per label stamp on an own PR.
  _hive_owns_pr() {
    local num="$1" repo="$2" cache author self owned
    cache="${LABEL_CACHE_BASE}-owner-${repo//\//_}-${num:-current}"
    if [[ -f "$cache" ]]; then
      owned="$(tr -d '[:space:]' < "$cache" 2>/dev/null || true)"
      if [[ "$owned" = "no" ]]; then
        return 1
      fi
    fi
    local view_args=(pr view)
    [[ -n "$num" ]] && view_args+=("$num")
    [[ -n "$repo" ]] && view_args+=(--repo "$repo")
    author="$("$REAL_GH" "${view_args[@]}" --json author --jq '.author.login' 2>/dev/null | tr -d '[:space:]' || true)"
    self="$(_resolve_self_login 2>/dev/null || true)"
    owned=no
    if [[ -n "$author" && -n "$self" ]] && _author_matches_login "$author" "$self"; then
      owned=yes
    fi
    if [[ "$owned" = "no" ]]; then
      printf '%s\n' no >"$cache" 2>/dev/null || true
    else
      rm -f "$cache" 2>/dev/null || true
    fi
    [[ "$owned" = "yes" ]]
  }

  # _pr_labels_csv: LABELS_CSV when the hive owns the PR named by the current
  # argv, empty otherwise. Callers test for emptiness before injecting.
  _pr_labels_csv() {
    _extract_item
    if _hive_owns_pr "$item_num" "$item_repo"; then
      printf '%s\n' "$LABELS_CSV"
    fi
  }

  case "$subcmd/$action" in
    issue/create|pr/create)
      _inject_identity true
      # ── Relay issue creation through the hive (issue-request watcher) ──
      # The direct path rode the agent's shell tool: one GHE secondary-rate-
      # limit stall, network blip, or mangled multiline command and the
      # finding was silently lost (root-caused live 2026-08-21: sec-check's
      # creates timed out mid-flight, repeatedly, and survived only as beads).
      # hive-open-issue writes a request file (milliseconds, no network); the
      # hive creates the issue server-side with the App token — retried with
      # backoff, deduped by exact open-issue title, and gated by the SAME
      # CanCreateIssues mode check + UID forge-resistance as this wrapper.
      # Mode gates (NO_GITHUB/ADVISORY capture) have already run above, so an
      # advisory agent's finding still lands in the digest, never the queue.
      # Contributors are EXEMPT (they file under their own identity), mirroring
      # the hive-open-pr redirect.
      if [ "$subcmd" = "issue" ] && ! _contributor_mode && command -v hive-open-issue >/dev/null 2>&1; then
        exec hive-open-issue "${args[@]}" --label "$LABELS_CSV"
      fi
      # The relay did not take the call (contributor, or the shim is missing).
      # For an enforced lane there is no direct fallback: the write surface is
      # the relays or nothing (#9587).
      _write_surface_refuse_direct "gh ${subcmd} create" "hive-open-issue (file an issue), hive-open-pr (open a PR)"
      _ensure_labels
      # `|| rc=$?` (not `cmd; rc=$?`): this script runs under `set -e`, so a
      # bare failing gh exited the wrapper BEFORE rc was ever read — the
      # unlabeled retry below was dead code, and a missing injected label
      # failed the whole create (#4043). gh resolves label names before
      # mutating, so the retry cannot double-create.
      rc=0
      "$REAL_GH" "${args[@]}" --label "$LABELS_CSV" || rc=$?
      if [ $rc -ne 0 ]; then
        exec "$REAL_GH" "${args[@]}"
      fi
      exit $rc
      ;;
    issue/edit|pr/edit)
      # ── Issue self-claim → hive/claimed-by-<agent> label ──
      # An App bot cannot be a GitHub assignee (Issues.AddAssignees silently
      # drops it), so `gh issue edit <n> --add-assignee @me` would be a no-op —
      # the agent thinks it claimed the issue but nothing is recorded. Relay it
      # to hive-open-issue claim, which applies a visible, AUDITED
      # hive/claimed-by-<agent> label (agent_issue_claimed). Only for a plain
      # self-claim on an issue; anything else falls through to the label path.
      if [ "$subcmd" = "issue" ] && ! _contributor_mode && command -v hive-open-issue >/dev/null 2>&1; then
        _claim_self=false
        _prev_arg=""
        for a in "${args[@]}"; do
          case "$a" in
            --add-assignee=@me|--add-assignee=@self) _claim_self=true ;;
            @me|@self) [ "$_prev_arg" = "--add-assignee" ] && _claim_self=true ;;
          esac
          _prev_arg="$a"
        done
        if $_claim_self; then
          _extract_item
          if [ -n "$item_num" ]; then
            _claim_repo=""
            [ -n "$item_repo" ] && _claim_repo="--repo $item_repo"
            exec hive-open-issue claim $_claim_repo "$item_num"
          fi
        fi
      fi
      # ── Relay label/reviewer edits through the hive (issue-request watcher) ──
      # `gh issue edit --add-label` and `gh pr edit --add-reviewer` used to
      # ride the agent's own token straight through the proxy; the `label` and
      # `request_review` relays existed only as an opt-in path. A PURE
      # label/reviewer edit is translated into relay request files here, so it
      # gets the same file-UID authorizer, lane allowlist, reserved-label
      # refusal and redacted audit entry as every other relay write
      # (hivecommons/hive#9773). The relay shim prints the request path and
      # where the .result.json will appear, so the caller sees the outcome.
      # An edit that mixes in anything the relays cannot express (--title,
      # --body, --milestone, --remove-reviewer, assignees, projects, …) falls
      # through to the direct path UNCHANGED, as does an edit with no explicit
      # number/URL or no resolvable repo — routing must not reduce what an
      # ACMM L6 hive can do by default (same contract as hive-open-pr: this
      # changes WHO performs the write, not WHAT an agent is allowed to do).
      # Contributors are EXEMPT (they edit under their own identity),
      # mirroring the other relay redirects.
      if ! _contributor_mode && command -v hive-open-issue >/dev/null 2>&1; then
        _re_add=""; _re_remove=""; _re_reviewers=""
        _re_num=""; _re_repo=""; _re_other=false
        _re_expect=""; _re_pos=0
        for a in "${args[@]}"; do
          if [ -n "$_re_expect" ]; then
            case "$_re_expect" in
              add)      _re_add="${_re_add:+${_re_add},}$a" ;;
              remove)   _re_remove="${_re_remove:+${_re_remove},}$a" ;;
              reviewer) _re_reviewers="${_re_reviewers:+${_re_reviewers},}$a" ;;
              repo)     _re_repo="$a" ;;
              other)    : ;;
            esac
            _re_expect=""
            continue
          fi
          case "$a" in
            --add-label)       _re_expect=add ;;
            --add-label=*)     _re_add="${_re_add:+${_re_add},}${a#*=}" ;;
            --remove-label)    _re_expect=remove ;;
            --remove-label=*)  _re_remove="${_re_remove:+${_re_remove},}${a#*=}" ;;
            --add-reviewer)    _re_expect=reviewer ;;
            --add-reviewer=*)  _re_reviewers="${_re_reviewers:+${_re_reviewers},}${a#*=}" ;;
            --repo|-R)         _re_expect=repo ;;
            --repo=*)          _re_repo="${a#*=}" ;;
            -*)
              # Any other flag is an edit the relays cannot express — use the
              # SAME flag table as the subcommand extractor (N7 lesson) so a
              # separated value is never misread as the item number.
              _re_other=true
              if gh_flag_takes_value "$a"; then _re_expect=other; fi ;;
            *)
              # Positions 1 and 2 are the subcommand and action; the next
              # positional is the issue/PR number or URL.
              _re_pos=$((_re_pos + 1))
              if [ "$_re_pos" -gt 2 ] && [ -z "$_re_num" ]; then _re_num="$a"; fi ;;
          esac
        done
        # The relay resolves a number or URL, not a branch name.
        case "$_re_num" in
          [0-9]*|http://*|https://*) : ;;
          *) _re_num="" ;;
        esac
        # `--add-reviewer` is only meaningful on a PR; on `issue edit` gh
        # itself refuses it, so let gh say so rather than half-relaying.
        if [ "$subcmd" = "issue" ] && [ -n "$_re_reviewers" ]; then
          _re_other=true
        fi
        if ! $_re_other && [ -n "$_re_num" ] && \
           { [ -n "$_re_add" ] || [ -n "$_re_remove" ] || [ -n "$_re_reviewers" ]; }; then
          if [ -z "$_re_repo" ]; then
            # gh infers the repo from the checkout; the relay request needs it
            # spelled out. Same inference, from the origin remote.
            _re_repo="$(git remote get-url origin 2>/dev/null || true)"
            case "$_re_repo" in
              git@*)                      _re_repo="${_re_repo#*:}" ;;
              ssh://*|http://*|https://*) _re_repo="${_re_repo#*://}"; _re_repo="${_re_repo#*/}" ;;
              *)                          _re_repo="" ;;
            esac
            _re_repo="${_re_repo%.git}"
            case "$_re_repo" in
              */*) : ;;
              *)   _re_repo="" ;;
            esac
          fi
          if [ -n "$_re_repo" ]; then
            # Deliberately NOT the provenance LABELS_CSV: the relay's audit
            # entry already attributes the write to this agent.
            _re_rc=0
            if [ -n "$_re_add" ] || [ -n "$_re_remove" ]; then
              _re_label_args=()
              [ -n "$_re_add" ] && _re_label_args+=(--label "$_re_add")
              [ -n "$_re_remove" ] && _re_label_args+=(--remove-label "$_re_remove")
              hive-open-issue label --repo "$_re_repo" "$_re_num" "${_re_label_args[@]}" || _re_rc=$?
            fi
            if [ -n "$_re_reviewers" ]; then
              hive-open-issue request-review --repo "$_re_repo" "$_re_num" --reviewer "$_re_reviewers" || _re_rc=$?
            fi
            exit $_re_rc
          fi
        fi
      fi
      # An edit the relays cannot express still reaches GitHub directly, which
      # is exactly what an enforced lane may not do (#9587). Express it as a
      # label / reviewer request the relays can perform, or leave it to a human.
      _write_surface_refuse_direct "gh ${subcmd} edit" "hive-open-issue label (labels), hive-open-issue request-review (reviewers)"
      # Issues: every hive-touched issue carries the lane's routing label.
      # PRs: only the hive's OWN PRs do (#12545) — editing a contributor's PR
      # (review-fix on fix_human_prs, a description touch-up) must not re-route
      # or re-own it.
      _settle_edit_body
      edit_labels="$LABELS_CSV"
      if [[ "$subcmd" = "pr" ]]; then
        edit_labels="$(_pr_labels_csv)"
        if [[ -z "$edit_labels" ]]; then
          exec "$REAL_GH" "${args[@]}"
        fi
      fi
      _ensure_labels
      # Label injection is provenance metadata, not a security gate. gh applies
      # an edit atomically, so a missing label (repo not yet ensured, create
      # denied, label deleted) failed the ENTIRE edit with "'<label>' not found"
      # — agents read that as a permissions failure and route around the wrapper
      # (#4043). Mirror the create arm: try labeled, retry unlabeled on failure.
      # `|| rc=$?` keeps the retry alive under `set -e` (see create arm above).
      rc=0
      "$REAL_GH" "${args[@]}" --add-label "$edit_labels" || rc=$?
      if [ $rc -ne 0 ]; then
        exec "$REAL_GH" "${args[@]}"
      fi
      exit $rc
      ;;
    pr/merge)
      _write_surface_refuse_direct "gh pr merge" "hive-merge"
      _extract_item
      # Only the hive's own PRs get routing/provenance labels (#12545).
      if [[ -n "$item_num" ]] && _hive_owns_pr "$item_num" "$item_repo"; then
        _ensure_labels
        local_repo=""
        [[ -n "$item_repo" ]] && local_repo="--repo $item_repo"
        "$REAL_GH" pr edit "$item_num" $local_repo --add-label "$LABELS_CSV" 2>/dev/null || true
      fi
      exec "$REAL_GH" "$@"
      ;;
    pr/review)
      _inject_identity
      # ── Relay reviews through the hive (review-request watcher) ──
      # A direct `gh pr review` lands under the agent's own shell token, is
      # never audited, and so PR-review activity is invisible to the hive-health
      # output signal. hive-review writes a request file the hive submits with
      # the App token and records as agent_pr_reviewed — same rationale as
      # issue/create, and gated by the SAME per-agent authorization + UID
      # forge-resistance. Contributors are EXEMPT (they review under their own
      # identity), mirroring the hive-open-pr / hive-open-issue redirects.
      if ! _contributor_mode && command -v hive-review >/dev/null 2>&1; then
        exec hive-review "${args[@]}"
      fi
      _write_surface_refuse_direct "gh pr review" "hive-review"
      # No relay available: fall through to real gh so a review is never lost.
      # `"${args[@]}"`, not `"$@"`: the injected identity belongs on this review
      # as much as on the relayed one, and since _inject_identity now reads a
      # `--body-file -` body itself (#7937), the original argv's `-` would find
      # stdin already at EOF and submit an empty review body.
      exec "$REAL_GH" "${args[@]}"
      ;;
    issue/comment|pr/comment)
      _inject_identity true
      _extract_item
      # ── Relay comments through the hive (same rationale as issue/create) ──
      # A lost review/triage comment is a lost work product.
      if [ "$action" = "comment" ] && ! _contributor_mode && command -v hive-open-issue >/dev/null 2>&1; then
        exec hive-open-issue comment "${args[@]}"
      fi
      _write_surface_refuse_direct "gh ${subcmd} comment" "hive-open-issue comment"
      # Issues always get the lane's routing label; a PR only when the hive
      # authored it (#12545) — commenting on a contributor's PR is review, not
      # ownership.
      post_label=yes
      if [[ "$subcmd" = "pr" && -n "$item_num" ]] && ! _hive_owns_pr "$item_num" "$item_repo"; then
        post_label=no
      fi
      [[ "$post_label" = "yes" ]] && _ensure_labels
      "$REAL_GH" "${args[@]}"
      exit_code=$?
      if [[ $exit_code -eq 0 && -n "$item_num" && "$post_label" = "yes" ]]; then
        local_repo=""
        [[ -n "$item_repo" ]] && local_repo="--repo $item_repo"
        "$REAL_GH" "$subcmd" edit "$item_num" $local_repo --add-label "$LABELS_CSV" 2>/dev/null || true
      fi
      exit $exit_code
      ;;
    issue/close)
      # Route direct/manual closes through the hive so the same
      # reporter-confirmation gate that downgrades unsafe PR `Closes #N`
      # references also covers `gh issue close`. Legitimate early closes use the
      # deliberate hive-open-issue close --override-reason path, which posts the
      # reason before closing.
      if ! _contributor_mode && command -v hive-open-issue >/dev/null 2>&1; then
        exec hive-open-issue close "${args[@]}"
      fi
      _write_surface_refuse_direct "gh issue close" "hive-open-issue close"
      exec "$REAL_GH" "$@"
      ;;
  esac
fi

# Last stop before the real gh. Everything routed above has already been
# decided; what reaches here is a read, or a write verb with no arm of its own
# (`gh issue reopen`, `gh pr ready`, `gh label create`, …). An enforced lane
# may not perform those directly (#9587); reads are never touched.
case "$subcmd/$action" in
  issue/reopen|issue/develop|pr/reopen|pr/ready|label/create)
    _write_surface_refuse_direct "gh ${subcmd} ${action}" "$WRITE_SURFACE_RELAYS" ;;
esac

exec "$REAL_GH" "$@"
