# ADR-0009: Trajectory review lane

Status: Accepted (retroactive)

**Current behavior:** periodically review an agent's recent transcript for goal
drift; pause and alert on divergence by default. Review-service failures do not
halt agents. Only a proven-idle agent with no output since its last review may
skip an interval, and overdue reviews always run. The addendum below gives the
exact skip predicate; an unchanged transcript alone never qualifies.

A **lane** is a recurring category of agent work. The **governor tick** is
the scheduler's periodic evaluation, which decides when this review is due.

## Context

Per-action guards can miss goal drift: a session may take individually ordinary
steps while the overall sequence moves toward an unauthorized outcome. The
trajectory-review docs describe this as the case where only the direction of the
transcript reveals sandbox escape, scanner evasion, credential splitting, or a
goal adopted from repository content
([trajectory review](../trajectory-review.md)).

## Decision

Run a periodic trajectory-review lane from the governor tick. The lane snapshots
running, non-exempt agents with their assigned intent and recent transcript,
sends a bounded transcript tail to a second model through the existing
OpenAI-compatible/LiteLLM endpoint, and asks for a compact JSON verdict
([reviewer](../../crux/trajectory/trajectory.go),
[lane](../../crux/trajectory/lane.go)). On divergence, the configured response is
to pause the agent and alert by default, or to alert only. The reviewer prompt
instructs the model to judge direction, not single commands, and to answer
non-divergent when unsure.

Reviewer errors fail open. Endpoint outages, timeouts, non-OK responses, and
unparsable replies are logged and skipped so a broken oversight model cannot
pause an otherwise working fleet.

## Consequences

Hive gains semantic oversight for behavior that static rules and API gates do
not see, while still reusing existing pause, alert, and audit primitives. Cost
is bounded by transcript tail size, token cap, and cadence. The trade-off is
intentional: fail-open behavior means an unavailable or weak reviewer catches
less rather than halting work, and the lane can only judge rendered transcripts,
not hidden model reasoning.

## Addendum (2026-10): skip predicate for proven-idle agents

Status: Accepted. Refs hivecommons/hive#12132, RFC hivecommons/hive#12134.

The lane originally reviewed every running, non-exempt agent on every interval
with no change detection — roughly 6K input tokens per agent per interval even
when nothing had happened. The token-usage review proposed skipping reviews
whose transcript tail had not changed. That proposal was rejected as written:
under the Context above, an unchanged or stalled tail on an agent whose turn is
still in progress can itself be the warning signal, so **"transcript bytes
identical" is never a skip condition**.

The lane now skips a review only when **all** of the following hold, as decided
by `skipDecision` in [lane.go](../../crux/trajectory/lane.go):

1. the agent has been reviewed at least once before;
2. the review is **not overdue** — fewer than `MaxSkipIntervals` (default 6)
   review intervals have elapsed since the agent's last review;
3. the agent manager reports the agent **idle** from its own state — the last
   kicked turn has ended (`KickOutcome` settled against `LastKick`), the agent
   is parked at the prompt awaiting CI, or it has never been kicked
   (`trajectoryIdle` in [trajectory_view.go](../../crux/agent/trajectory_view.go));
4. the manager has a known last-output time and it precedes the last review.

If any condition fails the review runs. An unknown last-output time is treated
as "may have new output", so a skip never rests on an unproven signal. An
overdue review always runs, so a long-idle agent is still re-read at least every
`MaxSkipIntervals` intervals.

Every decision is recorded with a reason (`first-review`, `overdue`, `active`,
`new-output`, `idle-no-new-output`). Counters are logged per skip, exposed via
`Lane.Stats()`, shown in the dashboard Trajectory Review panel, and exported on
`/metrics` as `hive_trajectory_reviews_total{decision}`,
`hive_trajectory_skipped_total{reason}` and `hive_trajectory_ran_total{reason}`.

Consequence: token cost scales with agents actually working rather than fleet
size. Active agents remain reviewed on cadence; only proven-idle agents may
skip, and even they are reviewed once the overdue limit is reached.
