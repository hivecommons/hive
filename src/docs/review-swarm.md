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

### Combined perspectives: recommended, opt-in

**Turn `combined_perspectives: true` on** (Governor → Features → Review Gate, or the config above) unless you have a reason not to. It is the single largest review-swarm token saving available (hivecommons/hive#12126):

- The per-perspective kick repeats ~4–6 KiB of shared instruction — scope contract, how to read the PR, review-bot thread obligations, grounding, output schema, publish contract — for every perspective a PR is sent for. The combined kick emits that block **once**, under a `SHARED INSTRUCTIONS` heading, then one short `[perspective:<name>]` section per perspective carrying only what differs (its focus line and, for `plan_match`, the plan wave). Five perspectives cost one shared block plus five short sections, not five full prompts.
- The diff and the surrounding tree are read once per session instead of once per perspective. The grounding tool-call budget is stated once for the whole session: `GroundingToolCallBudget` plus `GroundingToolCallBudgetPerExtraPerspective` for each perspective beyond the first.
- What is **not** changed by combining: every perspective still returns its own verdict object (the verdict file is a JSON array, one object per perspective, validated by `ValidateReportsFor`), the review-bot thread contract is unchanged (unresolved threads from `review_bots` are fetched, answered in the review, never replied to or resolved by the reviewer), and dispatch stays keyed on (repo, PR, head SHA, perspective) with durable pending state — a combined kick records one pending entry per perspective it covers.

**Decision: it stays off by default.** Flipping the zero value of a plain bool would silently change the dashboard toggle's meaning, the relay's derived per-head review quota (one review per head in combined mode, one per perspective otherwise), the reviewer selection (combined dispatch sends the first eligible reviewer rather than fanning out), and the blast radius of a malformed verdict (one schema slip discards every perspective's verdict at once) on hives that never asked for it. Those are operator choices, so the hive promotes the setting — here, in `hive.yaml.example`, and in the settings UI — rather than making it for them. To opt out after enabling, set `combined_perspectives: false`.

The review-bot thread fetch in both kick shapes is bounded to what the reviewer acts on: `reviewThreads(first:ReviewBotThreadPageSize)` pages through the threads (the command paginates, so this bounds the page, not the total), and `comments(first:ReviewBotThreadCommentsFetched)` fetches the bot's finding plus the first few replies per thread rather than the full back-and-forth. The reviewer answers the finding; it does not need the twentieth reply.

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

Each reviewer returns a JSON object that extends the `pkg/outputschema.AgentReport` contract — or, for a combined review, a JSON array of one such object per perspective, all for the same PR and head SHA. Required AgentReport fields remain `lane`, `kind`, `findings`, `prs_opened`, `beads_filed`, and `summary`; review reports add `perspective`, `verdict`, `repo`, `number`, and optional `head_sha`. `kind` must be `review`. `head_sha` should be the full 40-character head; an abbreviated one (7+ hex digits, as `git rev-parse --short` or the PR page prints) is accepted and widened by the relay to the dispatch head or the PR's current head it prefixes, so the verdict is stored under the spelling every lane looks it up by.

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
6. Every delivered perspective approving while some the PR is owed have not reported yields `incomplete`: not merge-eligible, not a human hold, with a reason naming the missing perspectives (`review coverage incomplete: missing docs-currency`). An incomplete head stays in the dispatcher: only the missing perspectives are re-asked (under `pending_ttl` / `max_redispatch`), never the ones whose reports already exist. The exception is a head that already holds a settled non-approve verdict (`requires_human`, `changes_requested` or `reject`): an incomplete re-review of the same head does not replace it. The artifact keeps the prior decision, its findings or question and its `recorded_at`, so the dispatcher never sees `incomplete` for that head — a `requires_human` head keeps taking the human-hold path and stays at 085, and a `changes_requested` or `reject` head stays on the fix path (rather than dropping back to 050 to re-ask the missing perspectives) until a complete replacement review lands. When the stale entries expire under `pending_ttl`, the whole perspective set is asked again for that head, not just the missing ones (hivecommons/hive#12878). Before this state existed a reviewer kick that died after its peers had reported left the head on a `requires_human` hold that no re-dispatch could clear (hivecommons/hive#12045).
7. Any other non-unanimous result requires human review.

One exception to rules 2–3: on a **hive superseding PR** (the App's own PR declaring `Supersedes #N`, `PerspectiveReport.hive_superseding`) a `requires_human` — security-sensitive included — becomes `changes_requested` and enters the fix cycle, because the owner's rule is that such PRs auto-merge with no operator decision; only the fix cap, a `reject`, or the stage engine's hard stops (sentinel, hold, red CI, unresolvable conflict) hand it to a person. `auto_merge.hive_superseding.auto_merge: false` turns this off. See [hive-merge.md](hive-merge.md#hive-authored-superseding-prs-auto-merge).

Out-of-scope findings alone are normalized to an approving perspective for aggregation and confidence. They remain in the structured record, but they never lower the verdict or the 0–5 confidence score for the PR.

The default human threshold is `high`. Review-triggered fix cycles use the same cap value as the escalation re-engagement circuit breaker (`escalation.MaxReEngagements`) so bot loops remain bounded.

### Out-of-scope backlog filing

When the relay accepts a review verdict, cited out-of-scope findings (`review_scope: "out-of-scope"` with `file` and positive `line`) are filed as follow-up issues labeled `agent/reviewer` (the reviewer lane provenance/routing label — see [labels-and-control-signals.md](labels-and-control-signals/reference.md#label-axes); the retired `from-review`, `origin/review`, and `origin/reviewer` names are accepted in config and migrated on open items). Each issue is titled `[reviewer] Review backlog (<perspective>): <finding>` — like every issue the hive files, the title starts with the filing agent in square brackets (`issueshape.AgentIssueTitle`, the one function every hive issue-creation path runs its title through — `CreateIssue`, the raw advisory/recommendations/upstream-watch creates and the egress proxy — so no filer can forget it; it puts the tag first, never doubles it, and title dedupe ignores it so items filed before the tag still match). Each issue body links back to the reviewed PR and records the perspective, severity, evidence location, and finding summary. The relay deduplicates against open issues through the normal issue-create path and also persists a finding key in `/data/review-backlog-issues.json`, so re-reviewing a new commit does not file the same backlog issue or summary comment again.

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
| `reviewing` | Review perspectives are dispatched for the current head and not yet aggregated, or the head's aggregate is `incomplete` (every delivered perspective approved; the rest are still owed). |
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

When the review carries `<!-- hive-redundant-of: #N -->` (the reviewer found a merged PR that already contains the change) and the score is below the *safe* band, the line is replaced by `**no-op merge: duplicate of #N**` so a redundant PR is not labelled as bad code.

The same marker is the identity of the decision question. Two decision-requested reviews on one PR with the same `hive-redundant-of` (or, without one, the same question once mentions, item numbers, digits and punctuation are stripped) are one question however a model reworded them: the relay refreshes the hive's standing `COMMENTED` review in place instead of posting again, the 085 entry comment is not repeated, and a hive-authored PR with that marker is never self-approved — it goes to the empty-diff closer (see [hive-merge.md](hive-merge.md#empty-diff-prs-of-the-hives-own)). A PR whose diff against its base is empty re-stamps its prior verdict on every new head rather than drawing a fresh review round ([#15499](https://github.com/hivecommons/hive/pull/15499)).

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
  self_authored_decision_policy: resolve    # resolve (default) non-risk-class Hive-authored decision questions, or escalate all of them
  # all_authors: true                       # deprecated no-op; every open PR is reviewed regardless of author
  fix_human_prs: false                      # optional; also answer review-bot threads on PRs Hive did not open, by suggestion only (default off)
  contributor_prs:
    base_sync: false                        # optional; let GitHub update-branch unstick hive-authored fork PRs (default off)
  priority_labels: false                    # optional; mirror the PR review queue rank onto review-priority/* labels (default off)
  require_labels: []                        # optional; review ONLY PRs carrying one of these labels (default empty = every open PR); see "Only review labelled PRs"
  pending_ttl: 45m                          # optional; re-dispatch a review that has posted no verdict after this long (default 45m)
  pending_fix_ttl: 2h                       # optional; dispatch the next bounded fix attempt when a fixer has pushed no new head after this long (default 2h; negative disables expiry)
  max_redispatch: 3                         # optional; expiries per head before the PR is held for a human (default 3)
  bot_author_model: ""                      # optional; model (or backend/model) that reviews PRs opened by bot_authors, e.g. gpt-6-luna (default empty = off)
  bot_authors: [dependabot[bot], renovate[bot], github-actions[bot], "*[bot]"]  # optional; logins routed to bot_author_model; the hive's own App never matches
  bot_author_full_review_paths: [".github/workflows/**", "Dockerfile*", "**/Dockerfile*"]  # optional; a bot PR touching these keeps the full-strength model
  delta_rereview: true                      # optional; re-review a PR already judged at an earlier head by its diff since that head (default on)
  delta_trivial_lines: 20                   # optional; a delta under this many lines in files the prior verdict passed re-stamps it without a model call (default 20; negative disables)
  delta_max_age: 72h                        # optional; a changes_requested verdict with findings older than this anchors no delta, the PR is reviewed in full (default 72h)
  severity:
    block_at: P2                            # optional; P1 | P2 | P3, see "Blocking line" below
    comment_below: true                     # default true
    backlog_below: true                     # default true
  backlog:
    destination: github_issue               # github_issue | github_project | linear | jira
    labels: [agent/reviewer]                # default [agent/reviewer]; from-review, origin/review, and origin/reviewer are mapped to it
    max_per_pr_per_day: 10                  # default 10
```

### Blocking line (`review.severity`, `review.backlog`)

### Only review labelled PRs (`review.require_labels`)

`review.require_labels` is the PR-side counterpart of the issue approval gate
`project.issue_filter.require_labels` ([agent-configuration.md](agent-configuration.md#label-policy-which-issues-agents-may-work)):
a team that wants hive reviews **only on PRs a maintainer has opted in** names
the label(s) here, and the hive reviews nothing else.

```yaml
review:
  require_labels: [hive-review]   # a PR is reviewed only if it carries at least one of these
```

Dashboard: **Settings → Features → Review Gate → "Only review labelled PRs"**
(toggle + comma-separated label list; saved through `PUT /api/config/review`
into the dashboard overlay, so it survives restarts and ACMM pack re-applies —
it is a `review.*` key, not an agent block).

Semantics:

- **Absent/empty = no gate.** Every open PR stays reviewable; no existing hive
  goes quiet on upgrade.
- A PR must carry **at least one** listed label. Matching is case-insensitive
  and **exact** (`approved-for-review` does not satisfy `approved`).
- **Enforced server-side in three places**, so a prompt cannot route around it:
  the cadence reviewer's `${PR_LIST}` and the fan-out dispatch planner never
  offer an unlabelled PR (log line `review label gate withheld PRs from …`),
  and the `hive-review` relay refuses any request — review, thread reply,
  resolution or bare `record_verdict` — on a PR that lacks the label after
  re-reading its labels with the App token (`.denied`, audit
  `review_label_gate_refused`). A failed label fetch is retried, never waved
  through.
- The reviewer's kick carries a `REVIEW LABEL GATE` notice naming the labels,
  mirroring the issue-filter notice.
- Labels are **never created** by the hive; name one the repo already
  maintains. Exempt/hold labels still exclude a PR that also carries an
  approval label.
- It is independent of `review.require_approval`: a hive that gates merge on
  review AND gates review on a label simply does not merge unlabelled PRs
  until a person labels (or merges) them.

`review.severity.block_at` is the lowest finding priority that still blocks merge, on the P0–P3 scale (critical → P0, high → P1, medium → P2, low/info → P3). **Settings → Features → Review Gate → Severity & backlog** offers three presets: *Strict (P0–P2 block)* sets `P2`, *Ship fast (P0–P1 block)* sets `P1`, and *Everything blocks* sets `P3`. Unset keeps today's behaviour. Findings below the line are posted as non-blocking comments when `comment_below` is true (default) and filed as follow-up work when `backlog_below` is true (default).

`review.backlog` says where that follow-up work goes:

| Key | Default | Notes |
|---|---|---|
| `destination` | `github_issue` | `github_project`, `linear` and `jira` require `governor.work_source.type` to be `github_projects`, `linear` or `jira` respectively. The dashboard only offers the active work source's destination, and the owner-only `PUT /api/config/review` rejects a mismatch. If the work source later changes, the hive falls back to `github_issue` rather than failing to load. |
| `project_column_id` | — | Projects v2 status option id; required when `destination: github_project`. |
| `linear_state` | team default | Linear workflow state for new items. |
| `jira_status` | project initial status | Jira status for new tickets. |
| `labels` | `[agent/reviewer]` | Applied to every item. The retired `from-review`, `origin/review`, and `origin/reviewer` are accepted and mapped to `agent/reviewer`. |
| `max_per_pr_per_day` | `10` | Cap per PR per day. |

**Precedence with `classification.review_bots.min_priority`.** One blocking line governs both the hive reviewer and external review bots. The bot threshold resolves in this order, first non-empty wins: `hive.yaml` `classification.review_bots.min_priority` (including an explicit `all`), then `hive-project.yaml` `classification.review_bots.min_priority`, then `review.severity.block_at`. With none set, every bot finding is routed. `block_at` is never copied into `min_priority`; clearing the explicit override hands control back to `block_at`. Invalid values fail config load with an error naming the field (for example `review.severity.block_at`, `review.backlog.project_column_id`).

#### Backlog routing

When `block_at` is set and `backlog_below` is on, every cited in-scope finding (with `file` and a positive `line`) whose priority is below the line is filed to the backlog when the relay accepts the verdict. Cited out-of-scope findings go to the same destination as before (see [Out-of-scope backlog filing](#out-of-scope-backlog-filing)).

- **Destination.** `github_issue` files an issue in the PR's repo. `github_project` also adds that issue to the `governor.work_source.github_projects` project and sets its Status to `project_column_id`. `linear` files an issue on the Linear team mapped to the repo, in `linear_state` when set (an unknown state is an error naming it; label names the team lacks are skipped). `jira` files a Task in the first `work_source.jira.project_keys` project and moves it to `jira_status` when set. All three use the work-source credentials. If the destination's work source is not active or cannot be built, the item is filed as a GitHub issue with the `agent/reviewer` label added, and the hive logs why.
- **Item body.** The finding title and summary, the PR link, a `file:line` permalink at the reviewed head SHA, the severity with its P-level, the fingerprint, and the line `filed by Hive review backlog`. `labels` are applied to every item.
- **Fingerprint dedup.** Each finding is fingerprinted as `sha256(rule | path | snippet)`: the title, the cleaned file path and the summary, lower-cased with whitespace collapsed. Line numbers are left out, so the same nit at a shifted line still matches. `/data/review-backlog-issues.json` keeps a per-repo map of fingerprint → item. When another PR raises a known fingerprint, the hive comments on the existing item with the new PR link and permalink instead of filing a second one. The same PR raising it again changes nothing. When the destination folds a new finding into an existing open item instead, the item receives the full finding — title, `file:line`, summary, severity, PR link and fingerprint — under an "Additional finding (folded in — same finding area and category)" heading with an unchecked task-list line, still tagged with the `hive:review-backlog-link` marker; the PR summary lists it as `#T (folded into an existing issue) — <finding title>` ([#13604](https://github.com/hivecommons/hive/issues/13604)). The `github_issue` sink folds into an open hive-filed issue that cites the same files only when that issue is in the same category (its `Review backlog (<perspective>)` title or `Perspective:` line matches the finding's perspective) and it either cites the same file within 10 lines of the finding or names a symbol the finding's title names. The same check applies to an open issue whose title differs from the finding's only after its `Review backlog (<perspective>):` prefix; only an exact title match is reused unconditionally. Sharing a file alone is not enough: the finding is filed as its own issue, which gets a `hive:review-backlog-same-file` comment reading "Same file as #N" ([#13906](https://github.com/hivecommons/hive/issues/13906)).
- **One item per finding per PR.** Each item filed from a PR is also remembered under that PR with its evidence file, line and title (`pr_findings` in the ledger). A later report from any perspective or at any head that names the same finding — same file and the same line, a shared code symbol, or the same title once its wrapping is stripped — is tracked against the existing item, never filed again. A report whose title is only a carry-over wrapper ("Previous style finding still present (non-blocking)", "Prior correctness note still present: …", "Previous security finding (…) unchanged at new head") or that carries the `<!-- hive-refers-to: #N -->` marker is a *reference*, not a finding: it is matched against the PR's tracked items and otherwise dropped with a log line, so one finding on one PR can no longer become three issues from three perspectives (hivecommons/hive#15627, #15628, #15629). The hive notes a tracked finding's persistence at most once per head, with one `Still present at <head> (<perspectives>).` comment on the item (counted against the daily cap). Title matching ignores a leading `[reviewer]` role tag, so an existing issue dedupes whichever filing path titled it.
- **Split on host close.** A folded finding lives only in its `hive:review-backlog-link` comment, so closing the host used to close it too. Every ten minutes, alongside the closed-issue stage sweep, the hive reads the hive-authored link comments on recently closed backlog hosts (the issues the ledger points findings at) and re-files each still-unchecked `- [ ]` finding as its own `github_issue` backlog item headed `hive:review-backlog-split` and "Split from #N (host closed)", comments the new link on the host under the same marker, and moves the finding's ledger item and fingerprint records to the new issue (or to the open twin the sink folded it into, which then carries the folded comment so its own close splits it again). A checked `- [x]` finding stays with the host. A host closed as not planned or duplicate keeps the findings that match it by the fold rule above and only splits out unrelated ones; the host itself is exempt from the issue filer's rejected-twin gate for that filing (a fold cites the host's own files, so the just-rejected host would otherwise always count as the rejection), while a rejection elsewhere still suppresses the finding and is logged and counted as `skipped`. `/data/review-backlog-issues.json` also keeps `split_keys`, link-marker key → the closed hosts it was split out of, so re-runs are idempotent. One pass files at most 20 findings across all repos; hosts the pass did not reach once that cap was hit are counted and logged as `deferred`. A host that received at least one split is bumped by its cross-link comment and re-surfaces in the next pass, but a deferred host keeps its old `updated_at` and is only revisited while it stays inside the pass window (the last ten-minute interval plus ten minutes of slack), so a backlog of more than 20 unchecked findings closing within one window may leave the trailing hosts unsplit; the `deferred` count in the sweep log is the signal to look for ([#13914](https://github.com/hivecommons/hive/issues/13914)). Hosts closed before the windowed sweep reached them are covered by a backfill pass that runs after each split sweep: it walks the ledger's host index, fetches each host directly and splits every closed one through the same idempotent path. A backfill pass that split, skipped or deferred anything logs `review backlog split backfill pass complete` with those counts. Once a pass visits every host without hitting the per-pass cap (including a host left part-split when the cap ran out) or a transient error, `/data/review-backlog-issues.json` records `split_backfilled` and later passes are no-ops; an incomplete pass logs `review backlog split backfill incomplete, retrying next pass` and the next pass retries. Clearing `split_backfilled` re-runs the walk ([#13947](https://github.com/hivecommons/hive/issues/13947)).
- **Cap.** At most `max_per_pr_per_day` items are filed or updated per PR per UTC day. Findings over the cap are retried on the next review. Out-of-scope findings also stay within `review.max_out_of_scope_backlog_issues` per PR.
- **Audit.** Each filing pass that wrote anything records one `review_backlog_batch_routed` entry on the PR (`destination`, `filed`, `updated`, `capped`). Each new GitHub issue also gets its own `review_backlog_issue_filed` entry; an issue split out of a closed host carries `split_from=<host>` and `reused`, and the dashboard audit log records one `review-backlog-split` entry per split. The PR receives one summary comment that lists the items filed.
- **Blocked on the PR.** Every item the backlog creates is parked on the reviewed PR with the `blocked` label and the `<!-- hive:blocked-on pr=owner/repo#N -->` marker, in every destination ([#13833](https://github.com/hivecommons/hive/issues/13833)). A below-line or out-of-scope finding is almost always about lines the PR changes, so working it on the base branch first is wrong or conflicts. When the cited file only exists in the PR the note says so ([#13093](https://github.com/hivecommons/hive/issues/13093)); otherwise it says the finding is about code the PR is changing, and tells whoever picks it up to check it still applies after the merge and to close it without a PR if it is already fixed. The ledger records the PR each item waits on, and `ReconcileReviewBacklogBlocks` (every governor cycle) resolves it: when the PR **merged** the hive itself checks whether the finding still applies on the PR's base branch — the evidence file gone, or none of the code symbols the finding's title names left in it, closes the item as completed with a one-line note (`review_backlog_issue_closed` audit entry; GitHub-issue destination only); otherwise, or when the check cannot tell, the block is lifted with a one-line note and the item becomes actionable; the PR **closed unmerged** also lifts the block, with a `<!-- hive:review-backlog-retriage -->` note ("source PR #N closed unmerged — re-triage against current base"), and leaves the item open for normal triage and dedupe. The hive never closes or cancels a backlog item because its PR closed: a closed PR is often superseded by one that lands the same lines, so the finding may still apply. Closing it as not planned stays a human (or `/fixed`) action. Destinations without a resolver (Jira, GitHub Projects) get the note only. A finding folded into an existing open twin keeps the twin's own state ([#13364](https://github.com/hivecommons/hive/issues/13364)).
- **Heads mode.** With [review heads](#review-heads-several-models-per-perspective), only the decision step's decided reports file backlog items; raw head reports never do, so two heads' wordings of one finding cannot become two items. Instead of the once-per-PR summary comment, the relay appends a **Follow-ups filed** section to the decided review itself (edited in place, the App's own COMMENTED review only), listing every item that head's findings map to — filed, re-raised onto an existing item, or already tracked — so each head commit's review says where its nits went.

### Verdict labels (`review.verdict_labels`)

```yaml
review:
  verdict_labels:
    reviewing: hive-review/in-progress
    approve: hive-review/approved
    changes_requested: hive-review/changes-requested
    requires_human: hive-review/needs-human
```

Each governor cycle mirrors the current head's review state onto exactly one of these labels ([#13833](https://github.com/hivecommons/hive/issues/13833)): `reviewing` while the head has review kicks pending (or an incomplete aggregate), then the settled aggregate verdict (`reject` maps to `requires_human`). Stale ones are removed, a new head clears the previous head's label until its own review starts, and PRs already labelled correctly cost no API calls (at most 25 writes per cycle). A PR that merges or closes while carrying one of these labels loses it: every 10 minutes the governor searches each configured owner for closed PRs with a verdict label and removes up to 25 ([#14361](https://github.com/hivecommons/hive/issues/14361)). It is independent of the automerge engine's `stage/0NN` labels, so a hive with repo auto-merge off still shows the outcome on the PR. Every key is optional; an empty block is off. Like `human_decision_label`, the labels are never created — a repo without them is skipped with a warning — and they never block anything.

The confidence score also feeds the **PR review queue** (`GET /api/review/queue`), which ranks every open PR - agent- and contributor-authored - by triage class, confidence band, CI state and age, with the reasons for each position. An unreviewed PR ranks as *needs attention*, never *safe*. See [review-queue-triage.md](review-queue-triage.md#pr-review-queue).

When `review.require_approval` is false or omitted, `merge-eligible.json` is produced as before. When true, a PR is included only if `review-verdicts.json` contains an aggregate `approve` for the same repo, PR number, and head SHA.

For Hive-authored PRs, `review.self_authored_decision_policy: resolve` (default)
keeps reviewer decision questions advisory unless they are a **risk class**.
The test is a denylist, not an allowlist: a question escalates to
`stage/085-needs-decision` only when its wording lands in one of the named
classes in `review.DecisionRiskClasses` (`src/pkg/review/review.go`) or the
reviewer marked it `<!-- hive-decision-risk: high -->`; every other question —
naming, sequencing, defaults, config shape, scope, changelog wording, or
anything the list never anticipated — is resolved by the hive itself and
recorded on the approving aggregate as
`decision resolved by policy: <topic> — not a risk question; recorded as advisory: <question>`.
(Before [#13020](https://github.com/hivecommons/hive/pull/13020) the policy
only resolved questions matching a short allowlist of non-risk words, so a
green agent PR whose linked issue had already specified the default still
parked for a person.) The risk classes are:

| Class | Escalates when the question names … |
|---|---|
| `permissions-credentials` | permissions, privileges, tokens, secrets, credentials, write/admin access, grants |
| `security` | security, vulnerabilities, injection, crypto/TLS/certificates/signing keys, auth/authn/authz/OAuth/SSO |
| `sensitive-paths` | `.github/workflows`, OWNERS/CODEOWNERS, branch protection/rulesets, and every path in the sentinel `sensitive_path` default list (`hive.yaml`, `policies/`, `Dockerfile`, `go.mod`, `deploy/`, `.env`, …) |
| `data-loss-irreversible` | data loss, delete/drop/truncate/purge, destructive or irreversible actions, migrations, breaking or public-facing changes, force-push |
| `merge-trust-policy` | merge or auto-merge policy, trust tiers, ACMM levels, roles, approvers, governance, policy |
| `external-side-effects` | publishing, releases, deploys, billing/spend/cost, outbound email or notifications to humans, webhooks, registries |
| `licensing-legal` | license, copyright, legal, privacy, compliance, trademark, CLA |
| `unverifiable` | the reviewer says it could not read, judge or test the change, or the requirement is ambiguous |

Set the policy to `escalate` to preserve the older behavior where every
decision marker parks the PR for a person. The reviewer template
(`policies/reviewer-queue.md`) also tells the swarm to check the linked
issue/spec and PR body before asking, and to cite the answer
(`per #<issue>: …`) instead of raising a question the issue already settled.

**Contributor and human-authored PRs: no decision in CI/Review unless the
review found something fishy or insecure.** The owner's rules
([#15640](https://github.com/hivecommons/hive/issues/15640)), verbatim:

> there should be no decisions by a human while these are in ci/review —
> unless the review found something.
> unless you find something fishy and insecure happening at the pr in
> progress level, then we can let things go and then decide in merge
> queue, if needed.
> we dont want to create a log jam during pr in progress.
> i always have the last word when it comes to merge of untrusted.

So while a PR the hive did not author is in CI/Review, the hive runs CI and
the review swarm to completion on its own, and the stage always has an
active mover (CI, the swarm, the clanker fixing, the hive adopting). The
**only** early exit to `stage/085-needs-decision` is a concrete security /
trust finding — one predicate, `review.IsSecuritySensitiveFinding`
(`src/pkg/review/security_finding.go`):

- the reviewer's `<!-- hive-decision-risk: high -->` marker;
- the security perspective withholding approval (`requires_human`);
- a decision question in a security-class risk class — `security`,
  `permissions-credentials`, `sensitive-paths` (workflows, CODEOWNERS,
  branch protection, deploy, Dockerfiles, dependency manifests, …);
- an in-scope finding on a sensitive path: `sentinel.DefaultSensitivePaths`
  plus the operator's extra globs in `security.sensitive_paths`;
- an in-scope finding, or the review summary, flagging insecure or
  suspicious behaviour (injection, exfiltration, secrets, unpinned actions,
  supply chain, …).

A sentinel alert (`contributor-lane:sentinel-blocked`), a workflow-path or
tests-removed lane rule and a reporter-trust hold are the lane-side
equivalents and park the PR the same way. The finding is recorded as
`decision_finding` and the 085 comment and tracker popover print it as
`the review found: …` next to the reviewer's question, so the operator is
told WHAT was found.

Everything else the review raises — ordinary nits and findings, a duplicate,
naming, scope, sequencing, licensing, a failing optional check, a conflict
— never asks a person during CI/Review: a finding goes back to the
clanker / adoption path as `changes_requested`; a bare question is
advisory, the aggregate is `approve`, recorded as
`no security finding — the PR keeps moving; a person has the last word at the merge gate; recorded as advisory: <question>`,
and the PR moves on. At the merge gate the untrusted-author invariant
holds: a contributor the lane cannot vouch for parks at
`stage/090-ready-for-human-merge` (`contributor-lane:claim-tier-untrusted`,
`untrusted-author-role`, the human-merge rule) and waits for the operator's
merge-now / queue-merge — the hive never auto-merges an untrusted author's
PR, green, approved and soaked or not. `self_authored_decision_policy:
escalate` restores the old park-on-every-marker behavior for these PRs too.
review. The same `self_authored_decision_policy: escalate` switch restores
the old park-on-every-marker behavior for these PRs too.

Two decision questions never reach the swarm or a person at all, because
the hive already owns the answer:

- **"Should this hive PR be closed in favour of #M?"** A hive-authored PR
  whose closing targets are all closed, one of them by a *different* merged
  PR #M, and whose head conflicts with the base (or whose files #M already
  covers) is superseded. With `governor.auto_close_superseded` (default
  on) the self-authored lane closes it as `hive/superseded` with a notice
  naming #M *before* dispatching any review, the supersession sweep does
  the same on a conflicting rebase, and the close-on-merge of #M closes it
  immediately — no reviewer cycle, no `stage/085-needs-decision`
  ([#14965](https://github.com/hivecommons/hive/pull/14965) asked the owner
  this after #14957 merged and sat parked for over an hour). Human-authored
  PRs keep the notice and the decision.
- **A `requires_human` on a PR the hive adopted.** When a contributor PR is
  adopted in supersede mode (stage 074, superseding App PR #N carries the
  head), the swarm's question is answered on #N. The original's 085 yields
  to the adoption in the adopting pass: it leaves the merge gate for the
  CI/Review column with an "Adopted → #N" pill, and is closed as
  superseded when #N merges
  ([#14975](https://github.com/hivecommons/hive/pull/14975) → #14980,
  [#14947](https://github.com/hivecommons/hive/pull/14947) → #14977). A
  hold, sentinel alert, `needs-human` a person applied, fork guard or
  manual-merge lane rule still keeps 085. See
  [labels-and-control-signals.md](labels-and-control-signals/stage-labels.md#supersession-closed-issue-another-merged-pr).

`review.shadow` (default false) evaluates the same approval gate (including the unavailable-artifact case) without enforcing it. A PR that is otherwise merge-eligible stays eligible; the result is recorded as `shadow.review_gate.would_have_held` (with reason and confidence) on the PR head's evidence bundle and logged. Shadow never blocks a merge and never weakens `require_approval: true`, which keeps blocking as above. Recording needs `evidence` enabled and an existing bundle for the head; if `evidence.signing_key_path` points at a key that exists but fails to load, the shadow write is skipped with a warning rather than rewriting a signed bundle unsigned (an absent key path or missing key file still records unsigned).

`review.fan_out` is separately defaulted to false. When both `require_approval` and `fan_out` are true, the governor eval cycle plans review kicks for agent-authored PRs that do not yet have a fresh aggregate verdict for their current head SHA.

## Reviewer selection

**Settings → Features → Review Gate → Reviewers** lists every configured agent with its
engine and model as a pick-list bound to `review.reviewer_agents`; it shows how many agents
are review-capable under the current selection and, when **Fan out to multiple reviewers**
is on, warns if they all run on one engine (multi-model review needs reviewers on at least
two engines) and notes when **Review every perspective in one session** is also on, since
combined mode dispatches only the first eligible reviewer. The same section carries the
fan-out toggle and **Max parallel reviews**.

An agent is considered review-capable if it is enabled, not paused, and not on-demand,
**and** one of the following is true:

1. **Explicit list** — `review.reviewer_agents` names the agent, or the base agent a
   replica was expanded from (`replicas: N` lanes qualify through their base, so the list
   never needs each replica spelled out). When this list is non-empty, only agents on it
   and their replicas qualify; the keyword scan below is skipped entirely.
2. **Keyword scan** — the string `review` (case-insensitive) appears in any of:
   the agent's name, `role`, `aliases`, `lane_keywords`, or `detect_keywords`.

The dashboard security section (the `security` block of `GET /api/config/governor`) exposes a `reviewCapableAgents`
count, and `GET /api/config/review` carries a `reviewer_candidates` list (name, backend,
model, `available`, `keyword_match`, `review_capable`) that the Reviewers pick-list renders. If `review.require_approval` is true and that count is zero, the dashboard
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

Source: `ReviewCapable` in `src/pkg/review/dispatch.go`, which the dashboard
(`dashboardAgentReviewCapable` in `src/pkg/dashboard/status_builder.go`), the relay and
the merge-eligibility gate all call.

## Review heads: several models per perspective

`agents.<name>.review_heads` gives one base reviewer several model "heads" ([#13542](https://github.com/hivecommons/hive/issues/13542)). Each head is materialized like `replicas`: `<base>-<alias>-<n>` copies the base agent with only `backend` and `model` replaced, is stripped on save, and is provisioned at boot by the entrypoint. `alias` defaults to a slug of `<backend>-<model>` (without repeating the backend when the model already starts with it). The agent's Unix user and token group are `hive-<name>`, and group names are limited to 32 characters, so `<base>-<alias>-<n>` may be at most 27 characters; a longer name fails config load, so set a shorter `alias`.

```yaml
agents:
  code-reviewer:                     # the base; not dispatched as a head
    backend: claude
    model: claude-sonnet-5-5
    role: reviewer
    review_heads:
      - {backend: claude, model: claude-sonnet-5-5, alias: sonnet, replicas: 2}   # code-reviewer-sonnet-1, -2
      - {backend: codex,  model: gpt-6-sol, alias: sol}            # code-reviewer-sol-1
review:
  combined_perspectives: false
  reviewer_agents: [code-reviewer]   # naming the base admits its heads
```

With split perspectives, every perspective is dispatched once per head, to a replica of that head with no review kick in flight (see *One kick in flight per agent* below); the base and its plain `replicas` are not dispatched (they are reserved for the decision step). Because head dispatch asks every perspective of every head, a reviewer lane *without* `review_heads` alongside one with them would have nothing left to review, so config load rejects that mix under `combined_perspectives: false` (under `combined_perspectives: true` it still loads, and the hive logs a `review heads config` warning at boot that the plain lanes receive no combined perspectives while the heads cover them): every admitted reviewer (the `reviewer_agents` list, or every agent carrying the `review` token when the list is empty) must declare `review_heads`, or be left out. Only heads of admitted bases are expected by the aggregate. Each kick costs one `max_parallel_reviews` slot, and `max_perspectives_per_pr` still bounds how many perspectives a PR head receives. A head reviews **blind** (it is told not to read other reviews of this head) and **records only**: it delivers its verdict with `hive-review --record-verdict` and posts nothing on the PR. The relay stamps each verdict with the head from the dispatch record it bound to (an advisory verdict with no dispatch row carries none) and keeps one report per perspective per head; `review_model` is stamped per head, so each head's report names the model that head ran.

Heads have no `review_models` pool of their own (the head is the model).

**Combined heads.** Under `combined_perspectives: true` ([#15058](https://github.com/hivecommons/hive/issues/15058)) each head gets **one** record-only kick per PR head carrying every perspective it still owes, instead of one kick per (perspective, head): with 2 heads and 5 perspectives a PR costs 2 head kicks plus the decision rather than 10 plus the decision, and each model reads the diff once. The head records one JSON array with a verdict per perspective through a single `hive-review --record-verdict`; pending entries stay one per (perspective, head), so settling, expiry and `head_max_retries` work per pair, and a head that answers only some perspectives is re-asked for the rest only. As with any combined review, `max_perspectives_per_pr` does not apply and the kick costs one `max_parallel_reviews` slot. The decision step is unchanged.

Inside that one kick the head **fans out**: it fetches the PR metadata, the diff and any linked issue or contract once, then starts one sub-agent per perspective in parallel (the Task tool on Claude Code, the backend's sub-agent facility on Codex and others), handing each the diff, the contract text, the shared instructions and its own perspective's focus (including any `review.perspective_prompts` override). Every sub-agent is launched on the head's own model (the kick names it), so the per-head `review_model` stamp stays true; the per-perspective sub-agents replace the single pinned sub-agent of the independent-review instruction rather than adding to it, and a head whose sub-agent facility cannot pin its model reviews the perspectives itself. Sub-agents return one verdict object each; they post nothing, never call `hive-review` and do not refetch the PR. The head collects them into the one JSON array and records it once. Where sub-agents are unavailable the head reviews the perspectives one by one in the same session; the array contract does not change. A kick that re-asks a single perspective has nothing to fan out and carries no fan-out section. The point is latency: a head kick then takes about as long as its slowest perspective instead of the sum of all of them.

### The decision step

The heads' raw reports are evidence, never a verdict: while any head report exists without a decision, the PR head aggregates to `incomplete` (`Perspectives` shows the strictest head verdict and `heads` which heads answered, for the queue), so nothing routes on a single head.

1. **Settled.** Once every expected head has reported every perspective, or ran out of retries (below), the aggregate is marked `awaiting_decision` and the planner sends **one decision kick per PR** to a base reviewer with no review kick in flight. It costs one parallel slot and covers every perspective the heads judged; one pending entry per perspective (`decision: true`) binds the reports it records.
2. **The kick carries every head's full report**: verdict, every finding with its severity, scope, summary and `file:line`, the summary and any decision question, plus any head that never answered. The base reviewer re-reads the code, gives **every** head finding a disposition (`confirmed`, `not_reproducible` or `out_of_scope`) with a reason, de-duplicates findings raised by several heads, sets the **final severity** of each confirmed finding (blocking: correctness, security, data, a broken contract or a missing required test; backlog: docs wording, style, naming, behaviour-neutral refactors), and decides each perspective's verdict.
3. **One comment.** The base posts the PR's only review comment with `hive-review --comment --verdict-file`: findings under per-perspective headings tagged with the heads that raised them, and a `### Head findings` section listing every head finding with its disposition. The relay stores the reconciled verdicts as **decided** reports (`decided: true`, with `dispositions`), set from the dispatch record, never from the reviewer's JSON, in their own `…-decided` files beside the heads' raw reports.
4. **Routing is unchanged.** The aggregate is built from the decided reports, so `review.severity.block_at`, `comment_below`, `backlog_below`, `review.backlog`, out-of-scope backlog filing, fix kicks and human holds all run on the decision's final severities exactly as on any review. The aggregate keeps `head_reports`, `dispositions` and `missing_heads` as evidence.

**Skip rule.** When every head approved every perspective with no findings and no decision question, there is nothing to reconcile: the heads' unanimous approval is the decision and no session runs (`decision_skipped`). With `review.acknowledge_no_findings` on, the hive posts one line, once per PR head (`**Reviewed** — no findings from …, all N review heads agreed: …`); with it off, nothing is posted.

**Retries.** A head whose review of a perspective outlives `pending_ttl` is re-sent up to `review.head_max_retries` times (Settings → Review gate; 0 means `max_redispatch`). Past that, if another head answered the perspective, the head is recorded as missing (`missing_heads` in the dispatch state and the aggregate): the decision proceeds without it and the comment says so, e.g. `sol-1: no verdict after 3 attempts; decided on claude-sonnet-5-5 only`. If **no** head answered the perspective, the head SHA's `max_redispatch` budget and human hold apply exactly as before.

**Accuracy.** `GET /api/reviewer/accuracy` gains `review_heads`: per review model, how often its head's verdict agreed with the decided verdict (`missed_blocks` when it approved what the decision blocked, `over_blocks` the reverse), and how many of its findings the decision confirmed or rejected (`confirm_rate`). Only decided and skip-rule aggregates are scored; the decision is the reference, so no merge outcome is needed.

## Dispatch and prompt construction

`pkg/review` provides prompt builders for one prompt per perspective plus a sequential fallback prompt. These prompts instruct review-capable agents to emit the extended AgentReport JSON shape above.

**Who gets reviewed: every PR, regardless of author.** When review is on (`review.require_approval` and `review.fan_out`), the planner dispatches reviews for every open PR in the hive's repositories — hive-agent PRs, human PRs, clanker PRs at either trust tier, dependabot and any other bot. There is no author-class filter and no opt-out below the review feature switch itself; the only way to stop reviews is to turn review off ([#12974](https://github.com/hivecommons/hive/issues/12974): *"if pr review is selected in hive, then all prs should be reviewed"*). Before this, the swarm reviewed only agent and `[bot]` authors unless `review.all_authors` was set, and human PRs such as hivecommons/hive#12966 reached merge with zero swarm reviews. Author kind still decides what happens *after* the verdict — who may be pushed to (below), which auto-merge lane may act on it ([hive-merge.md](hive-merge.md#self-merge-review-gate)), and whether the hive may approve at all (a contributor-tier clanker's PR is reviewed like any other, then still parks at `090-ready-for-human-merge`; the hive never approves on their behalf).

**Who may review a PR.** A PR's author never reviews it, and neither does any lane in the author's family: when the author is a replica (`replicas: N`), its base agent and sibling replicas are excluded too, and when the author is a base agent, all of its replicas are. The author's base is resolved from the full configured agent set, enabled or not, so an authoring replica that is itself paused, disabled or not review-capable still rules out its siblings; a replica name that a reduced `replicas:` count no longer materialises still maps to its declared base. The verdict relay applies the same family rule: a verdict no dispatch row binds is refused as `author_self_approval` when the submitting lane shares the author's base.

Phase 2 adds dispatch state in `/data/review-dispatch-state.json`. Pending review kicks are scoped by repo, PR number, perspective, and head SHA; when a PR head changes, stale pending entries are pruned and the new head must be reviewed again. Multiple review-capable agents receive perspective prompts in parallel up to `max_parallel_reviews`; a single reviewer receives one perspective per eval round.

**One kick in flight per agent.** An agent's CLI works one prompt at a time, so the planner never sends a review kick (per-perspective, head, combined or decision) to an agent that still has one pending, i.e. dispatched and not yet answered or expired. It picks the first reviewer in name order that is free; when every eligible reviewer (or every replica of a head) is busy, that work waits for a later cycle. Waiting records nothing, so it costs no `max_redispatch` or `head_max_retries`, and `pending_ttl` starts when the kick actually goes to an idle agent. Load is counted in kicks, not PR heads: counting PR heads let replicas holding the same PR tie, and one PR's remaining perspectives all went to the first replica, four kicks typed into one Claude session within three minutes, three of whose verdicts never arrived.

### Reviewer autoscale (`review.reviewers.autoscale`)

By default the reviewer lane is a fixed pool: `agents.reviewer.replicas: 2` runs `reviewer` and `reviewer-2`, and when both have a kick in flight every other PR waits. `review.reviewers.autoscale` ([#14685](https://github.com/hivecommons/hive/issues/14685)) instead lets the lane grow and shrink with that wait. It is off by default.

```yaml
review:
  reviewers:
    autoscale:
      enabled: true
      agent: reviewer              # optional; default: first role:reviewer agent, else reviewer_agents[0]
      min: 1
      max: 4                       # capped at the replicas limit (5)
      target_backlog_per_reviewer: 2
      scale_up_cycles: 2
      scale_down_cycles: 5
      cooldown: 5m
      cpu_per_reviewer: 1.0
      mem_per_reviewer_mb: 2048
```

**Demand.** Each governor cycle the planner records the PRs it left *waiting*: PRs with perspectives still to review on their current head for which every eligible reviewer was busy (`no-free-reviewer`) or the `max_parallel_reviews` budget was spent (`no-parallel-slot`). Drafts, held PRs and PRs whose review is already dispatched are not waiting. The controller ages each wait across cycles, keyed on repo, number and head, so a push restarts the clock.

**Scale-up.** The lane is *behind* when the backlog exceeds `target_backlog_per_reviewer` × active reviewers, or when the oldest wait is older than the `stage/050-review-pending` dwell (`governor.labels.stage.dwell` overrides apply). Behind for `scale_up_cycles` consecutive cycles adds one replica. Guards refuse a wanted scale-up, in this order, and the refusal is logged with the guard's name: `at-max`; `cooldown` (less than `cooldown` since the last action); `saturated` (half or more of the active reviewers' last kicks were undeliverable — `CLI did not reach input prompt` — which is a saturated pod, not demand); `provider-budget` (the inference budget latch is on); `provider-headroom` (any provider under 10% remaining, when rotation publishes readings); `max-parallel-reviews` (more reviewers than slots is pointless); `cpu` / `memory` (the pod cgroup — `cpu.max`, `cpu.stat`, `memory.max`, `memory.current` — has less free than one reviewer's reservation; a host without cgroup limits never blocks). A refused scale-up keeps its streak, so it fires as soon as the guard clears.

**Scale-down.** The lane is *idle* when nothing is waiting and at least one reviewer has no kick in flight. Idle for `scale_down_cycles` consecutive cycles, with more than `min` replicas active and the cooldown elapsed, marks the highest-numbered replica *draining*: the planner and the cadence kick both skip it (the eval log shows `reviewer-3 (reviewer-autoscale draining)`), and it is retired — its tmux session ended and its config entry dropped — only once its turn has ended and it has no review pending. A reviewer is never interrupted mid-turn. If demand returns while a replica is draining it is simply un-drained rather than a fresh replica being added.

**What a dynamic replica is.** Nothing new: the controller raises or lowers the lane's materialised replica count and the ordinary `ExpandAgentReplicas` path creates `reviewer-3`, `reviewer-4` … with the same role, prompt, beads directory, scoped token, tmux pane and cadence key as a configured `reviewer-2`. The planner, kick governor, dashboard agent list and token cache cannot tell them apart. The entrypoint pre-creates the per-agent user and token cache for every name up to `max`, so a replica added at 03:00 can read its token. The declared `replicas:` only seeds the initial count (clamped into `[min, max]`) and is what is persisted; the live count survives a config reload and is never written to `hive.yaml`.

**Observability.** One log line per decision — `reviewer autoscale: 2→3 reason=backlog=7 oldest=41m0s`, `reviewer autoscale: 3→2 drain reviewer-3 reason=idle for 5 cycles`, `reviewer autoscale: 3→2 retire reviewer-3 reason=idle`, `reviewer autoscale: 2→3 blocked by cpu`. `/api/status.reviewerAutoscale` carries `enabled`, `agent`, `min`, `max`, `current`, `target`, `draining`, `backlog`, `oldestWait`, the streaks and `lastDecision`; the Agents section shows the lane's reviewer count with an *auto* badge (replicas show *auto* / *draining*) whose tooltip carries the last decision. The knobs live in Settings → Review Gate → Reviewers → *Scale reviewers with the backlog* (owner only; `PUT /api/config/review` sub-object `reviewer_autoscale`).

### Pending-review expiry (`review.pending_ttl`, `review.max_redispatch`)

A pending entry is the planner's only record that a perspective was asked, so a reviewer whose kick turn dies — model timeout, proxy read timeout, pod restart — leaves entries that post no verdict and, with no expiry, made every perspective look covered until someone pushed a new commit (hivecommons/hive#12045 and #12025 sat at queue position 1 for hours with CI green and the contributor lane stuck at `no-verdict`). Each cycle the planner therefore expires pending entries older than `pending_ttl` (Go duration string, default `45m`; a negative value disables expiry) whose head still has no verdict in the artifact: the expired perspectives are dropped from state and re-dispatched in the same cycle, the governor logs `review pending expired repo=… pr=… head=… perspective=… age=… expiries=N`, and the state file's `redispatches[]` counts expiries per head. Entries younger than the TTL are left alone, so a review genuinely in flight is never re-kicked, and a verdict for the head settles it however old the entries are. The same expiry releases a `revise_verdicts_before` revisit whose reviewer died.

The retry is bounded. Once a head's expiry count reaches `max_redispatch` (default 3) the planner stops re-kicking it, leaves the entries in place, and records a `requires_human` hold whose reason starts `review-dispatch-exhausted`; `review.human_decision_label` is applied the same way as for a reviewer's own requires_human verdict, and the governor logs `review dispatch exhausted; holding PR for a human`. A new push clears the counter and the hold along with the head's pending entries.

Expiry also covers a head whose aggregate is `incomplete`: the delivered perspectives are taken off the pending list (their reports exist) and only the undelivered ones expire and re-dispatch, against the same per-head budget.

The PR review queue (`GET /api/review/queue`) shows this state on unreviewed items: `pending_since` (first dispatch for the current head still pending) and `redispatch_count`, plus a reason such as `review dispatched 32m ago, no verdict yet (re-dispatched 2x after expiry)` or `review dispatch exhausted: …; waiting on a human` after the generic `not reviewed at this head` line.

When an aggregate verdict is `changes_requested`, Hive builds a review-fix kick containing the aggregate findings and sends it to `review.fixer_agent`, the classified PR lane, or `scanner`. Fix dispatches are capped by `escalation.MaxReEngagements`; once exhausted, dispatch state records a `requires_human` hold for the PR head so the automated loop stops.

### Who may be pushed to: `all_authors` and the retired `fix_human_prs` push

A review-fix kick tells the fixer agent to check out the PR branch and push a commit to it. That is fine on a PR one of the hive's own agents opened; on a contributor's fork (reachable through GitHub's "allow edits by maintainers") or a maintainer's branch it is a bot rewriting someone else's work before any person has looked at it. Reviewing and pushing are therefore separate:

- Every open PR is **reviewed**, whoever opened it ([#12974](https://github.com/hivecommons/hive/issues/12974)). `review.all_authors` used to be the switch for this; it is now a deprecated no-op that is still accepted (so existing `hive.yaml` files load) and logged once at boot. Remove it. Being reviewed never implies being pushed to.
- `review.fix_human_prs` is retained as an audited refusal setting only. It no longer lets the fix kick be dispatched for a PR whose login author is **not** a hive agent/App (unless the hive adopted the PR in place, `stage/074-adopted-by-hive`): GitHub's squash attributes the landing commit to the PR author, so a hive follow-up commit can only pass "Sign-off survives the squash" by forging that author's `Signed-off-by`, which Hive must not do.
- `review.contributor_prs.base_sync` (default off) is narrower: it only lets PR follow-up call GitHub's update-branch API for a hive-authored lane PR whose head is in a contributor fork, whose base moved (`behind`/`dirty`), and whose author enabled "allow edits by maintainers". `behind` produces a GitHub-authored merge commit; `dirty` falls back to the base-moved repair note and kick. Fork pushes still do not pass through Hive, so stale-head prevention is a post-push note/kick rather than a true pre-push rejection.

For PRs the hive did not author, the scanner never pushes new content or fixup commits: it comments with the proposed change (or a GitHub suggestion) and leaves the author to apply it. The only sanctioned writes are the base-sync above and hive-owned cleanup of a bad commit the hive itself already pushed, which must drop/amend that commit or re-home it through the adoption/DCO supersede path; governance and policy text is never changed without the author's or a maintainer's consent.

Before every fix kick the planner checks authorship. A PR may be pushed to only when its author LOGIN is an agent (`github.ai_author` or a `[bot]` login). The audit-trail agent and the `— hive:` attribution trailer are not consulted here: under a person login the squash is attributed to that person, so a hive-signed fix commit would be rejected by the squash sign-off gate as `bot-signoff` ([#12897](https://github.com/hivecommons/hive/issues/12897), [#13124](https://github.com/hivecommons/hive/issues/13124)). That fix is withheld, regardless of `fix_human_prs`, and proposed in the review instead; the contributor relay's `fix-pr` task is the channel that lands a correctly signed commit. The one person-login exception is a PR the stage reconciler adopted in place (`stage/074-adopted-by-hive`), where adoption is the explicit decision that the hive owns the branch and fixes it there.

When the check refuses, the `changes_requested` verdict still stands and the review is still published (`post_comments`). The reviewer's kick for such a PR carries an extra instruction: no agent will push to this branch, so every fix it wants goes into the review comment as a ```` ```suggestion ```` block or a ```` ```diff ```` patch the author can apply, and the reviewer must not check out or push to the branch itself. The refusal is recorded once per PR head in dispatch state (`withheld_fixes`) and on the audit log as `review_fix_withheld`, with the PR, its author, and the setting's value at the time (`setting=review.fix_human_prs=<value>`; a reason naming the squash sign-off gate when it was on), so a fix that did not happen can be traced rather than guessed at. A new push to the PR is a new decision.

**Upgrading.** Older configs may still show `fix_human_prs: true` because a prior compatibility rule promoted `all_authors: true`. Both values are left in config for audit/UI continuity — `all_authors` no longer does anything, and branch-push fix kicks still require hive authorship.

### Cheaper model for bot-authored PRs (`review.bot_author_model`)

PRs opened by other bots (Dependabot, Renovate, github-actions) are mostly lockfile and version bumps, and over Oct 13-27 2026 they cost $38.61/day across three hosted hives ($30.60/day on the busiest), every one reviewed on the frontier model. Setting `review.bot_author_model` routes those reviews to a cheaper model:

- `review.bot_authors` is the login list. Entries match case-insensitively; a leading `*` matches a suffix, so the default `*[bot]` catches any GitHub App. The hive's own App login (`project.ai_author`, or the App's `[bot]` login when it authors PRs) never matches, whatever the list says: the hive's own PRs are reviewed as before.
- `review.bot_author_model` is `model` or `backend/model` (`gpt-6-luna`, `copilot/gemini-flash-3-5`). It replaces whatever the reviewer's `review_models` pool or `perspective_models` chose for that kick, the same way: the kick's independent-review instruction names it and the reviewer runs the review in a sub-agent with that `model`, so **the reviewer agent's engine must be able to launch it**. A pool decision to skip or hold for a human is kept. Recommended values from `pkg/tokens/pricing.go`: `gemini-flash-3-5` ($0.075/$0.30 per MTok) or `gpt-6-luna` ($0.10/$0.50), against `claude-fable-5.1` at $10/$50. Empty (the default) leaves the route off.
- `review.bot_author_full_review_paths` are globs (`**` spans directories; a pattern without `/` also matches the base name) that keep a bot PR on the full-strength model when any changed file matches. The default is CI workflows and Dockerfiles only; lockfiles are deliberately not excepted, since they are the bulk of bot PRs. Set it to `[]` for no exceptions. The planner lists the PR's files once per head (only for a listed bot's PR about to be reviewed) and a PR whose files could not be listed is **not** downgraded.

Routed kicks carry `bot_author_routed` on the dispatch kick and the model lands in the verdict's `review_model` as usual; the governor logs `review routed bot-authored PR to bot_author_model`. The route applies to the swarm's per-perspective and combined kicks; the cadence (queue-mode) kick's `review_with=` annotation still comes from the agent's `review_models` pool alone.
### Delta re-review (`review.delta_rereview`, `review.delta_trivial_lines`, `review.delta_max_age`)

A re-review is a review of a PR the hive already judged at an earlier head SHA. Over Oct 13-27 2026 re-reviews were 28% of hive's attributed reviewer spend and 53% of Bluefin's, because every one re-read the whole PR. With `review.delta_rereview` on (the default) the planner looks for the most recent verdict for the same PR at another head within the artifact's retention and, when it finds one, builds the kick differently:

- The prompt opens with a **`DELTA RE-REVIEW`** block naming the previous head, when it was judged, the aggregate and per-perspective verdicts, and the exact `gh api repos/O/R/compare/<prev>...<new>` commands for the file list and the unified diff. It tells the reviewer to read only the hunks in that compare (and enough of their surroundings to judge them), to re-check each previous finding by file and line and say whether it is fixed, still present or no longer applies, and that its verdict is still for the whole PR at the new head. When the governor fetched the compare itself, the block also lists the changed files and line count.
- When the compare is known and the delta is **empty** (a force-push or rebase with the same tree), or under `review.delta_trivial_lines` changed lines (default 20) touching no file the previous verdict recorded a finding in, no reviewer is kicked: the previous aggregate is **re-stamped** onto the new head with the new `head_sha`, `body_digest` and `recorded_at`, a `delta` block (`previous_head`, `trivial: true`, `changed_lines`, `note`) and a `delta re-review: …` reason, and the governor logs `review delta re-stamped verdict without a session`. A negative `delta_trivial_lines` keeps the re-stamp for identical trees only. A head that already has a review in flight is never re-stamped.
- The **full** review stays for a first review (no prior verdict at another head), when the prior verdict was `reject` or `incomplete`, and when it was `changes_requested` with findings recorded more than `review.delta_max_age` ago (default `72h`): the flagged code has had time to move on and the record of it is stale. A verdict at the current head is never treated as a prior.

Every delta aggregate records `delta.previous_head` (and `delta_previous_head` on each perspective report, stamped from the dispatch state like `review_model`), so the cost dashboard can split delta reviews, re-stamps and full reviews. The compare is fetched once per candidate head (no verdict, no pending entry, a prior at another head) with one 300-file page; a bigger or failed compare still gets the delta instruction, just without the file list and never the re-stamp.

## Revising a recorded verdict

Dispatch skips any PR that already carries a verdict for its head SHA. That is the right default — it stops the reviewer re-reviewing unchanged code every sweep — but it also makes a review final the moment it is written, even when the reviewer itself was defective. Two settings, both required, re-open such verdicts narrowly:

```yaml
review:
  revise_repos: [org/repo]                       # allowlist; empty (default) = no repo may be revised
  revise_verdicts_before: 2026-09-19T14:00:00Z   # RFC 3339; only verdicts recorded before this instant
```

- `revise_repos` allowlists the repos whose reviews may be edited. Silently rewriting text a maintainer has already read is a power worth granting deliberately, per repo — so the empty default means the feature is entirely off. A `--revise` request for a repo that is not allowlisted is denied once (`.denied`, result `authorization denied: repo … is not allowlisted for in-place review revision`) rather than retried on every watcher pass (#13252).
- `revise_verdicts_before` re-opens PRs whose verdict was recorded before the cutoff even though their head SHA has not moved. The cutoff is self-limiting: a complete re-review records a fresh timestamp that is necessarily after it, so each PR is revisited at most once per bump rather than looping. An incomplete re-review of a head holding a settled `requires_human`, `changes_requested` or `reject` verdict is the one exception: the settled verdict and its original `recorded_at` are retained (see verdict-flow rule 6 above), so the PR remains revisitable until a re-review with full perspective coverage replaces it. When a revisit is due, the head's pending dispatch entries are cleared so every enabled perspective is asked again, and an in-flight guard stops a revisit whose verdict is still outstanding from being re-kicked every cycle.

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
