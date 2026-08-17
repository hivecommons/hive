# Local development

This guide describes the local workflow for contributing to the Hive Go codebase on the `v2` branch.

## Prerequisites

- Git and GitHub CLI (`gh`) for normal issue and PR workflows.
- Go `1.25.6`, as declared by [`src/go.mod`](../src/go.mod).
- Docker or Podman if you are exercising containerized contributor relay or deployment paths.
- `tmux` for local agent/contributor workflows that attach CLIs to terminal sessions.
- `just` if you use the repository's helper recipes (`brew install just` on macOS, or install from the `just` project for your platform).
- Optional agent CLIs depending on what you test locally: Claude Code, GitHub Copilot/`gh`, Gemini, Bob, Goose, Codex, Pi, or Antigravity.

## Clone and branch

```bash
git clone -b v2 https://github.com/kubestellar/hive.git
cd hive
git switch -c <topic-branch> origin/v2
```

Use `v2` as the PR base for ordinary Hive development. Rebase or recreate your branch from a fresh `origin/v2` before opening or updating a PR.

## Build

Build every package in the Go module:

```bash
cd v2
go build ./...
```

To build only the main Hive binary during quick iteration:

```bash
cd v2
go build ./cmd/hive
```

## Test

Run the module tests from `src/`:

```bash
cd v2
go test ./...
```

The `src/test/` package contains integration/regression coverage and may exercise local ports, temporary state, and helper processes. If a local environment dependency prevents a full run, include the failing package and error summary in the PR and still run the narrower package tests affected by your change.

Useful narrower loops:

```bash
cd v2
go test ./pkg/...
go test ./cmd/hive
```

## Format and lint expectations

Run `gofmt` on Go files you edit:

```bash
gofmt -w path/to/file.go
```

The v2 CI workflow runs `go vet ./...` after building the Hive binary. Reproduce that check locally from the Go module:

```bash
cd v2
go vet ./...
```

There is no public `just lint` recipe in the current root `Justfile`; use `go vet ./...` for the repository's documented local lint-equivalent check, plus `gofmt`, `go build`, and targeted `go test` for the files you change.

## Running Hive locally

The quickest operator path remains Docker Compose from the root README:

```bash
cd v2
cp hive.yaml.example hive.yaml
export HIVE_GITHUB_TOKEN=ghp_...
docker compose up -d
```

For source-level debugging, build with `go build ./cmd/hive` and run the generated binary with a local `hive.yaml`. Keep real tokens in your shell or local ignored files, never in commits.

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

1. Rebase on the latest `origin/v2`.
2. Run the build and tests that match your change.
3. Commit with DCO sign-off: `git commit -s`.
4. Open a PR against `v2` with an emoji-prefixed title, testing notes, and `Fixes #...` lines for closing issues.
