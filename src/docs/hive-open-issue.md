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
hive-open-issue --repo <owner/repo> --title "<t>" [--body "<b>"|--body-file f] [--label a,b] [--parent <n>] [--blocked-by <n[,n]>] [--needs-decision]
hive-open-issue comment --repo <owner/repo> <number|url> --body "<b>"
hive-open-issue claim   --repo <owner/repo> <number|url>
hive-open-issue label   --repo <owner/repo> <number|url> [--label a,b] [--remove-label c]
hive-open-issue request-review --repo <owner/repo> <number|url> [--reviewer a,b] [--team-reviewer t]
```

| Flag | Aliases | Applies to | Notes |
| --- | --- | --- | --- |
| `--repo` | `-R` | all | required |
| `--title` | `-t` | issue | required for `issue` |
| `--body` | `-b` | issue, comment | required for `issue` and `comment`; not used by `claim` |
| `--body-file` | `-F` | issue, comment | reads body from a file; `-` reads stdin |
| `--label` | `-l`, `--add-label` | issue, label | repeatable; comma-separated values are split |
| `--remove-label` | — | label | repeatable; labels to take off the item |
| `--reviewer` | `--add-reviewer` | request-review | repeatable; comma-separated GitHub logins to ask for a review |
| `--team-reviewer` | — | request-review | repeatable; team slugs (an `org/` prefix is accepted) to ask for a review |
| `--parent` | — | issue | issue number in the same repo to link the new issue to as a GitHub sub-issue |
| `--blocked-by` | — | issue | repeatable; issue numbers in the same repo the new issue is blocked by, recorded as GitHub "blocked by" dependencies (`#` prefix tolerated) |
| `--needs-decision` | — | issue | park the new issue on a maintainer decision: the watcher applies the hive's configured needs-decision label itself (see [Parking an issue that needs a decision](#parking-an-issue-that-needs-a-decision)) |
| `--number` | — | comment, claim, close, label, request-review | the issue/PR number; a bare positional number or a `.../issues/N` or `.../pull/N` URL is also accepted |
| `--dry-run` | `-n` | all | validate the arguments and print the exact request that would be written, then exit `0` **without writing it** — nothing is created, commented, claimed, or closed |

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
recorded on the request's result file (`parent_link_error`) instead.

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

`--needs-decision` is the way an agent parks an issue it files. Use it when
the body asks the maintainer to choose between options, or to approve before
work can start. The script sends `"needs_decision": true` on the request, and
the watcher adds the hive's configured needs-decision label
(`project.issue_filter.hard_suppress_labels.needs_decision`, first entry;
`needs-decision` by default) to the create itself, whatever labels the agent
named ([#11215](https://github.com/hivecommons/hive/issues/11215)). The issue
is then parked like any other `needs-decision` issue: it stays out of the
actionable queue, the un-park sweep posts the "What to reply" notice, and the
maintainer answers with `/hive approve` or `/hive decision` (see
[maintainer-commands.md](maintainer-commands.md)). Every lane policy that files
issues tells the agent to pass the flag rather than name the label, so parking
no longer depends on which lane filed the issue or on the label's name in this
hive. The flag only applies to a create; on any other kind the script exits
`2` and writes nothing.

### `comment`

`--repo`, a number or URL, and `--body` are all required, or the script exits
`2`.

### `claim`

`--repo` and a number or URL are required; no body or title needed. A claim
records that this agent is starting work on an issue. Because App bots cannot
be GitHub assignees, the watcher applies a `hive/claimed-by-<agent>` label
instead — the visible, auditable ownership signal — and audits it as
`agent_issue_claimed`.

### `label`

`--repo`, a number or URL, and at least one `--label` or `--remove-label` are
required. Labels are added first, then removed, and the change is audited as
`agent_label_applied` with the repository and number
([#9587](https://github.com/hivecommons/hive/issues/9587)).

Hive-controlled labels are refused in both directions: the merge-queue label
(`lgtm`, or whatever the hive renamed it to), the hold labels, anything in the
`hive/` namespace, and the labels that record a human decision
(`approved-direction`, `design-approved`, `needs-human`, `needs-decision`,
`blocked`). They are inputs to hive automation or records of someone's
verdict, so an agent may not set them through this operation; one reserved
label refuses the whole request. Use `hive-open-issue claim` to record
ownership. This refusal applies only to the `label` operation on an existing
item: to park an issue you are filing, pass `--needs-decision` to the create
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
never count as a shared component. Below the threshold, overlapping findings
file normally; an exact file-set twin still takes precedence.

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
