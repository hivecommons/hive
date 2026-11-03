# `hive-open-issue` — create an issue, comment, claim, or label as the App bot

`bin/hive-open-issue.sh` is how an agent creates an issue, posts a comment, or
claims an issue. Agents call it **instead of `gh issue create` /
`gh issue comment`**.

It does not perform the GitHub write itself. It writes a request file that the
hive's issue-request watcher executes with the App installation token —
server-side, retried with backoff, and deduplicated against open issues by
title — exact match first, canonical subject as the fallback (see
[Idempotency](#idempotency)).

## Why it exists

The direct path rode the agent's own shell tool, and that path lost work
silently. Root-caused live on 2026-08-21 on a hosted hive: the sec-check
agent's `gh issue create` timed out mid-flight — repeatedly — and the finding
it was recording survived only as a bead, not as the intended GitHub issue.
One GHE secondary-rate-limit stall, a network blip, or a mangled multiline
command was enough to lose a finding with no visible failure.

Routing through this script makes the agent's job "record the request" — a
local file write that takes milliseconds and cannot fail on network — and the
hive owns delivery: retried, backed off, and immune to the agent's own shell
timing out.

**Authorization + forge resistance.** The script runs **as the agent**, in
that agent's tmux session under that agent's UID, so the request file it
writes is owned by that UID. The watcher re-derives the requesting agent from
the **file's owner**, not from anything written inside the file. The watcher
enforces the same per-agent mode gate (`CanCreateIssues`, mode ≥
`ISSUES_ONLY`) and UID forge-resistance as the direct `gh` path would need —
this shim **adds no privilege**. The same `CanCreateIssues` gate covers all
kinds (issue, comment, claim, close, label, request-review): commenting,
claiming, labeling and requesting review are all issue-writes under the same
tier.

## Usage

Shapes are selected by an optional leading positional keyword (`comment`,
`claim`, `close`, `label` or `request-review`; the default with no keyword is
`issue`):

```sh
hive-open-issue --repo <owner/repo> --title "<t>" [--body "<b>"|--body-file f] [--label a,b] [--parent <n>] [--blocked-by <n[,n]>] [--needs-decision] [--no-consolidate <reason>] [--step-kind release --release-tag <tag> --release-target <sha>]
hive-open-issue comment --repo <owner/repo> <number|url> --body "<b>" [--needs-decision] [--blocked-on-pr <n>]
hive-open-issue claim   --repo <owner/repo> <number|url>
hive-open-issue label   --repo <owner/repo> <number|url> [--label a,b] [--remove-label c]
hive-open-issue request-review --repo <owner/repo> <number|url> [--reviewer a,b] [--team-reviewer t]
```

| Flag | Aliases | Applies to | Notes |
| --- | --- | --- | --- |
| `--repo` | `-R` | all | required |
| `--title` | `-t` | issue | required for `issue`; must start with `[<your agent name>]` (`[scanner] …`) — the watcher and the proxy's `gh issue create` path both add the tag when it is missing and never double it |
| `--body` | `-b` | issue, comment | required for `issue` and `comment`; not used by `claim` |
| `--body-file` | `-F` | issue, comment | reads body from a file; `-` reads stdin |
| `--label` | `-l`, `--add-label` | issue, label | repeatable; comma-separated values are split |
| `--remove-label` | — | label | repeatable; labels to take off the item |
| `--reviewer` | `--add-reviewer` | request-review | repeatable; comma-separated GitHub logins to ask for a review |
| `--team-reviewer` | — | request-review | repeatable; team slugs (an `org/` prefix is accepted) to ask for a review |
| `--parent` | — | issue | issue number in the same repo to link the new issue to as a GitHub sub-issue |
| `--blocked-by` | — | issue | repeatable; issue numbers in the same repo the new issue is blocked by, recorded as GitHub "blocked by" dependencies (`#` prefix tolerated) |
| `--step-kind` | — | issue, comment | `release`; requires `--needs-decision`, `--release-tag` and `--release-target`; records metadata only, never publishes a release |
| `--release-tag` | — | issue, comment | release tag from the repository's version source |
| `--release-target` | — | issue, comment | full lowercase 40-character commit SHA for the proposed release |
| `--needs-decision` | — | issue, comment | park the new issue, or the existing issue being commented on, on a maintainer decision: the watcher applies the hive's configured needs-decision label itself (see [Parking an issue that needs a decision](#parking-an-issue-that-needs-a-decision)). On both subcommands a Hive-authored non-risk question is downgraded instead of parked under `project.agent_issue_decision_policy: resolve` (the default); a typed `--step-kind` owner step always parks |
| `--no-consolidate` | — | issue | requires a non-empty reason; the watcher skips open-issue consolidation (same-subject, file-set and component folds) and files a new issue, echoing the reason as `consolidation_skipped_reason` in the result and logging it. Use it for unrelated work that only shares a file or component with an open issue ([#14360](https://github.com/hivecommons/hive/issues/14360)). The request carries `"no_consolidate_reason"` |
| `--blocked-on-pr` | — | comment | park the issue on open PR `<n>` in the same repo: the watcher prefixes the note with `<!-- hive:blocked-on pr=owner/repo#n -->`, strips `@`-mentions from it, adds the `blocked` label and removes `help wanted` (see [Blocked on an unmerged PR](#blocked-on-an-unmerged-pr)) |
| `--number` | — | comment, claim, close, label, request-review | the issue/PR number; a bare positional number or a `.../issues/N` or `.../pull/N` URL is also accepted |
| `--dry-run` | `-n` | all | validate the arguments and print the exact request that would be written, then exit `0` **without writing it** — nothing is created, commented, claimed, or closed. **This is how you test the tool.** A live call creates a real GitHub issue/comment/label change under the hive's App identity |

### `--repo` is validated before anything is written

Every shape checks `--repo` first, and a refusal exits `2` with the reason and
writes **nothing** — including under `--dry-run`:

- it must be `owner/name` (letters, digits, `-`, `_`, `.`), so `<owner>/<repo>`,
  `just-a-name` and `a/b/c` are refused;
- it must not be a documentation placeholder — `owner/repo`, `org/repo`,
  `OWNER/REPO`, `example/…`, `your-org/…`, `my-org/…`, `acme/…`, `…/my-repo`,
  `…/placeholder` and the like (case-insensitive); the one exception is a name
  the hive has literally configured as a repo, which is then real whatever it
  is called;
- when `$HIVE_REPOS` is set (every agent sandbox exports it, comma-separated),
  it must be one of the repos listed there — the hive's configured repo set.

The watcher re-applies the same three checks server-side (an unqualified
`owner/name`, a placeholder, or a repo outside the hive's primary + fleet set
is quarantined `.failed` on the first tick, never retried), so a request that
bypasses the script cannot loop either. Why this exists: on 2026-10-22 three
agents "smoke-tested" the relay with `--repo owner/repo --title t --body b`.
Eleven identical requests landed in the queue, the first create 404ed, the
effect ledger recorded the operation as *Unknown*, and because all eleven
shared one content-derived idempotency key every retry for three hours failed
with "operation must be reconciled against authoritative external state"
until an operator quarantined them by hand. Use `--dry-run` to test; name your
real repo.

Both `--flag value` and `--flag=value` forms work. Flags `gh` accepts but this
path does not need — `--assignee`/`-a`, `--milestone`/`-m`, `--project`/`-p`,
`--template`/`-T`, `--web`/`-w`, `--editor`/`-e` — are **accepted and
ignored** (the value-taking ones correctly consume their following argument so
it isn't misread as the issue number). **Every other flag is refused** with
exit `2` and an `unsupported flag` error naming it; nothing is written. The
shim used to drop unknown flags silently, and an agent probing its access with
`gh issue create --dry-run` filed a real issue whose body was `placeholder` —
which its create-only token could not then edit, comment on, or close
([#7400](https://github.com/hivecommons/hive/issues/7400)). A flag whose whole
purpose may be to *prevent* a write must never be ignored on the way to a
write the agent cannot undo, so the parser now fails closed on anything it
does not understand.

To retract an issue you filed by mistake, use `hive-open-issue close --repo
<owner/repo> <number>` — it goes through the watcher with the App token, so it
works even when the agent's own scoped token cannot edit or comment.

### `issue` (default)

`--repo`, `--title`, and `--body` are all required — an empty body is treated
as an agent bug, not a valid issue, and the script exits `2` rather than
letting the watcher quarantine it later. This is a deliberate, pinned contract
(`bin/test_hive_open_issue.sh`), not an oversight. `--label` may be repeated;
labels are always sent as a JSON array (empty if none given).

`--parent <n>` links the new issue as a GitHub sub-issue of issue `n` in the
same repo, via the sub-issues REST API
(`POST /repos/{owner}/{repo}/issues/{n}/sub_issues`), after the watcher
creates it. This is how a split-out child issue gets a real sub-issue link and
shows up in the parent's sub-issue list and completion progress bar, instead
of only the plain-text "Part of #`n`" line agents are asked to keep in the
body regardless
([#9435](https://github.com/hivecommons/hive/issues/9435)). A failed link
(parent missing, GitHub's sub-issue cap reached, a transient API error, …)
never stops the child issue from being created; the failure is logged and
recorded on the request's result file (`parent_link_error`) instead. The
watcher also appends a `<!-- hive:parent n -->` marker to the child body, so
the relation survives a failed link. Once every child of `n` is closed the
issue stage sweep closes `n` as completed, and while any child is open `n`
is never marked `stage/stalled`
([#12857](https://github.com/hivecommons/hive/issues/12857); see
[labels-and-control-signals.md](labels-and-control-signals/stage-labels.md#decomposed-parents--closed-by-their-children-never-stalled)).

`--blocked-by <n[,n]>` records that the new issue must wait for issue(s) `n`
in the same repo, as GitHub "blocked by" dependencies
(`POST /repos/{owner}/{repo}/issues/{new}/dependencies/blocked_by`), after the
watcher creates it. When an agent splits an issue into steps that have an
order — a schema change, then the code that reads it, then the docs — each
later child names the one(s) before it so the order is recorded where GitHub
and the hive both read it, not only in prose
([#9839](https://github.com/hivecommons/hive/issues/9839)). The hive reads the
same links back on every scan: an issue with an open blocker is kept out of
the actionable list an agent picks from and named in a short footer with its
blockers instead; the moment the blocker closes it is offered again with no
further action. Linking is best-effort like `--parent`: a blocker that cannot
be resolved is reported on the result file (`blocked_by_errors`, alongside
`blocked_by_linked`) and never stops the issue from being created. A mutual
block (A blocked by B, B blocked by A) would hide both forever, so the hive
drops that pair with a warning and offers both.

#### Parking an issue that needs a decision

`--needs-decision` is the way an agent parks an issue it files or, on `comment`, an existing issue. Hive-authored issues must not use it for sequencing, naming, numbering, config-shape, scope, or "approach A vs B" engineering questions: with `project.agent_issue_decision_policy: resolve` (default), the relay records `Decision taken by hive: …`, keeps the issue actionable, and audits the downgrade. Use it only when the body asks the maintainer to decide a risk category, and include `Risk: <category>` for security posture, data loss, irreversible or public-facing behaviour, governance/policy/licensing, spend, owner-only credentials/secrets/infra, or uninferable human intent. The script sends `"needs_decision": true` on the request, and
the watcher adds the hive's configured needs-decision label
(`project.issue_filter.hard_suppress_labels.needs_decision`, first entry;
`stage/045-needs-decision` by default — the retired `needs-decision` spelling
and the PR-only `stage/085-needs-decision` are upgraded, [#12683](https://github.com/hivecommons/hive/issues/12683)) to the
create itself, whatever labels the agent named
([#11215](https://github.com/hivecommons/hive/issues/11215)). The issue is then
parked like any other `stage/045-needs-decision` issue: it stays out of the
actionable queue, the un-park sweep posts the "What to reply" notice, and the
maintainer answers with `/hive approve` or `/hive decision` (see
[maintainer-commands.md](maintainer-commands.md)). Every lane policy that files
issues tells the agent to pass the flag rather than name the label, so parking
no longer depends on which lane filed the issue or on the label's name in this
hive.

On `comment` the same flag parks an **existing** issue
([#12329](https://github.com/hivecommons/hive/issues/12329)). Use it when a
diagnosis ends in a step only the owner can take (repository settings,
rulesets, secrets, app installs, billing, anything agents are barred from):
the comment names the exact command or step, and the watcher adds the
configured needs-decision label before posting it, so the issue leaves the
agent queue and reaches the owner's "Your turn" inbox instead of falling back
to `Unclaimed` when the claim expires. Because the park applies a
hive-controlled label, the lane needs `label` in its write allowlist as well
as `comment`, and the target must be an issue: a pull request is refused
before any write (a PR that needs a person is a `requires_human` review
verdict, not a needs-decision label). The watcher tags such a comment, and the
body of a new issue filed with the flag, with a hidden `<!-- hive:owner-step -->`
marker; while it is the newest owner step not yet answered by an accepted
`/hive` command and lists no lettered options, the "What to reply" notice
offers `/hive done` so the owner can say the step is done. `/hive done` clears
the park labels and adds `approved-direction` like `/hive approve`; the reply
Hive posts records the done decision, and that reply is what tells the lane
that next picks the issue up to re-check the step and close the issue once the
fix is verified — nothing else routes it. The flag applies only to `issue` and
`comment`; on any other kind the script exits `2` and writes nothing.

#### Recording a release-only owner step

When the only remaining step is a release, add `--step-kind release`
(a typed owner step is never downgraded by `agent_issue_decision_policy`,
whatever the wording),
`--release-tag <tag>` and `--release-target <full-commit-sha>` to an `issue`
or `comment` request with `--needs-decision`:

```bash
hive-open-issue comment --repo owner/repo 736 --body "Remaining step: release v0.12.0." \
  --needs-decision --step-kind release --release-tag v0.12.0 \
  --release-target aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
```

Replace the example SHA with the repository's actual commit ID. Read the tag
from the repository's version source (for example `VERSION`) and resolve the
target commit on its default branch, rather than inferring either from issue
prose. Targets must be full, lowercase 40-character SHAs; moving branch names,
abbreviations, invalid tags, unknown kinds and incomplete metadata are refused.
All three flags also accept `--flag=value`. The watcher validates direct JSON
requests too, before making any GitHub API call.

The request carries an `owner_step` object with `kind`, `tag` and `target`.
The watcher appends the canonical marker:

```html
<!-- hive:owner-step kind=release tag=v0.12.0 target=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa -->
```

Supply metadata via the flags, not an additional marker in the body. Legacy
untyped markers remain supported. Only the newest unanswered Hive-authored
owner-step comment (or the initial issue body) supplies the proposal; a marker
in another commenter's text is ignored. Lettered choices remain choices, not
release actions.

While that typed release step is unanswered, a verified-open issue behind a
merged PR remains parked in "Your turn" instead of returning to agent lanes as
generic remainder work. An accepted `/hive done`, `/hive decision` or
`/hive approve` answer restores the normal merged-claim behavior.

**This metadata is a proposal, not authorization by itself.** Hive parks the
issue and offers `/hive done` and `/hive decision` as before. With the default
`project.repo_policies[].release_on_approve: false`, `/hive approve` retains its
existing direction-only behavior and publishes nothing.

When that per-repo opt-in is `true`, an eligible maintainer's `/hive approve`
authorizes the hub App—not an agent—to create the exact proposed tag and GitHub
Release. The hub re-reads the default branch, tag, and required checks first. It
accepts only a target that is still the branch head or an ancestor of it. If
`release_version_source` names a repository-relative text file, the proposed tag
must match that file's value at the target commit (an optional leading `v` is
ignored). Success posts the release URL, records an audit event, and clears the
park. A failed precondition posts a specific refusal and leaves the issue
parked. Answer markers and the absent-tag precondition prevent duplicate
release creation. The agent `gh release create` gate remains unchanged.

Every lane that can comment or file issues carries this rule in its policy (ci-maintainer, sec-check, quality, architect, outreach, strategist, guide and scanner), including the ADR-0019 `needs-direction` escalation and workflow-file hand-offs. Lanes that only record beads (advisory) note the owner-only step in the bead instead.

### `comment`

`--repo`, a number or URL, and `--body` are all required, or the script exits
`2`.

#### Blocked on an unmerged PR

When the code an issue names exists only in an open PR — a review-backlog
finding whose evidence file is absent on the base branch, or a triage that
concludes "blocked on #N (not starting)" — there is nothing to ship until that
PR lands ([#13093](https://github.com/hivecommons/hive/issues/13093)). Post
one neutral note with `--blocked-on-pr <n>`:

```sh
hive-open-issue comment --repo o/r 13093 --blocked-on-pr 13092 \
  --body "Blocked on #13092: the flagged file exists only on that PR; nothing to ship until it lands."
```

The watcher prefixes the body with the marker `<!-- hive:blocked-on
pr=o/r#13092 -->`, strips any `@login` mention from it (the PR's author is not
asked to do the hive's work; address them only in a review comment on their
own PR), adds the `blocked` label and removes `help wanted`. A `blocked` issue
is filed under the enumeration's filtered bucket, derives no stage label,
and every automated claim producer — governor kicks, the contributor relay,
`hive-open-issue claim` — is refused on it (`blocked: o/r#13092`). The
blocked sweep that follows each stage-label sync lifts the label and posts
one `<!-- hive:unblocked -->` note once the tracked PR is merged or closed,
and strips `help wanted` again should the org workflow re-add it while the
PR is open. The review backlog applies the same parking itself when it files
a finding whose file does not exist on the reviewed PR's base branch.

A posted comment is the agent's **start signal** on that issue only when it
says work is starting. The hub records a 2h agent claim the first time the
agent's own `claim` request, a `hive-open-pr --issues` request, or a comment
with an *explicit start statement* names an issue a live kick listed to it,
so a second agent is not kicked onto the same work
([#10527](https://github.com/hivecommons/hive/issues/10527)). A comment states a
start when it carries the structured marker `<!-- hive:start -->` (anywhere in
the body, attributes allowed), says "starting now" / "started now" /
"resuming now" / "implementing now" / "working on it now" / "picking this up
now" / "no longer blocked", or opens (after markdown and an optional
`Status:`/`Update:` label) with "Starting", "Started", "Resuming",
"Implementing", "Working on", "Picking this up" or "Taking this". A negation
("not starting now") does not count. Every other comment is posted and
records **no claim**: a triage verdict, a verification result, a gate check,
a status report, a progress note that does not say work begins, an explicit
*deferral* ("Deferred …", "Blocked by #N", "Waiting on #N", "picks up once
…", [#12387](https://github.com/hivecommons/hive/issues/12387)) or an
*escalation* (`needs-decision (scanner): …`, "Parking this …"). A start phrase
or the marker overrides a deferral or escalation in the same comment
("Picking this up after #N merged, starting now."). `label` requests are
never start signals — the labels an agent may add through the relay are
triage and verification verdicts — and `claim` requests always are. The
scanner's "Gate check … no implementation started" on
[#13011](https://github.com/hivecommons/hive/issues/13011) and its
`hive/verified-open` on the epic
[#12999](https://github.com/hivecommons/hive/issues/12999) were taken as
starts when any comment or label counted; neither is now.
The claim applies the `hive/claimed-by-<agent>` label and posts a bot
comment with a timestamped marker so maintainers can see when work began.

### `claim`

`--repo` and a number or URL are required; no body or title needed. A claim
records that this agent is starting work on an issue. Because App bots cannot
be GitHub assignees, the watcher applies a `hive/claimed-by-<agent>` label
instead — the visible, auditable ownership signal — and audits it as
`agent_issue_claimed`. The same label call adds `stage/030-pr-in-progress` (formerly `hive/in-progress`), and the
watcher posts one bot comment carrying a timestamped start marker
(`<!-- hive:claim who=<agent> kind=agent hive=<spoke> at=<RFC3339>
until=<at+agent TTL> -->`) so maintainers can see exactly when an agent
began a PR for the issue ([#11841](https://github.com/hivecommons/hive/issues/11841)). Every kick
instructs the agent to claim before touching an issue; while the marker is
live every hive that scans the repo withholds the issue from kicks. The hold
ends early when the hive relaunches the agent's CLI (hive boot, a restart):
the context that claimed is gone, so the ledger releases the agent's claims
and posts a `<!-- hive:release who=<agent> kind=agent reason=… at=<RFC3339> -->`
comment that readers of the 🔒 marker honour, instead of parking the issue at
`stage/020-claimed` until `until=`. The claim
is refused (exit non-zero, reason in the result) on a pull request, on an
issue labelled `needs-human` or held, on an issue parked on a person's
decision (`stage/045-needs-decision`, `needs-spec`, the retired decision
labels), and on an umbrella issue wearing a never-claim label
(`governor.claims.never_claim_labels`, default `epic`, case-insensitive):
an epic is the list of work, not the work. The same gate admits every
automated claim producer, so a start signal on such an issue records nothing. The
`stage/030-pr-in-progress` label is swapped for `stage/040-covered-by-pr` when
the agent's `hive-open-pr --issues` request opens the PR, and cleared when the
claim is released or at enumeration once PR evidence exists or the marker
lapsed (default 2h).

### `label`

`--repo`, a number or URL, and at least one `--label` or `--remove-label` are
required. Labels are added first, then removed, and the change is audited as
`agent_label_applied` with the repository and number
([#9587](https://github.com/hivecommons/hive/issues/9587)).

Hive-controlled labels are refused in both directions: the merge-queue label
(`lgtm`, or whatever the hive renamed it to), the hold labels, anything in the
`hive/` namespace, and the labels that record a human decision
(`approved-direction`, `design-approved`, `needs-human`, `blocked`, the
retired `needs-decision` / `needs-direction`, and every `stage/*` label
except the parks — `stage/045-needs-decision` on an issue,
`stage/085-needs-decision` on a PR — which an agent may *add* to park an item on
a maintainer ruling — never remove, [#12683](https://github.com/hivecommons/hive/issues/12683)). With the default issue decision policy, Hive-authored non-risk engineering parks are rewritten to `stage/010-actionable` with a note instead; risk parks should include `Risk: <category>`. They are inputs to hive automation or records of someone's
verdict, so an agent may not set them through this operation; one reserved
label refuses the whole request. Use `hive-open-issue claim` to record
ownership. This refusal applies only to the `label` operation on an existing
item: to park an issue you are filing, pass `--needs-decision` to the create,
and to park an existing issue pass it to `comment`
(see [Parking an issue that needs a decision](#parking-an-issue-that-needs-a-decision)).
See [github-write-surface.md](github-write-surface.md).

### `request-review`

`--repo`, a PR number or URL, and at least one `--reviewer` or
`--team-reviewer` are required. The hive asks those users and teams to review
the PR and audits it as `agent_review_requested` with the repository and number
([#9587](https://github.com/hivecommons/hive/issues/9587)). It is the audited
alternative to `gh pr edit --add-reviewer`. Logins and team slugs are checked
before any GitHub call; an impossible name, or more than 15 reviewers in total,
is quarantined as malformed rather than retried.

## It is asynchronous, by design

On success the script prints the request path and returns `0`. **The
issue/comment/claim has not happened yet at that point.** It executes on the
next watcher tick (polling every 10 seconds); poll the `.result.json` written
next to the request file for the resulting number/URL.

## Retries and what happens when a request can't be fulfilled

Unlike the merge and PR-open watchers' fixed attempt caps, the issue-request
watcher backs off **exponentially per request**: starting at 30 seconds and
doubling up to a 15-minute ceiling. A request that still hasn't succeeded
after **24 hours** is given up on and quarantined (renamed `.failed`), so a
persistently failing request cannot hammer the forge indefinitely and the
queue directory cannot grow without bound.

Not every failure is worth retrying, so the watcher classifies the error
first (`src/pkg/github/issue_request_errors.go`):

| Class | Errors | What the watcher does |
|---|---|---|
| **Permanent** | `--repo` not `owner/name`; a documentation placeholder (`owner/repo`, `org/repo`, `example/…`, …); a repo outside the hive's configured set; GitHub `404`/`410` (repo or issue does not exist or the App is not installed there); `422` validation; `401`; `403` that is **not** a rate limit | Quarantined **immediately** (renamed `.failed`, `.result.json` written next to it, one `ERROR` log naming the `.failed` path). No retry: the next tick cannot change any of these. |
| **Transient** | `5xx`, network/timeouts, primary or secondary rate limits (`429`, `403` rate-limit), effect-ledger *Unknown* (`must be reconciled against authoritative external state`) | Retried with the exponential backoff above. For *Unknown* the watcher first **reconciles**: it re-lists the repo's open issues for one with the same title/origin marker and settles the ledger entry as applied (returns that issue) or not applied (creates). If the lookup itself keeps failing, an entry older than **1 hour** is expired so the create can proceed; after **3** consecutive reconciliation failures the request is given up. |

Two further give-up rules shorten the 24-hour horizon for `kind: issue`
creates: **3** consecutive `must be reconciled` failures, or **8** consecutive
failures with the *identical* error string (the error text is not changing,
so neither is the outcome) — either quarantines the request `.failed` with an
`ERROR` log naming the file.

**Where to look.** Quarantined requests sit in
`/var/run/hive-metrics/issue-requests/` on the hive host as
`<agent>-<nonce>.json.failed` (gave up / permanent error), `.bad` (malformed)
or `.denied` (authorization), each with a `<agent>-<nonce>.result.json`
carrying `ok: false` and the final `error`. Grep the hive log for
`issue-request watcher: request cannot succeed, quarantined as .failed` to
find them; the `ERROR` line names the path, the agent and the reason. Delete the `.failed` file once understood, or rename
it back to `.json` to retry after fixing the cause (for a placeholder repo
there is nothing to retry — tell the agent to use `--dry-run`).

A request that is structurally invalid — missing required fields for its
kind, or an unrecognized kind — is rejected before authorization or any API
call and quarantined immediately (renamed `.bad`); it is never retried, since
no amount of retrying changes a shape that can never succeed. Likewise,
invalid JSON in the request file is quarantined `.bad`. An authorization
denial (forge-resistance failure or `CanCreateIssues` gate failure) is
quarantined `.denied` immediately, also without retry — policy won't change
on the next tick.

### Content shape validation

Structural completeness is not the only terminal check. The same pre-flight
validates the **content** of issue and comment requests, using the shared
validators in `pkg/issueshape` (see
`src/pkg/github/issue_request_watcher.go`):

- **Unsubstituted template placeholders.** A title or body still carrying an
  unfilled policy-template token — `<analysis>`, `<fix>`, or a multi-word
  lowercase phrase like `<specific description of the documentation gap>` —
  is rejected: an agent that copied a template line without substituting it
  has nothing worth filing. The matcher is deliberately narrow so legitimate
  angle-bracket constructs still pass: URLs, generic type parameters
  (`Result<T, E>`), tokens containing `/`, `=`, or `,`, recognized
  GitHub-flavored-Markdown HTML tags including attribute forms
  (`<details open>`, `<img src=…>`), and ordinary prose containing comparison
  operators — *"holds when a < b and c > d"* spans `< b and c >`, which is not
  a placeholder because a real one is written tight, with no space inside the
  brackets.
- **Mis-escaped newlines.** A body that is a single physical line whose
  markdown structure was encoded as literal `\n` escape sequences (the
  classic `## X\n\n…\n\n## Y` specimen, typically from shell-quoting a
  multiline `--body`) is rejected with the hint *use real newlines or
  `--body-file`*. A body with real line breaks may freely discuss `\n` in
  prose or code blocks; the check requires the combination — no real
  newline, several literal `\n` tokens, `\n\n` or `\n## ` structure —
  before flagging.

Both rejections are terminal like any other shape failure: the request is
quarantined `.bad`, never retried, and the `.result.json` carries the exact
reason including the offending placeholder token.

**The direct path is covered too.** An agent's raw `gh issue create` /
`gh issue comment` never touches this watcher — it becomes a REST
`POST`/`PATCH` through the hive's egress proxy. Since
[#7020](https://github.com/hivecommons/hive/pull/7020) (fixing
[#7014](https://github.com/hivecommons/hive/issues/7014)) the proxy enforces
the same two validators on issue and comment create/edit routes, denying the
request with the same actionable reason before it leaves the sandbox.
`pkg/issueshape` is the single source of truth for those two enforcement
points, so their rules cannot drift apart. One asymmetry to know about: a body
larger than the proxy's buffering limit is forwarded unchecked rather than
truncated — the watcher path has no such bypass.

### A third enforcement point, with a stricter title rule

`CreateIssue` — the hive's own programmatic creation path in
`src/pkg/github/issue_request_watcher.go` — applies a **separate** guard,
`validateIssueTemplateFilled` in `src/pkg/github/issue_template_guard.go`,
added for [#7153](https://github.com/hivecommons/hive/issues/7153) and
tightened for [#7141](https://github.com/hivecommons/hive/issues/7141). It runs
alongside `validateRepoRef`, before the dedupe and rejected-twin gates, and
like them it **fails closed**.

It enforces the same two ideas as `pkg/issueshape` but is implemented
independently, with its own regexes and its own HTML-element skip list. The
behavioural difference worth knowing is that it **holds a title to a stricter
standard than a body**:

| span | in a **title** | in a **body** |
| --- | --- | --- |
| `<specific description of the gap>` | refused | refused |
| `<analysis>`, `<fix>` | refused | refused (via the shared `pkg/issueshape` rule) |
| `<foobar>` (any single lowercase word, 3+ chars) | **refused** | **allowed** |
| `<br>`, `<td>`, `<details>` (known HTML elements) | allowed | allowed |
| `<t>` (under 3 chars) | allowed | allowed |

The asymmetry is deliberate. A GitHub issue **title is plain text** — it
renders no markup, carries no autolinks and holds no code — so a bare
`<foobar>` there is almost certainly an unsubstituted placeholder. A **body**
is Markdown, where a lone angle-bracket token is routinely legitimate, so the
body rule stays conservative and requires two or more lowercase words. The
residual risk on the title side is a title that genuinely names a tag
(*"support `<html>` tags"*); the refusal names the exact span it objected to
and rewording is cheap.

> [!IMPORTANT]
> Because this third point is implemented separately rather than wholly on
> `pkg/issueshape`, the claim above that "the rules cannot drift apart" covers
> the watcher and proxy in full, and `CreateIssue` only in part.
>
> The body rule here now **delegates to `pkg/issueshape` first** and falls back
> to its own multi-word rule, so it can never be laxer than the other two
> points. That closed a real gap: `pkg/issueshape` refuses the bare tokens
> `<analysis>` and `<fix>` anywhere, the local multi-word rule cannot match a
> single word, and so before the delegation a body of exactly the
> [#7141](https://github.com/hivecommons/hive/issues/7141) shape went unchecked
> on this path — only its title stopped it.
>
> Two differences remain, both narrow:
>
> 1. The title rule has no counterpart in `pkg/issueshape`; it lives only
>    here.
> 2. The mis-escaped-newline check here requires only *no real newline plus
>    two or more literal `\n`*, without `pkg/issueshape`'s additional
>    `\n\n` / `\n## ` structure requirement — so this path is the stricter of
>    the two.
>
> Moving the title rule onto `pkg/issueshape` as a second exported validator
> would finish the job. Until then, **a change to the title rule or to the
> escape check must be considered against both implementations.**

### Idempotency

Issue creation is deduplicated by title against open issues in the target
repo (scanning up to the 3 most recent pages), in two tiers
([#6927](https://github.com/hivecommons/hive/issues/6927)):

1. **Exact match** — an open issue whose whitespace-trimmed title is
   identical is always preferred and reused.
2. **Canonical-subject match** — otherwise, titles are compared after
   stripping any trailing free-text qualifier (everything from a ` — `,
   ` – `, or ` -- ` separator to the end) and normalising case and
   whitespace. Agents append a model-authored qualifier to an otherwise
   stable subject and reword it on every scan (`…(1237 lines) — extract
   cache helpers module`, `…(1237 lines) — extract snapshot/report
   helpers`), so before #6927 the same finding was re-filed every cycle —
   one file accumulated six simultaneously-open issues. When several open
   issues share the canonical subject, the **oldest** is reused, so repeats
   consolidate onto the original rather than the newest copy.

A canonical subject is only trusted as a dedupe key when enough of the title
survives stripping (at least 20 characters and 3 words —
`src/pkg/github/issue_dedupe.go`); short or generic stems such as
`[operations] CI failure` fall back to exact-match-only, so they cannot
collapse unrelated findings.

If a matching open issue already exists, the watcher reuses it instead of
creating a duplicate — this is what makes the retry loop safe: a create that
actually succeeded server-side but crashed before the request file was
consumed (or an agent-side "timed out but maybe it worked" ambiguity) never
produces a second issue. The result file's `already_existed` field reports
which case happened.

### Rejected findings are not re-filed

Open-issue title dedupe cannot stop the second failure mode observed live
([#6463](https://github.com/hivecommons/hive/issues/6463)): a maintainer
closes an agent-filed finding as **not planned**, the closed issue leaves the
agent's field of view (agents may not list issues), and on the next kick the
same finding is rediscovered, *reworded*, and filed again — one false positive
was filed five times in four days, each closure re-litigated by a human.

Before creating, the watcher therefore also scans **recently closed** issues in
the target repo that were filed by this hive's own App bot and closed as
`not_planned` or `duplicate` within the last 30 days. The identity key is the
**file-reference set**: the set of file paths named in title + body, with line
numbers stripped (line numbers drift with unrelated commits; the files are the
finding's actual subject and survive any rewording). When the pending request
names exactly the same file set, the create is refused **terminally** — the
request is consumed, never retried, and the result file carries
`rejected_duplicate: true` with the closed issue's number and URL so the agent
reads the maintainer's rebuttal instead of arguing with it. The refusal is
recorded in the audit log as `agent_issue_rejected_duplicate`.

The gate fails toward filing on every uncertainty: a failed lookup, an empty
file set on either side, any difference between the sets (exact equality, not
overlap — a genuine new defect involving one more file files normally), a
`completed` closure (that means *fixed*, and a re-report may be a real
regression), a rejection older than 30 days, or a hive with no App-bot
identity all fall through to a normal create.

### Feature requests that already exist are not filed

Rejection dedupe only catches a finding a maintainer has already turned down.
The fourth failure mode ([#13879](https://github.com/hivecommons/hive/issues/13879))
is an agent asking for something the hive *already has*: a setting, a merged
PR, a doc page, a knowledge bead. A person who files such an issue is parked
on `stage/017-possible-existing` and asked to confirm; a hive agent gets no
such round trip, so the watcher applies the hive-agent filing rule
(`priorart.FilingVerdictFor`) **before** creating, with the same searcher and
`runs.triage.prior_art` settings the park uses:

- **Feature kinds only.** The rule judges requests whose labels (or a leading
  `[RFC]` / `RFC:` / `Epic:` title tag) name one of `runs.triage.prior_art.kinds`
  (default `feature`, `rfc`, `epic`, `enhancement`). A request carrying a bug
  label is never judged, whatever else it carries.
- **At or above the threshold** (`runs.triage.prior_art.threshold`, default
  `70`) the create is **refused**: the error names the top match and a
  `decision` bead is written to the requesting agent's bead store (the
  scanner's, then the supervisor's, then any store when the agent has none)
  so the next scan finds the refusal instead of rediscovering the request.
  The verdict depends only on the match confidence: re-submitting the same
  request, with or without a gap section, is refused the same way. If the
  existing item genuinely lacks something, that gap is a *different*,
  narrower request.
- **Within 20 points under the threshold** the request is filed **only if its
  body carries a `## Gap vs existing` heading** (markdown heading or bold line,
  any case) stating what the existing items do not cover; the watcher then
  appends a findings note listing the matches to the body. Without the
  heading the create is refused and the error carries a rendered template of
  the section to add before re-submitting.
- **Further under the threshold**, or with no findings at all, the request
  files unchanged.

Both refusals are **terminal** like the rejection gate: the request is
consumed, never retried, and the result file is `ok: false` with the reason in
`error`. There is no structured field for this class — the error text is the
contract. The refusal is recorded in the audit log as
`agent_issue_prior_art_refused` with the verdict (`do_not_file` or
`missing_gap_section`), the match's URL and kind, and the confidence and
threshold. Setting `runs.triage.prior_art.enabled: false` turns the rule off
along with the park; until the hive has built its prior-art searcher at boot,
requests file as before.

### Findings over the same files are consolidated

Title dedupe also misses the third failure mode
([#9376](https://github.com/hivecommons/hive/issues/9376)): agents file
findings one at a time and cannot see each other's, so two *different*
findings about the same file — two defects in one test file — became two
issues, two agents, two PRs editing the same lines, and a rebase for whichever
merged second.

The same open-issue scan that does title dedupe therefore also looks for an
**open** issue filed by this hive's App bot whose file-reference set (the key
described above) exactly equals the pending request's. When one exists, the
watcher posts the finding on it as a comment — headed with the shared file list
and a note to handle both in one PR — instead of creating a second issue. When
several match, the **oldest** is used. The result file carries
`consolidated: true` and `already_existed: true` with that issue's number and
URL, and the audit entry records `consolidated=true`. A failed comment keeps
the request queued for retry; it never falls back to filing the duplicate.

Consolidation fails toward filing exactly like the rejection gate: no App-bot
identity, an empty file set, a set that differs in any file, or a matching
issue filed by a human all create normally. It applies only to agent requests
through this watcher; hive-internal filings (fleet report, review backlog) keep
plain create semantics. An exact or canonical title match still wins and
reuses the issue without a comment, so a retried create never comments on the
issue it itself created.

### A stream of findings on one component is folded into a tracker

Exact file-set equality misses a *stream* of variants against one file
([#11239](https://github.com/hivecommons/hive/issues/11239)): a security lane
reporting one parser bypass per finding cites the shared file plus that
variant's own fixture or helper, so no two sets are equal and every variant
became its own issue and PR.

The same open-issue scan therefore also counts, per path, how many **open**
App-bot-filed issues cite each path the pending request cites. When at least
3 of them already cite one path, the finding is posted as a comment on the
**oldest** of them — the component's de facto tracker — instead of filed. The
comment starts with a `<!-- hive-finding-folded -->` marker, names the shared
path and how many open issues cite it, and asks for one structural fix over a
PR per variant. The result carries `consolidated: true` exactly like an
exact-set twin, and a failed comment keeps the request queued.

Paths that unrelated findings mention in passing (`README.md`,
`CHANGELOG.md`, `go.mod`, `package.json`, lock files, and similar manifests)
never count as a shared component. Build artefacts and generated output —
`dist/`, `node_modules/`, `vendor/`, any `coverage*/` directory, `lcov.info`
and other coverage reports, `*.min.js`, `*.map`, `*.pb.go`, lock files — are
dropped from the file-reference set altogether
([#12733](https://github.com/hivecommons/hive/issues/12733)): every coverage
finding on a bundled Action cites `dist/index.js`, so sharing it is not
evidence of a shared root cause, and a finding that cites only such paths has
no citation evidence at all. Below the threshold, overlapping findings file
normally; an exact file-set twin still takes precedence.

The tracker's subscribers are notified once, not once per variant: the second
and later folds **edit** the newest existing `<!-- hive-finding-folded -->`
comment on the tracker to append the new finding as another section (GitHub
does not notify on an edit). A new fold comment is posted only when none
exists or the existing one is near GitHub's body size limit.

Open issues alone rarely reach the threshold in steady state: each variant's
PR merges and closes its issue before the next variant is filed. So when at
least one open App-bot issue cites the path but the open count falls short,
App-bot issues on that path closed as **completed** within the last 14 days
count toward the threshold too (one extra, bounded list call: up to 2 pages
of 100 closed issues by the App bot, updated since the window start). The
fold target is still the oldest **open** sibling — a finding is never folded
into a closed issue — so if every sibling is closed the finding files
normally. Issues closed as `not_planned` or `duplicate` never count: they are
the rejected-twin gate's evidence, not a sign of a stream.

### Parked and other-lane issues never take a fold

An open issue parked on a maintainer decision (`stage/045-needs-decision`,
the hive's configured needs-decision label, or any other parking label) or
carrying a different `agent/<lane>` label than the requester's is not a fold
target ([#14360](https://github.com/hivecommons/hive/issues/14360)): an
outreach issue asking an admin to edit repository topics cites the files it
describes, but no PR closes it, so a code-change finding folded there is never
worked. Such an issue is skipped for the exact file-set twin and the
same-component fold, and does not count toward the component threshold; the
finding files normally unless an eligible sibling matches. An exact or
canonical title match is unaffected.

Every consolidated result also carries `consolidated_reason`: `subject` (a
canonical title match on a human-filed issue), `file_set` (an exact file-set
twin) or `component:<path>` (a same-component fold on `<path>`), so the agent
can tell a true twin from a shared-path fold.

### Unattended comments are rate-limited per issue

Every automated comment is a notification to the issue's subscribers, and a
tracker that collects folds, an agent re-posting its verification each kick
and the claim that follows added up to 47 comments on one downstream issue
([#12733](https://github.com/hivecommons/hive/issues/12733)). Three guards
bound that:

- **Verification markers are idempotent.** A `comment` request whose body
  carries a verification marker (`<!-- hive-<n>-verified -->`,
  `<!-- hive-verification: … -->`) is acknowledged with `already_existed:
  true` and not posted when that exact marker is already on the issue. The
  whole marker is the key, so a verification that names a new head SHA still
  posts.
- **A claim right after a verification is refused.** A `claim` request is
  denied (parked as `.denied`, with the reason in the result) when the issue's
  newest comment is a hive-bot verification less than 24h old — nothing has
  changed, so re-verifying is the loop, not new work. Any later comment lets
  the claim through.
- **Per-issue daily budget.** The relay's `comment` kind, finding folds,
  claim marks and release markers (including normal relay task completions)
  share a cap of 5 comments per issue per rolling 24h per
  hub process (`HIVE_ISSUE_COMMENT_BUDGET_PER_DAY`; 0 disables). A suppressed
  comment is logged (`per-issue bot comment budget reached`); a relay comment
  over budget is denied rather than retried, and a fold over budget still
  folds (the result is `consolidated: true`) but leaves no comment that day.
  A suppressed release marker leaves the original claim comment unchanged;
  this hive uses its authoritative ledger to reject its own stale claim marker,
  while other hives may still read the hold until its `until=` expires.
  Reviewer and operator-driven comments are not budgeted.

## Where things live

| Path | What |
| --- | --- |
| `/var/run/hive-metrics/issue-requests` | request files the watcher consumes |
| `/var/run/hive/uid-map.json` | UID → agent-name map, used for a nicer log line |

The UID map is **informational only** here. The watcher re-derives ownership
from the file's UID regardless.

## Related

- [`hive-open-pr`](hive-open-pr.md) — the equivalent relay for opening a PR;
  same request-file mechanism and authorship model
- [`hive-merge`](hive-merge.md) — the equivalent relay for merging a PR
- [Security threat model](security-threat-model.md) — forge resistance and the
  UID-ownership anchor
- [Agent configuration](agent-configuration.md) — ACMM levels and the
  `CanCreateIssues` gate that governs whether an agent may create issues,
  comment, or claim at all
- [Audit log](audit-log.md) — issue creation, comments, and claims are
  recorded there with the requesting agent
