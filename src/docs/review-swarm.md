# Review swarm

Hive's review swarm adds a structured review-to-fix decision point for pull requests. It includes the review data model, prompt construction, verdict collector, optional merge-gate integration, governor fan-out to review-capable agents, and a bounded auto-fix cycle for review findings.

## Perspectives

The `pkg/review` package ships five built-in review perspectives:

- `correctness` — regressions, edge cases, data races, test adequacy
- `security` — exploitable vulnerabilities, unsafe permissions, injection, secrets, trust-boundary regressions
- `intent-alignment` — does the diff solve the linked issue without unrelated scope creep
- `style` — maintainability, conventions, readability, repository idioms
- `docs-currency` — documentation, examples, generated docs, operator-facing text that must change with behavior

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

Each reviewer returns a JSON object that extends the `pkg/outputschema.AgentReport` contract — or, for a combined review, a JSON array of one such object per perspective, all for the same PR and head SHA. Required AgentReport fields remain `lane`, `kind`, `findings`, `prs_opened`, `beads_filed`, and `summary`; review reports add `perspective`, `verdict`, `repo`, `number`, and optional `head_sha`. `kind` must be `review`.

Allowed verdicts are `approve`, `changes_requested`, `requires_human`, and `reject`. Finding severities reuse `outputschema.Severity`: `info`, `low`, `medium`, `high`, and `critical`.

## Verdict flow

The collector reads review report artifacts from `/var/run/hive-metrics/review-report-*.json`, validates the AgentReport envelope plus review fields, aggregates by PR, and writes `/var/run/hive-metrics/review-verdicts.json`.

Those artifacts are written by the **review relay**, not by the reviewing agent. `/var/run/hive-metrics` is owned by the hive and each agent runs under its own account, so an agent-side write is refused by the filesystem; returning the JSON in kick output stores it nowhere. The agent hands its verdict over with `hive-review --verdict-file <path>` alongside the comment, or with `hive-review --record-verdict --verdict-file <path>` when it has nothing to post. The watcher validates the report, checks it names the PR that was actually reviewed, and writes the artifact server-side — so a malformed or mis-targeted verdict never reaches the collector, which fails the whole collection on the first unparseable file.

A verdict that is never handed over is not a neutral outcome: aggregation reports "never reviewed", and the PR is dispatched for review again from scratch.

Aggregation rules are deterministic:

1. Any `reject` recommends closing the PR.
2. Any finding at or above the human threshold defaults to `requires_human`.
3. Any explicit `requires_human` yields `requires_human`.
4. Any `changes_requested` enters a fix cycle while below the fix cap.
5. Every perspective in the hive's configured set approving (or, under `max_perspectives_per_pr`, every perspective the PR was eligible to receive) yields a merge-eligible aggregate.
6. Missing perspectives or any other non-unanimous result requires human review.

The default human threshold is `high`. Review-triggered fix cycles use the same cap value as the escalation re-engagement circuit breaker (`escalation.MaxReEngagements`) so bot loops remain bounded.

## Configuration

Merge-gate and fan-out use are opt-in and preserve existing behavior by default:

```yaml
review:
  require_approval: true
  fan_out: true
  max_parallel_reviews: 5
  reviewer_agents: [reviewer-a, reviewer-b] # optional; otherwise agents with review role/keywords are selected
  fixer_agent: scanner                      # optional; defaults to the PR lane, then scanner
```

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

## Deferred work

- Map aggregate verdicts to labels/comments (`hold`, `needs-human`, close recommendation) once fan-out exists.
- Add dashboard visibility for review verdict artifacts.
