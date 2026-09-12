# Contributing to Hive

Thank you for helping improve KubeStellar Hive. This guide is for contributing code and documentation to this repository. If you want to donate compute to a running hive, see [Contribute to a Hive](README.md#contribute-to-a-hive) instead.

**New here?** Start with the [getting-started guide for first-time contributors](docs/getting-started-contributing.md) — it walks the end-to-end journey (finding an issue, local setup, testing without a cluster, key concepts, and what the review/CI process looks like) and links back into this guide for the mechanics.

## Where to work

- Open issues and pull requests in this repository. Use the issue templates when they are available, and link related issues from the PR body.
- **A good issue is a real contribution here, not a lesser one.** Much of this repository is written by its maintainers and by the hive's own agents, so the highest-leverage thing an outside contributor can usually do is describe a problem precisely enough to be acted on. When a PR resolves your issue, the commit credits you as a co-author — you appear in the repository's contributor list and on your own GitHub contribution graph, exactly as if you had written the patch. See [Crediting issue authors](#crediting-issue-authors).
- Pull requests are welcome too, and nothing above changes how they are reviewed.
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

Container mode limits the contributor workload to 4 GiB of memory and 2 CPUs,
matching the `contribute-k8s` workload ceiling. Set `HIVE_CONTAINER_MEMORY` or
`HIVE_CONTAINER_CPUS` before `just contribute-hive` to tune those limits for
your machine (for example, `HIVE_CONTAINER_MEMORY=6g just contribute-hive`),
or set either value to `none` on a host that cannot enforce that controller.

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

Every commit must include a Developer Certificate of Origin (DCO) sign-off. The
DCO is your certification that you have the right to submit the contribution
under this repository's license, and Hive requires it on every non-merge commit.
Use:

```bash
git commit -s
```

The `-s` flag adds a `Signed-off-by:` trailer using your configured git identity,
for example:

```text
Signed-off-by: Your Name <you@example.com>
```

The sign-off email must match the commit author email. A GitHub noreply address
for the same authoring account is also acceptable, in either GitHub form:
`<login>@users.noreply.github.com` or
`<id>+<login>@users.noreply.github.com`. Do not sign with an arbitrary second
personal address unless it is also the commit author email; the checker cannot
verify that two unrelated email addresses belong to the same person.

Check your local identity before committing:

```bash
git config user.name
git config user.email
```

If your last commit is missing the trailer, or it used the wrong email, fix it
before review:

```bash
git commit --amend -s
git push --force-with-lease
```

To add sign-offs across a branch, rebase with sign-off and then force-push:

```bash
git rebase --signoff origin/v4
git push --force-with-lease
```

Only rewrite your own pull-request branch. Once bad DCO history lands on a
protected branch such as `v4`, contributors cannot repair it in place: protected
branch history is not rewritten, and maintainers must not add a DCO sign-off on
someone else's behalf. That is why the post-merge checker has narrow per-commit
waivers for already-merged history; waivers record a maintainer disposition, but
they are not a substitute for signing new commits correctly.

### How the DCO is enforced

Four workflows check sign-offs, at different points and for different failure
classes. The first two run at pull-request time, **while your branch is still
writable** — if either fails, fix it by rewriting your branch as shown above.
The last two report on protected-branch history that can no longer be
rewritten; their failures are resolved by maintainer disposition, not by you.

- **Copilot DCO** (`.github/workflows/copilot-dco.yml`) — runs on every pull
  request. Every non-merge commit on the PR branch must carry a
  `Signed-off-by:` trailer matching the commit author's email (or an
  acceptable GitHub noreply form of it, as described above). This is the check
  you will see most often; remediation is `git commit --amend -s` or
  `git rebase --signoff`.
- **DCO squash attribution** (`.github/workflows/dco-squash-attribution.yml`,
  the "Sign-off survives the squash" check) — runs on every pull request.
  Rejects a human-authored PR whose commits are signed off only by a bot
  identity. GitHub's squash merge writes the landing commit with the *PR
  author* as author while keeping the branch commit's message — and therefore
  its bot `Signed-off-by:` — producing a human-authored commit signed off by a
  bot on history that can no longer be fixed
  ([#6798](https://github.com/hivecommons/hive/issues/6798)). So a sign-off
  that is valid on each branch commit can still fail here. Fix: re-sign the
  branch commits with your own identity (`git rebase --signoff` after setting
  your `user.email`). The check is skipped when the PR author is itself a bot.
- **DCO push-delta gate** (`.github/workflows/dco-push-delta.yml`) — runs on
  direct pushes to `v4`/`v5` and checks exactly the commits that push
  introduced ([#6756](https://github.com/hivecommons/hive/issues/6756)). This
  is a detection gate, not a rejection gate: by the time it turns red, the
  commits are already on the protected branch, so the failure is recorded and
  dispositioned rather than fixed in place.
- **Post-merge DCO trailer check** (`.github/workflows/dco-post-merge.yml`) —
  hourly sweep of a rolling window of recent `v4`/`v5` history. It owns the
  waiver accounting (`DCO_WAIVED_COMMITS` for single historical commits,
  `DCO_ALLOWLIST_EMAILS` for maintainer-accepted identities) and files issues
  on failures. The waiver semantics — what each instrument accepts and its
  blast radius — are documented in
  [`src/docs/v5-sync-policy.md`](src/docs/v5-sync-policy.md#inherited-dco-failures-from-v4).

All four reuse the same trailer validation (`src/scripts/check-dco-trailers.sh`
for the range-scanning gates), so they cannot disagree about what a valid
sign-off looks like.

## Crediting issue authors

When your PR resolves an issue somebody else filed, credit them on the commit with a `Co-authored-by:` trailer ([#6588](https://github.com/hivecommons/hive/issues/6588)). This is what turns "thanks, closing" into a contribution that GitHub actually records: a co-authored commit puts the filer in the repository's contributor list and on their own contribution graph.

`src/scripts/issue-coauthor.sh` builds the trailer from the issue, so you do not have to look the identity up:

```bash
# print it
src/scripts/issue-coauthor.sh 6588
# → Co-authored-by: hanthor <5840441+hanthor@users.noreply.github.com>

# or add it to the commit you just made
git commit -s -m "🐛 fix: ..."
src/scripts/issue-coauthor.sh --amend 6588
```

The script emits nothing (and exits 0) when there is nobody to credit — a bot filed the issue, or you did. It fails rather than emitting a half-right line if the issue or its author cannot be resolved, because GitHub silently ignores an address it cannot match: a malformed trailer looks like credit while crediting nobody.

Two things worth knowing if you write the trailer by hand instead:

- **Use the `<id>+<login>@users.noreply.github.com` form.** It is the only address guaranteed to resolve to the account, and it does not republish a personal address the filer never offered for this repository's git history.
- **Co-authorship is attribution, not certification.** It does not sign off for anyone: the `Signed-off-by:` trailer is still yours alone, and a commit carrying only a `Co-authored-by:` still fails the DCO check. Adding a co-author never changes your own DCO obligations, and never satisfies theirs.

Credit the filer of the issue the PR fixes, not everyone who commented. If several people's issues are genuinely resolved by one PR, add one trailer each.

## Closing issues filed by humans

A merged fix is not the same thing as a resolved symptom. Issue [#6500](https://github.com/hivecommons/hive/issues/6500) was closed after a fix merged while the reported symptom persisted; the reporter could not reopen it (GitHub only lets users with write access, or whoever closed the issue, reopen it — and hive issues are closed by the App bot), so the same problem came back as two fresh bug reports ([#6762](https://github.com/hivecommons/hive/issues/6762), [#6767](https://github.com/hivecommons/hive/issues/6767)). To keep the loop closed:

- **Do not close a human-filed `bug` issue on "fix merged" alone.** Comment instead: link the fix PR and ask the reporter to confirm — for example, `Fix merged in #<pr> — @<reporter> please confirm the symptom is gone.` Close only after the reporter confirms, or after 7 days with no objection (say in the closing comment which of the two it was).
- **Enforced at PR-request time.** The hive's PR-request watcher (`pkg/github.validatePRRequestClaims`) rewrites `Closes #N` to `Refs #N` in an outgoing PR body when the referenced issue looks like a human-filed bug (a bug-family label — `bug`, `kind/bug`, `type/bug`, `type:bug`, `adoption-blocker` — with no `— hive:` attribution trailer and a non-`Bot` author), so a merge cannot auto-close it under the App bot. The reporter (or a maintainer with write access) can opt back in by adding `hive: reporter-confirmed` to the issue body or applying that label; the gate then lets `Closes #N` through untouched. Tracked in [#6781](https://github.com/hivecommons/hive/issues/6781).
- **Bot- and agent-filed issues are exempt** — verify against the stated evidence (a green run, a passing check) and close when it is green.
- **If a reporter says a closed issue is not fixed, reopen it** (or file the reopen on their behalf if they cannot) rather than letting them re-file from scratch. A comment from the original reporter on a closed issue saying the symptom persists is always grounds to reopen.
- **Reporters can reopen their own issue with `/reopen`.** Comment `/reopen` (optionally followed by what you are still seeing) on the closed issue and the `Issue Reopen Command` workflow reopens it. This exists because GitHub's own permission model will not let a reporter without write access reopen an issue the App bot closed — the dead end that produced [#6762](https://github.com/hivecommons/hive/issues/6762). It is limited to the issue's own author and to people with write access. It is a backstop, not the primary control: the PR-request gate above is what should stop a human-filed bug being auto-closed in the first place.

## Pull requests

- Target `v4` for all code and documentation contributions (the active development branch).
- Start PR titles with the repository's emoji convention, for example `📖 docs: ...`, `🐛 fix: ...`, or `✨ feature: ...`.
- Include `Fixes #<issue>` lines for issues the PR closes.
- Credit the issue's author with a `Co-authored-by:` trailer on the commit when the PR resolves somebody else's issue — see [Crediting issue authors](#crediting-issue-authors).
- Describe what changed, why, and how you tested it.
- Include the relevant command output or a short note such as `Not run (docs only)` when tests are not applicable.
- Add a changelog fragment under [`changelog.d/`](changelog.d/README.md) for user-visible changes — features, fixes, security changes, migrations, deprecations, and breaking changes (see "Changelog fragments" below). Routine refactors, test-only changes, and dependency churn are explicitly out of scope — add the `no-changelog` label if the advisory `changelog-fragment-guard` check asks anyway. Do **not** append to `CHANGELOG.md`'s `## Unreleased` section directly: every PR editing that one shared heading is what made unrelated PRs merge-conflict with each other ([#5675](https://github.com/hivecommons/hive/issues/5675)); fragments are compiled into [CHANGELOG.md](CHANGELOG.md) automatically at release time.
- Expect maintainers to ask for focused follow-up changes rather than broad drive-by edits.

## Changelog fragments

One file per PR, named `changelog.d/<category>-<pr-or-slug>.md` where the category (`added`, `changed`, `deprecated`, `fixed`, `security`) picks the CHANGELOG subsection — and, through it, the semver bump of the next release. The file's content is exactly your entry: a single `- ` bullet in the same narrative style as existing `CHANGELOG.md` entries, no headings. The compiler owns the `###` headings, so do not put headings in a fragment. The complete workflow:

```bash
echo '- The relay no longer drops long tasks ([#1234](https://github.com/hivecommons/hive/issues/1234)).' > changelog.d/fixed-1234-relay-drop.md
git add changelog.d/fixed-1234-relay-drop.md
git commit -s
```

`changelog.d/README.md` has the full format, the `no-changelog` exemption, and the release-marker escape hatch. The transition window that let in-flight PRs rely on direct `CHANGELOG.md` edits closed on 2026-09-09. For a user-visible code change, write a fragment instead: the `changelog-fragment-guard` check no longer accepts a direct `CHANGELOG.md` edit as a substitute, and reports `This PR edits CHANGELOG.md's Unreleased section directly; the transition window ended 2026-09-09` before asking for a fragment or `no-changelog` label. If a fragment starts with prose or a heading instead of a `- ` entry bullet, the guard fails with `a fragment must start with a '- ' entry bullet (or a '<!-- release: ... -->' marker) — it IS the entry; the compiler owns the ### headings`. For refactors, test-only changes, docs-only changes, dependency churn, or other changes that are not user-visible, use the `no-changelog` label when the guard asks instead of adding a fragment.

## Maintainer resources

Project governance lives in [GOVERNANCE.md](GOVERNANCE.md). The current owner/approver signal is also reflected in [OWNERS](OWNERS). Security disclosure is handled through [SECURITY.md](SECURITY.md), not public issues.
