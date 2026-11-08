# Agent instructions for hivecommons/hive

These rules apply to every AI agent (hive agents, Copilot CLI, Claude Code, Codex, …) working in this repository. Human contributor guidance lives in [CONTRIBUTING.md](CONTRIBUTING.md); where the two differ for agents, this file wins.

## Testing: CI runs the tests, you do not

- **Do not run `go test`, `go build ./...`, `go vet`, `golangci-lint`, or the docs/citation guards locally.** Commit, push, open the PR and read `gh pr checks <n>` plus the failing job log. If CI fails, fix from the log and push again; do not "reproduce locally first". CI is the sole verdict.
- The only local check worth running is `gofmt -l` on files you changed.
- **Never run this repository's test suite from inside an agent pane.** Parts of it manage real processes and can kill your own session.

## Working style

- Keep changes surgical and PR-sized; one concern per PR.
- Sign commits (`git commit -s`); PR titles start with an emoji (`✨` feature, `🐛` fix, `📖` docs or proposal, `🌱` chore, `⚠️` breaking); `📝` fails the PR Type gate.
- Every PR with a user-visible `src/` code change needs a `changelog.d/<category>-<slug>.md` fragment at the repository root whose body is a single `- ` bullet.
- Do not hand-edit `src/docs` file:line citations; when CI's citation guard reports drift, run `python3 src/scripts/check-docs-citations.py --fix`.

## Project knowledge is scoped, not global

This file is tone and working style only. Repository facts (module layout, CI guards, exact changelog exemptions, gotchas) each live in one entry under [src/docs/knowledge/](src/docs/knowledge/) with a `scope:` block naming the paths, labels and lanes it applies to. Hive injects matching entries into a kick; from a plain checkout, read the entries whose `scope.paths` match the files you are about to touch. Format: [src/docs/knowledge-entries.md](src/docs/knowledge-entries.md); injection: `src/docs/agent-knowledge-and-memory.md`.
