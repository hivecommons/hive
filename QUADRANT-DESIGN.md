# Hive Quadrant — design notes (working draft, not for merge)

A 4-axis radar ("kite") scoring every hive on **Trust, Efficiency, Satisfaction,
Productivity**, rendered at three zoom levels off one computed struct.

Purpose: a **nudge instrument**, not a report card. Every sub-criterion must map
to a next action an operator or hive owner can actually take.

## Renderings

| Surface | Size | Content |
|---|---|---|
| Table column | ~22px kite | shape only, fleet-average ghost behind |
| Column hover | ~180px | full diamond, axis labels, scores, delta vs fleet median |
| Status hover | ~180px | same diamond inside the existing status panel |
| Dashboard header | large | filtered-fleet aggregate |

Fixed axis positions: **T top, P right, S bottom, E left.** Never reorder.

## Scoring

- **Fleet-relative percentile**, 0-100 per axis, four sub-criteria each.
- **Data-sufficiency floor**: an axis with insufficient evidence renders EMPTY
  (spoke collapsed to origin), never 0. "We don't know" must not read as "bad".
  Applies to every axis, not just Satisfaction.
- Composite = mean of scored axes only (empty axes excluded, not counted as 0).

## Axis criteria

### TRUST — how much rope the humans have given the hive
| Sub-criterion | Source | Nudge |
|---|---|---|
| Autonomy level | `ACMMLevel` | "At L3 for 6 weeks — try L4 on one repo" |
| Governor posture | `GovernorMode` | "Move the governor off observe-only" |
| Merge acceptance | agent PR merge rate | "1 in 3 agent PRs rejected — tune the mission" |
| Scope breadth | enrolled repos vs org repos | "Only 1 of 9 repos enrolled" |

### EFFICIENCY — cost of an outcome
| Sub-criterion | Source | Nudge |
|---|---|---|
| Cost per merged PR | #4116 cost ÷ `PRIssueCounts.MergedPRs` | vs fleet median |
| Cost per closed issue | #4116 cost ÷ `PRIssueCounts.ClosedIssues` | vs fleet median |
| Rework rate | agent PRs closed-unmerged ÷ opened | "Burning budget on PRs nobody wants" |
| Idle burn | agent-hours with no merged output | "3 agents, 1 shipping — pause two" |

### SATISFACTION — does it feel good to use
Weakest evidence. Renders EMPTY until a real pulse signal exists; the three
hygiene proxies below are deliberately NOT used as a stand-in, because
operational hygiene is not satisfaction and padding the axis would make the
whole chart untrustworthy.
Candidate future signal: lightweight in-dashboard pulse on agent output.

### PRODUCTIVITY — throughput, normalized
| Sub-criterion | Source | Nudge |
|---|---|---|
| Agent-merged PRs / week | metrics, attributed via `aiAuthor` | — |
| Agent-closed issues / week | metrics, attributed via `aiAuthor` | — |
| Work-source autonomy | `WorkSource` | "All work human-filed — let the scanner find work" |
| Relay throughput | ClankeR completed tasks | "Relay is idle" |

## CRITICAL correctness note

`github.PRIssueCounts` is **all-time cumulative and REPO-WIDE** — every merged PR
in the repo, not just agent-authored (see its doc comment: "not just ones
authored by the AI agent"). That is correct for cost-per-PR (total spend ÷ total
output) but **must NOT be used for Productivity**: a busy human repo with an idle
hive would score high. Productivity requires agent-attributed counts (`aiAuthor`).

## Plumbing (what does not exist yet)

Hub `RegistryEntry` has: `ACMMLevel`, `GovernorMode`, `AgentCount`,
`ActionableIssues`, `ActionablePRs`, `WorkSource`, `ContributorCount`,
`ActiveContributors`, `AdvisoryFindingCount`, `Health`, `LastHeartbeat`.

Hub does NOT have (all spoke-side today, must travel via heartbeat):
- cost totals (`pkg/dashboard/cost.go`)
- `PRIssueCounts` (merged PRs / closed issues)
- MTTR / issue-to-merge (`mttrCacheFile`)
- agent-attributed PR/issue counts

`acmmadvisor.MergeSuccessRate` is a **parameter name, not a live metric** —
`pkg/acmmadvisor` is unwired (see its own TODO: "wire RecommendFromStatus into
pkg/dashboard's status_builder.go"). Nothing computes it today.

## Structural precedent to follow

`saas.go:3123` — Journey is "Derived on read; never persisted on the registry
entry", sorted by a dedicated `journeySortValue`, rendered in the table. The
quadrant follows this exactly: computed on read in Go, browser only renders.

Sort: extend `sortedDashHives` with quadrant keys as a special case (like
`journey`), and use the existing `subSort`/`stackHeader` helpers for a folded
column header with T/E/S/P + composite sub-sort chips.

## Visibility

Assigned users (`h.role`) + hub admin (`_isAdmin`), matching existing row gates
like `canRenameHive`.

## Signal inventory — VERIFIED against v4@7a22faf1

| Need | Exists? | Where |
|---|---|---|
| ACMMLevel, GovernorMode, WorkSource, contributors | YES, hub-side | `RegistryEntry` |
| Cost totals | YES, spoke-only | `pkg/dashboard/cost.go` |
| Merged PRs / closed issues (repo-wide) | YES, spoke-only | `github.PRIssueCounts` |
| MTTR / issue-to-merge | YES, spoke-only | `mttrCacheFile` |
| Agent-ATTRIBUTED PR counts by state | YES, spoke-only | `Client.SearchOutreachPRCount(aiAuthor, org, project, state)` |
| MergeSuccessRate | NO — param name only, `acmmadvisor` unwired | — |
| Satisfaction pulse | NO — axis renders empty by design | — |

**Attribution primitives — pick the right one.** There are TWO, and they are not
interchangeable:

- `SearchPRCount(author, org, state)` — `type:pr author:X org:ORG` — agent PRs
  **inside** the org. **This is the Productivity primitive.**
- `SearchOutreachPRCount(author, org, project, state)` — same but **`-org:ORG`**,
  i.e. EXTERNAL outreach PRs only, further narrowed by project name in title.

Using the outreach counter for Productivity would omit all of a hive's main
in-org output and score it near zero. Only `SearchPRCount` measures throughput;
outreach is a separate (and much smaller) activity.

Both expose only open/merged, NOT closed-unmerged — so **rework rate is not
directly available** from either. Efficiency's rework sub-criterion needs a
closed-unmerged count that does not exist yet; until it does, Efficiency scores
on the two cost sub-criteria plus idle burn, and rework is omitted rather than
approximated.

## Build order
1. `src/pkg/hub/quadrant.go` — scorer, percentile ranking, sufficiency floor. Pure, table-driven tests.
2. Heartbeat extension — spokes report cost + PRIssueCounts + agent-attributed counts; hub stores on RegistryEntry.
3. Derive-on-read attach in `saas.go` (Journey pattern) + `quadrantSortValue`.
4. Kite SVG renderer (one function, three mounts) + folded column w/ subSort chips.
5. Header aggregate over the FILTERED set.

## Heartbeat trace — MAJOR revision to the plumbing plan

Traced the spoke->hub path. Three findings change the build:

**1. FleetStatsCollector ALREADY sends agent-attributed PR counts.**
`HeartbeatPayload.PRsMerged90d` / `PRsRejected90d` / `CVEsClosed` (+
`FleetStatsCollectedAt`) travel every beat and land on RegistryEntry
(server.go:300-310). These are org-wide AI-author PR search on a 30-min timer —
exactly the agent attribution Productivity needs. **No new plumbing required for
the Productivity axis.**

**2. Rework rate IS available after all.** `PRsRejected90d` is the
closed-unmerged count I previously recorded as missing. Efficiency can score its
rework sub-criterion: rejected / (merged + rejected).

**3. `tokens_24h` is NOT a 24h window.** Despite the name it is a lifetime
cumulative total (comment at main.go:3332). It is the only cost-ish signal that
reaches the hub — `pkg/dashboard/cost.go` (USD) is spoke-local and NOT sent.

### Consequence for Efficiency
Cost-per-PR in USD would need new plumbing (cost.go -> heartbeat). But
`Tokens24h` (cumulative tokens) already travels, and tokens-per-merged-PR is
arguably the BETTER efficiency metric anyway — it is provider-price-independent,
so it does not shift when a model's pricing changes or when hives run different
backends. Use `Tokens24h / PRsMerged90d`.
CAVEAT: numerator is lifetime, denominator is 90d — mismatched windows. Either
normalize or accept it as a rough ratio; must be stated in the hover, not hidden.

### Revised plumbing verdict
NOTHING NEEDS NEW HEARTBEAT FIELDS for a first version. All four axes can be
computed from what the hub already receives:
- Trust: ACMMLevel, GovernorMode, Repos (all on RegistryEntry)
- Productivity: PRsMerged90d, WorkSource, ContributorCount/ActiveContributors
- Efficiency: Tokens24h / PRsMerged90d, PRsRejected90d rework, AgentCount
- Satisfaction: unscored by design

This removes the spoke-side change from the critical path entirely.

### Other trace facts worth keeping
- Spoke shares `hub.HeartbeatPayload` directly (no separate DTO) — package hub
  is compiled into the spoke binary. Adding a field is a one-struct change.
- Interval 2 min (main.go:3202); hub stale threshold 5 min; 10s collect budget
  with stale-payload fallback.
- No separate registration POST — the hub auto-registers on FIRST heartbeat
  (server.go:1476-1530).
- `MetricsCollector` (mttr, prIssueCounts) is spoke-LOCAL only; never sent.
  So `github.PRIssueCounts` is NOT available hub-side — the repo-wide cost
  denominator I originally planned does not exist at the hub.
