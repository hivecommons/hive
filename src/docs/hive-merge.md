# `hive-merge` — merge a PR as the App bot

`bin/hive-merge.sh` is how an agent merges a pull request. Agents call it
**instead of the GitHub MCP `merge_pull_request` tool** (or a GraphQL
`mergePullRequest` mutation). GitHub rejects that mutation for App
installation tokens with "Resource not accessible by integration" even when
the token holds `contents:write` + `pull_requests:write`. The hive merges the
PR over REST with the App token instead, which succeeds — so the merge is
authored by the App bot (`<slug>[bot]`) and uses the transport that actually
works for an App token.

It does not merge the PR itself. It writes a request file that the hive's
merge-request watcher picks up and acts on.

## Why it exists

Two reasons.

**Transport.** The MCP/GraphQL merge path is rejected outright for App
installation tokens. Routing through the hive's REST-based merge path is not
optional convenience — it's the only path that works.

**Authorization + forge resistance.** The script runs **as the agent**, in
that agent's tmux session under that agent's UID, so the request file it
writes is owned by that UID. The watcher re-derives the requesting agent from
the **file's owner**, not from anything written inside the file. `hive-merge`
**adds no privilege**: the watcher enforces the same per-agent forge-resistance
check and a `CanMerge` ACMM gate (`AuthorizeMerge`) that a direct merge would
need — an agent whose mode allows opening PRs but not merging them still
cannot merge through this path.

`project.repo_policies[].auto_merge: false` is also enforced here. A repo with
that switch off may still receive Hive-opened PRs, but the relay refuses to
merge them and reports that auto-merge is disabled for the repository. The same
repo policy is enforced by the App self-merge sweep and by the proxy's direct
REST/GraphQL merge denial path, so agents cannot bypass it by calling GitHub
directly.

On top of `AuthorizeMerge`, the watcher's authorizer is wrapped with a second,
target-specific check (the F4 target-binding, CWE-863) before any merge is
attempted:

- the request must carry a non-empty `expect_sha` — an empty expected SHA
  would mean "merge whatever HEAD is now," which is a TOCTOU hole: a commit
  pushed after the PR was judged eligible could get merged unseen;
- the `(repo, number)` pair must currently appear in the governor's
  merge-eligible list **at that exact head SHA** — so an agent cannot request
  landing an arbitrary reachable PR (including its own) merely because its
  checks happen to be green; it must be a PR the hive's governor already
  deemed eligible, at the commit it reviewed.

Both the agent/UID/`CanMerge` check and this target-binding check must pass
before the merge is attempted.

**The hive does not force-merge.** GitHub still enforces whatever write
permissions, branch protection, rulesets, required reviews, allowed merge
methods, and required checks apply to the App token. A PR GitHub refuses fails
here, and the result file records why.

**The hive verifies CI itself, and absent is not passing.** Before any merge is
attempted the watcher requires *positive* CI confirmation on the pinned head SHA
(#6173). This gate applies equally to protected and unprotected base branches:
a red PR, or one whose workflows all died at startup with zero jobs (and
therefore zero check runs and an empty status rollup), does not merge merely
because GitHub branch protection is absent. The watcher computes its own
verdict:

- **green** - at least one commit status or check run exists on the SHA, every
  gating one has succeeded (`neutral`/`skipped` count as success, the
  non-required ignore list still applies), every config-declared required
  check has reported, and no workflow run on the SHA failed without producing
  a job. The merge proceeds.
- **pending** - a gating check is still queued/in progress, a required check
  has not been created yet, or a workflow run is in flight without a job. The
  request is parked (`ci_waits` in the result file, no attempt consumed) and
  re-evaluated next tick, for up to `mergeRequestMaxCIWaits` ticks.
- **red** - a gating check failed, or a workflow run concluded failure with
  zero jobs. Refused; counts as a failed attempt and, at the retry limit, is
  classified and re-engaged into the fix loop like a branch-protection
  refusal.
- **unverified** - zero statuses, zero check runs, zero workflow runs. GitHub
  has no verdict, so neither does the hive. Refused; quarantined at the retry
  limit with the reason in the result file. Repos with genuinely no CI (docs-
  only, config-only) can be opted out per-repo with `auto_merge.no_ci_ok`
  (#6281): listing the repo (`owner/repo` or bare name) downgrades this — and
  ONLY this — verdict to green. Red and pending are never downgraded, and the
  default stays refuse.

An API error while gathering that evidence is a failed attempt, never a pass.

**Fork PR workflow approval remains manual.** When a cross-repo pull request's
workflow runs conclude `action_required`, the watcher refuses the merge with a
reason that says the fork PR workflow runs are awaiting maintainer approval.
The operator alert links to the repository Actions settings and tells the owner
to approve the runs manually or relax "Approval for running fork pull request
workflows". The hive does not call GitHub's workflow-run approval API.

**Unprotected base branches are allowed.** The watcher no longer makes a
separate branch-protection lookup and no longer refuses solely because the base
branch is unprotected. It merges into any protected or unprotected branch the
GitHub App can write, provided the hive's positive CI-evidence gate passes and
GitHub accepts the merge. The legacy `auto_merge.allow_unprotected_base` key is
accepted as a deprecated no-op so existing configs keep loading; it is no
longer needed.

**No-CI repositories require an explicit repo opt-in.** Repositories with no CI
by design can be listed under `auto_merge.no_ci_ok`. That opt-in only downgrades
the **unverified** verdict (zero statuses, zero check runs, zero workflow runs)
to green for that listed repo, and the watcher logs that
`auto_merge.no_ci_ok` enabled it. Any non-zero failing CI evidence still
refuses, and pending evidence still waits.

**Actionable merge failures alert the operator.** Failures that require an
operator configuration change raise one dashboard system alert per repo+reason,
not one per PR or retry tick. The alert text names the fix: install/grant the
App Contents and Pull requests write permissions, adjust review/ruleset bypass
or approve the PR, make a missing required check report, or enable/use an
allowed merge method. Conflicts re-engage the fix loop instead of alerting, and
rate limits are treated as transient. A later successful merge in that repo
clears its merge-failure alerts automatically.

```yaml
auto_merge:
  no_ci_ok:
    - your-org/docs-only-repo
```

### Trusted bot authors

The self-authored automerge sweep merges the App's own open, CI-green PRs. It
also merges PRs from `auto_merge.trusted_bot_authors` through the identical
gates (required checks green, mergeable, no hold/exempt label, intent tier,
approval desk, head SHA re-verified at merge time). The default is
`dependabot[bot]` only; set an explicit empty list to keep the sweep App-only,
or add other dependency bots you trust. Each merge is recorded with
`lane=trusted-bot` so audits can tell it from `lane=self-authored`.

```yaml
auto_merge:
  trusted_bot_authors:
    - dependabot[bot]
    - renovate[bot]
```

The same list is editable from the dashboard: **Settings → Features → Auto
merge → Trusted bot authors** shows a toggle per bot. Known dependency bots
(`dependabot`, `renovate`, `mergeraptor`, `pre-commit-ci`, `github-actions`)
are listed first, bots currently authoring open PRs in your repos are
discovered from the last scan and listed automatically, and any other login
can be typed in. Saving writes `auto_merge.trusted_bot_authors` in full;
turning every bot off writes an explicit empty list (App-only sweep). The
underlying endpoint is `GET/PUT /api/config/auto-merge` (owner-only), whose
response carries `bot_authors: [{login, source: known|discovered|custom,
trusted}]`.

## Usage

```sh
hive-merge --repo <owner/repo> --number <N> [--method squash|merge|rebase] \
           [--expect-sha <sha>] [--update-branch]
```

| Flag | Required | Default |
| --- | :---: | --- |
| `--repo` | yes | — |
| `--number` (alias `--pr`) | yes | — |
| `--method` | no | `squash` |
| `--expect-sha` | no | auto-resolved (see below) |
| `--update-branch` | no | off |

Both `--flag value` and `--flag=value` forms work. `--squash`, `--merge`, and
`--rebase` are accepted as method shorthands. `--admin` is accepted and
**ignored** — the hive never admin-bypasses branch protection.

`--repo` and `--number` must resolve, or the script exits `2`. `--number` must
be an integer, or the script exits `2`.

### `--expect-sha` is auto-resolved, not optional in effect

The merge-request watcher **requires** a non-empty `expect_sha` (the F4 TOCTOU
guard above) — a request with none is denied outright. Rather than push that
burden onto every call site, the script resolves the PR's *current* head SHA
itself when `--expect-sha` is not given, using the cached App token at
`/var/run/hive-metrics/gh-app-token.cache`, and pins it into the request. The
head is captured at request time, closing the TOCTOU window exactly as
intended, so existing call sites that never passed `--expect-sha` keep
working unchanged.

If the SHA cannot be resolved (no token cache, `gh` unavailable, or the
lookup fails), the script prints an error naming the token-cache path and
**exits `3`** rather than writing a request with no pinned head.

### `--update-branch`

When set, the watcher syncs the PR branch with its base before attempting the
merge (resolves the common "behind main" case). A failure to update the
branch is not fatal — the merge attempt still proceeds and surfaces the real
blocker.

## It is asynchronous, by design

On success the script prints the request path and returns `0`. **The PR has
not been merged yet at that point.** It merges on the next watcher tick
(polling every 10 seconds).

To confirm, poll the `.result.json` written next to the request file, or
simply check whether the PR is merged.

## Retries and what happens when a merge can't land

The watcher retries a failed merge attempt up to **3 attempts total**,
tracked across ticks via the prior `.result.json`. What happens after the
final attempt depends on why it failed:

- **A required check is failing or pending** (classified from GitHub's own
  branch-protection error text, e.g. "required status check ... has not
  succeeded") — the request is quarantined (renamed `.exhausted`), but the
  hive re-engages its fix loop for that PR instead of abandoning it, subject
  to a per-red-SHA re-dispatch cap.
- **The blocker is unfixable by pushing code** — a true merge conflict or a
  permission error (matched against GitHub's own wording, e.g. "merge
  conflict", "not accessible by integration", "403", "must be a member") —
  the request is quarantined (`.exhausted`) and not retried further; the
  result file records the last error.

An authorization denial (forge-resistance failure, `CanMerge` gate failure,
or F4 target-binding failure) is not retried at all — the request file is
renamed `.denied` immediately and the result file records the reason.

A malformed request file (invalid JSON) is renamed `.bad`.

## Where things live

| Path | What |
| --- | --- |
| `/var/run/hive-metrics/merge-requests` | request files the watcher consumes |
| `/var/run/hive-metrics/gh-app-token.cache` | cached App token, used to auto-resolve `--expect-sha` |
| `/var/run/hive/uid-map.json` | UID → agent-name map, used for a nicer log line |

The UID map is **informational only** here. The watcher re-derives ownership
from the file's UID regardless.

## Related

- [`hive-open-pr`](hive-open-pr.md) — the equivalent relay for opening a PR;
  same request-file mechanism and authorship model
- [Security threat model](security-threat-model.md) — forge resistance and the
  UID-ownership anchor
- [Agent configuration](agent-configuration.md) — ACMM levels and the
  merge gate (`CanMerge`, `ModeIssuesPRsMerge`) that governs whether an agent
  may merge a PR at all
- [Audit log](audit-log.md) — merges are recorded there with the requesting
  agent
