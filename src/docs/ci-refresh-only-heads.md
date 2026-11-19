# Refresh-only heads: which CI jobs skip a base refresh

Repositories governed by `auto_merge.require_current_base` (see
[Final base preparation](hive-merge.md#final-base-preparation-one-refresh-owner-no-ready-work-blockade))
get their behind pull requests refreshed by the hive App: an update-branch
merge commit, "Merge branch 'v5' into ...", authored by the App and signed by
GitHub. Each refresh is a new head, so every pull_request workflow runs again.
Measured on this repository: about 16 workflows and 38 runner-minutes per
refresh, and 133 refreshes in 48 hours. The heaviest jobs on a refresh are the
Docker multi-arch build (about 8.7 minutes), FOSSA (about 6.8 minutes) and
Mutation (about 3.2 minutes), and none of them can learn anything new from a
head whose PR content did not change.

## The rule

On a pull_request `synchronize`, `.github/scripts/refresh-only-head.sh`
answers `refresh_only=true` only when **all** of these hold:

1. The pull request's head branch is in this repository (fork heads always
   run everything).
2. The new head is a refresh of the previous head: a two-parent merge commit
   whose signature GitHub verified, authored by the hive App
   (`hivecommons-hive[bot]`) and committed by `web-flow` (or the App itself),
   with the previous head as one parent. The other parent is a commit on the
   base branch and is the merge-base of the new head with the base.
3. The three-dot diff of the new head against the base is identical, blob for
   blob, to the previous head's. A merge that resolved a conflict or changed
   any file the PR touches fails this.
4. The same workflow already concluded `success` on the previous head. If that
   run was cancelled (a refresh often cancels the run in flight), the check
   walks back through earlier refreshes, at most 5, each held to rules 2 and
   3, until it finds a head that passed. A head that is not a refresh stops
   the walk.
5. The base delta brought in since that passing head passes the workflow's
   delta policy (`DELTA_POLICY`):
   - **`docs-only` (Docker, an allowlist).** Every file in the delta is
     `CHANGELOG.md`, under `changelog.d/`, `docs/` or `src/docs/`, or any
     `*.md` file. Any other file, a `Makefile` or a `.go` file included,
     means the images build.
   - **`deny` (FOSSA and Mutation, the default).** No file in the delta is a
     dependency manifest or lock file anywhere in the tree (`go.mod`,
     `go.sum`, `go.work`, `package*.json`, `npm-shrinkwrap.json`,
     `yarn.lock`, `pnpm-lock.yaml`, `requirements*`, `pyproject.toml`,
     `poetry.lock`, `Cargo.*`), `.fossa.yml`, a Dockerfile or
     `.dockerignore`, under `src/deploy/`, `.github/workflows/` or
     `.github/actions/`, or the detector script itself.

This is the workflow form of the proof the hive uses to carry a recorded
decision across its own refresh, `HeadIsBaseRefreshOf` in
`src/pkg/github/pr_decision_base_refresh.go`. The script works on a blobless
clone (`--filter=blob:none`, about 3 seconds for this repository) and compares
`git diff --raw --no-renames` output, so it needs trees only.

**Fail safe.** Every lookup failure, unexpected shape or doubt answers
`false`, and `false` means run everything. The detector steps are
`continue-on-error: true`, so a broken detector leaves the answer empty, which
the jobs also treat as "run everything". Each run writes one line to the step
summary: either `skipped: refresh-only head.` with the evidence, or
`refresh-only check: running everything (<reason>)`.

## What skips, what still runs

| Workflow | On a refresh-only head |
| --- | --- |
| `docker.yml` (`docs-only`) | `gate` runs and reports success (it is a required context). The `build`, `build-contributor`, `build-hub` and `build-mx` multi-arch jobs are skipped. |
| `fossa.yml` (`deny`) | The `FOSSA license scan` job runs and reports success; its scan, baseline and policy-test steps are skipped. |
| `mutation-advisory.yml` (`deny`) | The job runs and reports success; setup, the mutation run and the comment upsert are skipped, so the previous head's comment stays (it describes the same diff). |
| everything else | Unchanged. `build-and-test`, the test shards and `coverage` always run, as do every other required context. |

**Post-merge backstop.** The skip only ever applies to `pull_request` runs.
Every push to `v5` still runs the full multi-arch build: the `push` trigger
matches every branch (`.github/workflows/docker.yml:4-15`), `v5` is in
`LONG_LIVED` (`.github/workflows/docker.yml:157`), so `gate` decides
`push=true` and each `build-*` job runs on it (`.github/workflows/docker.yml:317`).
A refresh-only PR that skipped its images is built again the moment it
merges.

The skip is decided **inside** the jobs, never with trigger `paths:` filters:
a required context must attach to every head, and a filtered workflow never
reports on the heads it does not match.

## Permissions and pinning

The detector reads the head commit and this workflow's runs on earlier heads
with the job's `GITHUB_TOKEN`, so `docker.yml`'s `gate` job, `fossa.yml` and
`mutation-advisory.yml` add `actions: read`. The script lives under
`.github/scripts/`, not `src/scripts/`, so it is outside the
`TRUSTED_SCRIPTS_TREE` pin in `.github/scripts/integration-governance.cjs`;
changes to it are covered by the `.github` tree that integration candidates
already record. A change to the script is itself in the sensitive list, so a
base refresh that brings one in always runs everything.

`.github/scripts/test-refresh-only-head.sh` exercises the script against a
local git fixture and a stubbed `gh` (clean refresh, multi-hop walk, and every
negative case); it runs in the `PR Auto-update Regression Tests` workflow.

## Related

- [Final base preparation](hive-merge.md#final-base-preparation-one-refresh-owner-no-ready-work-blockade): who refreshes a behind PR and when.
- [CI runner labels](ci-runner-labels.md): where these jobs run.
