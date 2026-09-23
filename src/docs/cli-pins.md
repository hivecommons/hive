# CLI pins and the automated pin bump

Every agent CLI hive drives is baked into the images at a fixed version:

| CLI | Pin (`ARG`) | Source of truth | Verified by |
|---|---|---|---|
| Claude Code | `CLAUDE_CODE_VERSION` | npm `@anthropic-ai/claude-code` | npm (no download hash) |
| Codex | `CODEX_VERSION` | npm `@openai/codex` | npm |
| Copilot | `COPILOT_VERSION` | npm `@github/copilot` | npm |
| pi | `PI_VERSION` (hub), `PI_CODING_AGENT_VERSION` (contributor) | npm `@earendil-works/pi-coding-agent` | npm |
| Goose | `GOOSE_VERSION`, `GOOSE_SHA256_{AMD64,ARM64}` | GitHub releases `block/goose` | SHA-256 per arch |
| Antigravity (agy) | `AGY_VERSION` + `AGY_BUILD`, `AGY_SHA512_*` (hub); `AGY_VERSION=<ver>-<build>`, `AGY_SHA256_*` (contributor) | Google's updater artifact, as tracked by the Homebrew `antigravity-cli` cask | SHA-512 (hub) / SHA-256 (contributor) per arch |
| Oh My Pi (omp) | `OMP_VERSION`, `OMP_SHA256_*` | GitHub releases `can1357/oh-my-pi` | SHA-256 per arch, cross-checked with `SHA256SUMS.txt` |
| Muse Code | `MUSE_VERSION`, `MUSE_SHA256_*` | `api.meta.ai/muse-code/channels/muse-stable` and its release manifest | SHA-256 per arch, cross-checked with the manifest |
| bobshell (bob) | `BOBSHELL_VERSION`, `BOBSHELL_SHA256_*` | `bobshell2-version.txt` on IBM COS | SHA-256, cross-checked with the vendor `.tgz.sha256` |
| gh | `GH_VERSION`, `GH_SHA256_*` (hub only) | GitHub releases `cli/cli` | SHA-256 per arch, cross-checked with `gh_<v>_checksums.txt` |

The pins live in `src/Dockerfile` (hub and spoke image) and
`src/Dockerfile.contributor` (containerized contributors), and the two must
agree: `bin/test_backend_smoke.sh` section A2/A3 fails when the claude or
codex pins differ. Pinning is deliberate ([#2903](https://github.com/hivecommons/hive/issues/2903)):
a floating `@latest` makes builds non-reproducible, and the per-arch digest
makes a tampered download fail the build instead of running.

## Why a bump workflow

Nothing else moves these pins. `.github/dependabot.yml` covers Go modules,
GitHub Actions, `FROM` tags and the proxy's npm tree, none of which reach an
`ARG *_VERSION` line. Left alone the pins drift until a user hits a wall,
which is what [#8417](https://github.com/hivecommons/hive/issues/8417) was:
Claude Code 2.1.226 refusing `claude-opus-5-5` six weeks after 2.1.280 shipped.

Letting the CLIs update themselves at runtime is the wrong fix: it undoes the
reproducible, hash-checked build, the update is lost on every container
restart (the CLIs live in the image layer), agents on one hive end up on
different versions, and a TUI change reaches production with no test in
between while hive reads those TUIs (pane scraping, onboarding dialogs,
working-state text). The contributor path already writes
`autoUpdates: false` into `~/.claude.json` for these reasons.

## What `.github/workflows/cli-pin-bump.yml` does

Once a day (`23 5 * * *`) and on `workflow_dispatch`, one job per CLI:

1. **Resolve.** `src/scripts/cli-pin-bump.sh bump <cli>` asks the CLI's
   source of truth for the latest release and, for download-verified CLIs,
   downloads every per-arch artifact and recomputes its digest. Where the
   vendor publishes a digest (omp, gh, Muse, bobshell, the agy cask) the
   computed value must match it or the job fails. A digest is never copied
   from a page and never guessed: the script refuses to write anything that is
   not a 64- or 128-hex-character value, and it validates every value before
   the first edit so a Dockerfile is never left half-updated.
2. **Edit.** Both Dockerfiles are rewritten in place (the `ARG` lines only),
   plus `src/pkg/config/dockerfile_contributor_test.go`, which asserts pi's
   exact `ARG` line.
3. **Smoke before the PR exists.** The job builds `src/Dockerfile` for
   linux/amd64 with the new pin and runs `<cli> --version` inside the image
   (for claude also an unauthenticated `claude --help`). A CLI whose install
   layer or startup path broke never becomes a PR.
4. **Open one PR per CLI** on branch `cli-pin-bump/<cli>` against the base
   branch (`v5` by default; the dispatch input `base` picks another). The PR
   body carries the resolved `KEY=VALUE` provenance and the smoke transcript.
   An open PR for the same CLI is refreshed rather than duplicated, and if it
   already carries the same target version the job does nothing.

Labels on a bump PR:

- `security` - admits the PR through the v4 feature-freeze gate
  (`.github/workflows/v4-freeze-gate.yml`), so a CLI fix can still land on a
  frozen line.
- `dependencies`, `no-changelog` - the dependabot convention; a pin bump is
  dependency churn and needs no changelog fragment.
- `needs-human` - added only for a **major** version change. Patch and minor
  bumps follow the repo's normal merge-on-green path; a major bump waits for
  a person to read the release notes.

The PR then runs the normal gates. The docker workflow builds both images for
linux/amd64 and linux/arm64, where every hash-verified layer runs
`sha256sum -c` / `sha512sum -c`; the [backend smoke](backend-smoke.md)'s
pinned lane picks the new CLI up on its next scheduled run.

Credentials: the workflow opens PRs with `GITHUB_TOKEN` under
`pull-requests: write`, the same path `v5-topup.yml` uses, and pushes with
`TOPUP_PUSH_TOKEN` when that optional PAT is set (the bump never touches
`.github/workflows/`, so `GITHUB_TOKEN` can push it too). No new secret.

## Running it by hand

```sh
# what is pinned right now
src/scripts/cli-pin-bump.sh current codex

# resolve the latest release and its digests without editing anything
src/scripts/cli-pin-bump.sh resolve goose

# resolve + edit both Dockerfiles; prints OLD=/NEW=/MAJOR=/CHANGED=
src/scripts/cli-pin-bump.sh bump claude
```

`resolve` and `bump` need `curl`, `jq` and `sha256sum`/`shasum`; set
`GH_TOKEN` to avoid the anonymous GitHub API rate limit. `apply <cli> <kv>`
edits from a saved `KEY=VALUE` file with no network at all, which is what the
self-test (`src/scripts/test-cli-pin-bump.sh`, run by the workflow's
`selftest` job on any PR that touches the script or workflow) exercises.

To bump one CLI on demand from the Actions tab, dispatch **CLI pin bump**
with `cli=<name>`; `all` checks every pin.

## Adding a CLI

1. Add the `ARG` lines to both Dockerfiles following an existing
   download-verified layer (version plus per-arch digest, `sha256sum -c`
   before install, never a `curl | sh` installer).
2. Add a `resolve_<cli>` function and the `apply_cli` / `hub_version_arg`
   cases to `src/scripts/cli-pin-bump.sh`, and a fixture line plus assertions
   to `src/scripts/test-cli-pin-bump.sh`.
3. Add the name to the workflow's matrix and dispatch `options`, and to the
   `CLIS` list in the script.

## Not covered (yet)

- The workflow does not record each CLI's model list before and after a bump
  (issue item 3). The dashboard's live probes (`codex app-server` model/list,
  `agy models`, `omp models --json`) remain the source of truth on a running
  hive; a static-catalog refresh, when one is needed, is a hand edit to
  `codexStaticModels` in `src/pkg/dashboard/cli_models.go` and its
  `static/index.html` mirror.
- No fast lane from a "this model needs version X" runtime error
  ([#8418](https://github.com/hivecommons/hive/issues/8418)) to a dispatch of
  this workflow. Dispatching by hand with `cli=<name>` is the interim path.
- `KILO_CLI_VERSION` and `COPILOT_SDK_VERSION` are pinned but not bumped by
  this workflow.
