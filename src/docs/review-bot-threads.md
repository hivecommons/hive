# Review-bot threads on hive-mediated PRs

When a hive agent opens a PR on a repository that has an external review bot
installed — Copilot code review, `chatgpt-codex-connector[bot]`, CodeRabbit —
the bot reviews it a few minutes later and leaves inline threads. On a
repository with "require conversation resolution before merging" the PR cannot
merge until every thread is resolved. Before
[#7360](https://github.com/hivecommons/hive/issues/7360) nothing in the hive
noticed: the PR sat open until a human addressed the comments and resolved the
conversations by hand.

The hive now reconciles those threads itself, for every PR it opened, with no
per-agent prompt rule. Three pieces, and one config key that turns them on.

## Which PRs qualify

A PR is the hive's to follow up on — *hive-mediated* — when **either** of two
things is true:

- its **author is a hive login**: the App bot, or `project.ai_author` when
  configured. These are the PRs the PR-request watcher opens; or
- its **body carries the `-- hive:` attribution trailer**
  (`github.HasAttributionTrailer`). These are the PRs a hive agent opened on a
  *person's* credentials — a contributor relay, or an operator running agents
  under their own GitHub auth. GitHub shows the person as the author, so the
  login rule alone can never recognise them.

The second rule exists because of
[#7638](https://github.com/hivecommons/hive/issues/7638): the reconciler
originally keyed on the author login only, and every relay-run PR was dropped
before its threads were even fetched. Those are precisely the PRs external
review bots review — Codex skips bot-authored PRs — so the only PRs getting
Codex threads were the ones the hive could not follow up on. The trailer is
the same rule the task-list sweep already uses to decide an issue is the
hive's to close.

Do **not** fix this by setting `project.ai_author` to the operator's own
login: that would make every PR they write by hand "hive-authored" too, and
agents would be told to fix and resolve review threads on the operator's own
work. `ai_author` is for a dedicated bot login.

A relay-run PR's trailer usually has no `agent=` (the relay does not know a
hive-side agent name), and there is no App-bot audit entry for a PR the hive
did not open itself, so it is listed with an empty `agent` and reaches the
**scanner**, exactly as any other unattributed PR does.

**On spoofing.** The trailer is a line of text; anyone can paste `-- hive:`
into a hand-written PR and have the hive answer that PR's review-bot threads.
This is accepted, as it was for the task-list sweep closing issues, because
the consequence is bounded: the hive will only ever reply in and resolve
threads that a *configured review bot* opened on that PR — the watcher-side
guard (§3) still refuses a human's thread, and it keys on the thread's first
author, never on the PR's author — which is something the PR's own author
could already do by hand. Nothing about the trailer lets a PR merge, be
approved, or have its human conversations touched.

## Configuration: `classification.review_bots`

```yaml
classification:
  review_bots:
    logins:
      - "chatgpt-codex-connector[bot]"
      - "Copilot"
    min_priority: P1            # optional: only P0/P1 plus unknown badges
    max_attempts_per_thread: 1   # default 1
    resolve_after_fix: true      # default true
```

The key lives in `hive-project.yaml` next to its sibling monitor's config
(`classification.copilot_check`), and the Go binary also accepts the identical
block in `hive.yaml`; when both name a login, `hive.yaml` wins. A
`hive.yaml` `min_priority` (the field the dashboard edits) also overrides the
project file's threshold on its own. It is the one
key the Go side reads from the project file
(`config.LoadProjectReviewBots`, path `HIVE_PROJECT_YAML` or
`/etc/hive/hive-project.yaml`).

- **Absent, or empty `logins`: the feature is off.** The monitor writes an
  empty `review-threads.json` and the watcher denies every thread request.
- `logins` are matched case-insensitively against the **first** comment's
  author, ignoring surrounding whitespace and a trailing `[bot]` suffix. Both
  REST/web (`chatgpt-codex-connector[bot]`) and GraphQL
  (`chatgpt-codex-connector`) spellings match either configured form. Do not
  list a human here: it would let agents resolve that person's threads.
- `max_attempts_per_thread` is how many times the hive replies in one thread
  before leaving it for a human. The counter is the thread itself — replies
  authored by the App bot — so there is no state file to drift.
- `resolve_after_fix: false` makes the agent reply but leave the thread open.
- `min_priority` optionally limits routing to `P0` through `P3` (P0 is most
  urgent). `P1` includes P0/P1 and excludes P2/P3. When neither `hive.yaml`
  nor `hive-project.yaml` sets it, `review.severity.block_at` supplies the
  threshold (see [review-swarm.md](review-swarm.md#blocking-line-reviewseverity-reviewbacklog)).
  With no threshold anywhere, or an unrecognised value, every finding is
  routed. Case and surrounding whitespace
  in the threshold are ignored.
  Only Codex's Markdown badge format, `![P0 Badge]` through `![P3 Badge]`,
  is recognised today, using the first matching badge in the first comment.
  Unknown formats (including unbadged Copilot/CodeRabbit findings) always
  remain included. Filtered threads stay open for humans: they are omitted
  from the FIX-BEFORE-NEW work list and are not auto-resolved.
  Each PR in `review-threads.json` reports `excluded_by_priority`, counting
  otherwise actionable threads excluded solely by this threshold (not
  resolved, outdated, human-authored, or attempt-capped threads). The
  reviewer's jq filter applies the same badge threshold, so it need not
  answer the lower-priority threads in its review.

The dashboard's **Settings → Features → Review Gate** card displays the
effective review-bot settings, including defaults (one attempt per thread and
resolve after fix enabled). The governor settings bundle at
`GET /api/config/governor` exposes them in `review_bots` — `logins`,
`min_priority` (`P0`–`P3`, or empty when every finding is routed),
`max_attempts_per_thread`, `resolve_after_fix`, and `enabled`; `load_error`
reports an unreadable or malformed project file rather than hiding it as an
unconfigured feature. Logins, attempts, and resolve-after-fix are read-only
there: logins stay config-file-only because adding one grants agents
thread-resolution rights.

`min_priority` is editable from the same card (**Minimum bot-finding
priority**: All, P0, P1, P2, P3). The owner-only `PUT /api/config/review`
accepts it as `{"review_bots": {"min_priority": "P1"}}`; an absent key leaves
it untouched, an empty string clears the dashboard override, `all` routes
every finding, and any other value is rejected with 400. The value is written
to `hive.yaml`'s `classification.review_bots.min_priority` and overrides the
project file's threshold even when `hive.yaml` names no login — the project
file's logins are never copied into `hive.yaml`. The change applies to the
thread reconciler and the reviewer without a restart.

The dashboard's **Settings → Features → Review Gate** card displays the
three effective review-bot settings read-only, including defaults (one attempt
per thread and resolve after fix enabled). The governor settings bundle at
`GET /api/config/governor` exposes them in `review_bots`, along with `enabled`;
`load_error` reports an unreadable or malformed project file rather than hiding
it as an unconfigured feature. Logins remain config-file-only because adding
one grants agents thread-resolution rights. This surface does not yet include
the priority filter proposed in [#10479](https://github.com/hivecommons/hive/issues/10479).

`copilot_check` / `bin/copilot-comment-checker.sh` are untouched: they cover
*merged* PRs; this covers *open* ones.

## 1. Monitor: `/var/run/hive-metrics/review-threads.json`

The governor's eval tick (`writeReviewThreads`, throttled to once per five
minutes) runs `Client.CollectReviewThreads` over the enumerated actionable
PRs. It keeps only hive-mediated PRs (see [Which PRs qualify](#which-prs-qualify):
a hive login as author, or the `-- hive:` trailer in the body — the trailer
test is computed from the PR list payload at enumeration time, so it costs no
extra call), skips drafts and anything carrying a hold / `do-not-merge` /
exempt label (the same exclusions as every other kick input), and for each
remaining PR runs one GraphQL `pullRequest.reviewThreads` query. A thread is listed when it is
**unresolved**, **not outdated**, its **first comment is from a configured
bot**, and it has **fewer than `max_attempts_per_thread` replies from the
hive**. Threads a human opened never appear.

```json
{
  "generated_at": "2026-09-17T12:00:00Z",
  "enabled": true,
  "total_threads": 1,
  "prs": [
    {
      "repo": "org/repo",
      "number": 123,
      "title": "fix: nil deref in x",
      "head_ref": "hive/fix-123",
      "agent": "scanner",
      "threads": [
        {"thread_id": "PRRT_kwDO…", "path": "src/x.go", "line": 42,
         "author": "chatgpt-codex-connector[bot]", "body": "x may be nil here",
         "comment_id": 2211, "hive_replies": 0}
      ]
    }
  ]
}
```

`agent` is the hive agent whose relay request opened the PR, resolved from the
audit trail exactly as `ci-failing.json` does; empty means unattributed and the
kick builder defaults it to `scanner` (a PR opened on a person's credentials
is always in this state — see [Which PRs qualify](#which-prs-qualify)). `escalated: true` marks a PR the
escalation sweep has handed to a human. A PR whose bot threads are all
resolved (or all at the attempt cap) is listed with an empty `threads` array,
so the file shows the reconciler has nothing left to do there rather than
silently dropping the PR.

## 2. Kick injection: FIX-BEFORE-NEW

`Scheduler.addReviewThreadFixFirst` is a sibling of the red-CI
`addRedPRFixFirst`, called at the same post-resolution seam so no kick
template can omit it. For a PR-capable agent it prepends a block listing that
agent's own non-escalated PRs with actionable threads — path, line, and a
bounded excerpt of each finding (`redPRFixMaxDetailed` PRs,
`redPRFixExcerptRunes` per excerpt) — and the instructions: check out
`head_ref`, address each thread, `git commit -s`, push to the same branch, then
for each thread reply in-thread with one line and (if `resolve_after_fix`)
resolve it. One pass per kick; a thread already answered is filtered out by
the monitor, and the agent is told to skip it if it still appears.

## 3. Resolution through the review-request watcher

`ReviewRequest` gained a `thread_id` field and a `resolve_thread` event:

```bash
hive-review <number> --repo <owner/repo> --comment --thread <PRRT_id> --body "<one line>"
hive-review <number> --repo <owner/repo> --resolve-thread <PRRT_id>
```

A `comment` with `--thread` is an **in-thread reply** (GraphQL
`addPullRequestReviewThreadReply`), not a PR-level review; `--resolve-thread`
runs `resolveReviewThread`. Both execute with the App token, go through the
same per-agent authorizer and UID forge-resistance as every other review
request, are audited as `agent_pr_reviewed` with `state=thread_replied` /
`state=thread_resolved` and `thread=<id>`, and write the usual
`<name>.result.json`.

**The guard is in the watcher, not the prompt.** Before acting it re-fetches
the thread by id and quarantines the request as `.denied` (reason in the
result file, nothing touched on the forge) when:

| Condition | Reason recorded |
|---|---|
| `review_bots.logins` is empty | `classification.review_bots is not configured` |
| the id resolves to nothing this token can see | `thread … not found` |
| the thread is on a different PR than the request names | `thread … belongs to o/r#N, not the requested PR` |
| the thread's first comment is not from a configured bot | `… opened by <login>, which is not a configured review bot; human threads are never resolved by the hive` |
| the thread is already resolved | `thread … is already resolved` |
| (reply only) the hive already replied `max_attempts_per_thread` times | `… already has N hive replies (max_attempts_per_thread=N); leaving it for a human` |

Malformed requests (`resolve_thread` without `thread_id`, `thread_id` on an
`approve`) are quarantined as `.bad` before authorization, like any other
shape error. A transient forge failure on the re-fetch or the mutation keeps
the request for the same exponential-backoff retry and give-up horizon as a
failed review.

## 4. The hive reviewer answers bot findings on every PR it reviews

The three pieces above act only on hive-mediated PRs. The review swarm reads
the same threads on **every** PR it reviews
([#9360](https://github.com/hivecommons/hive/issues/9360)): before that, a
reviewer could post "Confidence: 5/5 (safe)" minutes after Codex left a correct
P1 finding on the same commit, without mentioning it.

When `review_bots.logins` names any bot, each review kick
(`buildReviewBotFindingsInstruction`, `pkg/review/prompts_instructions.go`) carries a
`gh api graphql` read of the PR's `reviewThreads`, filtered to unresolved
threads whose first comment is from a configured bot. Logins are compared
lower-cased with the `[bot]` suffix removed, because GraphQL reports an App's
login without it. The reviewer must then:

- answer every returned thread in its review: **agree**, **disagree** (reason
  plus the refuting `file:line`), or **cannot verify**;
- verify each finding against the code at head and classify it with
  `review_scope`, like one of its own;
- treat a confirmed, in-scope, severe finding (the bot's P0/P1) as a blocker:
  the owning perspective may not approve, and the review may not call the PR
  clean or safe. A refuted or out-of-scope finding is still answered but does
  not lower the verdict.

The reviewer stays comment-only. It never replies in, reacts to, or resolves
a bot thread; that remains the reconciler's job on hive-mediated PRs. With no
logins configured, the kick has no bot section.

## What a human sees

- A PR with two bot threads gets, within one kick, a push, two in-thread
  replies from the App bot saying what changed, and two resolved threads.
- If the bot comments again on the fix, the new thread appears once, gets one
  more attempt, and then stops: an open bot thread with one hive reply
  explaining what it tried.
- A human's thread on the same PR is never listed, never replied to by this
  path, and never resolved — including on a PR whose *author* is a human
  because a hive agent opened it on their credentials: the guard looks at who
  opened the thread, not who opened the PR.

## The hive's own review threads

Since [#18531](https://github.com/hivecommons/hive/issues/18531) the hive's
reviewer posts each finding that points at a changed line as its own inline
review thread, opened by the App bot. Everything above acts only on threads a
configured review bot opened, so nothing resolved the hive's own threads. On a
repository that requires conversation resolution, one open "non-blocking, you
can ignore it" note from the hive held the merge forever: GitHub refused every
merge-queue enqueue with "All comments must be resolved", and the lane retried
on every sweep (projectbluefin/gutenprint-printer-app#254 and
projectbluefin/ps-printer-app#221, 2026-11-20).

The hive now resolves its own non-blocking notes itself, at two points, both
only on a repository that requires conversation resolution:

- **after GitHub refuses a merge the hive attempted because review
  conversations are unresolved**, once the merge lane's own gates passed;
- **when the stage pass hands a head to a person at
  `090-ready-for-human-merge`**, once per head. The hive never attempts that
  merge itself (auto-merge is off for the repository, a human-merge path, a
  workflow fence), so without this the person's merge was refused for the
  hive's own notes and they had to reply to and resolve each one by hand
  (projectbluefin/actions#779 and #783, 2026-11-25). See
  [At a person's merge](#at-a-persons-merge) below.

A reviewer's verdict never resolves anything by itself, and nothing else (a
person's thread, a blocking finding) is ever resolved; see the limits below.

**Opting out.** There is no separate switch. A hive that wants none of its
own threads resolved sets `review.severity.block_at: P3` (every finding
blocks): then no hive thread is non-blocking, and every such refusal parks
the PR at `085-needs-decision` with `unresolved-review-threads` for a person.
`block_at` is hive-wide and is not only about threads: at `P3` every info and
low finding also blocks the hive's merges, and it lowers
`classification.review_bots.min_priority` when that key is not set, so external
bots' P3 findings are routed too.

**Which merge paths.** The automerge sweep lanes — self-authored (including
the trusted-bot lane), trusted-author, the `/lgtm` queued lane and the
contributor lane — on each of their three merge calls: the native merge-queue
enqueue (GraphQL `UNPROCESSABLE`, "Pull request All comments must be
resolved." or "Pull request A conversation must be resolved before this pull
request can be merged."), the direct REST merge (405 "Repository rule violations found …
All comments must be resolved."), and the serialized lane
(`merge_strategy: hive_serialized`, the same 405 on the lane's merge). The
agent `hive-merge` relay (`merge_pr`) does not: it keeps its own refusal
handling, and on a merge-queue base it already reports
`merge-queue-required` and leaves the PR to the lanes.

**What the lane does with the refusal:**

- every unresolved thread is the hive's own and marked non-blocking: it
  resolves them all and reports `review-threads-resolved`
  (`100-merge-queue`); the next pass merges or enqueues the same head;
- any thread only a person can clear is open (below): it resolves nothing,
  parks the PR at `085-needs-decision` with `unresolved-review-threads`, and
  does not ask GitHub again for that head for 30 minutes (or until a new
  push);
- neither — the thread read failed, GitHub still counts a thread that was
  just resolved, or every resolve call failed: the PR waits at
  `100-merge-queue` with `review-threads-pending` and the lane asks again
  after 5 minutes. After three such refusals on one head it parks as above.
  A refusal nothing explains therefore costs at most three merge attempts
  per head before a person is asked. When a park expires the lane tries
  once more, and another unexplained refusal parks again at once, so a
  parked head costs one merge call per 30 minutes and does not flap
  between stages. While waiting, `review-threads-pending` is not a ready
  merge-train front: the train merges the PRs behind it.

See [stage-labels.md](labels-and-control-signals/stage-labels.md).

**Which threads are the hive's own.** The thread's first comment is from a
`Bot` account whose login is the hive's App bot (GraphQL reports it without
the `[bot]` suffix; both spellings match). `project.ai_author` does not
count, because it can be a person's own account. A hive with no App identity
(static-token mode) has no own threads here and resolves nothing.

**Never resolved:**

- a thread a person or another bot opened (including a configured review bot:
  those stay with the reconciler above);
- a hive thread anyone other than the hive has replied in: the conversation
  is that person's too;
- a hive finding that is not marked non-blocking, outdated or not. An
  outdated thread is not resolved for being outdated: some lanes merge with
  no approving review at all (the trusted-bot lane while
  `auto_merge.trusted_bot_skip_review` is on, or the review gate off or in
  shadow), so "the merge gate passed" says nothing about the finding.

**How "non-blocking" is read.** When the relay posts an inline comment, it
matches the comment to the verdict report's finding on the same file and line
(or inside the comment's line range) and appends a hidden marker naming that
finding's severity, for example `<!-- hive-finding-severity: low -->`. When
several findings match, the most severe wins. A comment no finding matches
(no report, a report that does not validate, a finding with no file or line,
or no severity) is marked `unmatched`, which reads as blocking. A comment a
security-perspective finding is anchored on is marked `security`, whatever
severity that finding was filed at, and also reads as blocking: a reviewer
misled into rating a real security problem low cannot unblock the merge by
it. A marker the agent wrote itself is stripped first. The marked severity is then compared
with the blocking line:

- with `review.severity.block_at` set, a finding is non-blocking when it is
  below the line, the same rule the review backlog uses
  (`ReviewFindingBelowLine`): `P1` leaves medium, low and info below it, `P2`
  low and info, `P3` nothing (the opt-out above);
- with no `block_at` (the default, where the backlog rule puts nothing below
  the line), `info` and `low` count as non-blocking, the reviewer's own
  "non-blocking" tier;
- only the five known severities qualify; `required`, `unmatched`,
  `security` or any other value is blocking.

Threads posted before the marker existed carry only prose. For those the
first line of the hive's comment is read (emphasis removed, at most 160
characters): it must open with a low severity label (`style (low):`,
`Info:`, `Nit:`) or say `non-blocking`, and must not also say blocking,
medium, high, critical, required or must. Such a thread is treated as a `low`
finding against the rule above; anything else is blocking.

Each resolution is audited as `agent_pr_reviewed` with `state=thread_resolved`,
`thread=<id>` and `trigger=merge-refused-unresolved-conversations` (or
`trigger=ready-for-human-merge` at a person's merge, below).

**Known limit.** The hive does not remember threads a person resolved and
then re-opened. A person who unresolves one of the hive's non-blocking notes
without replying in it will see the hive resolve it again on the next
refused merge, and, at `090-ready-for-human-merge`, on the next new head or
after the hive restarts (the once-per-head mark is kept in memory). It is
not replied in again there: the hive's earlier reply is still in the thread.
Replying in the thread is what keeps it open: a hive thread with anyone
else's reply is never resolved.

### At a person's merge

When the stage pass writes `090-ready-for-human-merge` for a head with no
hold or sentinel gate, it applies the same rules to that head once, on the
pass that writes the 090 label and entry comment (for a repository whose
auto-merge is off too; the observe-only lane pass itself still writes
nothing):

- the write-back gate decides first, under the kind the 090 / 085 entry
  comment is gated under (`needs_decision_comment`): a repository in
  `labels-only`, `issues-only` or `off` mode gets nothing read or written,
  and a person's PR needs what that kind needs (the review opt-in label, or
  per-repository mode `full`). A refusal costs no GitHub call and holds for
  that head;
- it then reads whether the base branch requires conversation resolution,
  from the same sources as the required checks: the branch's rulesets (a
  `pull_request` rule with `required_review_thread_resolution`, readable
  without admin rights) and classic branch protection
  (`required_conversation_resolution`). The answer is cached per repository
  and branch for 10 minutes, or for an hour when GitHub refused a read with
  403 (the App lacks the permission). Not required: the notes stay open,
  since they are useful reading. Cannot be read: nothing is resolved, the
  hive logs it once per cache fill, and a later pass asks again;
- when it is required and every unresolved thread is the hive's own and
  marked non-blocking, it replies once in each of those threads to say the
  hive is resolving its own non-blocking note so the branch rule does not
  block the person's merge, then resolves it (a reply that fails does not
  stop the resolve, and a thread that already carries the reply gets no
  second one). Each reply is audited as `agent_pr_reviewed` with
  `state=thread_replied` and `trigger=ready-for-human-merge`. With
  `github.notifications.mode: quiet`, where a plain 090 emails nobody, the
  threads are resolved without replies;
- when any thread is not one of those, it resolves nothing. The 090 entry
  comment is unchanged; the hive logs how many threads still need a person.

A paused repository is skipped. A held 090 (a hold label, an open sentinel
alert) waits until the gate lifts. A new push is a new head and is checked
again, and so is a head where every resolve call failed. The extra cost is
bounded: only a head entering 090 that was not checked before, one cached
rule read per repository and branch, one thread query for the PR, and one
reply and one resolve per thread.
