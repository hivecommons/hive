# Spektacular stage runner

The Spektacular stage runner is the Hive side of long-running runs
(hivecommons/hive#8303, part of the #8290 umbrella). A run moves through the
`spec`, `plan`, and `implement` stages on one task lease. Spektacular owns the
state of each artifact; Hive owns the workflow: it asks Spektacular whether the
current artifact is finished and advances the lease when it is.

The runner is OFF by default. With `runs.spektacular.enabled` unset or `false`
Hive never starts a Spektacular process and every existing lease behaviour is
unchanged.

## Enabling it

```yaml
runs:
  max_stage_retries: 2        # default; generations one stage may burn
  spektacular:
    enabled: true             # default false
    binary: spektacular       # default; resolved through PATH
    poll_interval_s: 30       # default
```

The Governor dialog's Features panel exposes the toggle and the binary path
under "Long-running runs"; the poll interval and the retry budget are yaml
only. The same keys are accepted by `PUT /api/config/governor/features` as
`spektacularEnabled` and `spektacularBinary`. The runner is installed at boot
(`cmd/hive`, `wireSpektacularRunner`), so a change to the toggle takes effect
on the next boot, like the other feature switches.

Package layout: `pkg/spektacular` owns the CLI boundary, the poll loop, and
the receipt; `pkg/dashboard` exposes its lease registry as a primitives-only
surface on `*Server` (`VisitActiveStageLeases`, `AdvanceStageLease`,
`RetryStageLease`, `RefuseStageLease`, `EscalateStageLease`, `ImportRunPlan`)
and drives whatever `StageRunner` was installed with `SetStageRunner` from the
contribute hub's cleanup tick. The dashboard never imports `pkg/spektacular`
(its internal-import ratchet); `pkg/spektacular.NewHubRunner` takes the
server through the `LeaseRegistry` interface.

## Triage

The runs triage pass is separate from the Spektacular runner and is also OFF by
default. When `runs.triage.enabled: true`, the scheduler classifies each
incoming actionable GitHub issue before a direct-fix kick:

- `run/spec` always admits a `spec` run.
- `run/fix` always leaves the issue on the normal direct-fix path.
- Complex issues, or issues carrying `spec_labels` (default `kind/feature`,
  `Epic`, `architecture discussion`), admit a `spec` run by creating the first
  stage lease. The lease is keyed as the run-stage work source expects: the
  repository plus the run key and `:spec` suffix (see
  [work-sources.md](work-sources.md)).
- Simple issues, medium issues, or issues carrying `fix_labels` (default
  `kind/bug`, `good first issue`) stay on the existing direct-fix path.
- Short bodies (`min_body_chars`, default 80), unchosen option lists, and
  unsubstituted template placeholders get one marker comment (`hive-triage`)
  asking the reporter for the missing details and are skipped for that cycle.

The decision is stored on the stage lease as `triage_verdict` and
`triage_rationale`, surfaced by `GET /api/runs` and run detail, and copied into
the first stage receipt timeline event. Owner reset with reason `triage_fix`
retires a spec-stage run so the issue can return to the direct-fix path.

```yaml
runs:
  triage:
    enabled: false             # default
    spec_labels: [kind/feature, Epic, architecture discussion]
    fix_labels: [kind/bug, good first issue]
    min_body_chars: 80
    clarify_comment: true
```

## What the runner does

Every 30 seconds (the contribute hub's cleanup tick) the runner looks at every
lease that carries a stage. For each `spec` or `plan` stage whose poll interval
has elapsed it runs

```
spektacular <spec|plan> status <name>
```

where `<name>` is the bare artifact name (`000057_git-commit`, never
`000057_git-commit.md` or `000057_git-commit/plan.md`): the lease's canonical
work-item key `<repo>!<runKey>:<stage>` (see [work-sources.md](work-sources.md))
with the repository prefix and stage suffix stripped, then reduced to the bare
name (`spektacular.ArtifactKey`; the dashboard applies the same rule in
`runKeyOfLease`). The CLI has no `--json` flag (its only global flag is
`--fields`); every verb already prints JSON, and an unknown flag is a usage
error. Then:

- `document_status: draft` leaves the lease alone.
- `document_status: final` writes a stage receipt
  (`/data/runs/receipts/<runKey>/<stage>-gen<gen>.json`, the
  `stage-receipt/v1` shape from `pkg/outputschema`), records a `stage_receipt`
  timeline event carrying the receipt digest and path, and calls the same
  `advanceLeaseStage` path the API uses. That fires the `stage_completed` hook
  and CEL trigger and records the `lease_stage_advanced` audit entry exactly
  as a manual advance would.
- Progress is decided by `document_status`, `current_step` and
  `completed_steps` only. The status document's `updated_at` is never read
  for a progress or staleness decision: it is workflow activity only while
  Spektacular's in-progress state matches the artifact, and otherwise a file
  mtime that a `git checkout`, a reformat or a `touch` moves without anything
  having happened. Spektacular may also omit it entirely when no workflow
  state matches, so the parser treats an absent, `null` or empty
  `updated_at` (and an empty `created_at` / `closed_at`) as unknown. A stale
  stage is decided by Hive's own lease clock (the lease's expiry), never by
  the artifact's timestamps. `TestUpdatedAtNeverDecides` in `pkg/spektacular`
  keeps it that way.
- A lease that lapses without `final` is retried through `retryLeaseStage`
  (a new generation of the same stage, `lease_stage_retried` in the audit
  log) while the budget allows. `max_stage_retries` counts generations
  including the first: at the default of 2 the stage runs once, is retried
  once, and the second expiry raises an escalation with `decision` severity
  (`lease_stage_escalated` in the audit log, a `blocked` timeline event with
  `severity=decision`). No third generation is ever minted; a person resets
  the stage or abandons the run.
- The `implement` stage has no Spektacular document. The runner never polls
  it; its completion is the existing hold-gated PR flow.

Ticks are idempotent per lease generation: once a generation has advanced it
is never polled or advanced again, a generation change (a retry, or the
successor stage of an advance) keeps the poll pacing instead of earning an
extra status call, and a second tick at the same instant is a no-op.

A retry is the one lease mutation allowed on an expired lease: it exists
because the generation lapsed, so the expired lease is its expected input and
receives a fresh window under its new generation. An advance still requires a
live lease.

### Plan import

When a `plan` reaches `final` the runner runs

```
spektacular plan export <name>
```

(still an open ask on the Spektacular side; see the contract section) and
admits the returned tasks through `planning.DecomposeFromOutput` with
`AutoApprove: false`. Spektacular's structure is rendered verbatim into the
planner's task-list shape (`[T1] title (depends: T2) [agent_suitable]`); no
model is asked to redecompose an already-structured plan. The epic is found by
its `run_key` metadata (or created with it, bound to the run key as its external
ref). Its plan is a DRAFT: the run-stage work source lists `implement` only
after `ApprovePlan` (`POST /api/plans/{id}/approve`) sets `plan_status` to
`approved`. A run whose epic already carries a plan is not re-imported.

## Defensive handling of the open questions

The #8227 questions were answered on jumppad-labs/spektacular#45. The runner
encodes the answers and still handles the alternatives it cannot rule out:

- **The bare artifact name is the join key.** The stable key across spec,
  plan and implement is the artifact name itself (`000057_git-commit`),
  shared by convention across the spec file, the plan directory and the
  changelog record, and recorded as the workflow's `data.name`. It is NOT the
  `spec:` / `plan:` frontmatter cross-references the status response also
  carries: those are almost never populated (0 of 57 specs, 3 of 55 plans in
  the Spektacular repository itself), so the runner surfaces them as
  `ArtifactStatus.Spec` / `.Plan` for diagnostics only and never joins on
  them. Every spelling of an address (`<name>.md`, `<name>/plan.md`) reduces
  to the bare name before it reaches the CLI. If a status answer ever
  carries a different bare `name` than the one requested, the runner treats
  it as a contract violation and does not advance.
- **Invalidation after `final`.** If `document_status` goes from `final` back
  to `draft` under the same name, the runner treats the artifact as a stale
  plan: it refuses to advance, logs, and records `lease_stage_refused` with
  reason `stale_plan`. If the artifact the lease is bound to disappears after
  having been observed (a new document with a new name replaced it), the
  runner refuses with reason `replaced_document` and parks the lease for an
  explicit reset. It never rebinds a lease to a document it was not minted
  for. A parked stage is polled again only after its generation changes.

## What Hive never does

Hive never opens a Spektacular file. Every fact about an artifact reaches the
runner through the CLI boundary (`Runner.Exec`), which is also the seam tests
replace. `TestNoDirectFileAccess` in `pkg/spektacular` scans the package for
file access to keep it that way.

## Contract: what is confirmed, what changed, what is still open

`pkg/spektacular/testdata/spektacular-fake/spektacular` is a shell fake of the
CLI. It encodes the per-artifact status contract exactly as
jumppad-labs/spektacular#45 ships it, after the Spektacular maintainer's
review of 2026-09-23 answered the questions Hive had left open. The original
assumptions from the first cut of the runner (PR #8398) and their fate:

Confirmed:

1. `spektacular <spec|plan> status <name>` prints one JSON object with
   `kind`, `name`, `document_status` (`draft|final`), `current_step`,
   `completed_steps[]`, `created_at`, `updated_at`, `closed_at` (#8301). The
   response also carries `error: false` (every Spektacular result does) and
   the `spec` / `plan` frontmatter cross-references. `created_at` and
   `closed_at` are frontmatter dates emitted as RFC3339 midnight UTC.
2. A missing artifact exits non-zero and prints the JSON error envelope on
   stdout. The code is `artifact_not_found` (the `file` verbs use
   `not_found` for the same condition; both are typed as `NotFoundError`).
3. Returning a `final` artifact to `draft` flips `document_status` on the
   same document and clears its close date; it does not create a new
   document. This is stated in the #45 description from the metadata
   lifecycle rules and was not contradicted in review, though the maintainer
   did not address it explicitly. The `stale_plan` refusal is therefore the
   normal invalidation path and `replaced_document` the exception.

Changed:

4. The join key is the bare artifact name, not the `spec:` / `plan:`
   cross-references (see above). Hive now strips a document path and a
   markdown extension from every address before it reaches the CLI.
5. The error envelope is `{"error": true, "code", "message", "resource",
   "next_action"}`, not `{"error": "<message>", "code"}`. The parser accepts
   both (the string form stays as a defensive fallback).
6. `updated_at` is "last modified", not "last activity", and may be absent.
   Hive no longer uses it anywhere: the receipt ends at `closed_at` (or the
   advance instant) and its input hash excludes `updated_at` so two
   observations of one final document hash the same.
7. `closed_at` is `""` while the document is open, not `null`; `created_at`
   may be `""` when the frontmatter carries no date. Both decode as unknown.
8. There is no `--json` flag on any verb (`unknown flag: --json`); output is
   already JSON. Hive passes none. `spec file list` / `plan file list`
   likewise take no flag; Hive does not call them today, but Spektacular may
   add a `ModTime` per list entry so one list call can replace N status
   calls, which is the shape a future list-based poll would consume.
9. `plan status <name>` reports `plan.md` only (`PlanFilePath` hardcodes
   it), so a plan's `document_status` is that of `plan.md`, not of the whole
   plan document set. Hive advances on it as the plan's status; if a plan
   ever grows documents whose completion matters, the verb has to widen
   first.

Still open:

10. `spektacular plan export <name>` printing `{kind: "plan", name, tasks:
    [{ref, title, depends_on, execution}]}` for a final plan is still an
    assumption: the verb does not exist yet and remains an open ask on the
    Spektacular side. It is the only verb the runner needs beyond #8301.
    Until it lands, a plan that reaches `final` logs an export failure and
    does not advance.

Scenarios: `draft-final` (draft, draft, final), `never-final`,
`final-then-draft`, `final-no-updated-at` (final with the `updated_at`
member absent), `missing`, selected through `SPEK_FAKE_SCENARIO`. The fake
answers `artifact_not_found` for a name spelled with an extension or a
document path, as the real store does, and a usage error for `--json`.

## Related

- [work-sources.md](work-sources.md): run stages as work items
  (`run_stages: true`) and the `<repo>!<runKey>:<stage>` key.
- [hooks.md](hooks.md): the `stage_completed` transition.
- [design/run-artifacts.md](design/run-artifacts.md): multi-repo waves.
