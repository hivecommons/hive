# Readiness-first preparation

Preparation admission is not merge approval. A caller must pass its normal
lane gates on the exact head before requesting a base refresh. An unresolved
refresh, ambiguous mutation outcome, or active repair retains exclusive
preparation ownership.

A completed refresh can yield while its new head has a known pending CI
verdict. Completion requires a changed live head containing both the pinned
pre-refresh head and the authoritative refresh-target base commit. Legacy
attempts without authoritative base evidence remain exclusive. The former
owner keeps its ticket, original priority, and head/base attempt deduplication;
pending CI/review observations do not erase that ticket or trigger another
refresh. Once ready, it cannot be overtaken by another caller.

Overtaking requires complete changed-file evidence, including rename sources,
disjoint closing claims, explicit native issue dependency summaries reporting
no open blockers, and divergent head ancestry. An extra unclassified reference,
shared claim, supersession, stacked head, incomplete pagination, changed
metadata, missing required evidence, or failed read preserves FIFO. File
disjointness alone is not independence. This is a conservative rule using
repository-declared relationships, not semantic inference across packages.
At most three predecessors are checked for overtaking in one admission;
larger or ambiguous fronts retain FIFO instead of spending unbounded API work.

Final sweep admission and merge calls share a per-repository/base guard with
the GitHub merge-request relay. Production uses stable flock inodes under
`/data/destination-merges`, shared by writers using the same durable volume;
isolated test engines use process-local locks. Contention parks a caller
rather than allowing it to retain stale evidence while waiting. Locks are
released on return or process death, and retargeting cannot borrow another
destination's lease. No new parallel merge dispatch is introduced.

The guard serializes Hive writers sharing that volume, not independent human
or external GitHub clients. GitHub still performs atomic protected-base
updates and enforces configured checks. Its REST merge API has an expected
head condition, not an expected base condition: universal serialization
against external writers requires server-side up-to-date protection or a
merge queue. This change does not alter those policies.

Budgets, runner concurrency, review authority, merge methods, and sweep cadence
are unchanged. Admission reads remain paced by existing GitHub API controls.
Fixtures and CI can establish correctness, but deployed before/after
PR-to-merge measurements are still required to establish latency improvement.
