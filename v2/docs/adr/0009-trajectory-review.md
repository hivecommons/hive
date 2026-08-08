# ADR-0009: Trajectory review lane

Status: Accepted (retroactive)

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
([reviewer](https://github.com/kubestellar/hive/blob/v4/v2/pkg/trajectory/trajectory.go),
[lane](https://github.com/kubestellar/hive/blob/v4/v2/pkg/trajectory/lane.go)). On divergence, the configured response is
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
