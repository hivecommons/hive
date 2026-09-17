# Fleet self-reporting (`governor.fleet_report`)

A spoke can report **hive-attributable problems** upstream to
[hivecommons/hive](https://github.com/hivecommons/hive), so persistent defects
in hive itself surface to the maintainers even when no operator files them by
hand. The feature is **dry-run by default**: nothing leaves the hive until an
operator explicitly opts in. In dry-run mode the would-file reports are still
computed every eval cycle and shown on the dashboard for review.

Implementation: decision logic in `src/pkg/fleetreport` (deterministic,
side-effect free), evidence collection and state in
`src/pkg/dashboard/api_fleet_report.go`, upstream writes in
`src/pkg/github/fleet_report.go`, config in `src/pkg/config/config.go`
(`FleetReportConfig`).

## Opt-in

```yaml
governor:
  fleet_report:
    file_upstream: true   # default false = dry-run
```

`file_upstream: false` (the zero value) means dry-run: reports are attached to
the dashboard status payload (`fleetReport`) and rendered as a preview, and
the state file records what *would* have been filed. Setting
`file_upstream: true` is the explicit consent for this hive to create issues
and comments on hivecommons/hive.

## Triggers

There are two, each producing a deterministic fingerprint:

- **`acmm-shortfall`** — an ACMM criterion has been unmet for at least **2
  consecutive weekly epochs** *and* there is hive-attributable runtime
  evidence tied to the shortfall. A persistently unmet criterion with **no**
  attributable evidence is never reported upstream; it is listed on the
  dashboard as an *operator criterion* instead, because the fix is local.
- **`hive-code-defect`** — runtime evidence attributable to hive's own code
  paths (for example a crash-looping agent runtime, a periodic
  inference-gateway error streak, or a backend-auth failure pattern), reported
  even without an ACMM shortfall.

Evidence is classified from the last window of error events (default 10
minutes for the classifier; gateway/agent evidence uses 1h/24h windows):
grouped by component + agent + lane + error class, kept only when it recurs
(3+ events) or shows detectable periodicity (periodic streaks are marked
`high` severity — a periodic error is characteristic of a wedged loop, not
operator input). Benign classes are dropped.

## What leaves the hive (and what never does)

When `file_upstream` is on, a report contains **only**:

| Field | Notes |
|---|---|
| anonymous instance ID | first 16 hex chars of SHA-256 of the hive ID — the raw hive ID is never sent |
| hive version + short commit | build info |
| governor mode, ACMM level | |
| unmet criterion | `acmm-shortfall` trigger only |
| component, agent name, lane | sanitized tokens |
| error class, count, window, periodicity, severity | |

Every title and body passes the `logscrub` scrubber (the same one that redacts
GitHub token prefixes and JWT-shaped strings from logs), and the GitHub client
reuses the standard `CreateIssue`/`CreateIssueComment` paths, so body
scrubbing and ioscan canary checks apply. No repository names, URLs, log
excerpts, prompts, or credentials are included.

## Deduplication and recovery

- The fingerprint (24 hex chars, hashed from error class + component + version
  + criterion) is embedded in the issue body as
  `<!-- hive-fleet-fingerprint:... -->`. Before filing, the spoke searches for
  an existing open issue with that fingerprint: if found, it **comments** with
  its own evidence and adds a 👍 reaction instead of opening a duplicate.
- A body-hash check suppresses repeat comments that would say nothing new.
- When the condition clears (the criterion is met again, or the evidence
  stops), the spoke posts a **recovery** comment — and closes the issue if
  this hive was the one that opened it.
- Report state (epochs seen per criterion, open issue numbers, recovery
  flags) persists at `/data/fleet-report-state.json`.

## Where to see it

The dashboard status payload carries the current result under `fleetReport`
(`dry_run`, `reports`, `recoveries`, `operator_criteria`), rendered as a
preview panel so an operator can read exactly what would be — or was — filed.

## Related documentation

- **[ACMM advisor](acmm-advisor.md)** — where the unmet-criteria input comes from.
- **[Fleet health](fleet-health.md)** — the hub-side per-hive verdict; unrelated plumbing, similar name.
- **[Fleet drift signals](fleet-drift-signals.md)** — hub-side deviation badges.
