# Contributing to Hive

Thank you for helping improve KubeStellar Hive. This guide is for contributing code and documentation to this repository. If you want to donate compute to a running hive, see [Contribute to a Hive](README.md#contribute-to-a-hive) instead.

**New here?** Start with the [getting-started guide for first-time contributors](docs/getting-started-contributing.md) — it walks the end-to-end journey (finding an issue, local setup, testing without a cluster, key concepts, and what the review/CI process looks like) and links back into this guide for the mechanics.

## Where to work

- Open issues and pull requests in this repository. Use the issue templates when they are available, and link related issues from the PR body.
- Discuss design and review questions in GitHub issues and PRs so decisions remain public and searchable.
- Follow the [KubeStellar Code of Conduct](CODE_OF_CONDUCT.md) and [Hive governance](GOVERNANCE.md).
- Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## Repository layout

- `src/` — the current Go module (`github.com/hivecommons/hive`) and the main development target for this repository.
  - `src/cmd/hive` — main Hive binary.
  - `src/cmd/hivectl`, `src/cmd/apiproxy`, `src/cmd/hive-backup` — supporting command-line tools.
  - `src/pkg/` — Go packages for agents, GitHub integration, scheduling, policies, dashboards, hubs, backups, and related runtime behavior.
  - `src/policies/` — policy prompts and rule files used by the deterministic/agent pipeline. Treat policy changes like code: review the behavior they enable, test where possible, and explain risk in the PR.
  - `src/deploy/` and `src/examples/` — deployment manifests and example configuration.
  - `src/docs/` — architecture and operator/developer reference material.
  - `src/test/` — integration and regression tests.
- `bin/` — deterministic pipeline, supervision, enforcement, deployment, and maintainer helper scripts. See [`bin/README.md`](bin/README.md) for the script-by-script index.
- `config/hive-project.yaml.example` — project metadata for the top-level
  deterministic shell pipeline; see [config/README.md](config/README.md). This
  is separate from the Go runtime config in `src/hive.yaml.example`.
- `dashboard/`, `docs/`, `config/`, `systemd/`, `launchd/`, and top-level scripts — supporting assets for hub, dashboard, installation, and operational workflows.
- `Justfile` — contributor relay recipes; see [Just recipes](docs/development.md#just-recipes).

## Branches

Use `v4` as the base branch for Hive work and PRs unless a maintainer asks otherwise. The `main` branch is not the active target for changes.

Before starting work:

```bash
git fetch origin
git switch -c <topic-branch> origin/v4
```

## Local development

See [docs/development.md](docs/development.md) for the full local setup guide. The short path is:

```bash
cd src
go build ./...
go test ./...
```

The Go version is declared in [`src/go.mod`](src/go.mod). Install that version or newer compatible tooling before building.

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

See [src/docs/contributor-relay.md](src/docs/contributor-relay.md) for the end-to-end contributor relay workflow and Kubernetes workload details.

## Style and quality

- Format Go changes with `gofmt`.
- Prefer small, focused PRs with tests or a clear explanation when tests are not practical.
- Keep configuration values configurable instead of hard-coding environment-specific paths, tokens, or endpoints.
- Do not commit secrets, generated credentials, or local runtime state.
- For documentation changes, verify every command, path, and branch name you mention.

## Test policy

**A change to behavior must come with a test that would fail without it.** This
is the project's standing expectation, not a per-PR negotiation.

- **Bug fixes** add a test that reproduces the bug — one that fails on the
  parent commit and passes on the fix. A fix whose test passes either way has
  not demonstrated it fixes anything.
- **New functionality** adds tests covering its normal path and the failure
  modes a caller can actually hit.
- **Security-relevant changes** assert the invariant, not the implementation.
  A test that merely calls a guard proves nothing; it must fail when the guard
  is removed.
- **Tests are in scope for review.** A test asserting the wrong thing is worse
  than no test, because it reports green while the behavior is broken.

Where a test is genuinely impractical — a change that only affects real cloud
infrastructure, or a docs-only edit — say so in the PR body and explain what
you did to verify it instead. "Tests not practical" without that explanation is
a reason for a reviewer to push back.

Static analysis runs in CI (`go vet`, `golangci-lint`, `gosec`, and
`govulncheck`; see [`.github/workflows/go-security-analysis.yml`](.github/workflows/go-security-analysis.yml)).
Fix findings rather than suppressing them; when a suppression is genuinely
right, comment why at the suppression site.

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

- Target `v4` for all code and documentation contributions (the active development branch).
- Start PR titles with the repository's emoji convention, for example `📖 docs: ...`, `🐛 fix: ...`, or `✨ feature: ...`.
- Include `Fixes #<issue>` lines for issues the PR closes.
- Describe what changed, why, and how you tested it.
- Include the relevant command output or a short note such as `Not run (docs only)` when tests are not applicable.
- Add a changelog fragment under [`changelog.d/`](changelog.d/README.md) for user-visible changes — features, fixes, security changes, migrations, deprecations, and breaking changes (see "Changelog fragments" below). Routine refactors, test-only changes, and dependency churn are explicitly out of scope — add the `no-changelog` label if the advisory `changelog-fragment-guard` check asks anyway. Do **not** append to `CHANGELOG.md`'s `## Unreleased` section directly: every PR editing that one shared heading is what made unrelated PRs merge-conflict with each other ([#5675](https://github.com/hivecommons/hive/issues/5675)); fragments are compiled into [CHANGELOG.md](CHANGELOG.md) automatically at release time.
- Expect maintainers to ask for focused follow-up changes rather than broad drive-by edits.

## Changelog fragments

One file per PR, named `changelog.d/<category>-<pr-or-slug>.md` where the category (`added`, `changed`, `deprecated`, `fixed`, `security`) picks the CHANGELOG subsection — and, through it, the semver bump of the next release. The file's content is exactly your entry: a single `- ` bullet in the same narrative style as existing `CHANGELOG.md` entries, no headings. The complete workflow:

```bash
echo '- The relay no longer drops long tasks ([#1234](https://github.com/hivecommons/hive/issues/1234)).' > changelog.d/fixed-1234-relay-drop.md
git add changelog.d/fixed-1234-relay-drop.md
git commit -s
```

`changelog.d/README.md` has the full format, the `no-changelog` exemption, and the release-marker escape hatch. Transition note: direct `CHANGELOG.md` edits are still accepted until 2026-09-09 so in-flight PRs can land unreworked.

## Maintainer resources

Project governance lives in [GOVERNANCE.md](GOVERNANCE.md). The current owner/approver signal is also reflected in [OWNERS](OWNERS). Security disclosure is handled through [SECURITY.md](SECURITY.md), not public issues.
