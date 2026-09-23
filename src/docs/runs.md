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

## v6 run-surface acceptance

The v6 live acceptance variant lives in `src/test/runs_e2e_v6_test.go` and uses
the same `HIVE_URL`, `HIVE_TOKEN`, `HIVE_RUNS_E2E_KEY` or
`HIVE_RUNS_E2E_REPO`/`HIVE_RUNS_E2E_ISSUE` variables as the v5 run test. It
adds the operator-surface assertions from #8466: `!runs approve <key>` through
dashboard chat advances the same plan lease, `/api/status` exposes the run for
the Runs card within one SSE tick, and `runs.checkpoints.plan` changes whether
the plan stage projects as blocked on a human.

```sh
HIVE_URL=http://<host>:<port> \
HIVE_TOKEN=<owner-token> \
HIVE_RUNS_E2E_REPO=<owner/repo> \
HIVE_RUNS_E2E_ISSUE=<number> \
go test -tags integration ./test -run RunsE2EV6
```

From the repository root:

```sh
just runs-e2e-v6
```

## Live proving workload runbooks

The following #8466 acceptance boxes require a live hub and must not be faked in
unit tests or CI fixtures. Leave their checklist rows unticked until an operator
runs these commands against a provisioned hub and records the evidence.

### Audit campaign over a pinned scope

```sh
HIVE_URL=http://<hub>:<port> \
HIVE_TOKEN=<owner-token> \
HIVE_AUDIT_SCOPE=<owner/repo>@<pinned-ref> \
hivectl runs audit-campaign --scope "$HIVE_AUDIT_SCOPE" --mode report-only --no-github-credentials --with-duplicate-pair --withhold-publication

curl -fsS -H "Authorization: Bearer $HIVE_TOKEN" \
  "$HIVE_URL/api/runs/<audit-run-key>" | jq '.burndown, .artifacts, .audit'
```

Expected evidence: validated findings, one intentional duplicate pair, no stage
process environment carrying GitHub credentials, publication withheld, and a
burndown with satisfied/remaining/unknown/scope-changed where unknown values are
never replaced by fabricated numbers.

### Wavefront migration campaign burndown

```sh
HIVE_URL=http://<hub>:<port> \
HIVE_TOKEN=<owner-token> \
CRUSTIFY_WAVEFRONT_GRAPH_URL=<pinned-graph-url> \
CRUSTIFY_WAVEFRONT_REPO=<owner/repo> \
hivectl runs wavefront-campaign --repo "$CRUSTIFY_WAVEFRONT_REPO" --graph-url "$CRUSTIFY_WAVEFRONT_GRAPH_URL" --worktrees --crash-reconcile=unknown

curl -fsS -H "Authorization: Bearer $HIVE_TOKEN" \
  "$HIVE_URL/api/runs/<wavefront-run-key>" | jq '.burndown, .stages'
```

Expected evidence: ready nodes advance by dependency wave, a dependent node only
becomes ready after its upstream receipt lands, a graph revision change refuses
stale work, crash reconciliation reports Unknown rather than duplicating a node,
and burndown is read across waves.

## How long-running runs start

Long-running runs are the Hive workflow behind `spec` -> `plan` -> `implement`
work. The workflow is default-off: with `runs.spektacular.enabled` unset or
`false`, Hive does not create the first stage lease from inception and does not
start the Spektacular poll loop.

## How a run starts

A run starts when Hive admits a real GitHub issue into the stage lease registry.
Admission creates one unowned `spec` lease keyed as
`<owner/repo>!<owner/repo>#<issue>:spec`; the run-stage work source then offers
that lease to agents when `governor.work_source.run_stages` is enabled. Repeated
admission of the same repo/issue is idempotent and leaves one run active.

Today there are two admission paths:

- **Run triage**: when `runs.triage.enabled` and `runs.spektacular.enabled` are
  both true, incoming actionable issues classified as `spec` call `AdmitRun`
  before the ordinary direct-fix dispatch path.
- **Inception approval**: when an inception moves to `complete` and
  `runs.spektacular.enabled` is true, `POST /api/inception/approve` may include
  `issue_url` or `repo` plus `issue_number`. Hive uses that explicit issue as
  the run key and calls `AdmitRun`.

Inception does not currently create or link a GitHub issue by itself. If an
approve request does not name an issue, Hive logs that no target was supplied
and does not fabricate a run.

See [spektacular.md](spektacular.md) for runner configuration and stage
advancement, and [work-sources.md](work-sources.md) for how pending run stages
are listed as work items.
