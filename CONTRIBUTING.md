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

## Style and quality

- Format Go changes with `gofmt`.
- Prefer small, focused PRs with tests or a clear explanation when tests are not practical.
- Keep configuration values configurable instead of hard-coding environment-specific paths, tokens, or endpoints.
- Do not commit secrets, generated credentials, or local runtime state.
- For documentation changes, verify every command, path, and branch name you mention.

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
