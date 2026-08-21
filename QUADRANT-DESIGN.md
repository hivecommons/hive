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

## Windowed tokens ARE derivable — fixes the window-mismatch compromise

VERIFIED: `tokens.SessionSummary` (src/pkg/tokens/collector.go:31) carries BOTH
`TotalTokens int64` and `LastActive int64` (epoch) per session, and
`AggregateSummary.Sessions []SessionSummary` (collector.go:~55) is populated on
every scan (collector.go:279) into the cached aggregate, refreshed on a ticker
(collector.go:150).

So a 90-day windowed token total is a pure sum over already-cached data:
    sum(s.TotalTokens for s in Summary().Sessions if s.LastActive >= cutoff)

- No new GitHub API calls (rate-limit safe — the fleet is sensitive to this)
- No new disk I/O; the scan already happened
- Cheap enough for the 2-minute heartbeat

This REPLACES the lifetime-vs-90d compromise. Instead of ranking-only
tokens-per-PR, Efficiency gets a true 90d/90d ratio that is READABLE as an
actual cost-per-merged-PR figure, not merely comparable.

Caveat to verify: `LastActive` is `omitempty`, so a session with no timestamp
serializes as 0. Sessions with LastActive==0 must be EXCLUDED from the window
(unknown age), not treated as epoch-1970 and silently dropped — same rule, but
it must be explicit so the sum never quietly under-reports.

### Heartbeat additions under consideration (pending spoke survey)
1. `TokensWindowed90d *int64` — the above. HIGH value, zero cost. Near-certain.
2. MTTR / issue-to-merge — exists (`MetricsCollector.mttr`), never sent.
3. Merge-acceptance for TRUST — needs the survey's verdict.
4. Satisfaction candidates — survey must say honestly whether ANY exist.

## Spoke survey — FINAL instrumentation decisions

### Corrections to my own earlier plan
- **Windowed tokens: use `governor.BudgetInfo.CurrentSpend`** (governor.go:81), NOT
  my summed-SessionSummary idea. It is a true windowed delta (`totalTokens -
  WindowBaseline`), already computed per governor eval, already JSON-tagged.
  MUST ship with `WindowStartsAt`/`WindowEndsAt` — the number is uninterpretable
  without its bounds (0 could mean "window just rolled" or "nothing consumed").
  Window is `governor.budget.period_days`, default 7d — NOT 90d, so normalize to
  tokens/day before pairing with PRsMerged90d.
- **Trust merge-acceptance needs NO new field.** `BaselineMergeSuccessRate()` =
  merged/(merged+rejected) over 90d, and the hub ALREADY has both counts. I
  dropped this sub-criterion for nothing. Restore it computing hub-side.
  WEIGHTING CAVEAT (from the code's own doc, fleet_stats.go:41-56): Goodhart-
  gameable by PR size, measures one maintainer's judgment, and **was green
  throughout the 2026-08-14 fleet outage**. Never let it dominate the axis.

### Satisfaction — CONFIRMED empty, axis stays collapsed
Independent survey found no NPS, no feedback capture, no time-to-first-response,
nothing measuring how it FEELS to use a hive. Closest candidates measure
attention (engaged presence) or machine hygiene (stall/action nudges). Verdict
quoted: "if you ship a Satisfaction score built on nudge counts, you will be
measuring agent hygiene and calling it user delight." Axis remains unscored.

### Non-bug, checked and cleared
`api_contribute.go:7714` sets `TasksFailed: proc.RestartCount` — but that is in
`buildAgentLeaderboardEntries()`, which `LeaderboardForHub()` does NOT call (it
calls `buildLeaderboard()`, contributor profiles only). Local UI view only; the
hub is NOT receiving restart counts mislabelled as failures.

### Misleading names to never trust by identifier
- `Tokens24h` — lifetime cumulative, not 24h
- `MTTRResult` — NOT mean-time-to-recovery; issue-open-to-PR-merge lead time,
  sampled from `Fixes #N` in the 100 most recently UPDATED closed PRs =
  uncontrolled, variable window. NOT cross-hive comparable. Also the biggest
  existing rate-limit consumer (1 List + N unbounded Issues.Get every 5 min).
- `CVEsClosed` — all-time (unlike its 90d siblings) and a free-text "CVE-"
  search = PRs REFERENCING a CVE, not CVEs closed
- `PRIssueCounts.MergedPRs` — repo-scoped, ALL authors (vs PRsMerged90d which is
  org-wide, AI-author). Never ratio the two.

### Heartbeat additions — final shortlist (all zero new API calls)
| Field | Source | Axis | Notes |
|---|---|---|---|
| `BudgetCurrentSpend *int64` | governor.go:81 | Efficiency | + window bounds, mandatory |
| `BudgetWindowStartsAt/EndsAt string` | FrontendBudget | Efficiency | makes spend interpretable |
| `BudgetExhausted *bool` | FrontendBudget | Effic+Prod | hive is being throttled |
| `HoldTotal *int` | server.go ~600 | Productivity | human-bottleneck, direct |
| `AwaitingReview *int` | FrontendPlanning | Productivity | autonomy-inverse |
| `SLAViolations *int` | governor.go:45 | Productivity | work aging past SLA |
| `TasksCompleted7d *int` | contribute_metrics.go:75 | Productivity | sum 168 hourly buckets ON SPOKE |

ALL pointers — old spokes send nil, which the scorer already treats as
"not measured" rather than zero. No version-skew handling needed.

REJECTED for the heartbeat: MTTR (uncontrolled window, not comparable),
PRIssueCounts (wrong scope/author, expensive), coverage badge (0 is ambiguous
with error), native gateway spend (outbound probe per beat), green-CI streak
(does not exist, needs new API calls + new state).

SEND SCALARS NEVER RINGS: MTTR.History ~5KB, token/cost rings up to 8640
entries, contribute series 168 buckets. Reduce on the spoke first.
Total budget for the shortlist: ~150 bytes.
