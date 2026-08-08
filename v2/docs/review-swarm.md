# Review swarm (phase 1)

Hive's review swarm adds a structured review-to-fix decision point for pull requests. Phase 1 implements the data model, prompt construction, verdict collector, and optional merge-gate integration; governor fan-out to parallel review agents is deferred.

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

Aggregation rules are deterministic:

1. Any `reject` recommends closing the PR.
2. Any finding at or above the human threshold defaults to `requires_human`.
3. Any explicit `requires_human` yields `requires_human`.
4. Any `changes_requested` enters a fix cycle while below the fix cap.
5. All five default perspectives approving yields a merge-eligible aggregate.
6. Missing perspectives or any other non-unanimous result requires human review.

The default human threshold is `high`. Review-triggered fix cycles use the same cap value as the escalation re-engagement circuit breaker (`escalation.MaxReEngagements`) so bot loops remain bounded.

## Configuration

Merge-gate use is opt-in and preserves existing behavior by default:

```yaml
review:
  require_approval: true
```

When `review.require_approval` is false or omitted, `merge-eligible.json` is produced as before. When true, a PR is included only if `review-verdicts.json` contains an aggregate `approve` for the same repo, PR number, and head SHA.

## Prompt construction

`pkg/review` provides prompt builders for one prompt per perspective plus a sequential fallback prompt. These prompts instruct review-capable agents to emit the extended AgentReport JSON shape above.

## Deferred work

- Wire governor/scheduler fan-out so the five perspective prompts run in parallel review-capable agents.
- Feed `changes_requested` aggregates directly into an auto-fix kick lane.
- Map aggregate verdicts to labels/comments (`hold`, `needs-human`, close recommendation) once fan-out exists.
- Add dashboard visibility for review verdict artifacts.
