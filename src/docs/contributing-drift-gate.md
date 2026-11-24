# Contributing drift gate

A reusable workflow, `.github/workflows/contributing-drift.yml`, that fails when a command a repository's CI runs is not mentioned in that repository's `CONTRIBUTING.md` or `README.md` ([#19487](https://github.com/hivecommons/hive/issues/19487)).

## Why

Guide audits found the same drift in six sibling repos (hotshot, pluk, promptargs, docs, rationguard, benchmarks). In each one, CI ran a test, lint or guard command that the contributor docs never mentioned, so a contributor could pass every documented local step and still fail CI. Fixing each repo by hand did not stop the next case. This gate catches it in CI.

## What it checks

The check is deterministic and offline. It uses stdlib `python3` only, with no network and no token.

1. It reads every `.github/workflows/*.yml` and `*.yaml` file in the calling repo.
2. It takes each `run:` step. Steps with `continue-on-error: true` are skipped. `uses:` steps such as checkout or artifact upload have no `run:`, so the check never reads them.
3. From each step it picks out the script and test commands:
   - a repo script path: `scripts/check-no-temp-artifacts.sh`, `python3 tools/x.py`, `./ci.sh`
   - a module run: `python3 -m unittest`, `python -m pytest`
   - a task-runner target: `make test`, `just lint`, `npm test`, `npm run lint`
   - a known test or lint tool: `go test`, `cargo clippy`, `pytest`, `ruff`, `eslint`, `vitest`, and so on

   Setup and plumbing commands are never candidates. These include package installs (`npm ci`, `pip install`, `apt-get`), `mkdir`, `cp`, `git`, `curl`, `echo`, `command -v`, `bash -n`, absolute or `$VAR` paths, and heredoc bodies.
4. It fails, with one annotation per command, when a command is mentioned in none of `CONTRIBUTING.md`, `.github/CONTRIBUTING.md`, `docs/CONTRIBUTING.md` and `README.md`. A script must appear by its path (a leading `./` is optional). A module or runner target matches with any interpreter or package manager: `python -m unittest` matches `python3 -m unittest discover ...`, and `yarn test` matches `npm test`.

## Exemptions

A repo can list more commands to skip in a checked-in file. The default is `.github/contributing-drift-exemptions.txt`; the `exemptions` input changes the path. Put one entry per line, and give each entry a reason:

```text
scripts/publish.sh            # release-only, run by the release workflow
workflow:nightly-bench.yml    # scheduled benchmark job, not a PR check
```

A plain entry exempts every finding whose command line contains its text. A `workflow:<file>` entry exempts every finding in that workflow file. An entry without a `# reason` fails the check. If the file is missing, nothing is exempt.

## Adopting it

The caller pins the workflow by commit SHA, never by branch:

```yaml
jobs:
  contributing-drift:
    uses: hivecommons/hive/.github/workflows/contributing-drift.yml@<sha> # v5
```

The checker is written inline in the workflow file. It does not check out this repo, so the pinned SHA pins the checker too.

Roll it out as a non-required job first. Make it required in a repo only once it is green there. Caller workflow edits need a human or merge-lane push, so each sibling repo has its own rollout issue.

## Testing

`.github/scripts/test-contributing-drift.py` pulls the checker out of the workflow (between its `BEGIN`/`END` markers) and runs it against fixture repos. One fixture is the benchmarks `pages.yml` case that motivated the gate. `contributing-drift-test.yml` runs that test on PRs that touch the gate.
