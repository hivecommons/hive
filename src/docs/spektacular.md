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

## What the runner does

Every 30 seconds (the contribute hub's cleanup tick) the runner looks at every
lease that carries a stage. For each `spec` or `plan` stage whose poll interval
has elapsed it runs

```
spektacular <spec|plan> status <name> --json
```

where `<name>` is the run key: the lease's canonical work-item key
`<repo>!<runKey>:<stage>` (see [work-sources.md](work-sources.md)) with the
repository prefix and stage suffix stripped. Then:

- `document_status: draft` leaves the lease alone.
- `document_status: final` writes a stage receipt
  (`/data/runs/receipts/<runKey>/<stage>-gen<gen>.json`, the
  `stage-receipt/v1` shape from `pkg/outputschema`), records a `stage_receipt`
  timeline event carrying the receipt digest and path, and calls the same
  `advanceLeaseStage` path the API uses. That fires the `stage_completed` hook
  and CEL trigger and records the `lease_stage_advanced` audit entry exactly
  as a manual advance would.
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
spektacular plan export <name> --json
```

and admits the returned tasks through `planning.DecomposeFromOutput` with
`AutoApprove: false`. Spektacular's structure is rendered verbatim into the
planner's task-list shape (`[T1] title (depends: T2) [agent_suitable]`); no
model is asked to redecompose an already-structured plan. The epic is found by
its `run_key` metadata (or created with it, bound to the run key as its external
ref). Its plan is a DRAFT: the run-stage work source lists `implement` only
after `ApprovePlan` (`POST /api/plans/{id}/approve`) sets `plan_status` to
`approved`. A run whose epic already carries a plan is not re-imported.

## Defensive handling of the open questions

Two questions from the #8227 discussion are still owed by the Spektacular side.
The runner handles both answers so it does not have to wait:

- **Stable `data.name` as the join key.** The runner assumes it is stable and
  uses it as the run key across spec, plan, and implement. If a status answer
  ever carries a different `name` than the one requested, the runner treats
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

## Contract assumptions encoded in the fixture

`pkg/spektacular/testdata/spektacular-fake/spektacular` is a shell fake of the
CLI. It encodes the following assumptions; the ones marked "assumed" go beyond
#8301 and must be confirmed on the Spektacular side:

1. `spektacular <spec|plan> status <name> --json` prints
   `{kind, name, document_status, current_step, completed_steps, created_at,
   updated_at, closed_at}` with `document_status` in `draft|final`, RFC3339
   timestamps, and `closed_at` `null` while not final (#8301).
2. A missing artifact exits non-zero and prints a JSON error object on
   stdout (#8301). Assumed: the object is `{"error": "...", "code":
   "not_found"}`; the runner also accepts a message containing "not found".
   Any other JSON error object is a `VerbError`; a non-zero exit without JSON
   is a `ContractError`.
3. Assumed: `spektacular plan export <name> --json` prints
   `{kind: "plan", name, tasks: [{ref, title, depends_on, execution}]}` for a
   final plan. This is the only verb the runner needs beyond #8301.
4. Assumed: the artifact name equals the run key (`data.name`) and is shared
   by the spec and the plan of one run.

Scenarios: `draft-final` (draft, draft, final), `never-final`,
`final-then-draft`, `missing`, selected through `SPEK_FAKE_SCENARIO`.

## Related

- [work-sources.md](work-sources.md): run stages as work items
  (`run_stages: true`) and the `<repo>!<runKey>:<stage>` key.
- [hooks.md](hooks.md): the `stage_completed` transition.
- [design/run-artifacts.md](design/run-artifacts.md): multi-repo waves.
