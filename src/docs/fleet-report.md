# Fleet self-reporting (`governor.fleet_report`)

A hive can report *its own* hive-attributable failures upstream to
`hivecommons/hive`, so a defect that shows up across many hives gets noticed
without every operator filing the same issue by hand.

Because this files issues on a public repository from your hive, it is
**off by default** and the dry-run preview is designed to be read before you
turn it on. This page is the reference for what it sends, when, and what it
never sends.

## The opt-in

```yaml
governor:
  fleet_report:
    enabled: true              # tri-state; unset = hosted on, self-hosted off
    repo: hivecommons/hive     # default
    max_per_day: 5             # cap on NEW issues per 24h (joins never count)
    file_upstream: false       # legacy alias for enabled: true
```

`enabled` is a tri-state ([#11868](https://github.com/hivecommons/hive/issues/11868)):

- **unset** (the default) resolves to **on** for hub-managed hives — those
  booted with `HIVE_HEARTBEAT_KEY` by the hub, which run under the
  hivecommons-hive GitHub App and so can write to `hivecommons/hive` — and
  to **off** (dry-run) for self-hosted hives whose PAT is scoped to their
  own repos;
- `enabled: false` forces dry-run everywhere;
- `enabled: true` (or the legacy `file_upstream: true`) forces filing.

Dry-run means the hive evaluates everything, shows you the reports it
*would* file on the dashboard, and writes nothing upstream. The publish path
returns immediately when dry-run is set, before any GitHub client is touched.
There is one knob, and it is all-or-nothing: there is no per-trigger opt-in.
The same settings are exposed in the dashboard under Settings → Governor →
"Self-Reported Issues" (owner role) and over
`GET`/`PUT /api/config/governor/fleet-report`.

## The four triggers

A report is only ever built from evidence marked **attributable** — evidence
the hive believes points at hive's own behaviour rather than at your repos,
your prompts, or your infrastructure. Non-attributable evidence never produces
the report under a trigger.

### `acmm-shortfall`

Fires when an ACMM criterion has been unmet for **two or more weekly epochs**
(`PersistentUnmetEpochs = 2`) *and* there is attributable evidence to explain
it. The two-epoch requirement means a single bad week never files anything.

The point of this trigger is the pairing: "this hive cannot reach an ACMM bar,
and here is the hive-attributable reason why". A shortfall with no attributable
evidence is treated as an **operator** concern instead — it is surfaced to you
on the dashboard and is not reported upstream.

### `hive-code-defect`

Fires on attributable evidence pointing at a component on hive's own
allowlist — `proxy`, `scheduler`, `request-watcher`, `watcher`, `dashboard`,
`agent-runtime`, `agent-lifecycle`, `backend-auth`, `inference-gateway`,
`merge`, `pr-title`, `github-client` — with **no ACMM shortfall required**. A
crash loop in hive's own scheduler is worth reporting whether or not it has
dragged an ACMM criterion down yet.

The allowlist is what keeps this trigger narrow: evidence from a component that
is not recognisably hive's own code does not qualify. Reporting components
(anything matching `fleet-report` or `reporting`) are excluded outright, so the
reporter cannot report on itself.

Evidence already covered by an `acmm-shortfall` report in the same pass is
skipped, so one underlying fault does not file two issues.

### `runtime-error`

Unusual runtime errors the hive notices in itself. "Unusual" is an explicit
allow-list of classes defined in `pkg/fleetreport/runtime.go`; a condition
only becomes a report once it crosses that class's threshold, and the report
title is `[selfreport] <class>: <short message>`.

| Class | Fires when | Severity |
|---|---|---|
| `restart_storm` | the restart-storm breaker pauses an agent | high |
| `kick_failed_repeatedly` | ≥3 consecutive undeliverable kicks for one agent *after* the busy-no-activity stall restart has been attempted (or that restart itself errored) — i.e. the recovery did not bring the session back; kicks that fail before the restart are a dashboard/log warning only | medium |
| `panic_recovered` | a dashboard handler recovered from a panic | high |
| `model_discovery_degraded` | model discovery has been failing for >1h | medium |
| `inference_auth_failed` | 401/403 from an inference backend (LiteLLM, Copilot) | high |
| `advisory_post_failed` | the advisory digest could not be posted (rate-limit skips excluded) | medium |

Deferred to follow-ups (defined, not yet wired): `upgrade_stalled`,
`data_disk_pressure`, `github_app_token_mint_failed`,
`unexpected_http_5xx_from_hub`, `unclassified_error_repeated`.

Each report attaches the class, severity, count and window, the normalised
message (numbers, hashes, paths and quoted strings replaced by placeholders)
and a redacted diagnostics excerpt inside `<details>`. Secrets are never
collected; the diagnostics go through the same scrubber as the other
triggers.


### `observation`

Nous detector findings use the `observation` self-report class and the
`hive-observation` label. The body contains only hive metadata (hive id/slug,
project organization, ACMM level, hive version, and `cluster_id`) plus
aggregate evidence and a JSON daily series suitable for a sparkline. Watched
repository names, issue numbers, and file paths are not part of this report.

Observation reports never use the spoke's GitHub client. A hub-managed spoke
posts them to `POST /api/selfreport/ingest`; the hub forces the destination to
`hivecommons/hive`, adds the configured needs-decision label, and searches the
open issue body for the detector fingerprint before creating or commenting.

## Exactly what leaves the hive

Each report carries:

| Field | Value |
|---|---|
| anonymous instance | first **16 hex characters of the SHA-256** of your hive ID |
| owner(s) | the GitHub login(s) of the hive's owner(s) — `dashboard.authorized_users` entries with the owner role plus the device-flow operator — as `@login`; `(anonymous)` when `include_owners: false` (`runtime-error` only) |
| hive version, commit | build stamp, short commit |
| mode | the hive's mode |
| ACMM level | e.g. `L3` |
| unmet criterion | name and key (`acmm-shortfall` only) |
| component, agent, lane | which part of the hive produced the evidence |
| error class | the classified error, not the raw message |
| count + window | how many occurrences, over what window |
| detected periodicity | if the failures look periodic |
| repos, owners | `runtime-error` only: the `owner/repo` names this hive watches (`project.repos`) and the distinct owners derived from them, as `N: a/b, c/d` — see [Repositories and repo owners](#repositories-and-repo-owners) |
| repo, ref | `runtime-error` only: the item the error was scoped to, when the class carries one (today `advisory_post_failed` → the digest's repo); `—` otherwise |
| self-recovered | `no` on a report, `yes` on a recovery |

Alongside the rendered body, each report — and the hub relay request to
`POST /api/selfreport/ingest` — carries a structured `hive_state` block: the
hive's 24 h / 7 d state trends from `pkg/hivestate` (median/p90 queue depth
for issues, PRs and held items, SLA-violation rate, agent idle ratio,
kicks/day, tokens/day; see
[Hive state time series](strategy-lab.md#hive-state-time-series)). It contains
only aggregate counts and rates — no agent names, issue numbers, or repository
content — and is kept out of the body, so moving trends never make an open
report look changed. It is omitted when the hive has no snapshot in the last
7 days.

The same fields appear as issue labels (`fleet-report`, `component:…`,
`severity:…`, `version:…`, `instance:…`, `trigger:…`, and `criterion:…` for
shortfalls). Observation issues additionally carry `hive-observation` and
`observation:<kind>`.

### What it does not send

- Legacy ACMM, hive-code-defect, and runtime reports send only an anonymous
  instance digest; the raw hive id never leaves the hive for those triggers.
- Observation reports use the hub-provided hive id/slug and project
  organization metadata required for maintainer triage, but never send watched
  repository names, issue numbers, or file paths.
- **No raw log lines, error messages, or repository content.** Evidence is
  reduced to a classified error *class*, a component, and counts.
- Titles and bodies are passed through `logscrub` before leaving the hive,
  which redacts credential-shaped substrings (GitHub tokens, AWS access keys,
  bearer tokens, JWTs, private-key blocks). This is a backstop, not the primary
  protection — the report is built from structured fields, not from free text.

## Deduplication, comments, and recovery

Every report gets a deterministic **fingerprint**: a SHA-256 over the error
class, component, version, and (for shortfalls) the criterion key. The same
fault on the same version always produces the same fingerprint.

Before filing, the hive searches upstream for an existing issue carrying that
fingerprint:

- **Found** → it adds a comment and a 👍 reaction instead of opening a
  duplicate. The reaction is how "N hives see this" gets counted.
- **Not found** → it opens one issue.

For `runtime-error` reports the hive goes further and **joins first**. If no
fingerprint match exists it looks for an open issue labelled
`hive/selfreport` for the same class, then for a human-filed issue whose
title or body mentions the class keywords (for example "restart storm",
"model discovery degraded", "LiteLLM 401"). When any of those matches, the
hive adds itself to that issue instead of opening a new one:

- one comment `Also seen on **<hive>** (v<version>, <cluster>) at <time>`
  carrying a `<!-- hive:selfreport-seen hive=<slug> fp=… -->` marker and its
  diagnostics excerpt — at most once per hive per 24h;
- the `hive/selfreport` and `origin/selfreport` labels (the per-hive `hive/<slug>` label is retired, #12539; `hive/selfreport` is deprecated in favour of the `origin/selfreport` provenance label and is kept only so existing filters and the joiner's `label:hive/selfreport` search keep working);
- a bump to the `Affected hives (N)` block in the issue body, edited
  idempotently between `<!-- hive:selfreport-affected -->` markers: the
  joining hive gets (or updates) its own line naming its owner(s), and its
  repos are merged into the block's `Affected repos (N)` /
  `Affected repo owners (N)` union (below).

### Who is affected

So a maintainer can tell **who** is affected and ping them — not just which
namespace-style hive name — every hive renders on its own line of the
affected block (hivecommons/hive#12529 follow-up):

```
<!-- hive:selfreport-affected -->
**Affected hives (2):**
- hosted-aslom (instance b04ca59b7312017e) — owner(s): @alice, @bob — seen 3, last 2026-10-17T11:00:00Z
- hosted-oke-11-r05x — owner(s): (anonymous) — seen 1, last 2026-10-16T19:56:47Z
seen_count: 4
<!-- /hive:selfreport-affected -->
```

- `owner(s)` are the GitHub logins the spoke knows for its owner:
  `dashboard.authorized_users` entries holding the owner role (the first
  entry without an explicit role, plus any `:owner` entry) and the operator
  who completed the device flow (`/data/gh-user-token`). They travel in the
  report as an explicit `owners` field; non-GitHub identities (`ibmid:…`)
  and anything that is not a plausible login are dropped before they leave
  the hive. `unknown` means the hive sent none (a pre-owners spoke).
- `seen N, last <time>` are per hive; `seen_count` stays the total.
- **Privacy.** `governor.fleet_report.include_owners: false` (default
  `true`) renders `(anonymous)` in both the evidence table and the hive's
  line, sends no logins, and a later report never re-attaches them.
- When a spoke that predates the field relays through the hub, the hub
  fills the line from its own record of the hive owner (unless the spoke
  opted out). Existing issues with the older inline
  `**Affected hives (1):** <hive>` list are upgraded to the per-hive form on
  the hive's next join, so an old issue gains the owner on the next sweep.

### Repositories and repo owners

So a maintainer can tell whether a fleet issue is confined to one project or
one org, a `runtime-error` report names the repositories the reporting hive
serves (hivecommons/hive#12338). The evidence table carries the hive's own
view; the affected block carries the union across every hive that has
reported, re-derived idempotently on each join:

```
<!-- hive:selfreport-affected -->
**Affected hives (2):**
- hosted-aslom (instance b04ca59b7312017e) — owner(s): @alice — seen 2, last 2026-10-17T11:00:00Z
- hosted-available-oke-11-placeholder-r05x — owner(s): @carol — seen 1, last 2026-10-16T19:56:47Z
**Affected repos (3):** acme/widgets, hivecommons/hive, hivecommons/pluk
**Affected repo owners (2):** acme, hivecommons
seen_count: 3
<!-- /hive:selfreport-affected -->
```

- `Affected repos` is the sorted union of every reporting hive's watched repos
  plus any triggering `repo`; `Affected repo owners` (the orgs/users owning
  those repos — distinct from the per-hive `owner(s)` logins above) is
  always re-derived from it, never parsed back. A hive that re-reports with nothing new leaves the body
  untouched; a known hive bringing a new repo edits only the block.
- Repo names are validated to exactly `owner/name` (lower-cased, GitHub-safe
  characters) before they leave the hive; anything else is dropped.
- **None of this enters the fingerprint.** The fingerprint stays class +
  normalised message + version, so two hives watching different repos still
  dedupe onto one issue.
- The lines are omitted entirely (and the table cells render `—`) when no
  reporting hive contributed repos, so pre-#12338 hives and opted-out hives
  produce exactly the block they always did.

**Privacy.** The report repo is public and `hive.yaml` does not record
whether a watched repo is private, so the hive cannot filter private repos
for you. `governor.fleet_report.include_repos` (default `true`; Settings →
Governor → "Self-Reported Issues" → "Name the watched repositories in
self-reports") turns the whole feature off for a hive whose `project.repos`
include private repositories: the table shows `—`, the report carries no
repos, and the hive's joins never add to the union. The hub relay forwards
the body as built, so the switch governs relayed reports too.

A new issue is opened only when nothing matches. Rate limits apply to **new**
filings only: at most `max_per_day` per hive per 24h, at least 1h between
new filings of different classes, and a fingerprint that was closed upstream
within the last 7 days is never re-filed (treated as known / fixed pending
rollout). Filing is also deferred while the GitHub API budget is not
`normal`. Hub-managed hives whose credential is refused by the report repo
(403/404) relay the report to the hub's `POST /api/selfreport/ingest` with
their heartbeat bearer, and the hub files or joins under the App identity.

The hive also remembers the body hash of what it last posted, so a condition
that persists unchanged does not re-comment every cycle. It re-reports only
when the body actually changes, when the issue is gone, or when a previously
recovered condition comes back.

When the condition clears, the hive posts a **recovery** comment
(`self-recovered: yes`). If the hive opened that issue itself, it also closes
it; if a human opened the issue, the hive comments but **leaves it open** —
it will not close someone else's issue.

`runtime-error` issues are never auto-closed, even by the hive that opened
them: when the condition clears for that hive it posts a single
`cleared on <hive> at <time>` comment and leaves the issue for a human to
confirm the fix.

## State

Report state lives at `/data/fleet-report-state.json`: the per-criterion epoch
history that drives the two-epoch threshold, and the open-issue records
(number, URL, whether the hive opened it, body hash, recovery flag). Deleting
it resets the epoch counters and makes the hive forget which upstream issues
are already open, so a persisting condition can file again.

## Previewing before you opt in

The evaluation runs on every dashboard status refresh **regardless of the
knob** — dry-run changes only whether anything is written upstream. So you can
leave `enabled` unset (self-hosted) or `enabled: false`, run normally, and read the reports the hive would
have filed on the dashboard's fleet-report panel.

That is the intended way to adopt this: watch the dry-run output until you are
satisfied that what it would send is both accurate and something you are
willing to publish, then opt in.

## See also

- [Fleet health: the verdict and remediation hints](fleet-health.md) — the
  per-hive verdict on `/fleet`, which is about *your* hive's state rather than
  upstream reporting.
- [Fleet drift signals](fleet-drift-signals.md) — per-hive deviation badges.
