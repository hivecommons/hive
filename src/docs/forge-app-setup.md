# Forge setup: GitLab, Gitea, and Forgejo

> **Read this first.** On **GitHub.com and GitHub Enterprise** the Forge App is a
> GitHub App and everything works — see [GitHub App setup](github-app-setup.md).
> On **GitLab, Gitea, and Forgejo** it does not. The adapters exist in the source
> tree, are tested, and are **not wired into any running code path**: nothing in
> Hive constructs one today. Configuring `project.forge: gitlab` changes what the
> dashboard *displays* and nothing else. There is no non-GitHub setup path to
> follow yet, and this page exists so you can establish that in one read instead
> of discovering it after an install.

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
| GitLab (SaaS or self-managed) | **No** | Adapter only, unwired | Adapter only, unwired | **Not implemented** | **Not implemented** | **No** |
| Gitea | **No** | Adapter only, unwired | Adapter only, unwired | **Not implemented** | **Not implemented** | **No** |
| Forgejo | **No** | Adapter only, unwired | Adapter only, unwired | **Not implemented** | **Not implemented** | **No** |

"Adapter only, unwired" means the Go code is written and covered by tests, but no
running code path calls it. Gitea and Forgejo share one adapter because they
share one REST API surface.

**A hive cannot presently run against GitLab, Gitea, or Forgejo.** If that is
your forge, there is nothing to install and no configuration that will make
agents work.

This is the state the [roadmap](roadmap.md) records for the abstraction: the
adapters are built, and what remains is "moving scheduler operations behind the
abstraction — production callers still use the forge-specific client directly."
The design rationale is [ADR-0005](adr/0005-forge-abstraction.md).

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

- **No merge primitive.** `Merger` is declared as an optional extension interface
  that no adapter implements, and it carries a `TODO` in the source. Merge
  semantics diverge too sharply across forges — strategy names, gate checks,
  MR-versus-PR — to neutralize convincingly, so it was left explicit rather than
  half-built.
- **No issue or change-request creation.** The interface has no `CreateIssue` and
  no `CreatePR`. Even fully wired, the adapters could comment and label but could
  not file the work.

That second point is the ceiling on this path: the abstraction was scoped to the
read path plus light write operations, not to the create-and-merge lifecycle an
agent needs.

## The configuration surface that exists

These keys parse and validate today. They are documented here because they are
real and you will find them in the source — **not** because setting them enables
anything. Their only consumer is the dashboard's Platform card, which reads them
to display a forge name and instance URL.

```yaml
project:
  forge: gitlab        # "github" (default) | "gitlab" | "gitea"

gitlab:
  gitlab_url: https://gitlab.example.com   # default https://gitlab.com
  token_env: GITLAB_TOKEN                  # env var NAME, never the token

gitea:
  gitea_url: https://gitea.example.com     # no default; required for Gitea
  token_env: GITEA_TOKEN                   # env var NAME, never the token
```

Notes that will save you a wrong guess:

- `gitlab:` and `gitea:` are **top-level** blocks, siblings of `github:` — not
  nested inside it.
- `token_env` names the **environment variable to read the token from**. The
  token value is never written to config, matching Hive's no-secrets-in-config
  rule. Use `GITLAB_TOKEN` / `GITEA_TOKEN` unless you have a reason not to.
- Omitting `project.forge` means GitHub, so every existing config is unaffected.
- GitLab defaults to `https://gitlab.com`. Gitea has **no** default — there is no
  single public Gitea host, so selecting the Gitea forge without a URL is a
  configuration error at client construction.
- The instance URL is the **bare instance root**. The adapters append `/api/v4`
  (GitLab) and `/api/v1` (Gitea) themselves; adding the suffix yourself produces
  a doubled path.

### Do not confuse `project.forge` with `github.forge`

Two unrelated settings share the word "forge", and mixing them up is the most
likely way to break a working hive:

| Key | Meaning | Values |
| --- | --- | --- |
| `project.forge` | Which forge **family** a spoke executes against — selects the `pkg/forge` adapter (display-only today) | `github`, `gitlab`, `gitea` |
| `github.forge` | Which GitHub **instance** this hive's App and repos live on — drives App ID, app slug, and API URL | a GitHub host label |

`github.forge` is part of the GitHub App identity system and is fully live. Do
not set it to `gitlab` or `gitea`; those are not GitHub hosts and it is not that
kind of setting.

## If you evaluate the adapters anyway

Should you exercise `pkg/forge` directly — from a test or your own program, since
Hive itself will not call it — the tokens it expects are ordinary access tokens,
not apps:

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

1. `CreateIssue` and `CreateChangeRequest` on the `Forge` interface, plus a
   settled `Merge` — the create-and-merge lifecycle agents depend on.
2. A forge-neutral agent execution path, since agents reach their forge through
   the `gh` CLI wrapper rather than through `pkg/forge`.
3. Credential plumbing for token-based forges alongside the GitHub App minting
   machinery, including per-tier scoping.
4. A GitLab/Gitea dashboard login provider.
5. Egress-proxy mode enforcement registration for the non-GitHub hosts.

## See also

- [GitHub App setup](github-app-setup.md) — the supported path: app creation,
  permissions, Setup URL, and `/gh-setup`.
- [Getting started](getting-started.md) — first-session setup, including Step 0.
- [Troubleshooting](troubleshooting.md) — Forge App and credential symptoms.
- [Operator reference](operator-reference.md) — the full configuration surface.
