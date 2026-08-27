# Advisory digest staleness — what the hub flags, and what it deliberately does not

The hub shows a **stale advisory** pill (and raises a warning alert) when a hive
that *should* be posting an advisory digest has quietly stopped. The rule lives
in `src/pkg/hub/advisory_staleness.go` and is evaluated on read, so the browser
never re-derives it.

## Signals

| Signal | Produced by | Notes |
| --- | --- | --- |
| `AdvisoryLastPostedAt` | spoke, `RecordAdvisoryPost` after a **successful** digest post | in spoke memory only; empty until the first success |
| `AdvisoryError` | spoke, `RecordAdvisoryError` on a failed post **attempt** | cleared by the next success |
| `GitHubAppState` / `GitHubAppRequired` / `PendingGitHubAppInstall` | spoke App diagnosis | decides whether silence is expected |

## The gates

1. **Participation** — no post time *and* no error means "not an advisory hive
   (or a spoke too old to report)". Unknown, never stale.
2. **Reported error** — flagged with its cause, unless the App is still
   *undelivered* (`appAwaitingDelivery`: not-installed, no-app-assigned,
   key-missing, pending install, or an unclassified spoke with the install
   banner up). An App that *has* been delivered but cannot write is a fault the
   operator must see.
3. **Expected quiet** — with no error reported, an ageing digest is suppressed
   when *every* agent on the hive is deliberately quiet — paused, or off its
   schedule (`allAgentsQuietByDesign`). Findings come from agents; with none
   running there is nothing to post, so the silence is the operator's own pause
   rather than a wedge. Deliberately conservative in the unknowns' favour: a
   hive reporting no agents at all, or a legacy agent entry missing the newer
   protocol fields, does *not* qualify, so the gate never silences a spoke the
   hub cannot read. A reported post **error** still flags regardless — a broken
   post path is broken whoever is paused.
4. **Age** — with no error reported, a post time older than
   `advisoryStaleThreshold` (90 min) is stale, but only when the App can write
   at all (`appCanWriteForAdvisory`). Unparseable timestamps are unknown.

## Suppression matrix (#4167)

| Scenario | Behaviour before | Expected | Fix |
| --- | --- | --- | --- |
| Digest post 403s on a delivered App | The same failure raised `GitHubAppRequired` / `write-forbidden`, which suppressed the error path — **no pill** | Flag stale with the reported cause | Gate 2 now suppresses a reported error only while the App is *undelivered* (`appAwaitingDelivery`) |
| Spoke restarts while the digest path is wedged | `AdvisoryLastPostedAt` reset to empty each restart → read as "not participating" → **never flagged, forever** | Timestamp keeps ageing across restarts | Hub carries the last known post time forward (`carryAdvisoryPostTime`), guarded on the primary repo being unchanged |
| Hive is re-tenanted (placeholder reclaimed) | n/a | Clean advisory history for the new project | Carry-forward skipped when `PrimaryRepo` changes |
| No pinned advisory issue resolvable | Silent skip: the spoke reports neither post time nor error, so a hive that has *never* posted looks like a PR-only hive | Flag stale once the hive has posted at least once | Carry-forward keeps the last post time ageing, so the wedge surfaces on the age path; the spoke-side "record the skip as an error" change is tracked separately on #4167 |
| App never installed / key never delivered | Suppressed | Suppressed — onboarding, not a fault | unchanged (expected suppression) |
| Aged digest, App cannot write, no error reported | Suppressed | Suppressed — no evidence the hive tried | unchanged (expected suppression); counted as `hidden_stale` in diagnostics |
| Hive offline | Row pill hidden (offline state dominates) | Unchanged in the UI | counted as `hidden_stale` in diagnostics so fleet-level staleness is still visible |
| Pure PR/merge hive | Suppressed | Suppressed | unchanged (expected suppression) |
| Every agent paused or off-schedule, digest ageing | Flagged stale — the pause itself lit the pill | Suppressed — no agents, no findings, no digest | Gate 3 (`allAgentsQuietByDesign`, #4528); counted as `hidden_stale` in diagnostics |

## Diagnostics

Suppression is intentional in several of the rows above, which makes "how many
hives are silently stale?" unanswerable from the pill alone. The hub therefore
measures it:

- `GET /api/saas/admin/advisory-diagnostics` (admin-only) returns per-hive
  classification (`stale`, `fresh`, `not-participating`,
  `suppressed-app-undelivered`, `suppressed-app-cannot-write`,
  `suppressed-agents-quiet`, `unknown-timestamp`), the digest age in minutes,
  the reporting spoke version, and a fleet `hidden_stale` count. Each suppressed
  class also carries `hidden_stale` per hive — whether the digest *would* have
  been flagged had that gate not fired, which is what makes the fleet count a
  prevalence number rather than a tally of gates.
- The same roll-up is logged every 30 minutes as a single `advisory diagnostics`
  line, so prevalence can be read off hub logs without calling the endpoint.

Diagnostics are read-only measurement: they never change a pill, an alert, or a
registry entry.
