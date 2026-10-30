# Spektacular and Project Inception

## Audience

Use this page to choose between operating the existing runner and implementing a compatible planning CLI. Project Inception can turn an approved idea into a specification, a plan, then implementation work.

## Operator path: use the existing runner

Follow [Using Spektacular with Hive](https://docs.hivecommons.dev/docs/hive/spektacular-runs) for installation, project visibility, enablement, and a first run. Before enabling it, the Spektacular binary and the project/artifacts must be reachable **from the Hive process**, not just a contributor's clone.

The `runs:` and `governor:` settings belong to **Hive's configuration** (the file selected by `--config`/`HIVE_CONFIG`, normally `hive.yaml`), not Spektacular's project `config.yaml`. Use the deployment's supported settings path; see [configuration ownership](../agent-configuration.md#where-configuration-lives). The runner is installed **at boot**, so restart after enabling it and check for `[spektacular] stage runner installed`. Then verify a known artifact with `spektacular spec status <name>` in Hive's own working directory and environment.

Operators do not need to implement the interface below. Keep the tool's version compatible with the commands and JSON fields Hive consumes.

## Concepts

A **lease** records temporary ownership of one workflow stage. An **artifact** is the resulting specification or plan; a **receipt** records stage completion. The **planner** imports plan tasks into work that an owner must approve before implementation. For example, a final specification can finish the `spec` stage without granting permission to implement its unapproved plan.

Project Inception can admit an approved issue into a long-running `spec` run when Spektacular support is enabled. The approval handler checks `runs.spektacular.enabled` before admission (`src/pkg/dashboard/inception_handlers.go:269`). The stage runner is installed at boot only when that same config flag is true (`src/cmd/hive/spektacularwire.go:19`).

Spektacular owns artifact state; Hive owns the workflow lease. Hive polls Spektacular for `spec` and `plan` artifacts, advances leases on final documents, and imports final plan tasks into Hive's planner (`src/pkg/spektacular/adapter.go:97`). The dashboard side stays decoupled through the stage lease interface and `SetStageRunner` (`src/pkg/dashboard/stage_leases.go:87`).

## Interface

<details>
<summary>CLI-author contract, payloads, and implementation references</summary>

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

The response maps to `Plan`/`PlanTask` (`src/pkg/spektacular/runner.go:225`). This is a **fragment**, not a complete importable plan: `T0` must also exist in the full plan before `T1` can depend on it.

```json
{
  "kind": "plan",
  "name": "000057_git-commit",
  "tasks": [
    {
      "id": "T1",
      "ref": "T1",
      "repo": "hivecommons/hive",
      "title": "Add encoding helpers",
      "depends_on": ["T0"],
      "execution": "agent_suitable"
    }
  ]
}
```

If export is unavailable, Hive falls back to `spektacular plan file read <name>/tasks.json` and then `<name>/plan.md` (`src/pkg/spektacular/runner.go:429`, `src/pkg/spektacular/runner.go:445`).

</details>

## Step-by-step: use or emulate Spektacular

**For CLI authors:** these are compatibility requirements, not additional operator setup. For an existing installation, use the operator route above.

1. Enable the runner:

   ```yaml
   runs:
     max_stage_retries: 2
     spektacular:
       enabled: true
       binary: spektacular
       poll_interval_s: 30
   ```

   Put these in the Hive configuration described above. These fields are `SpektacularConfig` and default off (`src/pkg/config/runs_config.go:137`); restart Hive to install the runner.
2. Ensure the binary prints JSON for `spec status`, `plan status`, and preferably `plan export --format json`; Hive passes no `--json` flag.
3. Use the bare artifact name as the CLI address. Hive normalizes `<name>.md` and `<name>/plan.md` to `<name>` with `ArtifactKey` (`src/pkg/spektacular/runner.go:65`).
4. Let Hive own stage leases. `RunStageLeaseAccessor` lists pending stages as work items when `run_stages: true` is enabled (`src/pkg/worksource/run_stage.go:37`).
5. Let Hive import final plan tasks. `ImportRunPlan` admits exported tasks as a draft Hive plan; implement work is listed only after approval (`src/pkg/dashboard/stage_leases.go:485`).

## Example flow

1. An operator approves an Inception run with an issue URL; when Spektacular is enabled, Hive admits the issue as a `spec` run (`src/pkg/dashboard/inception_handlers.go:269`).
2. The runner polls `spektacular spec status <name>` until the spec is final (`src/pkg/spektacular/runner.go:359`).
3. Hive writes a stage receipt and advances the lease to `plan` (`src/pkg/spektacular/adapter.go:97`).
4. The runner polls `plan status`; on final it imports the structured plan (`src/pkg/spektacular/runner.go:403`).
5. Hive exposes implement work as run-stage items after plan approval (`src/pkg/worksource/run_stage.go:62`).

## Testing

Use `pkg/spektacular/testdata/spektacular-fake/spektacular`, which implements the CLI scenarios documented by the runner page. Tests cover no direct file access, status parsing, stale/final transitions, retries/escalations, plan export fallback, and dashboard lease integration (`src/docs/spektacular.md:277`, `src/pkg/spektacular/adapter_test.go:142`, `src/pkg/dashboard/spektacular_runner_test.go:153`).

## Operational notes

- Auth and secrets: Hive invokes a local binary; any Spektacular backend credentials belong to that tool's own environment, not to Hive's dashboard token.
- Rate limits: tune `poll_interval_s`; every active `spec` or `plan` stage is polled at that cadence (`src/pkg/config/runs_config.go:145`).
- Failure modes: missing artifacts become typed not-found errors; stale/replaced documents are refused rather than silently rebound; expired leases retry until `max_stage_retries` then escalate (`src/pkg/spektacular/runner.go:300`, `src/pkg/spektacular/runner.go:520`).
- What is exposable: status JSON, plan export JSON, run-stage work items, stage receipts, and campaign projections through `/api/campaigns`.
- What is not exposable: Hive does not open Spektacular files directly, does not provide a generic planning-engine registry, and does not let a third-party engine mutate Hive's leases except through the configured runner boundary.

## Gaps

- A different inception/planning engine can work only by behaving like the Spektacular CLI or by adding new Hive code. There is no named `runs.<engine>` registry for Project Inception yet; tracked in [#10175](https://github.com/hivecommons/hive/issues/10175).
