# GitHub Actions trigger

Hive v6 can be called from a GitHub Actions workflow through either the comment-relay transport or the hub OIDC dispatch transport shipped in `hivecommons/hive/.github/actions/hive@v6`. Both transports feed the same action guard path: role floor, ioscan input scanning, run dedupe, mode ladder, audit, and reply/kick handling stay shared with human mentions.

The separate `hive-action` repository described in the original proposal is out of scope; this repository-local composite action is the supported entry point.

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
    oidc:
      enabled: true
      audience: hive-prod
```

`identity_map` maps `github.actor` from the comment marker or the OIDC `actor` claim to an existing Hive dashboard identity. If omitted, the actor login itself must already have the required Hive role. Bot actors, including `github-actions[bot]`, are refused unless the repository is already governed by the hive and the command is in `allowed_commands`. OIDC dispatch is off by default; when `oidc.enabled` is true, `oidc.audience` is required and must match the workflow input exactly.

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


## Hub OIDC dispatch

Use `transport: oidc` when the workflow can reach the hub directly. The job must grant `id-token: write`; the action requests a GitHub Actions OIDC token for the configured audience and posts to `/api/contribute/actions/dispatch`.

```yaml
name: Ask Hive through hub OIDC

on:
  workflow_dispatch:
    inputs:
      issue:
        required: true
        type: number

permissions:
  id-token: write

jobs:
  hive-status:
    runs-on: ubuntu-latest
    steps:
      - uses: hivecommons/hive/.github/actions/hive@v6
        with:
          transport: oidc
          hub_url: https://hive.example.com
          audience: hive-prod
          command: status
          issue: ${{ inputs.issue }}
          prompt: Status check from GitHub Actions OIDC dispatch.

      - name: Read Hive receipt
        run: echo '${{ steps.hive.outputs.receipt }}' | jq .stage_receipt
```

The hub verifies the JWT issuer, signature, audience, expiry, not-before, and issued-at claims against GitHub's Actions JWKS. Refusals are audited without echoing prompt text. Reruns are deduped by repository, `run_id`, and `run_attempt` in the same store used by the comment transport. For `transport: oidc`, the composite action exposes `steps.<id>.outputs.receipt`, a JSON `stage_receipt` report that callers can archive or assert in workflow steps.
