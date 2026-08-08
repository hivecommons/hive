# Retro lane

The retro lane is a deterministic, post-completion analysis pass for Hive work. It is disabled by default (`retro.enabled: false`) so existing hives see no behavior change unless operators opt in.

When enabled, the lane runs from the governor tick on its own scan interval. It scans done/closed beads that have a closed or merged PR association in bead metadata or the lifecycle timeline, reconstructs a compact `RetroRecord`, and applies rule-based pattern detection only. It does not call an LLM and it does not post to GitHub.

## Reconstructed record

For each eligible bead the lane combines local ledgers:

- bead metadata: issue/bead identity, PR reference/state, claim/close times, explicit counters when present;
- lifecycle timeline: kicks, PR-open/merge events, and trajectory drift/pause markers;
- escalation ledger when still available: failed fix-attempt count for the PR.

The compact record tracks bead metadata, issue/PR refs, kicks received, CI failures/fix attempts, drift pauses, and wall-clock time from claim to close.

## Deterministic findings

Named threshold defaults are:

- excessive fix attempts: `>= 3`;
- excessive kicks before completion: `>= 5`;
- long stall: claim-to-close `> 7 days`;
- drift pause occurred: any trajectory drift/pause marker.

Each finding is filed as an `advisory` bead attributed to actor `retro`, using the existing advisory-bead digest path. Source beads are marked with `retro_analyzed_at` after analysis to avoid duplicate findings.

## Follow-ups

LLM-powered retrospective summaries and durable knowledge-graph lesson extraction are intentionally deferred. This PR provides the deterministic foundation and local advisory-bead feedback loop only.
