# CI runner labels, and why a fork must be able to run this repository's CI

Most of this repository's heavy CI runs on a self-hosted fleet labelled
`[self-hosted, hive]` (#5850–#5868). Those labels only exist in
`hivecommons`, and until #6083 they were written into `runs-on:` directly.

## What that cost a fork

A fork inherits the workflows and not the fleet, so 24 `runs-on:` declarations
across 12 workflows asked for a runner that repository does not have. The
result is not a failure — it is worse than one:

- **The jobs never start.** They sit in `queued` indefinitely. A red X is a bug
  report; a job that never runs reports nothing, and the pull request simply
  never reaches a conclusion.
- **They wedge the workflow.** A queued run holds its workflow's concurrency
  group, so the next push's run of the same workflow queues behind it.
  `cancel-in-progress` does not clear it, because the blocker was never *in
  progress*.
- **They cannot be cancelled.** There is no runner to deliver the cancellation
  to. Measured: `POST /actions/runs/<id>/cancel` returned `202 Accepted` and the
  run was still `queued` 22 minutes later. Short of deleting the run, the wedge
  is permanent.

Measured on a fork of `v4`: one pull request produced 26 workflow runs, **14 of
which were still `queued` an hour later**, having never started.

## How it works now

The label set is a repository (or organisation) **variable**, and every
expression falls back to a GitHub-hosted runner when it is unset:

```yaml
runs-on: ${{ fromJSON(vars.HIVE_RUNNER_LABELS || '["ubuntu-latest"]') }}
```

Jobs that additionally keep untrusted pull-request code off the fleet keep the
guard they already had, and only the label source changes:

```yaml
runs-on: ${{ (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)
             && fromJSON(vars.HIVE_RUNNER_LABELS || '["ubuntu-latest"]') || 'ubuntu-latest' }}
```

**The fallback goes inside `fromJSON`'s argument, deliberately.** The obvious
alternative —

```yaml
runs-on: ${{ vars.HIVE_RUNNER_LABELS && fromJSON(vars.HIVE_RUNNER_LABELS) || 'ubuntu-latest' }}
```

— only works if GitHub short-circuits `&&` before evaluating
`fromJSON('')`, which would otherwise be an error rather than a fallback. That
is true today and is not something to rest fleet-wide CI on. Written as above,
`fromJSON` is handed valid JSON whether or not the variable is set.

`src/deploy/test_runner_labels_portable.sh` enforces both halves: no `runs-on:`
may name the fleet directly, and no use of the variable may omit the inner
fallback.

## For `hivecommons` — one variable, set once

> **This must be set for the fleet to keep being used.** With
> `HIVE_RUNNER_LABELS` unset, every job that used to run on the fleet runs on
> `ubuntu-latest` instead. Nothing breaks silently — the jobs run, and run
> green or red on their merits — but the heavy lanes lose the fleet's
> resources until the variable exists.

```sh
# Organisation-wide (preferred: one setting covers every repo in the org)
gh variable set HIVE_RUNNER_LABELS --org hivecommons --body '["self-hosted","hive"]'

# Or per repository
gh variable set HIVE_RUNNER_LABELS --repo hivecommons/hive --body '["self-hosted","hive"]'

# Confirm
gh variable list --repo hivecommons/hive | grep HIVE_RUNNER_LABELS
```

The value is a JSON array of labels, exactly what `runs-on:` accepts.

## For a fork — nothing to do

Set nothing and CI runs on GitHub-hosted runners. If your fork *does* have its
own fleet, set `HIVE_RUNNER_LABELS` to its labels and the same workflows will
use it.

## Clearing runs already wedged

This change stops new wedges; it does not clear existing ones, because a run
queued for a label nothing carries cannot be cancelled. Delete the run:

```sh
gh api -X DELETE repos/<owner>/<repo>/actions/runs/<run_id>
```

Check for stuck runs first, so you delete only what is actually wedged:

```sh
gh run list --repo <owner>/<repo> --status queued --limit 50 \
  --json databaseId,workflowName,createdAt,status
```

## Still open

A job that can never be scheduled queues rather than failing, upstream
included — a **fleet outage looks exactly like a missing fleet**. #6083
suggests a short-timeout preflight job on a GitHub-hosted runner that asserts
the fleet is reachable, turning that into a legible red X. That is not done
here: it needs a decision about whether every workflow pays for such a job, and
guessing at the shape is worse than raising it.

