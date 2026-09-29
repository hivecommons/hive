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
`MaxReEngagements` (six) per current SHA. A branch that moves resets the
re-engagement counter; a permanently red, never-moving branch is not nudged
forever.

A re-engagement is a DELIVERED kick, not a counter increment: the reaper and
the merge watcher resolve the PR's owning agent (falling back to
`review.fixer_agent`, then `scanner`, when the owner is paused or unreachable),
send it a targeted FIX-BEFORE-NEW kick for that one PR, and charge the budget
only once the kick is accepted. Spacing is at least the staleness window and at
least the owner's slowest cadence, so six attempts cannot be spent inside one
cadence window. The staleness clock starts when CI SETTLES — a head red on one
check while others are still running is not yet stuck. The escalation comment
quotes the number of kicks actually delivered.

Shared CI breakage is not a fix attempt. A failing check red on at least three
other open PRs in the same pass is an incident (a broken base branch, a runner
outage), so the pass is treated as no-information: no attempt counted, no
staleness clock, no re-engagement. Attempts are counted per head TREE, so an
empty `ci: retrigger` commit does not consume one. An escalation is un-parked
automatically — `needs-human` removed, ledger reset, one comment saying why —
when the head goes green or when the shared breakage it escalated over clears.

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

The `needs-human` label on the forge, not the ledger, is the authoritative
record that a PR has been escalated. The ledger is a cache of it: a PR that
wears the label reads as escalated even to an empty ledger (so the evidence
comment is never posted twice, whatever happens to `/data`), and a PR whose
confirmed label a human removes is un-parked with a fresh budget. A pass that
cannot conclude CI state (checks running, or the check-run fetch failed — both
surface as `pending`) leaves the ledger untouched; only a conclusive green
clears history. Entries are pruned 24h after their PR stops being enumerated,
not on the first pass that misses it.

Dependency bots (`renovate[bot]`, `dependabot[bot]`, `mergeraptor[bot]`) are
not agent authors: their red PRs are not fix loops to break.

## Consequences

The fleet stops spending cycles on fix loops that are not converging and gives a
human the evidence needed to unblock the PR. The ledger is deterministic and
language-agnostic: it keys on CI state, head SHAs, and elapsed time rather than
agent judgment. The trade-off is that some recoverable failures will require
manual label removal after the cap, and stale/failure detection depends on the
quality of CI observations and excerpts available during enumeration.
