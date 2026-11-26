# Forges: GitLab, Gitea and Forgejo

## Audience

This page is for teams whose code lives on GitLab, Gitea or Forgejo and who want to know what Hive does with those forges today. It sits next to the [work source providers](work-source-providers.md) page: a **work source** (tracker) tells Hive what work exists; a **forge** is where the repositories, branches and change requests live. It is written from v5 code.

**Short answer:** GitHub.com and GitHub Enterprise are the only forges a hive can fully run against. A hive whose repositories are all on GitLab, Gitea or Forgejo now boots without GitHub credentials and finds its work: the governor lists each repository's open issues and change requests through the forge adapter (see [Work discovery on a forge-only hive](#work-discovery-on-a-forge-only-hive)). The rest of the loop is not finished yet, as the [Gaps](#gaps) list says. Setup and the full list of reasons are on [Forge setup: GitLab, Gitea, and Forgejo](../forge-app-setup.md); the remaining work is tracked in [#19494](https://github.com/hivecommons/hive/issues/19494).

## Trackers and forges are separate settings

| Setting | Chooses | Values | Page |
| --- | --- | --- | --- |
| `governor.work_source.type` | Where the governor reads work items | `github` (default), `github_projects`, `linear`, `jira` | [Work sources](../work-sources.md) |
| `project.forge` | Which forge adapter the hive's repositories use | `github` (default), `gitlab`, `gitea`, `forgejo` | This page |
| `project.repo_policies[].forge`, `forge_url` | A different forge for one repository | same values; `forge_url` is a bare instance root | This page |

`project.forge` sets the forge for every repository in `project.repos`. A `project.repo_policies[]` entry can move one repository to another forge with `forge`, and to a different URL with `forge_url`, which defaults to `gitlab.gitlab_url` / `gitea.gitea_url` / `forgejo.forgejo_url`. A `forge_url` must be on the same host and port as its forge block's URL, because the block's token is only sent to that host. A mismatch fails config load. For `gitlab` the block URL defaults to `https://gitlab.com`. A `gitea` or `forgejo` block with no URL of its own is not checked. Unlike `project.forge`, the per-repo keys are validated at load. See [the setup page](../forge-app-setup.md#the-configuration-surface-that-exists). `gitea` and `forgejo` are separate kinds with separate config blocks. They share one REST client today because the two APIs match for the calls Hive makes, and a call that differs can change in one kind without touching the other, as [#19493](https://github.com/hivecommons/hive/issues/19493) describes. The `forgejo` kind adds naming only. It is no further along than `gitea`. Do not confuse it with `github.forge`, which names a GitHub host for the GitHub App; see [the setup page](../forge-app-setup.md#do-not-confuse-projectforge-with-githubforge).

## Configuration

```yaml
project:
  forge: gitlab        # "github" (default) | "gitlab" | "gitea" | "forgejo"

gitlab:
  gitlab_url: https://gitlab.example.com   # default https://gitlab.com
  token_env: GITLAB_TOKEN                  # env var NAME, never the token

gitea:
  gitea_url: https://gitea.example.com        # no default; required for Gitea
  token_env: GITEA_TOKEN                   # env var NAME, never the token

forgejo:
  forgejo_url: https://codeberg.org        # no default; required for Forgejo
  token_env: FORGEJO_TOKEN                 # env var NAME, never the token
```

One repository on a different forge from the rest:

```yaml
project:
  repo_policies:
    - repo: mirror
      forge: gitea
      forge_url: https://codeberg.org      # bare instance root
```

| Key | Default | Notes |
| --- | --- | --- |
| `gitlab.gitlab_url` | `https://gitlab.com` | Instance root. The adapter appends `/api/v4`. |
| `gitlab.token_env` | `GITLAB_TOKEN` | Sent as the `PRIVATE-TOKEN` header. |
| `gitlab.token_file` | none | A file holding the token. See below. |
| `gitea.gitea_url` | none | Instance root. The adapter appends `/api/v1`. Without it the adapter cannot be built and the governor falls back to the GitHub client with a warning. |
| `gitea.token_env` | `GITEA_TOKEN` | Sent as `Authorization: token <TOKEN>`. |
| `gitea.token_file` | none | A file holding the token. See below. |
| `forgejo.forgejo_url` | none | Instance root. The adapter appends `/api/v1`. Same fallback as Gitea when unset. |
| `forgejo.token_env` | `FORGEJO_TOKEN` | Sent as `Authorization: token <TOKEN>`. |
| `forgejo.token_file` | none | A file holding the token. See below. |
| `forgejo.login` | none | The hive's own Forgejo username. Optional: lets the close-on-merge sweep use it when the `GET /user` lookup fails with 403, as it does for a repository-limited token ([#19994](https://github.com/hivecommons/hive/issues/19994)). It must be the token's own user, and it is trusted only with write on the repository. For a non-admin token that check fails for any other user; for an admin token it does not, so a wrong login there is not caught. |

Each block reads its token the same way. When `token_file` is set, hive reads the token from that file and ignores the env var. The path must be absolute and under `/secrets` or `/data/secrets`, like other secret files in hive. The file is read each time the token is needed, so a rotated file takes effect on the next tick. A path outside those directories, an unreadable file, or an empty file is an error, and no request is sent. This includes the token git clone and push use on a Forgejo repo when `forgejo.integration` is unset. With no `token_file`, hive reads the env var as before.

The keys and defaults come from `GitLabConfig`, `GiteaConfig` and `ForgejoConfig` in `src/pkg/config/config.go`. The adapter is built per governor cycle in `src/cmd/hive/forgewire.go`, so a config reload takes effect on the next tick. An unknown `project.forge` value also falls back to the GitHub client and logs a warning.

## Supported operations

The neutral interface is `forge.Forge` in `src/pkg/forge/forge.go`. Merging is a separate, optional `forge.Merger` interface.

| Operation | GitHub | GitLab | Gitea / Forgejo |
| --- | --- | --- | --- |
| `GetRepo`, `ListOpenIssues`, `ListOpenChangeRequests` | Yes | Yes | Yes |
| `CreateIssueComment`, `AddLabels`, `RemoveLabel` | Yes | Yes | Yes |
| `SetHold` (the `hold` label gate) | Yes | Yes | Yes |
| `Merge` (`forge.Merger`) | No | Yes: accepts the MR with `merge`, `squash` or `rebase` (rebase, then merge), an optional head-SHA guard and branch removal | Yes: with a hold gate and an optional head-SHA guard, run against a real Forgejo by the nightly smoke workflow |
| `CreateIssue`, `FindIssueByMarker` (`forge.IssueSeam`) | Yes | No | Yes: the issue is created, then its labels are added by name |
| `CreateChangeRequest` (`forge.ChangeRequestCreator`) | No: GitHub opens pull requests through the GitHub client | No | Yes: reuses an open pull request from the same head, and resolves an empty base to the default branch |

The GitLab merge maps forge errors onto shared ones: an MR with the `hold` label returns `ErrHoldActive`; HTTP 405 returns `ErrNotMergeable`; 406 returns `ErrAlreadyMerged`; 409 returns `ErrConflict`. No production caller merges through `forge.Merger` yet. Routing the merge gate, stage engine and attribution trailer by forge kind is [#14113](https://github.com/hivecommons/hive/issues/14113).

## What runs through the adapter today

Two governor writes take their writer from `project.forge` instead of the GitHub client:

- the **escalation sweep**, which posts the evidence comment and the `needs-human` label on a stuck PR;
- the **hold guard**, which posts the drift comment and re-applies the `hold` label when a held PR's head moves.

Both are fed by the governor's enumeration. On a forge-only hive that enumeration runs through the forge adapter, so these writes reach the forge.

## Work discovery on a forge-only hive

When `project.forge` is `gitlab`, `gitea` or `forgejo` and no `project.repo_policies[]` entry puts a repository on GitHub, the governor cycle enumerates through `pkg/forge` instead of `github.Client.EnumerateActionable` ([#19669](https://github.com/hivecommons/hive/issues/19669)). The code is `src/cmd/hive/forge_enumerate.go`.

- Each repository in `project.repos` that is not paused is listed with `ListOpenIssues` and `ListOpenChangeRequests`.
- The items fill the same actionable snapshot a GitHub hive builds, so the queue, kicks and dashboard read them the same way.
- Held items (the `hold` labels, the hive's pause label and the sentinel label) go to the hold list. Exempt labels, the hard-suppress labels and `project.issue_filter` are applied as on GitHub. Draft change requests are not offered.
- Reporter trust fails closed. The forge list payload has no author association, so only `reporter_trust.trusted_logins` can trust an author. Any other author's issue needs one of the `untrusted_require_labels`.
- Every issue author is treated as a person, so the injection scan and clarity gate judge the text.
- A repository whose listing fails is marked stale. If every repository fails, the cycle keeps its previous snapshot.
- The cycle runs with no GitHub client, even when one is configured. CI status, mergeability, review signals and dependency links are GitHub enrichments, so they stay unknown.

A hive with every repository on GitHub keeps the GitHub enumeration unchanged.

A mixed hive has repositories on GitHub and at least one on GitLab, Gitea or Forgejo ([#19703](https://github.com/hivecommons/hive/issues/19703)). It enumerates through the GitHub client first. The GitHub result's share for each non-GitHub repository is then dropped: its items, counts, patches and stale mark. Each such repository that is not paused is then listed through its forge adapter with the gates above, and the result is merged in.

- If a forge listing fails, that repository is marked stale with a zero count, and the GitHub share of the pass is kept.
- If the GitHub enumeration itself fails, the cycle keeps its previous snapshot, as on a GitHub hive.
- CI status and workflow approval enrichment run only on PRs that live on GitHub. A change request on another forge keeps unknown CI and mergeability.

Agent PR and issue requests for a non-GitHub repository in a mixed hive still go to the GitHub relays, and agents still reach their forge through the `gh` CLI wrapper, not through `pkg/forge`. [Why: the agent execution path is GitHub-only](../forge-app-setup.md#why-the-agent-execution-path-is-github-only) explains this.

## Agent requests on a forge-only hive

Agents do not write to a forge directly. They drop a request file (`hive-open-pr`, or an issue, comment or label request), and a hive relay makes the write. On a GitHub hive those relays run on the GitHub client. On a forge-only hive, the forge request relay serves the same request directories through `pkg/forge` ([#19670](https://github.com/hivecommons/hive/issues/19670)). The code is `src/cmd/hive/forge_request_relay.go`.

- Each request is authorized by the same agent-manager gate as on GitHub. The file's owner must be the agent it names, and the agent must be allowed the write at the hive's ACMM level. With no authorizer, every request is denied.
- The repository must be in `project.repos`, on a non-GitHub forge, not paused and in the agent's scope.
- Every pull request opened this way carries the ready-for-review gate label, so a person releases it. The GitHub relay's self-authorization, reporter-trust and duplicate-claim checks read GitHub data and are not ported, so the label holds the pull request in their place. A retried request for the same head returns the open pull request.
- An issue request files the issue with the agent's origin label. A comment request posts the comment. A label request adds and removes labels. No request may set or clear a hive-controlled label: the hold and gate labels, `approved-direction`, `needs-human`, `blocked`, the auto-merge label, `hive/*` and `stage/*`.
- A forge 4xx other than 408 or 429 is final. Other failures are retried a few times.
- Results are written next to the request as `<name>.result.json`. A refused request is renamed `.denied`, `.rejected` or `.bad`. A transient forge failure is retried a few times and then renamed `.failed`.

Pushing the branch the pull request is opened from is a separate relay and is not covered here.

## Dashboard behavior

- The **Platform** tile (in Diagnostics on v5, its own section on v6) shows the forge kind and, for a self-managed instance, its URL. See [Dashboard sections](../dashboard-sections.md#platform).
- Each **Projects** card names its forge and host in its header, for example `🦊 GitLab · gitlab.example.com`. The card's forge is its `repo_policies[].forge` override when one is set, else `project.forge`. The card links to that instance, using GitLab's `/-/issues` and `/-/merge_requests` paths or Gitea's `/issues` and `/pulls`, including the per-item links built for an issue or change request with no URL of its own. A GitHub card shows its GitHub host as before. The section header names the hive's forge when every card agrees, and shows `N forges` when they differ. The data is the `forge` and `forgeUrl` fields of each repo in `/api/status`. See [Dashboard sections](../dashboard-sections.md#projects).

## Testing

Each adapter has unit tests against a fake HTTP server (`src/pkg/forge/*_test.go`). `merge_contract_test.go` checks the merge contract. The dashboard fields are covered by `src/pkg/dashboard/repo_forge_14115_test.go`.

## Gaps

- Enumeration through the forge covers forge-only hives and the non-GitHub repositories of a mixed hive. Agent request routing for a mixed hive is not done yet ([#19703](https://github.com/hivecommons/hive/issues/19703)). The stage engine, the advisory report and the merge gate are GitHub-only ([#14113](https://github.com/hivecommons/hive/issues/14113), [#14096](https://github.com/hivecommons/hive/issues/14096)).
- No adapter can open a change request.
- The forge request relay serves only `hive-open-pr` and the issue, comment and label requests. Claim, close, review-request and PR-edit requests are refused on a forge-only hive.
- There is no GitLab or Gitea webhook receiver, dashboard login provider or egress-proxy rule table.

The complete list is in [What full support would require](../forge-app-setup.md#what-full-support-would-require). For Forgejo specifically, what works on v5 and what is in progress is on [Forgejo](forgejo.md).
