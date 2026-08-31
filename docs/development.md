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
git clone https://github.com/kubestellar/hive.git
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
