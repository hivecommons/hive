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

## Reviewer selection

An agent is *review-capable* when `dashboardAgentReviewCapable` (`pkg/dashboard/status_builder.go`) accepts it. The dashboard counts those agents and warns when the count is zero while `require_approval` is true. Selection runs in two stages, and the first stage applies to **both** configuration paths.

### Stage 1 — availability gate (always applied)

An agent is disqualified outright, before any name or keyword is examined, if any of the following is true:

- it is not enabled (`enabled: false`),
- it is paused (`paused: true`),
- it is on-demand (`on_demand: true`).

An on-demand agent is excluded even though it can be invoked manually: this count reflects agents available to the governor's automatic fan-out, not agents that exist.

### Stage 2 — which agents qualify

**When `review.reviewer_agents` is non-empty**, only agents whose names appear in that list qualify. Matching is an exact string comparison of the agent name against each list entry after trimming surrounding whitespace — it is *not* case-insensitive and *not* a substring match. A listed agent that fails the stage-1 gate still does not qualify, and no keyword scan is performed as a fallback: if every listed agent is missing, disabled, paused, or on-demand, the effective reviewer set is empty.

**When `review.reviewer_agents` is empty or omitted**, an agent qualifies if the string `review` appears, case-insensitively, in any of these fields:

- the agent's name,
- `role`,
- any entry in `aliases`,
- any entry in `lane_keywords`,
- any entry in `detect_keywords`.

The match is a substring check, so `reviewer`, `pr-review`, and `code-reviewer` all qualify.

### Making an agent review-capable

The smallest change that works on the keyword path is a `role` containing `review`:

```yaml
agents:
  reviewer:
    enabled: true
    role: reviewer
```

Or name the agent explicitly, which bypasses the keyword scan entirely:

```yaml
review:
  require_approval: true
  reviewer_agents: [reviewer]

agents:
  reviewer:
    enabled: true
```

### Troubleshooting "no enabled review-capable agents were detected"

The dashboard emits this warning whenever `require_approval` is true and zero agents pass both stages. Because the availability gate runs first, an operator who has a correctly named reviewer that is merely **paused** sees exactly the same message as one who has no reviewer at all. Check in this order:

1. **Is your intended reviewer enabled, unpaused, and not on-demand?** This is the most common cause and it is invisible in the warning text. Verify all three flags before touching any review configuration — a correct `reviewer_agents` list cannot rescue a paused agent.
2. **Is `review.reviewer_agents` set?** If it is, the keyword scan never runs. Every name in the list must match an existing agent's name exactly, after whitespace trimming; a typo, a case difference, or an alias instead of the real agent name silently contributes nothing.
3. **If `reviewer_agents` is unset, does any enabled agent carry `review` somewhere?** Search the five fields listed above. An agent whose only connection to review is an English description elsewhere in its config does not qualify — the scan reads name, `role`, `aliases`, `lane_keywords`, and `detect_keywords` and nothing else.

Until at least one agent passes, `require_approval: true` means no PR ever acquires the aggregate `approve` it needs, so merge-gate output stays empty rather than falling back to unreviewed merges.

When `review.require_approval` is false or omitted, `merge-eligible.json` is produced as before. When true, a PR is included only if `review-verdicts.json` contains an aggregate `approve` for the same repo, PR number, and head SHA.

`review.fan_out` is separately defaulted to false. When both `require_approval` and `fan_out` are true, the governor eval cycle plans review kicks for agent-authored PRs that do not yet have a fresh aggregate verdict for their current head SHA.

## Dispatch and prompt construction

`pkg/review` provides prompt builders for one prompt per perspective plus a sequential fallback prompt. These prompts instruct review-capable agents to emit the extended AgentReport JSON shape above.

Phase 2 adds dispatch state in `/var/run/hive-metrics/review-dispatch-state.json`. Pending review kicks are scoped by repo, PR number, perspective, and head SHA; when a PR head changes, stale pending entries are pruned and the new head must be reviewed again. Multiple review-capable agents receive perspective prompts in parallel up to `max_parallel_reviews`; a single reviewer receives one perspective per eval round.

When an aggregate verdict is `changes_requested`, Hive builds a review-fix kick containing the aggregate findings and sends it to `review.fixer_agent`, the classified PR lane, or `scanner`. Fix dispatches are capped by `escalation.MaxReEngagements`; once exhausted, dispatch state records a `requires_human` hold for the PR head so the automated loop stops.

## Deferred work

- Map aggregate verdicts to labels/comments (`hold`, `needs-human`, close recommendation) once fan-out exists.
- Add dashboard visibility for review verdict artifacts.
