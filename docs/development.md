# Local development

This guide describes the local workflow for contributing to the Hive Go codebase on the `v4` branch.

## Prerequisites

- Git and GitHub CLI (`gh`) for normal issue and PR workflows.
- Go `1.25.6`, as declared by [`src/go.mod`](../src/go.mod).
- Docker or Podman if you are exercising containerized contributor relay or deployment paths.
- `tmux` for local agent/contributor workflows that attach CLIs to terminal sessions.
- `just` if you use the repository's helper recipes (`brew install just` on macOS, or install from the `just` project for your platform).
- Optional agent CLIs depending on what you test locally: Claude Code, GitHub Copilot/`gh`, Gemini, Bob, Goose, Codex, Pi, or Antigravity.

## Clone and branch

```bash
git clone https://github.com/hivecommons/hive.git
cd hive
git switch -c <topic-branch> origin/v4
```

Use `v4` as the PR base for ordinary Hive development. Rebase or recreate your branch from a fresh `origin/v4` before opening or updating a PR.

## Build

Build every package in the Go module:

```bash
cd src
go build ./...
```

To build only the main Hive binary during quick iteration:

```bash
cd src
go build ./cmd/hive
```

## Test

Run the module tests from `src/`:

```bash
cd src
go test ./...
```

The `src/test/` package holds the inception e2e/regression suite. Those tests
talk to a **live hive over the network** and sit behind the `integration` build
tag, so the command above compiles the package but runs none of them — a plain
`go test ./...` will never exercise this suite, and a green run says nothing
about it. To actually run it:

```bash
cd src
HIVE_URL=http://<host>:<port> HIVE_TOKEN=<token> go test -tags integration ./test/...
```

The suite skips itself (exit 0) when `HIVE_URL` is unset or the endpoint does
not answer a fast TCP dial, so a passing run without those variables means
"skipped", not "verified". Run it when you touch inception code; the normal
`go test ./pkg/...` loop is enough otherwise. See `src/test/doc.go` for the
package's own description.

If a local environment dependency prevents a full run, include the failing package and error summary in the PR and still run the narrower package tests affected by your change.

Useful narrower loops:

```bash
cd src
go test ./pkg/...
go test ./cmd/hive
```

### The plain command above is weaker than the gate

`go test ./...` is not what CI runs. Every shard in
[`.github/workflows/v2-tests.yml`](../.github/workflows/v2-tests.yml) — the
workflow that publishes the required `test` check — uses the same three flags:

```bash
cd src
go test ./pkg/hub     -short -race -count=1 -run '<1/3 slice>'   # test (hub i/3)
go test ./pkg/agent   -short -race -count=1 -run '<1/5 slice>'   # test (agent i/5)
go test $PKGS         -short -race -count=1                      # test (rest i/3)
```

The workflow shards for wall-clock only: `pkg/hub` and `pkg/agent` are each
sliced across several jobs by test *function* (they are single packages too
slow to run whole), and everything else from `go list ./pkg/... ./cmd/...` is
partitioned into balanced buckets. Every shard also runs with `-v` and posts
its slowest test functions to the job's step summary — look there before
reaching for a local profile when the gate feels slow. Scheduled runs add
`-shuffle=on` (the seed is in the log); the PR gate does not, so an
order-dependent test shows up as a filed issue rather than as a red PR. The union of the shards is the whole of `./pkg/...` and
`./cmd/...`, so what changes between your loop and the gate is the *flags*, not
the coverage. To reproduce the gate locally in one unsharded run:

```bash
cd src
go test ./pkg/... ./cmd/... -short -race -count=1
```

What each flag changes:

- **`-race`** is the one most likely to catch a bug you would otherwise ship. A
  data race or a lock-ordering mistake usually passes a non-race run every time
  and only surfaces under load in production. This repository has repeatedly
  paid for that: see the `writeMu` / `WriteControl` reasoning in
  [`src/pkg/dashboard/contribute_ws.go`](../src/pkg/dashboard/contribute_ws.go),
  which exists because concurrent writers to a single WebSocket produced real
  mutex re-entrancy deadlocks. If you run only one thing before pushing, run
  the race build of the packages you touched.
- **`-short`** sets `testing.Short()`, so any test guarded by
  `if testing.Short() { t.Skip(...) }` does **not** run in CI. This cuts both
  ways, and both directions bite. A slow test you write without a `Short` guard
  runs on every shard and adds to the gate's wall clock. A test you write
  *behind* a `Short` guard is never executed by the gate at all — a green
  required check says nothing about it, exactly as a plain run says nothing
  about the `integration` suite above. Guard slow *setup*, not the assertion
  that proves your fix.
- **`-count=1`** disables the test result cache. Without it, an unchanged
  package reports its previous verdict instead of re-running, which is
  precisely what you do not want when you are trying to reproduce a failure or
  chase a flake.

Two flags that appear in CI but are *not* part of the PR gate:

- **`-timeout 600s`** is used only by the hourly coverage cron
  ([`.github/workflows/coverage-hourly.yml`](../.github/workflows/coverage-hourly.yml)),
  which runs the suite unsharded. The PR shards pass no `-timeout`, so they take
  Go's default of 10 minutes per shard binary. The practical bound on a shard is
  therefore the same 10 minutes, applied to a much smaller slice of the suite.
- **`-coverprofile`** is added by each shard to feed a per-package coverage
  report. Coverage is scored, but it is not the required merge gate; a failing
  test is.

Neither the PR gate nor the cron runs `./test/...`: both enumerate
`./pkg/... ./cmd/...` explicitly, and the integration suite additionally needs
the `integration` build tag and a live hive, as described above.

## Format and lint expectations

Run `gofmt` on Go files you edit:

```bash
gofmt -w path/to/file.go
```

The v4 CI workflow runs `go vet ./...` after building the Hive binary. Reproduce that check locally from the Go module:

```bash
cd src
go vet ./...
```


## If CI says "NOTICE is out of date"

`NOTICE` lists every Go module compiled into the shipped binaries. It is
**generated**, not hand-edited, so any change to `src/go.mod` or `src/go.sum`
— including a Dependabot version bump — makes it stale and fails the
`notice-drift` job ("NOTICE matches the module graph") in
`.github/workflows/go-security-analysis.yml`.

Regenerate and commit it:

```bash
bash src/scripts/generate-notice.sh   # writes NOTICE at the repo root
```

Three things that will otherwise cost you a CI round trip:

- **Commit the output verbatim.** The check is byte-exact. Do not reformat it,
  do not strip trailing whitespace — several dependency licences contain
  trailing spaces on their own lines, and removing them produces a permanent
  diff against what CI generates.
- **The generator needs the module's Go toolchain.** `src/go.mod` pins a
  specific version; running under an older `go` makes `go-licenses` fail to
  resolve stdlib packages and abort before writing anything. Set
  `GOTOOLCHAIN` to the pinned version if your default `go` is older.
- **A red `notice-drift` is not always yours.** Because `NOTICE` lives on the
  branch, a dependency bump merged without regenerating it leaves `v4` itself
  stale — and then *every* open PR inherits the failure, including docs-only
  ones. Check whether `v4` is clean before assuming your change caused it.

A `FORBIDDEN` result is a different problem: the module graph contains a
licence the project cannot ship (this is how an AGPL-3.0 dependency was caught
in #5016). That needs the dependency removed or replaced, not a regeneration.

There is no public `just lint` recipe in the current root `Justfile`; use `go vet ./...` for the repository's documented local lint-equivalent check, plus `gofmt`, `go build`, and targeted `go test` for the files you change.

## Running Hive locally

The quickest operator path remains Docker Compose from the root README:

```bash
cp src/hive.yaml.example src/hive.yaml
# src/.env, NOT ./.env — `-f src/docker-compose.yaml` makes `src/` the project
# directory, so that is the `.env` Compose reads. A root `.env` is ignored.
echo "HIVE_GITHUB_TOKEN=ghp_..." > src/.env
# REQUIRED: the dashboard's auth proxy refuses to start without it.
printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$(openssl rand -hex 32)" >> src/.env
docker compose -f src/docker-compose.yaml up -d
```

For source-level debugging, build with `go build ./cmd/hive` from `src/` and run the generated binary with a local `src/hive.yaml`. Keep real tokens in your shell or local ignored `.env` files, never in commits.

## Just recipes

The root [`Justfile`](../Justfile) is the discoverable entry point for contributor relay automation. List the public recipes with:

```bash
just --list
```

Current public recipes are centered on the **contribute** workflow:

- `just contribute-check <backend>` — read-only preflight for an agent backend CLI.
- `just contribute-setup <backend>` — one-time setup for GitHub auth, hub registration, and backend readiness.
- `just contribute-hive [backend] [mode]` — start contributing work to a hive, using a container by default or local mode when requested.
- `just contribute-status`, `just contribute-browse`, and `just contribute-stop` — inspect, discover, or stop contributor relay activity.
- `just contribute-k8s [namespace] [outfile] [image_tag]` — print Kubernetes manifests for a headless contributor workload; it prints or writes the manifest you request and does not apply it.
- `just hive-api <endpoint>` and `just hive-api-docs` — inspect hub API endpoints for the configured hive.

Deployment and development tasks that are not listed by `just --list` are not public recipes today. Use the Go, Docker Compose, and Kubernetes commands documented in the README and `src/docs/` for those workflows.

## Before opening a PR

1. Rebase on the latest `origin/v4`.
2. Run the build and tests that match your change.
3. Commit with DCO sign-off: `git commit -s`.
4. Open a PR against `v4` with an emoji-prefixed title, testing notes, and `Fixes #...` lines for closing issues.
