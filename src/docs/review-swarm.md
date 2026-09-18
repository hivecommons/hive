# Review swarm

Hive's review swarm adds a structured review-to-fix decision point for pull requests. It includes the review data model, prompt construction, verdict collector, optional merge-gate integration, governor fan-out to review-capable agents, and a bounded auto-fix cycle for review findings.

## Perspectives

The `pkg/review` package defines five review perspectives:

- `correctness`
- `security`
- `intent-alignment`
- `style`
- `docs-currency`

Each reviewer returns a JSON object that extends the `pkg/outputschema.AgentReport` contract. Required AgentReport fields remain `lane`, `kind`, `findings`, `prs_opened`, `beads_filed`, and `summary`; review reports add `perspective`, `verdict`, `repo`, `number`, and optional `head_sha`. `kind` must be `review`.

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
5. All five default perspectives approving yields a merge-eligible aggregate.
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

## Deferred work

- Map aggregate verdicts to labels/comments (`hold`, `needs-human`, close recommendation) once fan-out exists.
- Add dashboard visibility for review verdict artifacts.
