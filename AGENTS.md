# Agent instructions for hivecommons/hive

These rules apply to every AI agent (hive agents, Copilot CLI, Claude Code, Codex, …) working in this repository. Human contributor guidance lives in [CONTRIBUTING.md](CONTRIBUTING.md); where the two differ for agents, this file wins.

## Testing: CI runs the tests, you do not

- **Do not run `go test`, `go build ./...`, `go vet`, `golangci-lint`, or the docs/citation guards locally.** Commit, push, open the PR, and read the CI results (`gh pr checks <n>`, then the failing job log). CI runs the full matrix with the right toolchain, caches and coverage gates; a local run duplicates it and burns tokens and pod CPU/disk. Inside the hive pod, `go test` and `go vet` are blocked for agents by the Go shim; use CI for the verdict.
- If CI fails, fix from the job log and push again. Do not "reproduce locally first". The hive opens requested PRs; CI is the sole verdict and records failures for agents to iterate on.
- The only local check worth running is `gofmt -l` on files you changed.
- `src/` is a two-module Go workspace (`src/go.work`): the root module and the Crux agent runtime at `src/crux/` (`github.com/hivecommons/hive/crux`, packages `crux`, `crux/agent`, `crux/inference`, …). From `src/`, `./...` is the root module only; the Crux module is `./crux/...`. CI covers both; see [src/docs/design/crux.md](src/docs/design/crux.md).
- **Never run this repository's test suite from inside an agent pane.** Parts of `crux/agent` and `pkg/dashboard` exercise real process management (tmux, `/proc` sweeps, SIGKILL by uid). Run as an agent user they can kill the agent's own tmux server, CLI and terminal — this is how the scanner lost its session repeatedly (hivecommons/hive#9416).

## Working style

- Keep changes surgical and PR-sized; one concern per PR.
- Every PR with a user-visible `src/` code change needs a `changelog.d/<category>-<slug>.md` fragment (`added|changed|deprecated|fixed|security`) whose body is a single `- ` bullet. Changes outside `src/` (root-level docs, `changelog.d/`, `.github/`), plus `src/docs/`, `*_test.go` files, and `src/{deploy,scripts}/test[-_]*.sh` scripts, are exempt automatically (`src/scripts/check-changelog-fragment.sh` is the source of truth for the exact set); other test-adjacent changes (e.g., `testdata/` fixtures, non-`_test.go` test helpers) and non-user-visible `src/` code changes (refactors, dependency bumps) need the `no-changelog` label instead. `changelog.d/` is at the repository root, not under `src/` — `src/changelog.d/` is never compiled and the guard rejects it.
- Sign commits (`git commit -s`); PR titles start with an emoji (`✨` feature, `🐛` fix, `📖` docs, `📝` proposal, `🌱` chore, `⚠️` breaking) — `📝` is for proposals only; a docs PR is `📖 docs: …`.
- Do not hand-edit `src/docs` file:line citations (including `src/docs/api-reference.md` and `src/docs/api-reference/*.md`); when CI's citation guard reports drift, run `python3 src/scripts/check-docs-citations.py --fix` first — it runs `check-api-reference-citations.sh --fix` for `api-reference.md` and re-anchors other `.go` citations by their backticked Go declaration anchor. Only fall back to a manual edit for citations it still reports as `DRIFT`/`UNRESOLVED`, and run it only when CI tells you citations drifted. The required `changelog-fragment-guard` check runs it (non-fix) when a PR edits a hub or dashboard `server.go` or their `static/` pages, so the PR that causes drift goes red.
