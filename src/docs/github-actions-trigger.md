# GitHub Actions trigger

Hive v6 can be called from a GitHub Actions workflow through the comment-relay transport shipped in `hivecommons/hive/.github/actions/hive@v6`. The action posts a normal `@hive` issue or pull-request comment, then the existing GitHub mention poller applies the same role floor, ioscan input scanning, dedupe, mode ladder, audit, and reply path as a human mention.

The separate `hive-action` repository described in the original proposal is out of scope for this phase; this repository-local composite action is the supported transport A entry point.

## Hive configuration

Enable the mention poller and the Actions transport on hives that should accept workflow-originated comments:

```yaml
github:
  mentions:
    enabled: true
  actions:
    enabled: true
    source_label: action
    allowed_commands: [status, review]
    allow_apply: false
    identity_map:
      octo-ci-bot: alice
```

`identity_map` maps `github.actor` from the workflow marker to an existing Hive dashboard identity. If omitted, the actor login itself must already have the required Hive role. Bot actors, including `github-actions[bot]`, are refused unless the repository is already governed by the hive and the command is in `allowed_commands`.

## Review on a PR

```yaml
name: Ask Hive to review

on:
  pull_request_target:
    types: [opened, synchronize, reopened]

permissions:
  issues: write
  pull-requests: write

jobs:
  hive-review:
    runs-on: ubuntu-latest
    steps:
      - uses: hivecommons/hive/.github/actions/hive@v6
        with:
          command: review
          issue: ${{ github.event.pull_request.number }}
          prompt: Please review this PR and reply with findings only.
```

Use `pull_request_target` carefully: it grants the base repository token. Do not check out or execute untrusted fork code in the same job unless you have separately sandboxed it.

## Nightly kick

```yaml
name: Nightly Hive status

on:
  schedule:
    - cron: '17 3 * * *'
  workflow_dispatch:
    inputs:
      issue:
        required: true
        type: number

permissions:
  issues: write

jobs:
  status:
    runs-on: ubuntu-latest
    steps:
      - uses: hivecommons/hive/.github/actions/hive@v6
        with:
          command: status
          issue: ${{ inputs.issue || 123 }}
          prompt: Nightly status check from GitHub Actions.
```

A rerun of the same workflow attempt is deduped by `run_id` and `run_attempt`, so it does not double-kick the hive.
