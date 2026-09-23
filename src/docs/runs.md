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
