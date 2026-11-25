# Forge setup: GitLab, Gitea, and Forgejo

> **Read this first.** On **GitHub.com and GitHub Enterprise** the Forge App is a
> GitHub App and everything works — see [GitHub App setup](github-app-setup.md).
> On **GitLab, Gitea, and Forgejo** it does not. The adapters exist in the source
> tree and are tested, but they are **still not on a path a non-GitHub hive can
> reach**. Two callers now construct an adapter from `project.forge` — the
> governor's escalation writes ([#5259](https://github.com/hivecommons/hive/issues/5259))
> and the hold guard's re-hold. A hive whose repositories are all on GitLab,
> Gitea or Forgejo now enumerates its work through the adapters
> ([#19669](https://github.com/hivecommons/hive/issues/19669)), so those writes
> are reached. Stage labels, merging, agent execution and mixed GitHub hives are
> still missing. The operation list per adapter is on
> [Forges: GitLab, Gitea and Forgejo](integrations/forges.md). There is no
> non-GitHub setup path to follow yet, and this page exists so you can establish
> that in one read instead of discovering it after an install.

## Terminology

The dashboard and docs call the app Hive installs on your source control system
the **Forge App**, and the docs call the system itself your **forge**. That
naming is deliberately forge-neutral because the code abstraction underneath it
is. The naming does not, on its own, imply the non-GitHub paths are finished —
this page is the difference.

## What is supported today

| Forge | Agents do work | Read API | Comment / label / hold | Open issues & PRs | Merge | Dashboard login |
| --- | --- | --- | --- | --- | --- | --- |
| GitHub.com | Yes | Yes | Yes | Yes | Yes | Yes |
| GitHub Enterprise | Yes | Yes | Yes | Yes | Yes | Yes |
| GitLab (SaaS or self-managed) | **No** | Adapter only, unwired | Wired but unreachable | **Not implemented** | Adapter only, unwired | **No** |
| Gitea | **No** | Adapter only, unwired | Wired but unreachable | **Not implemented** | **Not implemented** | **No** |
| Forgejo | **No** | Adapter only, unwired | Wired but unreachable | **Not implemented** | **Not implemented** | **No** |

"Adapter only, unwired" means the Go code is written and covered by tests, but no
running code path calls it. "Wired but unreachable" is the narrower state the
comment/label column is now in: two production callers — the governor's
escalation sweep and the hold guard — select their adapter from `project.forge`.
They would post the evidence comment and the `needs-human` label, or the drift
comment and a re-applied `hold` label, to your forge. Both are driven by the
governor's enumeration. On a forge-only hive that enumeration now runs through
the adapter, so they fire; in a hive with any repository on GitHub, a repository
on another forge is still enumerated through GitHub and finds nothing. Gitea and Forgejo are separate
kinds (`gitea` and `forgejo`) that share one REST client, so a call that differs
between them can change in one kind without touching the other. Forgejo has its
own kind, config block and adapter type. It is no further along than Gitea.

**A hive cannot presently run against GitLab, Gitea, or Forgejo.** If that is
your forge, there is nothing to install and no configuration that will make
agents work.

This is the state the [roadmap](roadmap.md) records for the abstraction: the
adapters are built, the first production caller is behind the seam, and what
remains is the read path — neutralizing enumeration, which today owns hold-label
filtering, issue filters and SLA tracking inside `pkg/github`. The design
rationale is [ADR-0005](adr/0005-forge-abstraction.md).

## Why: the agent execution path is GitHub-only

The forge abstraction and the thing agents actually run are two different
mechanisms, and only the second one moves work.

Agents do not call the forge abstraction. They shell out to the **`gh` CLI**,
through the wrapper Hive installs ahead of it on `PATH`
(`bin/gh-wrapper.sh`), which injects a **GitHub App installation token** and
enforces the per-agent restriction rules. The higher-level helpers agents use to
file work — `bin/hive-open-issue.sh`, `bin/hive-open-pr.sh`, `bin/hive-merge.sh`
— are all built on that same `gh` path. There is no `glab` equivalent, no Gitea
CLI, and no forge-neutral shell entry point.

The wrapper does not even let an agent call GitHub directly for writes: it
intercepts `gh pr create`, `gh issue create`, and `gh issue comment` and redirects
them to those helpers, which write request files that the hive daemon consumes
and executes with an App installation token. The GitHub MCP server's write tools
are explicitly denied for the same reason, so that route is closed too.

So the write path that matters — open an issue, push a branch, open a PR, merge
it — is GitHub-shaped end to end, independently of anything `project.forge` says.

Three further consequences follow, and each is load-bearing:

- **Authentication.** Every credential path is a GitHub App: App ID, private key,
  installation ID, per-tier minted installation tokens. GitLab and Gitea have no
  GitHub App, so none of that machinery has an analogue. The adapters authenticate
  with a plain personal access token instead, which is why they could never simply
  inherit the existing credential plumbing.
- **Dashboard login.** Signing in uses GitHub OAuth or a configured OIDC provider.
  There is no GitLab or Gitea login provider, including for a hive whose
  `project.forge` names one.
- **Webhooks.** Both forge webhook receivers verify `X-Hub-Signature-256` and read
  `X-GitHub-Event`. There is no GitLab (`X-Gitlab-Token`) or Gitea receiver.

## Why the adapters exist anyway

`src/pkg/forge` is a genuine, tested abstraction, written so Hive is not
permanently welded to GitHub. It defines a forge-neutral `Forge` interface plus
neutral types — note `ChangeRequest` rather than "pull request", since that is a
GitHub term — and three adapters: GitHub (wrapping `pkg/github`), GitLab (REST
v4), and Gitea/Forgejo (REST v1).

All three implement the same operation set:

| Operation | Meaning |
| --- | --- |
| `GetRepo` | Fetch one repo/project by its slug |
| `ListOpenIssues` | Open issues (GitHub's adapter filters out PRs, which its REST API models as issues) |
| `ListOpenChangeRequests` | Open pull requests / merge requests |
| `CreateIssueComment` | Comment on an issue or change request |
| `AddLabels` / `RemoveLabel` | Label maintenance |
| `SetHold` | Apply or clear the `hold` merge gate, via the label |

What is deliberately **absent** matters as much as what is present:

- **Merge is optional and partial.** `Merger` is an optional extension interface
  kept off `Forge`, because merge semantics diverge across forges — strategy
  names, gate checks, MR-versus-PR. Only the GitLab adapter implements it so far
  (accept the MR with `merge`, `squash` or `rebase`, a head-SHA guard and branch
  removal). No production caller uses it yet; routing the merge gate by forge
  kind is [#14113](https://github.com/hivecommons/hive/issues/14113).
- **Creation is partial.** Gitea and Forgejo implement `CreateIssue` and
  `CreateChangeRequest` through optional seams (`forge.IssueSeam`,
  `forge.ChangeRequestCreator`), and on a forge-only hive the forge request
  relay uses them for agents' `hive-open-pr`, issue, comment and label requests
  ([#19670](https://github.com/hivecommons/hive/issues/19670)). GitLab has
  neither seam, and the `Forge` interface itself still has no create methods.

That second point is the ceiling on this path: the abstraction was scoped to the
read path plus light write operations, not to the create-and-merge lifecycle an
agent needs.

## The configuration surface that exists

These keys parse and validate today. They are documented here because they are
real and you will find them in the source — **not** because setting them makes a
non-GitHub hive work. They have two consumers: the dashboard, whose Platform card
and Projects cards read them to display the forge name and instance host, and
the governor's escalation and hold-guard writes, which build an adapter from them
(see the table above for why those callers are not reached on a non-GitHub hive
today).

```yaml
project:
  forge: gitlab        # "github" (default) | "gitlab" | "gitea" | "forgejo"

gitlab:
  gitlab_url: https://gitlab.example.com   # default https://gitlab.com
  token_env: GITLAB_TOKEN                  # env var NAME, never the token

gitea:
  gitea_url: https://gitea.example.com     # no default; required for Gitea
  token_env: GITEA_TOKEN                   # env var NAME, never the token

forgejo:
  forgejo_url: https://codeberg.org        # no default; required for Forgejo
  token_env: FORGEJO_TOKEN                 # env var NAME, never the token
```

Notes that will save you a wrong guess:

- `gitlab:`, `gitea:` and `forgejo:` are **top-level** blocks, siblings of
  `github:` — not nested inside it.
- `token_env` names the **environment variable to read the token from**. The
  token value is never written to config, matching Hive's no-secrets-in-config
  rule. Use `GITLAB_TOKEN` / `GITEA_TOKEN` / `FORGEJO_TOKEN` unless you have a
  reason not to.
- Omitting `project.forge` means GitHub, so every existing config is unaffected.
- GitLab defaults to `https://gitlab.com`. Gitea and Forgejo have **no** default
  — there is no single public host, so selecting either forge without a URL is a
  configuration error at client construction.
- The instance URL is the **bare instance root**. The adapters append `/api/v4`
  (GitLab) and `/api/v1` (Gitea and Forgejo) themselves; adding the suffix
  yourself produces a doubled path.
- A Forgejo instance configured as `gitea` is detected. The spoke reads
  `/api/v1/version` from each instance a `gitea` repo resolves to, at startup
  and every ten minutes for instances it has not read yet (a failed request,
  or one added by a reload). When the answer is a Forgejo version
  (`X.Y.Z+gitea-A.B.C` from Forgejo 7.0 on, `A.B.C+N` before), it logs one
  warning per instance suggesting `forge: forgejo`, the only kind with the
  Forgejo automerge and close-on-merge sweeps. The request carries the
  `gitea.token_env` token only where that token may go (the `forge_url` host
  rule below).
- A single repo can live on a different forge from the rest of the project with
  `project.repo_policies[].forge` and `forge_url` (bare instance root; inherits
  `gitlab_url` / `gitea_url` / `forgejo_url` when omitted). Unlike
  `project.forge`, these keys are validated at load: an unknown kind, a
  non-http(s) URL, or a Gitea or Forgejo repo with no URL anywhere is a config
  error. A `forge_url` must also be on the same host and port as its forge
  block's URL, because the block's token is only sent to that host. A mismatch
  is a config error. The GitLab block URL defaults to `https://gitlab.com`. A
  Gitea or Forgejo block with no URL of its own is not checked. The governor's
  escalation writes and its repo-scoped kicks follow the per-repo value; the
  token still comes from the forge-wide `token_env`.

```yaml
project:
  repo_policies:
    - repo: mirror
      forge: gitea
      forge_url: https://codeberg.org
```

### Forgejo git credentials: one short-lived token per tier

Git clone and push on a Forgejo repo send a token only to that repo's forge
host. By default the token is the `forgejo.token_env` access token, the one
every agent tier shares, and it never expires. Set `forgejo.integration` to
have hive sign a one-hour JWT for each push and clone instead. Forgejo's
authorized integrations accept these tokens, and each tier gets its own
integration, the Forgejo analogue of a GitHub App installation token:

```yaml
forgejo:
  forgejo_url: https://forgejo.example.com
  integration:
    issuer: https://keys.example.com      # https origin serving the key list
    key_file: /etc/hive/forgejo-jwt.pem   # RSA key, mode 0600/0400, or 0440 group hive-launch
    audiences:                            # token tier -> integration audience
      contributor: "<audience>"
      advisor: "<audience>"
```

1. Generate the RSA key (2048 bits or more), and publish the two files that
   `hive forge key-list --issuer https://keys.example.com --key /etc/hive/forgejo-jwt.pem --out DIR`
   writes at the issuer, over https.
2. For each tier, run
   `forgejo admin user create-authorized-integration --issuer https://keys.example.com --scope <scope> --repo <owner/repo> --claim-eq sub=hive:<tier>`.
   Pass `--scope` and `--repo` explicitly, because both default to `all`.
3. Copy each integration's audience under `audiences`, keyed by tier.

A tier with no audience gets the closest less-privileged tier that has one,
and never a more-privileged one. A tier with none cannot clone or push. The
`sub` claim is `hive:<tier>`, so a token minted for one tier is refused by
every other tier's integration.

The key file must be owner-only (0600 or 0400), or group-readable only by
`hive-launch` (GID 1002, the pod `fsGroup`), which is how a Kubernetes Secret
mounted at `/secrets` with `defaultMode: 0440` presents it. hive refuses a
group-readable key under any other group, because agent UIDs share group
`node` (GID 1000), and refuses any key that others can read. A key kept under
`/data/secrets` is sealed in place by the keystore at boot and still loads
afterwards. The key file is read at every mint, so
replacing it rotates the key; list the old and new key in the key list while
the change rolls out.

### Do not confuse `project.forge` with `github.forge`

Two unrelated settings share the word "forge", and mixing them up is the most
likely way to break a working hive:

| Key | Meaning | Values |
| --- | --- | --- |
| `project.forge` | Which forge **family** a spoke executes against — selects the `pkg/forge` adapter (read by the dashboard and by the governor's escalation and hold-guard writes; see above) | `github`, `gitlab`, `gitea`, `forgejo` |
| `github.forge` | Which GitHub **instance** this hive's App and repos live on — drives App ID, app slug, and API URL | a GitHub host label |

`github.forge` is part of the GitHub App identity system and is fully live. Do
not set it to `gitlab`, `gitea` or `forgejo`; those are not GitHub hosts and it
is not that kind of setting.

## If you evaluate the adapters anyway

Should you exercise `pkg/forge` directly — from a test or your own program, which
is still the only way to reach most of it — the tokens it expects are ordinary
access tokens, not apps:

| Forge | Credential | Sent as |
| --- | --- | --- |
| GitLab | Personal, project, or group access token | `PRIVATE-TOKEN` header |
| Gitea / Forgejo | Personal access token | `Authorization: token <TOKEN>` |

Scope them to the repositories the hive would work on, with read access to
issues and merge/pull requests, plus write access to issue comments and labels if
you intend to exercise the comment/label/hold operations. A token may be omitted
entirely for unauthenticated reads of public projects; private projects and all
writes require one. Use a throwaway token — this is an evaluation path, not a
supported deployment.

One further caveat if you do: Hive's egress proxy enforces ACMM policy only on
hosts registered for mode enforcement, which today are GitHub hosts and the
Linear API. A GitLab or Gitea host is not among them, so its traffic is tunneled
without request-level ACMM enforcement. An advisory-tier hive would not have its
writes to such a host gated the way it would on GitHub — another reason this path
is not deployment-ready.

## What full support would require

Listed so the size of the gap is legible, not as a plan of record:

1. Forge-neutral enumeration for mixed hives. A forge-only hive now enumerates
   through the adapters ([#19669](https://github.com/hivecommons/hive/issues/19669)),
   with the label gates and issue filter applied above the forge boundary. A
   hive with any repository on GitHub still starts at
   `github.Client.EnumerateActionable` for every repository.
2. Create seams on GitLab, and the create relay for repositories on another
   forge inside a hive that also has GitHub repositories. Gitea and Forgejo
   have `CreateIssue` and `CreateChangeRequest` (`forge.IssueSeam`,
   `forge.ChangeRequestCreator`), served to agents on a forge-only hive.
3. A forge-neutral agent execution path, since agents reach their forge through
   the `gh` CLI wrapper rather than through `pkg/forge`.
4. Credential plumbing for token-based forges alongside the GitHub App minting
   machinery, including per-tier scoping.
5. A GitLab/Gitea dashboard login provider.
6. Egress-proxy mode enforcement registration for the non-GitHub hosts.

## See also

- [GitHub App setup](github-app-setup.md) — the supported path: app creation,
  permissions, Setup URL, and `/gh-setup`.
- [Getting started](getting-started.md) — first-session setup, including Step 0.
- [Troubleshooting](troubleshooting.md) — Forge App and credential symptoms.
- [Operator reference](operator-reference.md) — the full configuration surface.
