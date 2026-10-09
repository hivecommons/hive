# Review swarm

Hive's review swarm adds a structured review-to-fix decision point for pull requests. It includes the review data model, prompt construction, verdict collector, optional merge-gate integration, governor fan-out to review-capable agents, and a bounded auto-fix cycle for review findings.

## Perspectives

The `pkg/review` package ships five built-in review perspectives:

- `correctness` — regressions, edge cases, data races, test adequacy
- `security` — exploitable vulnerabilities, unsafe permissions, injection, secrets, trust-boundary regressions
- `intent-alignment` — does the diff solve the linked issue without unrelated scope creep
- `style` — maintainability, conventions, readability, repository idioms
- `docs-currency` — documentation, examples, generated docs, operator-facing text that must change with behavior

One more is built in but off by default:

- `plan_match` — whether the diff implements the approved plan wave named by the PR's `Hive-Run:` / `Hive-Plan:` trailers (hivecommons/hive#8317). See [Plan match](#plan-match) below.

Which perspectives run, and what each is told to look for, are per-hive settings under `review:` (Governor → Features → Review Gate → Perspectives in the dashboard):

```yaml
review:
  perspectives: [correctness, security, api-compat]   # empty = all built-ins
  perspective_prompts:
    style: "follow docs/CONVENTIONS.md; flag any exported symbol without a doc comment"
    api-compat: "breaking changes to the public Go API or CRD schema"
  combined_perspectives: true
```

- `perspectives` selects the set, in dispatch order. Empty means every built-in. A name that is neither built in nor described in `perspective_prompts` fails validation rather than being dropped — a typo that silently disappeared would read as enabled everywhere it is displayed while nothing reviewed it.
- `perspective_prompts` overrides the focus line for a built-in, or defines a hive-specific perspective entirely (custom names: lowercase letters, digits, single dashes, ≤ 40 chars). A blank entry means the built-in text.
- `combined_perspectives` reviews every enabled perspective in **one** agent session that leaves **one** review comment, with findings grouped under the perspective they belong to. Off, each perspective is its own session and its own comment, and `max_perspectives_per_pr` caps how many a PR receives. Combined mode ignores that cap: it exists to bound comments, and a combined review is always exactly one. The reviewer still emits one verdict per perspective — as a JSON array in the verdict file — so any single perspective can still hold a PR for a human.

Verdicts are validated against the hive's own perspective set: a verdict naming a perspective this hive does not review with is refused by the relay, since nothing dispatched it and nothing is waiting on it.

### Plan match

`plan_match` judges intent as well as diff. Implementation PRs opened for a long-running run carry `Hive-Run: <run key>` and `Hive-Plan: <plan ref>` trailer lines in their body (hivecommons/hive#8310). When the perspective is on, the hive reads those trailers at enumeration time, resolves the plan they name from its bead stores (the ref may be the epic bead ID or the source issue's `owner/repo#N`), and renders the approved wave - the epic and each planned item with its status - into the review kick. The reviewer compares the diff summary against that list and reports exactly two kinds of finding:

- `scope exceeds plan` - a file, behaviour or surface the diff adds that no planned item covers. High severity when the unplanned change is user- or operator-visible or lands outside every planned item's area; medium for supporting changes the plan implies but does not name; low for incidental cleanup.
- `planned item missing` - an item in the wave the PR should have delivered and did not.

A high-severity scope finding is a human decision: it promotes the aggregate to `requires_human` and the PR is held for a human exactly like any other blocking finding. The plan text is planner output, not the PR author's rationale, so quoting it does not reintroduce the measured worst-performing reviewer arm (see `groundingSection` in `pkg/review/prompts.go`).

A PR with no run trailer has nothing to judge. The reviewer returns a **not applicable** report (`verdict: approve`, `not_applicable: true`, no findings), which the aggregate treats as approve for unanimity and the confidence score drops from both the reported and the expected count - it neither helps nor hurts. A PR whose trailer names a plan the hive cannot find gets `requires_human` from this perspective, since the reviewer must not infer a plan from the diff.

Turn it on from Governor -> Features -> Review Gate -> "Match PRs against their approved plan", or in config:

```yaml
review:
  plan_match:
    enabled: true   # default false; appends plan_match to the perspective set
```

The toggle is separate from `review.perspectives` so enabling it never requires spelling out the whole selection; `plan_match` may also be listed there explicitly. It is omitted from the settings UI's built-in list for the same reason. Off by default because the perspective only earns its cost on a hive whose PRs carry run trailers and whose plans live in its bead stores.

Each reviewer returns a JSON object that extends the `pkg/outputschema.AgentReport` contract — or, for a combined review, a JSON array of one such object per perspective, all for the same PR and head SHA. Required AgentReport fields remain `lane`, `kind`, `findings`, `prs_opened`, `beads_filed`, and `summary`; review reports add `perspective`, `verdict`, `repo`, `number`, and optional `head_sha`. `kind` must be `review`.

Allowed verdicts are `approve`, `changes_requested`, `requires_human`, and `reject`. Finding severities reuse `outputschema.Severity`: `info`, `low`, `medium`, `high`, and `critical`. Each finding also carries `review_scope`: `in-scope` when it affects whether the PR satisfies its linked issue or lease task, `out-of-scope` when it is a real adjacent/pre-existing concern that should not block this PR. Older reports without the field are treated as in-scope.

## Verdict flow

The collector reads review report artifacts from `/var/run/hive-metrics/review-report-*.json`, validates the AgentReport envelope plus review fields, aggregates by PR, and writes `/var/run/hive-metrics/review-verdicts.json`.

Those artifacts are written by the **review relay**, not by the reviewing agent. `/var/run/hive-metrics` is owned by the hive and each agent runs under its own account, so an agent-side write is refused by the filesystem; returning the JSON in kick output stores it nowhere. The agent hands its verdict over with `hive-review --verdict-file <path>` alongside the comment, or with `hive-review --record-verdict --verdict-file <path>` when it has nothing to post. The watcher validates the report, checks it names the PR that was actually reviewed, and writes the artifact server-side — so a malformed or mis-targeted verdict never reaches the collector, which fails the whole collection on the first unparseable file.

Validation happens **before** the comment is posted, and a verdict that fails it refuses the whole request: nothing is posted, the request is quarantined as `.bad`, and the `.result.json` the agent polls carries `ok: false` with the validator's message and a complete example object. The two artifacts are atomic on purpose — a comment that landed without its verdict left the PR reading as never reviewed, so the next kick handed it back and the same comment was posted again. `hive-review` runs the same key check in the agent's own shell first, so the common mistake (a bare `{"repo","pr","verdict","summary"}`) fails immediately with the required shape.

The cadence reviewer's kick template (`reviewer-queue.md`) quotes that schema verbatim, lists the hive's configured perspectives with their focus text via `${REVIEW_PERSPECTIVES}`, and requires the verdict file to be an array with one object per listed perspective — clean ones as `approve` with empty findings — so a clean PR scores `5/5` rather than being capped as `1 of 5 perspectives reported`. Every `${PR_LIST}` row whose current head already carries a verdict is marked `[hive-reviewed: <verdict>@<sha7>]` so the reviewer skips it. The mark is keyed on the head SHA exactly as the dispatch lane is, so a new push reads as unreviewed again.

The mark has two sources: the verdict artifact, and the relay's own `review-links.json` ledger of what it actually posted (with the head it posted at), so a review whose verdict was discarded — a PR the fan-out lane never dispatched — still marks the row.

**Per-head backstop.** Independently of any prompt, the relay suppresses the comment of a new top-level review on a head that already carries its quota of hive reviews — the verdict is still recorded, so a dispatch-bound judgement is never lost to the cap: one in `combined_perspectives` mode, one per perspective otherwise, or `review.max_reviews_per_head` when set (negative disables). `--revise` (update the existing review), `--thread` (reply in it) and APPROVE are exempt. The current head is read from GitHub, not from the verdict; a failed lookup proceeds, since this is a backstop against a loop rather than the primary gate. The result is `ok: true`, `state: record_verdict`, with a `note` naming the existing review and the way out.

A verdict that is never handed over is not a neutral outcome: aggregation reports "never reviewed", and the PR is dispatched for review again from scratch.

Aggregation rules are deterministic:

1. Any `reject` recommends closing the PR.
2. Any in-scope finding at or above the human threshold defaults to `requires_human`.
3. Any explicit `requires_human` yields `requires_human`.
4. Any `changes_requested` enters a fix cycle while below the fix cap.
5. Every perspective in the hive's configured set approving (or, under `max_perspectives_per_pr`, every perspective the PR was eligible to receive) yields a merge-eligible aggregate.
6. Missing perspectives or any other non-unanimous result requires human review.

Out-of-scope findings alone are normalized to an approving perspective for aggregation and confidence. They remain in the structured record, but they never lower the verdict or the 0–5 confidence score for the PR.

The default human threshold is `high`. Review-triggered fix cycles use the same cap value as the escalation re-engagement circuit breaker (`escalation.MaxReEngagements`) so bot loops remain bounded.

### Out-of-scope backlog filing

When the relay accepts a review verdict, cited out-of-scope findings (`review_scope: "out-of-scope"` with `file` and positive `line`) are filed as follow-up issues labeled `from-review`. Each issue body links back to the reviewed PR and records the perspective, severity, evidence location, and finding summary. The relay deduplicates against open issues through the normal issue-create path and also persists a finding key in `/data/review-backlog-issues.json`, so re-reviewing a new commit does not file the same backlog issue or summary comment again.

The PR receives one summary comment listing the backlog issues filed from that review. Filing is enabled by default and bounded by `review.max_out_of_scope_backlog_issues` (default `3`) per PR; set `review.out_of_scope_backlog_disabled: true` to opt out.

Out-of-scope findings follow `review.backlog` (destination, labels, per-PR daily cap) and the fingerprint dedup described under [Backlog routing](#backlog-routing).

### Pipeline stages

`GET /api/review/pipeline` places every review-queue PR in exactly one stage, derived by the pure `pkg/review/pipeline.Derive` from the verdict artifact, the review dispatch state, the `review-links.json` ledger, labels, the escalation ledger, CI state and merge state. Rules are checked in this order; the first match wins:

| Stage | Meaning |
|---|---|
| `merged` | The PR merged. |
| `abandoned` | The PR closed without merging. |
| `human_hold` | A `hold`/`needs-human` label or held queue entry, a `requires_human` or `reject` verdict on the current head, a dispatch-state human hold, an escalated escalation entry, or a `changes_requested`/`fixing` PR whose loop counter reached the fix-cycle cap. |
| `approved` | A merge-eligible `approve` verdict on the current head, an `lgtm`/`approved`/`reviewer-passed` label, the escalation ledger's reviewer-passed SHA on the head, or an approving hive review on the head. The next action is merge, fix CI or wait for CI depending on the CI rollup. |
| `fixing` | A fix is dispatched for the current head. |
| `changes_requested` | The current head's verdict (or, without one, the hive's posted review) requests changes. |
| `reviewing` | Review perspectives are dispatched for the current head and not yet aggregated. |
| `unreviewed` | None of the above; a verdict on an older head does not count once the head moves. |

Each card also carries the reviewers (perspective and model), in-scope finding counts on the P0–P3 scale `review_bots.min_priority` uses (critical → P0, high → P1, medium → P2, low/info → P3), the loop counter against its cap, one next action and the reasons for the stage. The review queue lists open PRs only, so the endpoint does not return `merged` or `abandoned` cards yet.

### Loop safety

A card carries `loop_warning` (reason, plus the reviewer and thread behind it) while the fix-cycle counter is at `loop_cap − 1` or over, or a review-bot thread has reached `review_bots.max_attempts_per_thread`, and no human has the PR yet. `POST /api/review/pipeline/{owner}/{repo}/{number}/send-to-human` (merger or owner role) applies `needs-human` and posts one `<!-- hive-review-loop -->` comment saying why; repeating it posts no second comment, and each use is audit-logged. The endpoint does not load bot-thread counts yet, so cards warn on the fix-cycle counter only.

Review-loop safety checklist:

- [ ] The fix-cycle counter is below `loop_cap`, or the PR is already with a human.
- [ ] No review-bot thread is at its attempt cap without a human reply.
- [ ] A PR sent to a human carries `needs-human` and exactly one loop-safety comment.
- [ ] Nobody re-dispatches a fix on a PR that carries `needs-human`.

### Confidence score

Every aggregate also carries a derived 0–5 **mergeability confidence** (`confidence.score`, with `confidence.reasons`) so a maintainer working a queue can sort or glance without reading each finding (hivecommons/hive#8182). It is computed from the same reports as the verdict — never asked of the model — so it means the same thing on every PR:

| Input | Effect |
|---|---|
| clean, fully covered, unanimous approve | 5 |
| each `medium` finding | −1 |
| each `high` finding | −2 |
| any `critical` finding | 0 |
| any `changes_requested` or `requires_human` perspective | capped at 3 |
| a configured perspective that did not report | capped at 3 |
| a `not_applicable` report (`plan_match` on a PR without a run trailer) | excluded from both counts |
| any `reject` | 0 |

`info` and `low` findings cost nothing. Bands: 4–5 *safe*, 1–3 *needs attention*, 0 *do not merge*. `reasons` lists only what actually moved the score, worst first.

With `review.confidence_score: true` the relay appends one line to each posted review comment, above the attribution trailer:

```
**Confidence: 3/5** (needs attention) — 1 high finding
```

The line is derived from the verdict file the agent hands over with the comment; a request without a parseable verdict gets no line. It is off by default — the verdict marker already routes the decision, and a repo that did not ask for a score should not see one.

## Configuration

Merge-gate and fan-out use are opt-in and preserve existing behavior by default:

```yaml
review:
  require_approval: true
  fan_out: true
  max_parallel_reviews: 5
  reviewer_agents: [reviewer-a, reviewer-b] # optional; otherwise agents with review role/keywords are selected
  fixer_agent: scanner                      # optional; defaults to the PR lane, then scanner
  confidence_score: true                    # optional; append the 0–5 mergeability line to review comments
  all_authors: true                         # optional; review every open PR, not only agent-authored ones
  fix_human_prs: false                      # optional; let the fixer push commits to PRs Hive did not open (default off)
  contributor_prs:
    base_sync: false                        # optional; let GitHub update-branch unstick hive-authored fork PRs (default off)
  priority_labels: false                    # optional; mirror the PR review queue rank onto review-priority/* labels (default off)
  severity:
    block_at: P2                            # optional; P1 | P2 | P3, see "Blocking line" below
    comment_below: true                     # default true
    backlog_below: true                     # default true
  backlog:
    destination: github_issue               # github_issue | github_project | linear | jira
    labels: [from-review]                   # default [from-review]
    max_per_pr_per_day: 10                  # default 10
```

### Blocking line (`review.severity`, `review.backlog`)

`review.severity.block_at` is the lowest finding priority that still blocks merge, on the P0–P3 scale (critical → P0, high → P1, medium → P2, low/info → P3). **Settings → Features → Review Gate → Severity & backlog** offers three presets: *Strict (P0–P2 block)* sets `P2`, *Ship fast (P0–P1 block)* sets `P1`, and *Everything blocks* sets `P3`. Unset keeps today's behaviour. Findings below the line are posted as non-blocking comments when `comment_below` is true (default) and filed as follow-up work when `backlog_below` is true (default).

`review.backlog` says where that follow-up work goes:

| Key | Default | Notes |
|---|---|---|
| `destination` | `github_issue` | `github_project`, `linear` and `jira` require `governor.work_source.type` to be `github_projects`, `linear` or `jira` respectively. The dashboard only offers the active work source's destination, and the owner-only `PUT /api/config/review` rejects a mismatch. If the work source later changes, the hive falls back to `github_issue` rather than failing to load. |
| `project_column_id` | — | Projects v2 status option id; required when `destination: github_project`. |
| `linear_state` | team default | Linear workflow state for new items. |
| `jira_status` | project initial status | Jira status for new tickets. |
| `labels` | `[from-review]` | Applied to every item. |
| `max_per_pr_per_day` | `10` | Cap per PR per day. |

**Precedence with `classification.review_bots.min_priority`.** One blocking line governs both the hive reviewer and external review bots. The bot threshold resolves in this order, first non-empty wins: `hive.yaml` `classification.review_bots.min_priority` (including an explicit `all`), then `hive-project.yaml` `classification.review_bots.min_priority`, then `review.severity.block_at`. With none set, every bot finding is routed. `block_at` is never copied into `min_priority`; clearing the explicit override hands control back to `block_at`. Invalid values fail config load with an error naming the field (for example `review.severity.block_at`, `review.backlog.project_column_id`).

#### Backlog routing

When `block_at` is set and `backlog_below` is on, every cited in-scope finding (with `file` and a positive `line`) whose priority is below the line is filed to the backlog when the relay accepts the verdict. Cited out-of-scope findings go to the same destination as before (see [Out-of-scope backlog filing](#out-of-scope-backlog-filing)).

- **Destination.** `github_issue` files an issue in the PR's repo. `github_project` also adds that issue to the `governor.work_source.github_projects` project and sets its Status to `project_column_id`. `linear` files an issue on the Linear team mapped to the repo, in `linear_state` when set (an unknown state is an error naming it; label names the team lacks are skipped). `jira` files a Task in the first `work_source.jira.project_keys` project and moves it to `jira_status` when set. All three use the work-source credentials. If the destination's work source is not active or cannot be built, the item is filed as a GitHub issue with the `from-review` label added, and the hive logs why.
- **Item body.** The finding title and summary, the PR link, a `file:line` permalink at the reviewed head SHA, the severity with its P-level, the fingerprint, and the line `filed by Hive review backlog`. `labels` are applied to every item.
- **Fingerprint dedup.** Each finding is fingerprinted as `sha256(rule | path | snippet)`: the title, the cleaned file path and the summary, lower-cased with whitespace collapsed. Line numbers are left out, so the same nit at a shifted line still matches. `/data/review-backlog-issues.json` keeps a per-repo map of fingerprint → item. When another PR raises a known fingerprint, the hive comments on the existing item with the new PR link and permalink instead of filing a second one. The same PR raising it again changes nothing.
- **Cap.** At most `max_per_pr_per_day` items are filed or updated per PR per UTC day. Findings over the cap are retried on the next review. Out-of-scope findings also stay within `review.max_out_of_scope_backlog_issues` per PR.
- **Audit.** Each filing pass that wrote anything records one `review_backlog_batch_routed` entry on the PR (`destination`, `filed`, `updated`, `capped`). Each new GitHub issue also gets its own `review_backlog_issue_filed` entry. The PR receives one summary comment that lists the items filed.

The confidence score also feeds the **PR review queue** (`GET /api/review/queue`), which ranks every open PR - agent- and contributor-authored - by triage class, confidence band, CI state and age, with the reasons for each position. An unreviewed PR ranks as *needs attention*, never *safe*. See [review-queue-triage.md](review-queue-triage.md#pr-review-queue).

When `review.require_approval` is false or omitted, `merge-eligible.json` is produced as before. When true, a PR is included only if `review-verdicts.json` contains an aggregate `approve` for the same repo, PR number, and head SHA.

`review.fan_out` is separately defaulted to false. When both `require_approval` and `fan_out` are true, the governor eval cycle plans review kicks for agent-authored PRs that do not yet have a fresh aggregate verdict for their current head SHA.

## Reviewer selection

An agent is considered review-capable if it is enabled, not paused, and not on-demand,
**and** one of the following is true:

1. **Explicit list** — `review.reviewer_agents` names the agent exactly. When this list is
   non-empty, only agents on it qualify; the keyword scan below is skipped entirely.
2. **Keyword scan** — the string `review` (case-insensitive) appears in any of:
   the agent's name, `role`, `aliases`, `lane_keywords`, or `detect_keywords`.

The dashboard security summary (`GET /api/security`) exposes a `reviewCapableAgents`
count. If `review.require_approval` is true and that count is zero, the dashboard
shows a warning:

> Review approval is required, but no enabled review-capable agents were detected.

To make an agent review-capable without the explicit list, set its `role` or add a
keyword:

```yaml
agents:
  my-reviewer:
    engine: claude
    role: reviewer        # contains "review" → qualifies automatically
```

Or use the explicit list to name any agent regardless of its keywords:

```yaml
review:
  require_approval: true
  fan_out: true
  reviewer_agents: [my-reviewer, scanner]
```

Source: `dashboardAgentReviewCapable` in `src/pkg/dashboard/status_builder.go`.

## Dispatch and prompt construction

`pkg/review` provides prompt builders for one prompt per perspective plus a sequential fallback prompt. These prompts instruct review-capable agents to emit the extended AgentReport JSON shape above.

Phase 2 adds dispatch state in `/var/run/hive-metrics/review-dispatch-state.json`. Pending review kicks are scoped by repo, PR number, perspective, and head SHA; when a PR head changes, stale pending entries are pruned and the new head must be reviewed again. Multiple review-capable agents receive perspective prompts in parallel up to `max_parallel_reviews`; a single reviewer receives one perspective per eval round.

When an aggregate verdict is `changes_requested`, Hive builds a review-fix kick containing the aggregate findings and sends it to `review.fixer_agent`, the classified PR lane, or `scanner`. Fix dispatches are capped by `escalation.MaxReEngagements`; once exhausted, dispatch state records a `requires_human` hold for the PR head so the automated loop stops.

### Who may be pushed to: `all_authors` versus `fix_human_prs`

A review-fix kick tells the fixer agent to check out the PR branch and push a commit to it. That is fine on a PR one of the hive's own agents opened; on a contributor's fork (reachable through GitHub's "allow edits by maintainers") or a maintainer's branch it is a bot rewriting someone else's work before any person has looked at it. The two settings are therefore separate:

- `review.all_authors` makes every open PR eligible for **review**, whoever opened it. It never implies pushing.
- `review.fix_human_prs` (default off) lets the fix kick be dispatched for a PR the hive did **not** open. Owner-only; in the dashboard it sits next to "Review every PR" under Features -> Review Gate -> Reviewers, as "Push fix commits to PRs Hive did not open".
- `review.contributor_prs.base_sync` (default off) is narrower: it only lets PR follow-up call GitHub's update-branch API for a hive-authored lane PR whose head is in a contributor fork, whose base moved (`behind`/`dirty`), and whose author enabled "allow edits by maintainers". `behind` produces a GitHub-authored merge commit; `dirty` falls back to the base-moved repair note and kick. Fork pushes still do not pass through Hive, so stale-head prevention is a post-push note/kick rather than a true pre-push rejection.

For PRs the hive did not author, the scanner never pushes content or fixup commits: it comments with the proposed change (or a GitHub suggestion) and leaves the author to apply it. The only sanctioned pushes are the base-sync above and, with `fix_human_prs: true`, a fix kick; governance and policy text is never changed without the author's or a maintainer's consent.

Before every fix kick the planner checks authorship. A PR counts as the hive's own when its author login is an agent (`github.ai_author` or a `[bot]` login), when the audit trail attributes it to one of this hive's agents, or when its body carries the hive attribution trailer (a PR an agent opened on a person's credentials). Anything else gets a fix kick only with `fix_human_prs: true`.

When the check refuses, the `changes_requested` verdict still stands and the review is still published (`post_comments`). The reviewer's kick for such a PR carries an extra instruction: no agent will push to this branch, so every fix it wants goes into the review comment as a ```` ```suggestion ```` block or a ```` ```diff ```` patch the author can apply, and the reviewer must not check out or push to the branch itself. The refusal is recorded once per PR head in dispatch state (`withheld_fixes`) and on the audit log as `review_fix_withheld`, with the PR, its author, and `setting=review.fix_human_prs=false`, so a fix that did not happen can be traced rather than guessed at. A new push to the PR is a new decision.

**Upgrading.** Before `fix_human_prs` existed, `all_authors: true` alone made the fixer push to every PR. A hive already running that way keeps running that way: on load, a config with `all_authors: true` and `fix_human_prs` never set is stored with `fix_human_prs: true`, logged once, and shown as on in the dashboard so the operator can turn it off deliberately. New hives and hives with `all_authors` off get `false`; an explicit `fix_human_prs: false` is never overwritten; no other review field is touched.

## Revising a recorded verdict

Dispatch skips any PR that already carries a verdict for its head SHA. That is the right default — it stops the reviewer re-reviewing unchanged code every sweep — but it also makes a review final the moment it is written, even when the reviewer itself was defective. Two settings, both required, re-open such verdicts narrowly:

```yaml
review:
  revise_repos: [org/repo]                       # allowlist; empty (default) = no repo may be revised
  revise_verdicts_before: 2026-09-19T14:00:00Z   # RFC 3339; only verdicts recorded before this instant
```

- `revise_repos` allowlists the repos whose reviews may be edited. Silently rewriting text a maintainer has already read is a power worth granting deliberately, per repo — so the empty default means the feature is entirely off.
- `revise_verdicts_before` re-opens PRs whose verdict was recorded before the cutoff even though their head SHA has not moved. The cutoff is self-limiting: a re-review records a fresh timestamp that is necessarily after it, so each PR is revisited at most once per bump rather than looping. When a revisit is due, the head's pending dispatch entries are cleared so every enabled perspective is asked again, and an in-flight guard stops a revisit whose verdict is still outstanding from being re-kicked every cycle.

The correction **edits the hive's existing review in place** rather than posting a second one — editing notifies nobody, while a new review pings every subscriber. The reviewing agent requests this with `hive-review --revise` (alongside its usual body and verdict file). Three guards in the relay make the edit safe to point at someone else's repo, each pinned by a test:

- only a review authored by the hive's own App login is ever touched (no login configured → fail closed);
- only `COMMENTED` reviews are edited, never `APPROVED` or `CHANGES_REQUESTED`, since rewriting those would retroactively change what a formal state says;
- an identical body (modulo whitespace) issues no write at all.

Both keys are also writable at runtime through `PUT /api/config/review` (owner only). The cutoff is validated as RFC 3339, and the GitHub client's cached revise allowlist refreshes on write rather than at the next boot.

## Measuring effectiveness

Verdict counts say how much the reviewer did; they cannot say whether it helped. The **review-outcome ledger** (`/data/review-outcomes.json`, `pkg/review/outcomes.go`) answers the question the review gate exists for — *do reviewed PRs leave the queue faster than unreviewed ones?* — by keeping a control group.

Every eval cycle, after the verdict artifact is refreshed, the hive:

1. upserts every open PR the governor enumerated (repo, number, author, agent- or human-authored, first-seen time);
2. attaches the **earliest** review evidence it has for each — the verdict artifact's `recorded_at`, or the posted-review links ledger's `at` when a queue-lane review could not bind a verdict;
3. resolves PRs that left the list with one `GET /pulls/{n}` each (capped at 40 per cycle; the rest resolve next cycle) to `merged` or `closed`;
4. appends one daily queue snapshot (open count, open-and-reviewed count).

A PR counts as **reviewed** only if the hive's first review landed *before* its outcome; a review posted after the merge is control, not treatment. Rows are kept for 90 days after resolution.

Read it from:

- **Dashboard** — Features → Review gate → *Effectiveness* → "Load 30-day outcomes": reviewed vs control on PRs, merged %, closed, still open, median first-seen→merge hours and merged-within-72h, plus the queue trend from the snapshots.
- **`GET /api/review/outcomes?days=N`** (1–90, default 30) — the full `OutcomeSummary`: `reviewed`, `unreviewed`, `by_verdict`, `agent_authored`, `human_authored`, `snapshots`.
- **`GET /api/reviewer/accuracy?days=N`** (1–90, default 30 or `HIVE_REVIEWER_ACCURACY_WINDOW_DAYS`) — reviewer verdict calibration: false-approve/false-block rates with sample counts plus confidence buckets per perspective and reviewer model.
- **`/metrics`** — `hive_review_outcome_prs{reviewed,outcome}` and `hive_review_outcome_median_hours_to_merge{reviewed}` over the 30-day window.

Read the numbers with care: the cohorts are not randomised. The reviewer reaches PRs in queue order, so early on the reviewed cohort skews towards whatever it got to first. The comparison becomes meaningful once both cohorts have dozens of resolved PRs; until then treat it as directional.

## Event-driven dispatch

With webhooks configured, a pushed PR does not wait for the next governor cadence to be reviewed (hivecommons/hive#11091). The spoke's existing signed webhook receiver (`POST /api/webhook/github`) queues a review dispatch for `pull_request` deliveries with action `opened`, `reopened`, `synchronize`, `ready_for_review` or `review_requested` on an open, non-draft PR in a managed repository.

- **Debounce.** A PR fires once it has been quiet for `review.event_debounce_s` (default `90`, max `3600`). Each new delivery restarts the window and replaces the head SHA, so a burst of pushes is reviewed once, at its final head; a PR pushed continuously still fires four windows after its first event.
- **Idempotency.** A head SHA that already fired is not queued again for 24 hours. The per-head perspective budget and dispatch state still stop a second review of the same head either way.
- **Bounded.** At most 256 PRs wait at once; further deliveries are dropped and counted, and the cadence tick reviews those PRs as before. Wakes are spaced at least 30 seconds apart, so many PRs pushed together cost one early cycle.
- **Same gates.** Firing wakes the governor for an early eval cycle with the fired PRs first in the review list. Head age, loop caps, ACMM, hold labels and `max_parallel_reviews` apply exactly as on a cadence tick.
- **Fallback.** The cadence path is unchanged and still covers hives without webhooks, missed deliveries and dropped events.

`review.event_driven` unset means on exactly when `GITHUB_WEBHOOK_SECRET` is set; `false` turns it off, `true` forces it on. It never runs while `require_approval` or `fan_out` is off.

```yaml
review:
  event_driven: true
  event_debounce_s: 90
```

`GET /api/review/dispatch/events` lists pending and recent dispatches with their counters (received, queued, coalesced, duplicates, dropped, fired, wakes). Review pipeline cards carry `trigger`: `event` (a webhook dispatch fired for the current head), `event_pending` (one is queued), or `cadence` (the head has reviewers without an event dispatch).

## Deferred work

- Map aggregate verdicts to labels/comments (`hold`, `needs-human`, close recommendation) once fan-out exists.
- Add dashboard visibility for review verdict artifacts. The per-PR [review evidence bundle](review-evidence.md) (tracked in #11058) is the planned durable, downloadable record; see also the [SOC 2 control mapping](soc2-control-mapping.md) (not a compliance claim).
