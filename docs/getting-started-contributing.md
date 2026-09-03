# Getting started as a first-time contributor

This is the end-to-end path for making your first code or documentation
contribution to Hive. It ties together the reference docs and answers the
Hive-specific questions those docs don't. If you just want the mechanics
(branches, DCO, PR format), see [`CONTRIBUTING.md`](../CONTRIBUTING.md); for
build/test commands, see [`docs/development.md`](development.md).

## 1. Find something to work on

- Browse the [issue tracker](https://github.com/hivecommons/hive/issues). Issues
  labeled `documentation` and `help wanted` are good entry points; many
  `[guide]` doc-gap issues are small and self-contained.
- Docs-only fixes (a wrong path, a dead link, a missing README) are the fastest
  way to land a first PR and learn the review flow.
- Comment on the issue before starting anything non-trivial so work isn't
  duplicated.

## 2. Set up a local environment

For most changes you do **not** need a cluster.

- **Go code:** you need Go (see [`docs/development.md`](development.md) for the
  version and `make`/`just` targets). `cd src && go build ./...` compiles the
  binary; that's enough to iterate on most code.
- **Docs:** no toolchain needed — edit Markdown and preview locally.
- **Running the whole thing:** the fastest full run is Docker Compose from the
  repo's [Quick Start](../README.md#quick-start-docker-compose) (`docker compose -f src/docker-compose.yaml up -d`). You only need Kubernetes for deployment-specific work.

## 3. Test a change without a cluster

- **Go:** `cd src && go test ./...` runs the unit suite. Most tests are
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

- Open your PR against the **`v4`** branch (the active development branch).
- CI runs build, tests, a coverage check, and container image builds. Some
  checks (Playwright, `tide`) are non-blocking. The **required** checks are the
  build/test/coverage/docker ones.
- Coverage occasionally flakes; a maintainer will re-run it. A red `PR Verifier`
  status is a known repo-wide quirk, not your change.
- A maintainer reviews and merges once CI is green. Timelines vary; ping the
  issue or PR thread if it's been quiet for a few days.

## Next steps

- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — branches, DCO sign-off, PR format.
- [`docs/development.md`](development.md) — build, test, lint, and `just` recipes.
- [`src/docs/README.md`](../src/docs/README.md) — the full documentation index.
- [ClankeR contributor relay](../src/docs/contributor-relay.md) — contribute
  compute to a running hive from your own machine.
