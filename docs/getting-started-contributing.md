# Getting started as a first-time contributor

This is the end-to-end path for making your first code or documentation
contribution to Hive. It ties together the reference docs and answers the
Hive-specific questions those docs don't. If you just want the mechanics
(branches, DCO, PR format), see [`CONTRIBUTING.md`](../CONTRIBUTING.md); for
build/test commands, see [`docs/development.md`](development.md).

## 1. Find something to work on

- Start with open issues labeled
  [`good first issue`](https://github.com/hivecommons/hive/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
  or [`help wanted`](https://github.com/hivecommons/hive/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22).
  Hive applies these labels to small, well-specified issues and leaves a
  welcome comment with a link to claim the issue through the
  [contributor relay](../src/docs/contributor-relay.md). The label is not a
  reservation: claim before you start, or hive may pick the issue up itself.
  See [Hive-curated good first issues](../src/docs/good-first-issue.md).
- Skip issues that are already taken: an assignee, a `claimed`,
  `hive/in-progress`, `hive/claimed-by-*` or `stage/040-covered-by-pr` label
  (an open PR already covers it), or a live `hive:claim` comment.
- Docs-only fixes (a wrong path, a dead link, a missing README) are the fastest
  way to land a first PR and learn the review flow.
- Comment on the issue before starting anything non-trivial so work isn't
  duplicated.
- Need a quick human answer? [Join our Discord](https://hivecommons.dev/discord).
- Curious what a running hive looks like? [Watch the hive live on Twitch](https://www.twitch.tv/hivecommons).

## 2. Set up a local environment

For most changes you do **not** need a cluster.

- **Go code:** you need Go (see [`docs/development.md`](development.md) for the
  version and `make`/`just` targets). `cd src && go build ./... ./crux/...`
  compiles both Go modules (the root module and the Crux agent runtime at
  `src/crux/`); that's enough to iterate on most code.
- **Docs:** no toolchain needed — edit Markdown and preview locally.
- **Running the whole thing:** the fastest full run is Docker Compose from the
  repo's [Quick Start](../src/docs/quickstart-docker-compose.md) (`docker compose -f src/docker-compose.yaml up -d`). You only need Kubernetes for deployment-specific work.

## 3. Test a change without a cluster

- **Go:** `cd src && go test ./... ./crux/...` runs the unit suite. Most tests are
  hermetic; a few need env like `kubectl` and are skipped otherwise. See the
  Test section of [`docs/development.md`](development.md).
- **Shell scripts:** `*.test.sh` / `*.test.js` files under
  [`bin/`](../bin/README.md) are runnable directly with `bash`/`node`.
- **The proxy:** `cd src/proxy && npm test`.
- Don't run local build/lint as a merge gate — CI is the gate (see step 6).

## 4. Key concepts before touching agent policy

If your change touches how agents behave, understand these first:

- **The deterministic pipeline vs. agents.** Shell scripts in
  [`bin/`](../bin/README.md) filter, classify, and gate work *before* any LLM
  runs; agents only make judgment calls. Keep deterministic logic in the
  pipeline.
- **ACMM levels.** The [ACMM policy matrix](../src/docs/acmm-policy-matrix.md)
  controls what each agent may do at each maturity level (advisory → auto-merge).
- **Agent configuration.** [`agent-configuration.md`](../src/docs/agent-configuration.md)
  is the field-by-field reference; prompt/policy templates live under the
  policies directory.
- **The architecture.** [`src/docs/architecture.md`](../src/docs/architecture.md)
  is the system overview — read it before changing the governor loop or guardrails.

## 5. How the Hive dev bot interacts with your PR

Hive maintains its own repository with a hive — so a bot may interact with your
contribution:

- The hive's agents may **comment on or triage** issues and open their own PRs.
- If you see automated review comments or a bot-authored PR referencing your
  issue, that's the hive working its own backlog. Coordinate in the issue thread;
  a human maintainer still owns merge decisions on community PRs.
- Bot-posted content is neutralized (no raw `@`-mentions) to avoid notifying
  everyone on each update — you don't need to do anything special.

## 6. Review and CI: what to expect

- Open your PR against the **`v5`** branch (the active development branch;
  `v4` is feature-frozen and takes security/critical fixes only).
- CI runs build, tests, a coverage check, lint and security scans, and
  container image builds. The **required** checks are `gate`,
  `build-and-test`, `test`, `coverage`, `golangci-lint`,
  `gosec (insecure patterns)`, `govulncheck (known vulnerabilities)`, `DCO`,
  `Sign-off survives the squash`, `changelog-fragment-guard` and
  `verify PR contents` (the PR Verifier). If one of them is red, read its
  log; it is reporting on your change.
- User-visible code changes need a changelog fragment under
  [`changelog.d/`](../changelog.d/README.md). Docs outside `src/`, `src/docs/`
  and test files are exempt.
- Coverage occasionally flakes; a maintainer will re-run it.
- A maintainer reviews and merges once CI is green. Timelines vary; ping the
  issue or PR thread if it's been quiet for a few days.

## Next steps

- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — branches, DCO sign-off, PR format.
- [`docs/development.md`](development.md) — build, test, lint, and `just` recipes.
- [`src/docs/documentation-map.md`](../src/docs/documentation-map.md) — the full documentation map.
- [ClankeR contributor relay](../src/docs/contributor-relay.md) — contribute
  compute to a running hive from your own machine.
