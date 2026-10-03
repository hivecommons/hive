# Spektacular and Project Inception

## Audience

This page is for tool authors who want to understand Hive's Project Inception and long-running-run seam. It distinguishes what is actually pluggable from what is simply a Spektacular-compatible CLI contract.

## Concepts

Project Inception can admit an approved issue into a long-running `spec` run when Spektacular support is enabled. The approval handler checks `runs.spektacular.enabled` before admission (`src/pkg/dashboard/inception_handlers.go:269`). The stage runner is installed at boot only when that same config flag is true (`src/cmd/hive/spektacularwire.go:19`).

Spektacular owns artifact state; Hive owns the workflow lease. Hive polls Spektacular for `spec` and `plan` artifacts, advances leases on final documents, and imports final plan tasks into Hive's planner (`src/pkg/planengine/adapter.go:114`). The dashboard side stays decoupled through the stage lease interface and `SetStageRunner` (`src/pkg/dashboard/stage_leases.go:87`).

## Interface

Hive talks to a CLI executable, configured by `runs.spektacular.binary` with default `spektacular` (`src/pkg/config/runs_config.go:137`). The runner executes commands through `BinaryExec`, which runs the configured binary with arguments and captures stdout (`src/pkg/spektacular/runner.go:249`).

Required status command:

```sh
spektacular <spec|plan> status <name>
```

The JSON response maps to `ArtifactStatus` (`src/pkg/spektacular/runner.go:127`):

```json
{
  "error": false,
  "kind": "spec",
  "name": "000057_git-commit",
  "artifact_id": "artifact-uuid-or-stable-key",
  "document_status": "draft",
  "current_step": "outline",
  "completed_steps": ["intake"],
  "created_at": "2026-10-02T00:00:00Z",
  "updated_at": "2026-10-02T12:00:00Z",
  "closed_at": "",
  "spec": "000057_git-commit",
  "plan": "000057_git-commit"
}
```

Hive decides progress from `document_status`, `current_step`, and `completed_steps`; `updated_at` is informational and never decides progress (`src/pkg/spektacular/runner.go:127`). `document_status: final` advances; `draft` waits; `stale` parks the run for human attention (`src/docs/spektacular.md:139`).

Preferred final-plan export:

```sh
spektacular plan export <name> --format json
```

The response maps to `Plan`/`PlanTask` (`src/pkg/spektacular/runner.go:225`). Spektacular 0.23+ identifies each task by a UUID `id` (there is no plan-local `ref`) and emits `repo` and `execution` as objects:

```json
{
  "kind": "plan",
  "name": "000057_git-commit",
  "tasks": [
    {
      "id": "8f1c2b7e-0000-4000-8000-000000000001",
      "repo": {"name": "hivecommons/hive", "location": "."},
      "title": "Add encoding helpers",
      "depends_on": ["8f1c2b7e-0000-4000-8000-000000000000"],
      "execution": {"type": "agent_suitable", "reason": "code change"}
    }
  ]
}
```

The pinned 0.22 release has no `export` verb (it swallows `export` as a positional argument and rejects `--format`), so Hive falls back there; the string-valued `repo` / `execution` and `ref` forms are still accepted for the `tasks.json` convention below.

If export is unavailable, Hive falls back to `spektacular plan file read <name>/tasks.json` and then `<name>/plan.md` (`src/pkg/spektacular/runner.go:429`, `src/pkg/spektacular/runner.go:445`).

## Step-by-step: use or emulate Spektacular

1. Enable the runner:

   ```yaml
   runs:
     max_stage_retries: 2
     spektacular:
       enabled: true
       binary: spektacular
       poll_interval_s: 30
   ```

   These fields are `SpektacularConfig` and default off (`src/pkg/config/runs_config.go:137`).
2. Ensure the binary prints JSON for `spec status`, `plan status`, and preferably `plan export --format json`; Hive passes no `--json` flag.
3. Use the bare artifact name as the CLI address. Hive normalizes `<name>.md` and `<name>/plan.md` to `<name>` with `ArtifactKey` (`src/pkg/spektacular/runner.go:65`).
4. Let Hive own stage leases. `RunStageLeaseAccessor` lists pending stages as work items when `run_stages: true` is enabled (`src/pkg/worksource/run_stage.go:37`).
5. Let Hive import final plan tasks. `ImportRunPlan` admits exported tasks as a draft Hive plan; implement work is listed only after approval (`src/pkg/dashboard/stage_leases.go:478`).

## Example flow

1. An operator approves an Inception run with an issue URL; when Spektacular is enabled, Hive admits the issue as a `spec` run (`src/pkg/dashboard/inception_handlers.go:269`).
2. The runner polls `spektacular spec status <name>` until the spec is final (`src/pkg/spektacular/runner.go:359`).
3. Hive writes a stage receipt and advances the lease to `plan` (`src/pkg/planengine/adapter.go:114`).
4. The runner polls `plan status`; on final it imports the structured plan (`src/pkg/spektacular/runner.go:403`).
5. Hive exposes implement work as run-stage items after plan approval (`src/pkg/worksource/run_stage.go:62`).

## Testing

Use `pkg/spektacular/testdata/spektacular-fake/spektacular`, which implements the CLI scenarios documented by the runner page. Tests cover no direct file access, status parsing, stale/final transitions, retries/escalations, plan export fallback, and dashboard lease integration (`src/docs/spektacular.md:277`, `src/pkg/planengine/adapter_test.go:128`, `src/pkg/dashboard/spektacular_runner_test.go:153`).

## Operational notes

- Auth and secrets: Hive invokes a local binary; any Spektacular backend credentials belong to that tool's own environment, not to Hive's dashboard token.
- Rate limits: tune `poll_interval_s`; every active `spec` or `plan` stage is polled at that cadence (`src/pkg/config/runs_config.go:145`).
- Failure modes: missing artifacts become typed not-found errors; stale/replaced documents are refused rather than silently rebound; expired leases retry until `max_stage_retries` then escalate (`src/pkg/spektacular/runner.go:300`, `src/pkg/spektacular/runner.go:520`).
- What is exposable: status JSON, plan export JSON, run-stage work items, stage receipts, and campaign projections through `/api/campaigns`.
- What is not exposable: Hive does not open Spektacular files directly, does not provide a generic planning-engine registry, and does not let a third-party engine mutate Hive's leases except through the configured runner boundary.

## Gaps

- A different inception/planning engine can work only by behaving like the Spektacular CLI or by adding new Hive code. There is no named `runs.<engine>` registry for Project Inception yet; tracked in [#10175](https://github.com/hivecommons/hive/issues/10175).
