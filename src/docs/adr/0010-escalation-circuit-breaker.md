# ADR-0010: Escalation circuit breaker for CI fix loops

Status: Accepted (retroactive)

## Context

Hive agents can repair their own failing PRs, but an unbounded retry loop can
keep re-dispatching blind fixes without surfacing the root CI error. The
escalation package records the incident that forced this boundary: a console
test split kept `main` red for days while scanner fix PRs missed the one-line
failure in shard logs ([escalation package](../../pkg/escalation/escalation.go)).

## Decision

Track red PRs in a persistent ledger keyed by `repo#number`. Each sweep records
distinct failing head SHAs, retains the last CI excerpt, clears history when a
PR goes green, and marks escalation once the threshold is crossed. The default
threshold is three distinct red SHAs. When escalation fires, Hive posts a comment
headed "Fix loop escalated — human attention needed", includes failing checks
and raw failure evidence when available, and applies the `needs-human` label so
future fix dispatch skips the PR.

For unchanged red heads, track staleness separately and cap re-engagements at
three per current SHA. A branch that moves resets the re-engagement counter; a
permanently red, never-moving branch is not nudged forever.

A PR escalating for the SECOND time — after the reviewer lane ([#5480]) already
repaired or de-escalated it once — gets a structured hand-off note instead of
the generic body. The ledger stamps the head SHA the reviewer left on the branch
and when its verdict was reconciled, and keeps both across the reset that
reconciliation performs, so the comment can say what was already tried, that the
attempt count is measured from the reviewer's pass, and that no further
automated pass is coming. One reviewer pass per PR is the whole ladder: without
the note, nothing distinguished that terminal hand-off from a first escalation
except the label set.

[#5480]: https://github.com/hivecommons/hive/issues/5480

## Consequences

The fleet stops spending cycles on fix loops that are not converging and gives a
human the evidence needed to unblock the PR. The ledger is deterministic and
language-agnostic: it keys on CI state, head SHAs, and elapsed time rather than
agent judgment. The trade-off is that some recoverable failures will require
manual label removal after the cap, and stale/failure detection depends on the
quality of CI observations and excerpts available during enumeration.
