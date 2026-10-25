# Forges: GitLab, Gitea and Forgejo

## Audience

This page is for teams whose code lives on GitLab, Gitea or Forgejo and who want to know what Hive does with those forges today. It sits next to the [work source providers](work-source-providers.md) page: a **work source** (tracker) tells Hive what work exists; a **forge** is where the repositories, branches and change requests live. It is written from v5 code.

**Short answer:** GitHub.com and GitHub Enterprise are the only forges a hive can run against. The GitLab and Gitea/Forgejo adapters are built and tested, and two governor writes already go through them, but the governor cycle that would reach those writes still starts from GitHub. Setup and the full list of reasons are on [Forge setup: GitLab, Gitea, and Forgejo](../forge-app-setup.md); the work to finish it is tracked in [#14096](https://github.com/hivecommons/hive/issues/14096).

## Trackers and forges are separate settings

| Setting | Chooses | Values | Page |
| --- | --- | --- | --- |
| `governor.work_source.type` | Where the governor reads work items | `github` (default), `github_projects`, `linear`, `jira` | [Work sources](../work-sources.md) |
| `project.forge` | Which forge adapter the hive's repositories use | `github` (default), `gitlab`, `gitea` | This page |

`project.forge` is set once per hive, so every repository in `project.repos` is on the same forge. Gitea and Forgejo share one adapter (`gitea`) because they share one REST API. Do not confuse it with `github.forge`, which names a GitHub host for the GitHub App; see [the setup page](../forge-app-setup.md#do-not-confuse-projectforge-with-githubforge).

## Configuration

```yaml
project:
  forge: gitlab        # "github" (default) | "gitlab" | "gitea"

gitlab:
  gitlab_url: https://gitlab.example.com   # default https://gitlab.com
  token_env: GITLAB_TOKEN                  # env var NAME, never the token

gitea:
  gitea_url: https://codeberg.org          # no default; required for Gitea/Forgejo
  token_env: GITEA_TOKEN                   # env var NAME, never the token
```

| Key | Default | Notes |
| --- | --- | --- |
| `gitlab.gitlab_url` | `https://gitlab.com` | Instance root. The adapter appends `/api/v4`. |
| `gitlab.token_env` | `GITLAB_TOKEN` | Sent as the `PRIVATE-TOKEN` header. |
| `gitea.gitea_url` | none | Instance root. The adapter appends `/api/v1`. Without it the adapter cannot be built and the governor falls back to the GitHub client with a warning. |
| `gitea.token_env` | `GITEA_TOKEN` | Sent as `Authorization: token <TOKEN>`. |

The keys and defaults come from `GitLabConfig` and `GiteaConfig` in `src/pkg/config/config.go`. The adapter is built per governor cycle in `src/cmd/hive/forgewire.go`, so a config reload takes effect on the next tick. An unknown `project.forge` value also falls back to the GitHub client and logs a warning.

## Supported operations

The neutral interface is `forge.Forge` in `src/pkg/forge/forge.go`. Merging is a separate, optional `forge.Merger` interface.

| Operation | GitHub | GitLab | Gitea / Forgejo |
| --- | --- | --- | --- |
| `GetRepo`, `ListOpenIssues`, `ListOpenChangeRequests` | Yes | Yes | Yes |
| `CreateIssueComment`, `AddLabels`, `RemoveLabel` | Yes | Yes | Yes |
| `SetHold` (the `hold` label gate) | Yes | Yes | Yes |
| `Merge` (`forge.Merger`) | No | Yes: accepts the MR with `merge`, `squash` or `rebase` (rebase, then merge), an optional head-SHA guard and branch removal | No |
| `CreateIssue`, `FindIssueByMarker` | Yes | No | No |

The GitLab merge maps forge errors onto shared ones: an MR with the `hold` label returns `ErrHoldActive`; HTTP 405 returns `ErrNotMergeable`; 406 returns `ErrAlreadyMerged`; 409 returns `ErrConflict`. No production caller merges through `forge.Merger` yet. Routing the merge gate, stage engine and attribution trailer by forge kind is [#14113](https://github.com/hivecommons/hive/issues/14113).

## What runs through the adapter today

Two governor writes take their writer from `project.forge` instead of the GitHub client:

- the **escalation sweep**, which posts the evidence comment and the `needs-human` label on a stuck PR;
- the **hold guard**, which posts the drift comment and re-applies the `hold` label when a held PR's head moves.

Both are fed by the governor's enumeration, which is still `github.Client.EnumerateActionable`. On a GitLab or Gitea hive that enumeration finds nothing, so these writes never fire. Agents also reach their forge through the `gh` CLI wrapper, not through `pkg/forge`. [Why: the agent execution path is GitHub-only](../forge-app-setup.md#why-the-agent-execution-path-is-github-only) explains this.

## Dashboard behavior

- The **Platform** tile (in Diagnostics on v5, its own section on v6) shows the forge kind and, for a self-managed instance, its URL. See [Dashboard sections](../dashboard-sections.md#platform).
- Each **Projects** card names its forge and host in its header, for example `🦊 GitLab · gitlab.example.com`. The card links to that instance, using GitLab's `/-/issues` and `/-/merge_requests` paths or Gitea's `/issues` and `/pulls`. A GitHub card shows its GitHub host as before. The data is the `forge` and `forgeUrl` fields of each repo in `/api/status`. See [Dashboard sections](../dashboard-sections.md#projects).

## Testing

Each adapter has unit tests against a fake HTTP server (`src/pkg/forge/*_test.go`). `merge_contract_test.go` checks the merge contract. The dashboard fields are covered by `src/pkg/dashboard/repo_forge_14115_test.go`.

## Gaps

- Enumeration, the stage engine and the merge gate are GitHub-only ([#14113](https://github.com/hivecommons/hive/issues/14113), [#14096](https://github.com/hivecommons/hive/issues/14096)).
- Gitea/Forgejo has no merge yet. No adapter can open a change request.
- There is no GitLab or Gitea webhook receiver, dashboard login provider or egress-proxy rule table.

The complete list is in [What full support would require](../forge-app-setup.md#what-full-support-would-require).
