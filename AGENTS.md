# Agent instructions for hivecommons/hive

These rules apply to every AI agent (hive agents, Copilot CLI, Claude Code, Codex, …) working in this repository. Human contributor guidance lives in [CONTRIBUTING.md](CONTRIBUTING.md); where the two differ for agents, this file wins.

## Testing: CI runs the tests, you do not

- **Do not run `go test`, `go build ./...`, `go vet`, `golangci-lint`, or the docs/citation guards locally.** Commit, push, open the PR, and read the CI results (`gh pr checks <n>`, then the failing job log). CI runs the full matrix with the right toolchain, caches and coverage gates; a local run duplicates it and burns tokens and pod CPU/disk.
- If CI fails, fix from the job log and push again. Do not "reproduce locally first". The hive opens requested PRs; CI is the sole verdict and records failures for agents to iterate on.
- The only local check worth running is `gofmt -l` on files you changed.
- **Never run this repository's test suite from inside an agent pane.** Parts of `pkg/agent` and `pkg/dashboard` exercise real process management (tmux, `/proc` sweeps, SIGKILL by uid). Run as an agent user they can kill the agent's own tmux server, CLI and terminal — this is how the scanner lost its session repeatedly (hivecommons/hive#9416).

## Working style

- Keep changes surgical and PR-sized; one concern per PR.
- Every PR needs a `changelog.d/<category>-<slug>.md` fragment (`added|changed|deprecated|fixed|security`) whose body is a single `- ` bullet.
- Sign commits (`git commit -s`); PR titles start with an emoji (`✨` feature, `🐛` fix, `📖` docs, `🌱` chore, `⚠️` breaking).
- Do not edit `src/docs/api-reference.md` line citations by hand; CI's citation guard reports drift and the fix is `bash src/scripts/check-api-reference-citations.sh --fix` — run it only when CI tells you it drifted.
