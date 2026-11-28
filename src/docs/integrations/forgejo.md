# Forgejo

## Audience

This page is for teams whose repositories live on [Forgejo](https://forgejo.org/), on their own instance or on [Codeberg](https://codeberg.org/), and who want to know what Hive does with Forgejo today. It is written from v5 code. The full forge reference, which covers GitLab and Gitea too, is [Forges: GitLab, Gitea and Forgejo](forges.md).

## Status: partial, in progress

**A Forgejo-only hive finds work, clones, pushes and merges, with gaps.** Hive has a first-class `forgejo` forge kind, and a hive with every repository on Forgejo boots without GitHub credentials. The governor discovers issues and pull requests through the forge adapter ([#19673](https://github.com/hivecommons/hive/pull/19673)), sandbox Job kicks and dashboard Sessions clone from and push to the Forgejo host ([#19678](https://github.com/hivecommons/hive/pull/19678)), a pod-run agent (`sandbox.runtime` off) gets no credential for the Forgejo host and clones a private repository through the hive's clone relay, `hive-clone`, which needs `forgejo.integration` ([#19997](https://github.com/hivecommons/hive/issues/19997)), `hive-gh pr view`, `issue view` and `pr checks` on a Forgejo repository are answered by the hive's read relay with the same compact JSON key names as on GitHub (without `mergeStateStatus`, `reviewDecision`, the diff stats, `stateReason` or `milestone`), and the Forgejo-only kick tells agents to use them ([#20230](https://github.com/hivecommons/hive/issues/20230)), the same relay answers `hive-gh pr reviews` (reviewer, state, commit and inline comments), `pr comments` / `issue comments`, and `run view <run-id> --log` / `--log-failed` / `run view --job <job-id> --log` with the last 256 KiB of each job's log, at most 512 KiB a run; reading a job log needs a Forgejo token that may read the repository's Actions ([#20277](https://github.com/hivecommons/hive/issues/20277)), agent issue and pull request requests go through the forge ([#19681](https://github.com/hivecommons/hive/pull/19681)), and the automerge and close-on-merge sweeps run on Forgejo repositories ([#19676](https://github.com/hivecommons/hive/pull/19676)). The gaps a live run against Forgejo 17 found are tracked in [#19909](https://github.com/hivecommons/hive/issues/19909). This page is updated as each piece merges.

## What Forgejo is

Forgejo is a self-hosted software forge: Git hosting, issues, pull requests, packages and CI in one service. It is community-run, started as a fork of Gitea, and is developed under the umbrella of Codeberg e.V., the non-profit that runs codeberg.org. Forgejo serves a Gitea-compatible REST API under `/api/v1`.

## What works on v5 today

| Area | State on v5 |
| --- | --- |
| Forge kind | `project.forge: forgejo` and `forge: forgejo` on a `project.repo_policies[]` entry are valid, with a top-level `forgejo:` block. Forgejo is its own kind, separate from `gitea`, so a call that differs between the two can change for one without the other ([#19496](https://github.com/hivecommons/hive/pull/19496)). |
| Config load without GitHub | When `project.forge` is `forgejo` and no `repo_policies` entry puts a repository on GitHub, validation does not ask for `github.token`, `github.app_id` or `github.forge`. It requires `forgejo.forgejo_url` (or a `forge_url` on each repo's policy) instead ([#19497](https://github.com/hivecommons/hive/pull/19497)). |
| Token handling | `FORGEJO_TOKEN`, and the variable `forgejo.token_env` names, is stripped from agent panes, user sessions and tool shells ([#19498](https://github.com/hivecommons/hive/pull/19498)). The token is only sent to the host of the `forgejo:` block's URL. A `repo_policies[].forge_url` on another host fails config load ([#19572](https://github.com/hivecommons/hive/pull/19572)). |
| Adapter calls | The Forgejo adapter in `src/pkg/forge/forgejo_forge.go` reads a repository, lists open issues and pull requests, comments, adds and removes labels, sets the `hold` label, and merges a pull request with a hold gate and an optional head-SHA guard. Unit tests run against a fake server. The nightly [Forgejo smoke workflow](https://github.com/hivecommons/hive/blob/v5/.github/workflows/forgejo-smoke.yml) runs read, comment, label and merge against a real Forgejo container. |
| Dashboard and tools | A Projects card on a Forgejo repository names Forgejo and its host and links to the instance's `/issues` and `/pulls` pages. The Settings dialog has Forgejo URL and token variable fields. `hivectl` presets, the hub forge switch and the proxy deny messages accept `forgejo`. |

[What runs through the adapter today](forges.md#what-runs-through-the-adapter-today) has the details of each call.

## In progress

Each links to the issue that tracks it. [#19909](https://github.com/hivecommons/hive/issues/19909) lists them all.

- Short-lived, per-tier Forgejo credentials cover git clone and push ([#19682](https://github.com/hivecommons/hive/pull/19682), [#19760](https://github.com/hivecommons/hive/pull/19760)). REST calls use the `token_env` access token unless `forgejo.integration.rest_audience` is set, in which case they send a short-lived JWT too ([#19920](https://github.com/hivecommons/hive/issues/19920)). The spoke can serve the integration's key list on its own dashboard origin with `forgejo.integration.serve_key_list`, off by default, and `previous_key_files` keeps an old key listed during a rotation ([#19996](https://github.com/hivecommons/hive/issues/19996)). See [Forge setup](../forge-app-setup.md#forgejo-git-credentials-one-short-lived-token-per-tier).
- A forge-only hive serves push-branch requests. It answers review and merge requests with a reason instead of serving them ([#19916](https://github.com/hivecommons/hive/issues/19916)).
- When the hive's Forgejo user has write but not admin access, Forgejo refuses the permission API for other users. The automerge sweep then reads the repo's assignee list, and a labeller on that list may release a merge. The list also holds members of a team with write on any one unit, or with read on pull requests. The close-on-merge sweep does not accept the assignee list: it honours closing keywords only from the hive's own user or an author whose permission it can read as write ([#19914](https://github.com/hivecommons/hive/issues/19914)).

The hub's get-started flow provisions hosted hives on GitHub and GitHub Enterprise only. A Forgejo repository URL is refused there for now.

## Configure a Forgejo repository in hive.yaml

These keys parse and validate on v5. Setting them gives you a hive that loads your Forgejo repositories, finds their work and lets agents clone and push, within the gaps listed under [In progress](#in-progress).

```yaml
project:
  org: my-org                 # the Forgejo owner (user or organization)
  repos:
    - my-repo
  forge: forgejo              # "github" (default) | "gitlab" | "gitea" | "forgejo"

forgejo:
  forgejo_url: https://codeberg.org   # bare instance root; no default
  token_env: FORGEJO_TOKEN            # env var NAME, never the token
  # login: hive-bot                   # optional: hive's own username; lets the close-on-merge sweep skip GET /user when the token is repo-limited

agents:                       # required: the loader refuses a config with no agents
  guide:
    enabled: true
    backend: claude           # claude | copilot | gemini | ...
    model: claude-sonnet-4-6
    paused: true              # starts paused; resume it from the dashboard
  scanner:
    enabled: true
    backend: claude
    model: claude-sonnet-4-6
    paused: true
```

**The `agents` block is required.** A config with no agent fails to load with `at least one agent must be configured` (`src/pkg/config/validate.go`), and the pod crash-loops. The hub's provisioning template seeds `guide` and `scanner` for a hosted spoke, but a hand-written seed has no default roster. The two agents above start paused, so the hive boots without kicking work until you resume them. See [Agent configuration](../agent-configuration.md) for the other agent keys.

Put the Forgejo access token in the environment variable that `token_env` names. The token never goes in `hive.yaml`.

To keep most repositories on GitHub and put one on Forgejo, use a repository policy. Its `forge_url` must be on the same host and port as `forgejo.forgejo_url`, because the block's token is only sent to that host:

```yaml
project:
  repo_policies:
    - repo: mirror
      forge: forgejo
      forge_url: https://codeberg.org
```

| Key | Default | Notes |
| --- | --- | --- |
| `project.forge` | `github` | `forgejo` selects the Forgejo adapter for every repository in `project.repos`. |
| `forgejo.forgejo_url` | none | Instance root, for example `https://codeberg.org` or `https://forgejo.example.com`. The adapter appends `/api/v1` itself, so do not add it. |
| `agents` | none | Required. At least one agent must be configured, or config load fails. |
| `forgejo.token_env` | `FORGEJO_TOKEN` | Name of the environment variable that holds the token. Sent as `Authorization: token <TOKEN>`. |
| `forgejo.login` | none | The hive's own Forgejo username. Optional: set it when the token is limited to specific repositories, because such a token cannot read the token's own user through `GET /user`; the close-on-merge sweep uses this login instead ([#19994](https://github.com/hivecommons/hive/issues/19994)). It must be the token's own user: the sweep uses it only when `GET /user` answers 403 and the login holds write on the repository, and a token that can read its user refuses a different login. For a token whose user is not a repository or site admin, the write check fails for any other user; for an admin token it only proves the login is a writer, and the sweep treats that writer's issues as the hive's own. |
| `project.repo_policies[].forge` | `project.forge` | Moves one repository to `forgejo`. |
| `project.repo_policies[].forge_url` | `forgejo.forgejo_url` | Bare instance root for that repository. Must match the block's host and port. |

The keys come from `ForgejoConfig` in `src/pkg/config/config.go` and the repository policy fields in `src/pkg/config/repo_policy.go`. Do not confuse `project.forge` with `github.forge`, which names a GitHub host for the GitHub App. See [Forge setup](../forge-app-setup.md#do-not-confuse-projectforge-with-githubforge).

## Credits

The Forgejo work on v5 (the `forgejo` forge kind, GitHub-free config load, forge token stripping and the token host check) was contributed by `sdwilsh`, who also wrote the plan in [#19493](https://github.com/hivecommons/hive/issues/19493) and [#19494](https://github.com/hivecommons/hive/issues/19494).

The Forgejo logo was created by [Caesar Schinas](https://caesarschinas.com/) and is licensed under [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/). It is used unmodified from the [Forgejo source tree](https://codeberg.org/forgejo/forgejo/src/commit/31bfb80c03287ab527eca7a72a2bbb029e2a43ab/assets/logo.svg); the license is stated on the [Forgejo branding page](https://codeberg.org/forgejo/meta/src/commit/9810d8469ab58d4604ed222eb2a47f90d2bc03fc/branding/README.md#logo). Logos are trademarks of their owners and are used for identification only. See [Logo sources and licenses](https://github.com/hivecommons/hive/blob/v5/src/docs/assets/logos/SOURCES.md).

## See also

- [Forges: GitLab, Gitea and Forgejo](forges.md): the operations each adapter supports and the remaining gaps.
- [Forge setup: GitLab, Gitea, and Forgejo](../forge-app-setup.md): why the agent execution path is GitHub-only today, and what full support requires.
- [Integrations](../integrations.md).
