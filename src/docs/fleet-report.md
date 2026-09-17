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
    file_upstream: false   # default
```

`file_upstream: false` (the default, and the zero value) is **dry-run**: the
hive evaluates everything, shows you the reports it *would* file on the
dashboard, and writes nothing upstream. The publish path returns immediately
when dry-run is set, before any GitHub client is touched.

Set `file_upstream: true` to allow filing. There is one knob, and it is
all-or-nothing: there is no per-trigger opt-in.

## The two triggers

A report is only ever built from evidence marked **attributable** — evidence
the hive believes points at hive's own behaviour rather than at your repos,
your prompts, or your infrastructure. Non-attributable evidence never produces
a report under either trigger.

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

## Exactly what leaves the hive

Each report carries:

| Field | Value |
|---|---|
| anonymous instance | first **16 hex characters of the SHA-256** of your hive ID |
| hive version, commit | build stamp, short commit |
| mode | the hive's mode |
| ACMM level | e.g. `L3` |
| unmet criterion | name and key (`acmm-shortfall` only) |
| component, agent, lane | which part of the hive produced the evidence |
| error class | the classified error, not the raw message |
| count + window | how many occurrences, over what window |
| detected periodicity | if the failures look periodic |
| self-recovered | `no` on a report, `yes` on a recovery |

The same fields appear as issue labels (`fleet-report`, `component:…`,
`severity:…`, `version:…`, `instance:…`, `trigger:…`, and `criterion:…` for
shortfalls).

### What it does not send

- **Your raw hive ID never leaves the hive.** Only the truncated SHA-256
  digest is sent. The digest is stable, so upstream can tell "the same hive
  again" from "a second hive" without learning which hive either is.
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

The hive also remembers the body hash of what it last posted, so a condition
that persists unchanged does not re-comment every cycle. It re-reports only
when the body actually changes, when the issue is gone, or when a previously
recovered condition comes back.

When the condition clears, the hive posts a **recovery** comment
(`self-recovered: yes`). If the hive opened that issue itself, it also closes
it; if a human opened the issue, the hive comments but **leaves it open** —
it will not close someone else's issue.

## State

Report state lives at `/data/fleet-report-state.json`: the per-criterion epoch
history that drives the two-epoch threshold, and the open-issue records
(number, URL, whether the hive opened it, body hash, recovery flag). Deleting
it resets the epoch counters and makes the hive forget which upstream issues
are already open, so a persisting condition can file again.

## Previewing before you opt in

The evaluation runs on every dashboard status refresh **regardless of the
knob** — dry-run changes only whether anything is written upstream. So you can
leave `file_upstream: false`, run normally, and read the reports the hive would
have filed on the dashboard's fleet-report panel.

That is the intended way to adopt this: watch the dry-run output until you are
satisfied that what it would send is both accurate and something you are
willing to publish, then opt in.

## See also

- [Fleet health: the verdict and remediation hints](fleet-health.md) — the
  per-hive verdict on `/fleet`, which is about *your* hive's state rather than
  upstream reporting.
- [Fleet drift signals](fleet-drift-signals.md) — per-hive deviation badges.
