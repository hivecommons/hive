# Compact GitHub CLI reads (`hive-gh`) and one-line denials

Issue [#12127](https://github.com/hivecommons/hive/issues/12127) (RFC #12134,
token-usage review item C3).

The GitHub proxy gates **requests**; it forwards **responses** unprojected.
`gh pr view`, `gh issue view` and `gh run view` therefore print bodies, every
comment, every review and every check at full size, all of which lands in the
agent's context window. Agents need a dozen fields to decide what to do next.

Two changes shrink that footprint without touching what the gates can see:

1. **`hive-gh`** — a projection front for the common read verbs that runs the
   gated `gh` with a fixed `--json` field set and a `--jq` filter that flattens
   verbose sub-objects. Installed at `/usr/local/bin/hive-gh` in the agent image
   (`bin/hive-gh.sh`).
2. **One-line denials** — every refusal from the gh wrapper
   (`bin/gh-wrapper.sh`) and the proxy (`pkg/proxy/github_proxy.go`) is one line
   — what was blocked, why, what to do instead — ending in a machine-readable
   `reason=<code>`.

## Invariants

- **Gate fields are never projected away.** Every field an authorization or
  merge gate reads stays in the compact set: `labels`, `author` (login, with a
  `[bot]` suffix for apps), head SHA (`headRefOid` / `headSha`), `baseRefName`,
  `isDraft`, `state`, `mergeable`, `mergeStateStatus`, `reviewDecision`, and
  check **name + conclusion** (both CheckRun and legacy StatusContext kinds are
  normalised to `{name,status,conclusion}`).
- **Full fetch stays available on explicit demand.** `hive-gh --full <verb> …`
  (or `HIVE_GH_FULL=1`) hands the command to `gh` untouched. So does any call
  that already carries its own `--json`, `--jq`, `--template`, `--web`,
  `--comments`, `--log`/`--log-failed`: the caller chose a projection and
  `hive-gh` does not override it.
- **No new authority.** `hive-gh` is argv rewriting in front of whatever `gh`
  is on `PATH` — in the agent image that is the hive wrapper, so every mode,
  surface-allowlist, write-surface and merge gate still runs. Writes and verbs
  outside the table pass through unchanged.
- **The `--jq` filters only reshape.** They never `select()` rows away; a list
  is the same set of items `gh` would have printed, each one smaller.

## Field-set table

`hive-gh --fields` prints this table from the script itself; the script is the
source of truth.

| Verb | Compact `--json` fields | Reshaping | Dropped (`--full` restores) |
|---|---|---|---|
| `pr view` | number, title, state, isDraft, url, author, headRefName, **headRefOid**, baseRefName, **labels**, **mergeable**, **mergeStateStatus**, **reviewDecision**, **statusCheckRollup**, additions, deletions, changedFiles, updatedAt, body | labels → names; author → login; checks → `{name,status,conclusion}` | comments, reviews, latestReviews, reviewRequests, commits, files, assignees, milestone, projectCards, reactionGroups |
| `pr list` | number, title, state, isDraft, url, author, headRefName, **headRefOid**, baseRefName, **labels**, **mergeable**, **mergeStateStatus**, **reviewDecision**, updatedAt | per item: labels → names; author → login | body, statusCheckRollup (use `hive-gh pr checks <n>`), comments, reviews, commits, files |
| `pr checks` | name, **bucket**, **state**, workflow, link | — | description, event, startedAt, completedAt |
| `issue view` | number, title, state, stateReason, url, author, **labels**, assignees, milestone, createdAt, updatedAt, body | labels → names; author → login; assignees → logins; milestone → title | comments (`--full`, or `gh issue view <n> --comments`), projectCards, reactionGroups, closedByPullRequestsReferences |
| `issue list` | number, title, state, url, author, **labels**, assignees, updatedAt | per item, as above | body, milestone, comments |
| `run view` | databaseId, name, workflowName, displayTitle, status, **conclusion**, event, headBranch, **headSha**, attempt, url, jobs | jobs → `{name,status,conclusion,failed_steps[]}` | per-step detail and logs (`--full`, or `gh run view <id> --log-failed`) |

Bold marks the gate fields. `body` is kept on the `view` verbs because it is
the work content; it is dropped from the `list` verbs.

Note that `pr list` / `issue list` are still refused for staff agents by the
wrapper's enumeration gate (the governor supplies the work queue in the kick);
`hive-gh` does not change that — the denial is simply shorter now.

## Denial format

Every refusal is one line on stderr (wrapper) or a one-line 403 body (proxy):

```
⛔ BLOCKED: <what was blocked — why — what to do instead> reason=<code>
⛔ ACMM proxy: <agent> (<mode>) blocked <METHOD> <directive> reason=<code>
```

The proxy also sets `X-Hive-Proxy-Reason: <code>` beside the existing
`X-Hive-Proxy-Blocked: true`. JSON denials (auto-merge disabled) are unchanged
apart from the header.

The prose may be reworded; the codes are stable identifiers:

| Code | Emitted by | Meaning |
|---|---|---|
| `gh_unavailable` | wrapper | real `gh` binary missing from the image |
| `token_cache_unavailable` | wrapper | per-agent scoped token not delivered (audit H3) |
| `agent_restriction` | wrapper | per-agent pattern rule in `/etc/hive/restrictions/<agent>.json` |
| `contributor_auth` | wrapper | `gh auth` in contributor mode (#6659) |
| `surface_allowlist` | wrapper | subcommand not on the deny-by-default allowlist (#3840) |
| `ci_watch_blocked` | wrapper | blocking CI wait: `gh run watch` or `gh pr checks --watch` (#13587) |
| `write_surface_enforce` | wrapper, proxy | direct write from a lane under `write_surface.enforce` (#9587) |
| `author_identity_unresolved`, `author_mismatch` | wrapper | `--author` gate (#3072/#3982/#4044) |
| `enumeration_disabled` | wrapper | `gh issue list` / `gh pr list` for staff agents |
| `relay_missing` | wrapper | `hive-open-pr` shim absent |
| `mode_no_github`, `mode_advisory`, `mode_issues_only`, `mode_issues_and_prs`, `mode_unrecognized` | wrapper | agent-mode gates |
| `supervisor_no_issues`, `acmm_level` | wrapper | level-based fallback gates |
| `merge_no_identifier`, `merge_not_eligible`, `merge_gate_unavailable` | wrapper | merge-eligibility gate |
| `contributor_api_write`, `api_write_issues_pulls` | wrapper | mutating `gh api` |
| `empty_body`, `body_file_reference`, `blanking_edit` | wrapper | a create, comment or edit body that would post nothing usable: empty once trimmed (the footer would land alone; a create or comment with no body flag counts, unless `--fill`/`--template`/`--web` asks gh to build it; reviews may stay empty), only an `@path` (gh posts it literally; use `--body-file`), or an edit that blanks the existing body |
| `repo_paused` | proxy | repo paused by the operator (#6203) |
| `agent_repo_scope` | proxy | agent not scoped to the repo (#6204) |
| `repo_auto_merge_disabled` | proxy | `repo_policies[].auto_merge=false` |
| `linear_policy` | proxy | Linear GraphQL policy |
| `graphql_mutation`, `graphql_denied` | proxy | GraphQL refused by mode/capability |
| `hard_deny` | proxy | hard-deny rule (direct PR create/merge → use the relay named in the line) |
| `mode_denied` | proxy | REST rule refused by mode/capability |
| `repo_not_in_config` | proxy | repo filter |
| `issue_shape` | proxy | issue/comment shape gate (#7014) |
| `canary_egress` | proxy | canary token on egress (ADR 0008) |

`HIVE_GH_DENY_VERBOSE=1` makes the wrapper append a second line pointing at the
docs; it is never set in agent images.

## Tests

- `bin/test_hive_gh.sh` — contract suite (stub `gh` applies the real `jq`
  filters): projection added, every gate field retained per verb, reshaping
  keeps gate values, every escape hatch passes through untouched, unknown verbs
  and writes untouched, `--fields` output.
- `bin/test_gh_wrapper_gates.sh`, `bin/gh-wrapper.test.sh` — the existing gate
  suites, which pin the directive phrases the one-line denials keep.
- `pkg/proxy/denial_message_test.go` — one-line body, trailing `reason=`,
  header, directive retained for pause and hard-deny.
