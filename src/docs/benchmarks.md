# Benchmarks: publishing per-PR agent/model/runtime outcomes

Every hive already seals one [review evidence bundle](review-evidence.md) per
PR head. Those bundles record which agent and model opened the PR, which
backend (harness) ran it, how the review swarm scored it, whether CI was green
at merge, when it was opened and merged, and whether it was reverted. The
`benchmarks` feature turns that evidence into **anonymized per-PR benchmark
records** and publishes them to the public leaderboard at
<https://hivecommons.dev/benchmarks/> (data repo
[`hivecommons/benchmarks`](https://github.com/hivecommons/benchmarks)), so the
fleet can compare models, harnesses and sandbox runtimes on real work rather
than on vendor evals. Design discussion: hivecommons/hive#17694.

## What is published

One record per PR per role, conforming to
[`schema/result.schema.json`](https://github.com/hivecommons/benchmarks/blob/main/schema/result.schema.json)
(vendored in `pkg/evidence/benchmarks/testdata/`):

| Field | Source |
|---|---|
| `source.hive_id` | **Salted hash** of the hive id: `sha256("hivecommons-benchmarks/v1/" + hive_id)[:16]`. The hive id itself never leaves the spoke. |
| `source.hive_version`, `source.cluster` | Build version and `HIVE_CLUSTER_ID`. |
| `agent.name`, `agent.role` (`author` / `reviewer`), `agent.harness` | Bundle `author.agent`, `author.backend`; reviewer records come from the dominant `(backend, model)` among the swarm verdicts. |
| `model.provider`, `model.name` | Bundle `author.model`; the provider is inferred from the model name prefix. |
| `sandbox_runtime` | Stamped on new bundles from the agent's effective crux sandbox (`podman`, `kubernetes-job`, …) or `none` when agents run in-process. Old bundles fall back to the current config. |
| `task.kind` | Stamped from the agent's lane (`docs`, `security-fix`, `ci-repair`, `release`, `dependency-update`, `triage`, `refactor`), else `unknown`. Reviewer records are `pr-review`. |
| `task.acmm_level` | `policy.acmm_level` at review time. |
| `outcome.*` | `pr_opened`, `pr_merged`, `merged_at`, `time_to_merge_s`, `ci_status_at_merge`, `ci_green_first_try`, `review_verdict`, `reverted`, `closed_unmerged`, `human_interventions` (human approvals / label changes). |
| `quality.*` | Swarm verdict counts and confidence. |
| `cost.*` | Estimated spend — see [Cost](#cost-estimated-never-billed). |

**What never leaves the spoke:** the hive id, repository and organization
names (unless the repository is public — see below), PR numbers, commit SHAs,
agent prompts, diff content, reviewer comments, human logins, per-session
token usage and any billed figure. Only the per-PR *estimated* USD and its
token share leave, rounded by the price table.

## Cost: estimated, never billed

Each record carries `cost.basis` and, when attributable, `cost.usd_est`:

| `cost.basis` | Meaning |
|---|---|
| `tokens_x_pricing` | The spoke's launch-cost collector (`/api/launch-cost`, `/data/launch-cost.json`) saw a kick that named this PR — the swarm's `owner/repo#N` PR ref for reviewer records, or the governor's `repo#N` issue ref of an issue the PR closed (`issue_closures`) for author records. The kick's token usage is priced with `pkg/tokens/pricing.go` (public list prices, `price_table_date`) and split evenly across the refs the kick named — the same `share_usd` the Cost panel shows. `launches` counts the kicks folded in; `*_tokens` are the in-window token share, informational. |
| `unknown` | No kick with usage named the PR: kicked without a ref, older than the launch-cost store, or usage landed outside the 6 h ownership horizon. |

The spoke never emits `fleet_model_median`; the benchmarks site imputes that
at index-build time from attributed records of the same model and role and
labels it as such. `reported` (a gateway-billed figure) is reserved. List
prices are not what subscription plans or self-hosted inference cost, so the
site shows every dollar as an estimate with its basis mix.

## Privacy model: default on, anonymized

| `hive.yaml` key | Default | Effect |
|---|---|---|
| `benchmarks.publish` | `true` | Publish anonymized records (hashed hive id, no repo/org). Set `false` to **opt out** entirely: `/api/benchmarks/export` returns zero records and the hourly push to the hub stops. |
| `benchmarks.repo_consent` | `true` | Attach `task.repo` / `source.org` **only for repositories GitHub reports as public** (visibility is checked through the App client and cached for 24h; unknown ⇒ treated as private). Public-repo activity is already public, so this adds no new disclosure. Set `false` to anonymize even public repos. Private repos are never named regardless of this setting. |

Both toggles are in **Settings → Features → Benchmarks** in the dashboard,
which also shows this hive's hash so an operator can find their row in the
leaderboard's per-hive table.

## Cadence: hub-driven, hourly

```
spoke (/data/evidence)  --hourly POST /api/benchmarks/ingest-->  hub (/data/benchmarks/<hash>/<YYYY-MM>.json)
                                                                   |
                                                                   +--hourly Contents-API commit--> hivecommons/benchmarks data/fleet/<hash>/<YYYY-MM>.json
                                                                                                       |
                                                                                                       +--GitHub Pages rebuild on push--> hivecommons.dev/benchmarks
```

1. **Spoke → hub (push, hourly).** Three minutes after start and then every
   hour, the spoke converts bundles updated since its persisted watermark
   (`/data/benchmarks-publish.json`), caps a batch at 2000 records and POSTs
   them to `<hub>/api/benchmarks/ingest` with the same per-hive heartbeat
   bearer used for NPS and task-status pushes. The hub recomputes the hash
   from the authenticated hive id and rejects any record claiming a different
   `source.hive_id`, so one spoke cannot write into another's rows. Push was
   chosen over hub-pull because the hub holds no credential for spokes and
   most spoke dashboards are cluster-private.
2. **Hub → GitHub (hourly).** The hub merges records by id into month files
   under `/data/benchmarks/`, marks changed files pending, and once an hour
   (ten minutes after start, at most 60 files per run) writes them to
   `hivecommons/benchmarks` at `data/fleet/<hash>/<YYYY-MM>.json` through the
   Contents API using the hub's GitHub App identity (the installation needs
   `contents: write` on the data repo). Identical content is skipped.
3. **Pages** rebuilds `data/index.json` on every push, so the site lags the
   spoke by at most ~2 hours.

Until the hub publisher is rolled out, the data repo's
`scripts/backfill-from-cluster.sh` pulls `/data/evidence` straight from spoke
pods and a nightly workflow rebuilds the index.

## Endpoints

| Endpoint | Where | Auth | Purpose |
|---|---|---|---|
| `GET /api/benchmarks/export?since=<RFC3339\|YYYY-MM-DD>` | spoke | owner | Schema-valid records for bundles updated since `since` (default 90 days). Empty when `benchmarks.publish: false`. |
| `GET /api/status` → `benchmarks{}` | spoke | owner | `hive_hash`, `publish`, `repo_consent`, `last_push_at`, `last_push_count`, `total_pushed`, `last_error`, `watermark_at`, `site_url`. |
| `POST /api/benchmarks/ingest` | hub | per-hive heartbeat bearer | Spoke push target. |
| `GET /api/hub/benchmarks` | hub | public status | Last ingest/publish time, pending files, per-hive-hash record counts. |

## Hub environment

| Variable | Default | Purpose |
|---|---|---|
| `HIVE_BENCHMARKS_REPO` | `hivecommons/benchmarks` | Target data repo (`owner/name`). |
| `HIVE_BENCHMARKS_PUBLISH_DISABLED` | unset | Any value stops the GitHub publisher; ingest still stores records locally. |

## Code map

- `pkg/evidence/benchmarks` — record types, `HiveHash`, `Convert` (bundles → records), schema golden test.
- `pkg/dashboard/api_benchmarks.go` — export endpoint, status block, hourly pusher, Settings panel data.
- `pkg/dashboard/benchmarks_cost.go` — `CostLookup` over the launch-cost collector (daily rollup USD + snapshot tokens).
- `pkg/hub/benchmarks.go` — ingest, month-file store, hourly GitHub publisher, status endpoint.
- `pkg/evidence` `Author.runtime` / `Author.task_kind` — stamped at bundle write via `github.EvidenceAgentContext`.
