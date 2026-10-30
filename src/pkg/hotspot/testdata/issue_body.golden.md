<!-- hive:hotspot file=pkg/dashboard/api.go -->

`pkg/dashboard/api.go` keeps being touched by pull requests that are open at the same time. In the last 7 days 8 distinct PRs changed it, 11 pairs of them were open concurrently, and 2 merge conflicts or rebase failures were blamed on it. Every concurrent pair is a rebase someone (or the merge train) has to redo, and a 2400-line file makes each of those rebases slower and riskier.

Proposed fix: a **mechanical split** into 4 files in the same package. No behaviour change, exported API unchanged, tests move with the code they cover. Land it when no other PR is open against this file (check the merge-train overlap first) so the split itself does not become one more collision.

## Evidence

| metric | value |
|---|---|
| window | 7 days |
| distinct PRs | 8 |
| concurrent PR pairs | 11 |
| conflicts attributed | 2 |
| lines | 2400 |
| kind | code |
| score | 20.5 |

PRs in window: #1, #2, #3, #4, #5, #6, #7, #8

## Top-level units

- `Server`
- `handleAgents`
- `handleConfig`
- `handleStatus`
- `statusWithWebhooks`

## Split proposal

1. `pkg/dashboard/api.go` — core types and the entry points other files call; keeps `Server`, `statusWithWebhooks`
2. `pkg/dashboard/api_handleagents.go` — moves `handleAgents`
3. `pkg/dashboard/api_handleconfig.go` — moves `handleConfig`
4. `pkg/dashboard/api_handlestatus.go` — moves `handleStatus`

## Don't splinter

- At most 4 resulting files, each at least 150 lines; fewer is better.
- No behaviour change; the package, its exported API and every import path stay the same.
- Tests move with the code they cover; no new test helpers.
- One PR; land it when the file has no open PRs against it.

_Filed by the hive's merge-hotspot advisor (`GET /api/hotspots`). It will not file another issue for this file while this one is open._
