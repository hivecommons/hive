# ADR-0006: Planning intelligence with human review

Status: Accepted (retroactive)

## Context

Large GitHub issues can be too vague for a single agent kick. Hive needs a way
to turn an epic into ordered, claimable work without letting an agent immediately
execute an unreviewed decomposition. The planning docs describe the flow:
architect decomposition, a draft plan hidden from `Ready()`, a plan-review gate,
and bounded stall replanning
([planning intelligence](../planning-intelligence.md)). The package code records
the same metadata conventions and review transitions
([decompose](../../pkg/planning/decompose.go),
[plan review](../../pkg/planning/plan_review.go),
[stall replan](../../pkg/planning/replan.go)).

## Decision

Use the existing `architect` lane to decompose an epic into child beads with
dependency edges and execution tags. Decomposition writes `parent_epic`,
`execution`, `plan_ref`, and `plan_status` metadata; draft plans keep children
out of ready queues until a human approves them. Reviewers may inspect, retag,
remove, approve, or reject children before work is released.

The automatic issue-label entry point stays opt-in. `planning.plan_from_label`
defaults off because the label path can feed attacker-controlled issue text into
the architect prompt without per-kick review; even when enabled it is inert
below ACMM L5, where the architect is not scheduled. Approved plans that stop
making progress are re-kicked for unfinished work only, capped at five replans
before human escalation.

## Consequences

Hive can handle epics as DAGs instead of long-running, ambiguous agent sessions,
and operators get a clear approval point before work becomes claimable. The
system also preserves pause semantics: a paused architect queues decomposition
instead of being force-resumed. The cost is more state and review UI surface,
and deployments that opt into label planning must accept the prompt-injection
exposure of routing raw issue text to the architect.
