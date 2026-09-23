# Runs

Runs are long-lived, staged work items driven through the v5 HTTP API and the
Spektacular stage runner. A live run is visible through `/api/runs`, individual
run details are served at `/api/runs/{key}`, and approved plans release the
`implement` stage through the existing plan approval API.

## Acceptance

The layer-2 live acceptance test for #8466 lives in
`src/test/runs_e2e_test.go` and is excluded from normal builds by the
`integration` build tag.

Run it against a live hive configured with the in-tree Spektacular fake:

```sh
HIVE_URL=http://<host>:<port> \
HIVE_TOKEN=<token> \
HIVE_RUNS_E2E_REPO=<owner/repo> \
HIVE_RUNS_E2E_ISSUE=<number> \
go test -tags integration ./test -run Runs
```

From the repository root, the same command is available as:

```sh
just runs-e2e
```

The test uses the same `HIVE_URL` / `HIVE_TOKEN` gating as the inception suite:
if `HIVE_URL` is unset, the whole `src/test` integration suite skips. The run
target must be a pinned issue that triages to `run/spec`; use either
`HIVE_RUNS_E2E_KEY` or `HIVE_RUNS_E2E_REPO` plus `HIVE_RUNS_E2E_ISSUE`.

Known #8460 gaps are guarded in the test rather than hidden:

| Guard | Probe | What turns on when the gap lands |
| --- | --- | --- |
| gap 2 of #8460 | an HTTP-visible run-stage accessor / worksource feature probe | reclaim/lookupLease generation fencing and implement-stage queue listing |
| gap 3 of #8460 | `plan_epic_id` plus successful plan approval and plan-stage advance | final plan import and `plan -> implement` advancement |
| gap 7 of #8460 | terminal run state on `/api/runs/{key}` | implement completion ends the run |
| gap 10 of #8460 | `burndown` field on `/api/runs/{key}` | satisfied/remaining/unknown/scope-changed burndown assertions |

`GET /api/runs` lists active staged runs from live leases and keeps the shape cheap for polling.

`GET /api/runs/{key}` returns the same run detail plus timeline-derived stage history. When the run key maps to a wired convergence campaign, the detail response may include:

```json
"burndown": {
  "source": "wavefront",
  "satisfied": 3,
  "remaining": 21,
  "unknown": 0,
  "scope": 24
}
```

`source` identifies the provider (`wavefront` or `audit`). `scope` is the total obligation count, `satisfied` is completed work, `remaining` is known outstanding work, and `unknown` is evidence that could not be classified. The block is omitted when no burndown source is wired for the run key.

## Checkpoint policy

`runs.checkpoints.{spec,plan,implement}` controls checkpoint policy at run stage boundaries. Missing keys and explicit `true` keep the owner checkpoint blocking wherever that stage has an approval gate. Explicit `false` relaxes that checkpoint: Hive records the approval actor as `auto`, includes the config source in the audit/timeline payload, and, for plan/implement gates, releases the run exactly as a human approval would.

The implement checkpoint can only be relaxed at ACMM L5 or higher (`RunImplementCheckpointMinACMM`). Below that level Hive treats `runs.checkpoints.implement: false` as blocking and reports the ACMM reason in the run state.

Example:

```yaml
runs:
  checkpoints:
    spec: true
    plan: false
    implement: true
```

With this policy, the plan checkpoint is auto-approved after import; spec and implement still wait for owner approval.
