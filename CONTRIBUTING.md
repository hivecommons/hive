# Contributing to Hive

Thank you for helping improve KubeStellar Hive. This guide is for contributing code and documentation to this repository. If you want to donate compute to a running hive, see [Contribute to a Hive](README.md#contribute-to-a-hive) instead.

## Where to work

- Open issues and pull requests in this repository. Use the issue templates when they are available, and link related issues from the PR body.
- Discuss design and review questions in GitHub issues and PRs so decisions remain public and searchable.
- Follow the [KubeStellar Code of Conduct](CODE_OF_CONDUCT.md) and [Hive governance](GOVERNANCE.md).
- Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## Repository layout

- `v2/` — the current Go module (`github.com/kubestellar/hive/v2`) and the main development target for this repository.
  - `v2/cmd/hive` — main Hive binary.
  - `v2/cmd/hivectl`, `v2/cmd/apiproxy`, `v2/cmd/hive-backup` — supporting command-line tools.
  - `v2/pkg/` — Go packages for agents, GitHub integration, scheduling, policies, dashboards, hubs, backups, and related runtime behavior.
  - `v2/policies/` — policy prompts and rule files used by the deterministic/agent pipeline. Treat policy changes like code: review the behavior they enable, test where possible, and explain risk in the PR.
  - `v2/deploy/` and `v2/examples/` — deployment manifests and example configuration.
  - `v2/docs/` — architecture and operator/developer reference material.
  - `v2/test/` — integration and regression tests.
- `bin/` — shell helpers used by the existing automation and maintainer workflows.
- `config/hive-project.yaml.example` — project metadata for the top-level
  deterministic shell pipeline; see [config/README.md](config/README.md). This
  is separate from the v2 Go runtime config in `v2/hive.yaml.example`.
- `dashboard/`, `docs/`, `config/`, `systemd/`, `launchd/`, and top-level scripts — supporting assets for hub, dashboard, installation, and operational workflows.
- `Justfile` — contributor relay recipes; see [Just recipes](docs/development.md#just-recipes).

## Branches

Use `v2` as the base branch for Hive v2 work and PRs unless a maintainer asks otherwise. The `main` branch is not the active target for v2 changes. The `v4` branch is a separate forward-looking/synchronization line; do not target it for ordinary v2 fixes.

Before starting work:

```bash
git fetch origin
git switch -c <topic-branch> origin/v2
```

## Local development

See [docs/development.md](docs/development.md) for the full local setup guide. The short path is:

```bash
cd v2
go build ./...
go test ./...
```

The Go version is declared in [`v2/go.mod`](v2/go.mod). Install that version or newer compatible tooling before building.

## Contributor `just` recipes

The root [`Justfile`](Justfile) exposes the public contributor relay workflow. Run `just --list` to see the current recipe signatures; private implementation details are intentionally not listed there.

| Recipe | What it does |
| --- | --- |
| `just contribute-check <backend>` | Runs the same read-only backend CLI preflight used by setup, then reports whether the machine is ready for `contribute-setup`. |
| `just contribute-setup <backend>` | Checks the Justfile version, verifies the backend CLI, signs in with GitHub, registers with the configured hub, and writes `${HOME}/.config/hive/contributor.env`. |
| `just contribute-hive [backend] [mode]` | Starts the contributor relay. The default mode is containerized; pass `local` as the mode to run natively when the local tools are installed. |
| `just contribute-status` | Queries the configured hub for status and contributor profile information. |
| `just contribute-browse` | Discovers available public hive projects. |
| `just contribute-stop` | Stops a background contributor relay if one is running. |
| `just contribute-k8s [namespace] [outfile] [image_tag]` | Emits Kubernetes manifests for a headless contributor workload. It writes to stdout or the requested file; it does not apply the manifest. |
| `just hive-api <endpoint>` | Calls a hub API endpoint, defaulting to `/status`, using the configured hive URL. |
| `just hive-api-docs` | Opens the hub API documentation in a browser. |

See [v2/docs/contributor-relay.md](v2/docs/contributor-relay.md) for the end-to-end contributor relay workflow and Kubernetes workload details.

## Style and quality

- Format Go changes with `gofmt`.
- Prefer small, focused PRs with tests or a clear explanation when tests are not practical.
- Keep configuration values configurable instead of hard-coding environment-specific paths, tokens, or endpoints.
- Do not commit secrets, generated credentials, or local runtime state.
- For documentation changes, verify every command, path, and branch name you mention.


## Optional git hooks

The repository includes `githooks/post-checkout`. Install it only if you want the local checkout guard:

```bash
git config core.hooksPath githooks
```

The hook runs after branch checkouts in the primary worktree. It prevents that worktree from staying on a branch other than `main` by printing guidance and checking `main` back out. It is intended for long-running dashboard checkouts where feature work should happen in separate `git worktree add ...` directories. It does not run for file checkouts or linked worktrees, because linked worktrees have a `.git` file instead of a `.git` directory.

If the hook is not installed, normal Git behavior applies. If it blocks a checkout unexpectedly, use a separate worktree from an unprotected checkout or remove the hooksPath setting for repositories where the guard is not desired.

## DCO sign-off

Every commit must include a Developer Certificate of Origin sign-off. Use:

```bash
git commit -s
```

The sign-off adds a `Signed-off-by:` trailer certifying that you have the right to submit the contribution under this repository's license. If you forget, amend the commit with `git commit --amend -s` and force-push your branch.

## Pull requests

- Target `v2` for v2 code and documentation.
- Start PR titles with the repository's emoji convention, for example `📖 docs: ...`, `🐛 fix: ...`, or `✨ feature: ...`.
- Include `Fixes #<issue>` lines for issues the PR closes.
- Describe what changed, why, and how you tested it.
- Include the relevant command output or a short note such as `Not run (docs only)` when tests are not applicable.
- Expect maintainers to ask for focused follow-up changes rather than broad drive-by edits.

## Maintainer resources

Project governance lives in [GOVERNANCE.md](GOVERNANCE.md). The current owner/approver signal is also reflected in [OWNERS](OWNERS). Security disclosure is handled through [SECURITY.md](SECURITY.md), not public issues.
