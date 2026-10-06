# ADR-0022: Named planning-engine boundary for Project Inception runs

Status: Accepted

Tracked in #10175. Implementation in #10293.

Implemented on `v6` through the ordered children of #10293: #10352
(`pkg/planengine` neutral types, `Engine`, `Register`/`Lookup`), #10353 (the
stage observer ported off `ExecFunc`), #10354 (the CLI engine registered as
`spektacular`), #10355 (the `runs.engine` selector), #10356
(`wirePlanningEngine`), #10357 (engine-named `MetaSource`), #10358 (fake-engine
conformance tests) and #10359 (these docs). Operator-facing documentation is
[Spektacular runner](../spektacular.md) ("Selecting a planning engine" and
"Writing a planning engine") and
[Spektacular and Project Inception](../integrations/spektacular.md).

## Context

Project Inception runs move through three stages, `spec`, `plan`, and
`implement`, each held on a lease. When `runs.spektacular.enabled` is set,
`cmd/hive/spektacularwire.go` (`wireSpektacularRunnerStages`) probes the
binary with `spektacular.Probe`. It then installs
`spektacular.NewHubRunner(cfg.Runs, srv, logger)` as the dashboard's
`StageRunner` and, optionally, a `dashboard.SpekHubExecutor`. The runner polls
a **Spektacular-compatible CLI** for every active stage lease. To use a
different planning engine, an operator must either emulate that CLI byte for
byte or change Hive code. The [integration guide](../integration-guide.md)
lists this as a known gap.

#10175 asks for a named planning-engine boundary, but only if it keeps four
properties the current runner guarantees:

- **Lease ownership.** The engine is never told who holds a lease and cannot
  advance one. `spektacular.Runner` (`src/pkg/spektacular/poll.go`) observes
  documents and calls `Registry.Advance` / `Registry.Refuse`. It leaves
  `Unclaimed` admission leases alone, leaves `RelayHeld` leases to the
  contribute websocket, and has no retry. The retry budget
  (`runs.max_stage_retries`) belongs to the hub executor (#9143).
- **Stage receipts.** `spektacular.BuildReceipt`
  (`src/pkg/spektacular/receipt.go`) builds an
  `outputschema.StageReceipt` only from the status document and the lease.
  The receipt has `ContractRevision = "spektacular-status/v1"`,
  `Engine{Name: "spektacular", Version: "spektacular-pr-45"}`, an
  `InputRevision` that hashes kind, name, `document_status`, `current_step`,
  `completed_steps`, and `closed_at` (but not `updated_at`), and an
  `ExecutionKey` derived from run key, stage, and generation.
- **Plan import semantics.** On a final plan, `leaseAdapter.Advance`
  (`src/pkg/spektacular/adapter.go`) calls
  `LeaseRegistry.ImportRunPlan(runKey, repo, RenderTaskList(plan))`.
  `dashboard.Server.ImportRunPlan` (`src/pkg/dashboard/stage_leases.go`)
  creates or reuses the epic bound to `run_key`. It decomposes with
  `AutoApprove: false`, so the plan stays a DRAFT until `ApprovePlan`. It is
  idempotent by task-list digest. A regenerated plan replaces the previous one
  and returns the epic to draft. It leaves alone any epic whose children came
  from a different planner. It stamps `MetaSource = "spektacular"`.
- **Failure handling.** Failures are typed errors (`NotFoundError`,
  `VerbError`, `WorkDirError`, `PlanImportError`, `ContractError`) and
  refusal reasons (`stale_plan`, `replaced_document`, `archived_document`,
  `plan_import_failed`, `missing_workdir`) recorded through `Registry.Refuse`
  so an operator can see why a run is parked.

`pkg/dashboard` must not import `pkg/spektacular` (the internal-import
ratchet). The two sides share primitives through `spektacular.LeaseRegistry`,
and the `Attr*` keys are asserted equal in tests.

## Baseline engine: the current CLI contract

This is the `spektacular` engine. Its contract revision is
`spektacular-status/v1`. All verbs run with `cwd` set to the stage's resolved
`WorkDir`, print JSON on stdout, and exit non-zero with a JSON error envelope
on failure. Hive never passes `--json`.

| Purpose | Invocation | Hive code |
|---|---|---|
| Probe | `spektacular --version` (via `Probe`) | `probe.go` |
| Status | `spektacular <spec\|plan> status <name>` → `{kind,name,artifact_id,document_status,current_step,completed_steps,created_at,updated_at,closed_at,spec,plan}` | `Runner.statusInDir` |
| Resolve artifact | `spektacular <kind> file list`, then a project-file scan of `.spektacular/{specs,plans}` | `Runner.ResolveArtifact` |
| Plan export | `spektacular plan export <name> --format json` → `{kind,name,tasks:[{id,ref,repo,title,depends_on,execution}]}` | `Runner.exportPlanInDir` |
| Plan fallback | `spektacular plan file read <name>/tasks.json`, then `<name>/plan.md` parsed by `ParsePlanMarkdown` | `Runner.exportPlanFallbackInDir` |
| Spec body | `spektacular spec file read <name>.md`, retried as `<name>` on `unexpected_extension` (0.23+) | `Runner.readSpecInDir` |

`document_status` values are `draft`, `final`, `stale`, `superseded`, and
`archived`. Error envelope codes are `artifact_not_found` / `not_found`
(→ `NotFoundError`), `unexpected_extension` (retry without the extension), and
`plan_structure_invalid` (use the fallback). Any other code is a `VerbError`.

Any binary that meets this contract can already be plugged in through
`runs.spektacular.binary`. That stays supported and unchanged.

## Decision

Split the runner into a Hive-owned, engine-neutral **stage observer** and a
small named **planning engine** interface. The observer keeps lease, receipt,
import, and refusal logic. The engine only answers questions about documents.
The Spektacular CLI becomes the first registered engine and the default.

### Interface

New package `pkg/planengine`. It has no imports from `pkg/dashboard` or
`pkg/spektacular`.

```go
package planengine

// Engine observes planning documents. It never sees lease identity, task IDs,
// or generations, and it has no method that changes Hive state.
type Engine interface {
	// Name is the registry name and the receipt Engine.Name ("spektacular").
	Name() string
	// ContractRevision is stamped on every receipt ("spektacular-status/v1").
	ContractRevision() string
	// Probe reports presence/version for the dashboard status card.
	Probe(ctx context.Context) (ProbeResult, error)
	// Status returns the document status of artifact (kind = "spec"|"plan")
	// in dir, the stage's resolved WorkDir. Missing artifact → *NotFoundError.
	Status(ctx context.Context, dir, kind, artifact string) (ArtifactStatus, error)
	// ResolveArtifact maps Hive's run slug to the engine's artifact name.
	// Missing → *NotFoundError.
	ResolveArtifact(ctx context.Context, dir, kind, slug string) (string, error)
	// ExportPlan returns the structured task list of a FINAL plan, including
	// any engine-internal fallbacks. Failure → *PlanExportError.
	ExportPlan(ctx context.Context, dir, artifact string) (Plan, error)
	// ReadSpec returns the body of a FINAL spec for the checkpoint summary.
	ReadSpec(ctx context.Context, dir, artifact string) (string, error)
}

type ProbeResult struct{ Present bool; Version, Binary string }

// Builder constructs an engine from the runs config.
type Builder func(cfg config.RunsConfig, logger *slog.Logger) (Engine, error)

// Register panics on a duplicate name (same rule as worksource.RegisterAdditive).
func Register(name string, build Builder)
func Lookup(name string) (Builder, bool)
```

`ArtifactStatus`, `DocumentStatus`, `Plan`, `PlanTask`, `NotFoundError`,
`VerbError`, `ContractError`, `WorkDirError`, and `PlanImportError` move to
`pkg/planengine`. `pkg/spektacular` keeps type aliases so existing callers and
tests still compile. `PlanTask.UnmarshalJSON` (both export shapes) and
`ParsePlanMarkdown` stay in the Spektacular engine. They are wire parsing for
that engine, not part of the neutral contract.

The stage observer is the current `Runner` poll loop, `leaseAdapter`, and
`BuildReceipt`, moved into `pkg/planengine` and generalized as follows:

- `Runner.Exec ExecFunc` is replaced with `Runner.Engine Engine`. Every
  `statusInDir` / `ResolveArtifact` / `exportPlanWithFallbackInDir` /
  `readSpecInDir` call becomes an `Engine` call.
- `BuildReceipt(st, status, now)` gains an engine argument and takes
  `ContractRevision`, `Engine.Name`, and `Engine.Version` from it. For
  `spektacular` the receipt stays byte-identical: `"spektacular-status/v1"`,
  `"spektacular"`, `"spektacular-pr-45"`. The `Provenance.Query` text is
  supplied by the engine.
- The `LeaseRegistry`, `Registry`, `Stage`, `Attr*` keys, `Refuse*`
  constants, and `TickResult` do not change.

The CLI implementation (`BinaryExec`, verb constants, envelope parsing,
artifact-name normalization, export fallbacks) stays in `pkg/spektacular` as
`spektacular.Engine`. It registers itself as `"spektacular"` from `init()`.

### Configuration and selection

```yaml
runs:
  engine: spektacular        # optional; default "spektacular"
  spektacular:               # engine-specific block, unchanged
    enabled: true
    binary: /usr/local/bin/spektacular
    poll_interval_s: 30
    hub_executor: { … }
```

- Each engine reads its settings from `runs.<engine>`. Today only
  `runs.spektacular` exists, and all of its fields keep their meaning. A
  future engine adds its own block and its own `Enabled`.
- The stage observer is installed when the selected engine's block is
  enabled. With `engine` unset, the condition is exactly
  `runs.spektacular.enabled`, so existing configs behave the same. An unknown
  `runs.engine` fails config validation. If the dashboard live-rewires to an
  unknown engine, Hive logs the error and removes the stage runner
  (`SetStageRunner(nil)`). It never falls back to another engine.
- `wireSpektacularRunnerStages` becomes `wirePlanningEngine`. It looks up the
  builder, calls `Probe`, publishes the result through `SetSpektacularStatus`
  (the field set stays, and an `Engine` name is added), and installs the
  observer through `SetStageRunner`. `rewireSpektacular` keeps its
  keep/replace semantics from #10069.
- `ImportRunPlan` gains the engine name for `MetaSource`. `runPlanSource`
  becomes the engine name, which stays `"spektacular"` for the baseline.
  `findRunEpic`'s "another planner owns this epic" check compares against
  that value.

### What stays Hive-owned (invariants)

1. **Lease ownership.** The engine never receives identity, task ID,
   generation, or a registry handle. Only the observer calls `Advance` /
   `Refuse` / `RecordStageProgress`. `Unclaimed` and `RelayHeld` leases are
   never passed to the engine. An empty `WorkDir` is refused as
   `missing_workdir` before any engine call.
2. **Receipts.** The receipt is built by Hive from `ArtifactStatus` and the
   lease. The engine cannot provide receipt bytes, digests, or the
   `ExecutionKey`.
3. **Plan import.** The engine returns a `Plan`. Hive renders it with
   `RenderTaskList` and imports it through the unchanged `ImportRunPlan`:
   DRAFT, `AutoApprove: false`, idempotent by digest, replace on change, and
   no Wavefront fan-out until approval. Only a `final` plan is exported.
4. **Retries.** Retries are not part of the engine interface. The hub
   executor keeps `runs.max_stage_retries`.

### Failure mapping

| Engine outcome | Observer action (unchanged) |
|---|---|
| `Status` → `*NotFoundError` before first observation | `ResolveArtifact` and re-query; if still missing, counted in `TickResult.Errors` and retried next poll (not refused) |
| `Status` → `*NotFoundError` after observation | `Refuse(replaced_document)` |
| `document_status: stale`, or final → draft | `Refuse(stale_plan)` |
| `superseded` | `Refuse(replaced_document)` |
| `archived` | `Refuse(archived_document)` |
| `draft` | record progress, wait |
| `final` spec | `ReadSpec`, `BuildReceipt`, `Advance` |
| `final` plan, `ExportPlan` error or `ImportRunPlan` error | `Refuse(plan_import_failed)` |
| `*VerbError` / `*ContractError` / context deadline | counted in `TickResult.Errors`, retried next poll, not refused |
| `Probe` fails at wire time | dashboard shows engine absent; observer still installed (today's behavior) |

## Consequences

- A planning engine other than Spektacular can be added as a registered Go
  package without copying Spektacular's CLI quirks (extension handling, export
  fallbacks). Lease, receipt, and import guarantees are enforced in one shared
  place.
- Receipts name the engine that produced them, so mixed-engine history stays
  auditable.
- The boundary is compile-time. An out-of-process engine still has two
  routes: emulate the baseline CLI contract through `runs.spektacular.binary`,
  or wait for a later ADR that adds a remote engine behind this interface.
- `SpekHubExecutor` and the design-mode path
  (`pkg/dashboard/design_spektacular.go`, `spek_interview.go`) remain
  Spektacular-specific in this change. They write documents and do not
  observe them.

## Acceptance criteria for #10293

1. `pkg/planengine` contains `Engine`, `Builder`, `Register`/`Lookup`
   (duplicate registration panics), the moved neutral types, and the stage
   observer. `pkg/spektacular` registers `"spektacular"` and keeps type
   aliases. The `pkg/dashboard` import ratchet still passes.
2. `runs.engine` config with validation. Unset means `spektacular`. Unknown
   names are rejected. Existing `runs.spektacular` configs need no edits.
3. Parity tests: run the existing `spektacular` runner and adapter tests, and
   the `pkg/dashboard/spektacular_runner_test.go` integration, against the
   ported observer **unchanged** (only import paths and constructors may
   change). Add golden receipts showing byte-identical `StageReceipt` JSON
   before and after.
4. Tests with a fake `Engine` showing:
   - (a) `Unclaimed` / `RelayHeld` / empty-`WorkDir` stages never reach the
     engine;
   - (b) every row of the failure mapping table;
   - (c) a second registered engine name appears in `Receipt.Engine.Name`,
     `ContractRevision`, and `MetaSource`, and its plan imports as DRAFT;
   - (d) an unknown `runs.engine` installs no runner.
5. `wirePlanningEngine` replaces `wireSpektacularRunnerStages`, and the
   rewire keep path from #10069 is still covered.
6. Docs: update [Spektacular and Project Inception](../integrations/spektacular.md)
   and [Spektacular runner](../spektacular.md) with the `runs.engine`
   selector and a "Writing a planning engine" section that states the
   invariants above. Update the #10175 line in the integration guide's known
   gaps.

## Open questions

- Should `SpekHubExecutor` get a matching optional `StageAuthor` interface so
  a non-Spektacular engine can also author documents on the hub, or should
  authoring stay agent- and skill-driven?
- Should a remote engine (HTTP/JSON, as in ADR-0020 for work sources) be
  defined now as a second built-in, or only when a real second engine needs
  it?
- Should the baseline engine report its real CLI version in
  `Engine.Version` instead of `spektacular-pr-45`? That would change receipt
  bytes, so it needs its own contract revision.
