# GitHub App setup

> **Terminology:** the dashboard and docs call this the **Forge App** — the app Hive installs on your forge (your source control system, e.g., GitHub, GitHub Enterprise, GitLab, or Gitea). On **GitHub.com and GitHub Enterprise (GHE)** the Forge App **is a GitHub App**; this page covers creating and installing it. Dashboard controls live under **Governor Config → Forge App**.

> **On GitLab, Gitea, or Forgejo?** This page does not apply, and there is no
> equivalent setup to perform: those forges are **not supported for running a
> hive** today. The adapters exist in the source tree but are not wired into any
> running code path, and the agent execution path is GitHub-only. See
> [Forge setup: GitLab, Gitea, and Forgejo](forge-app-setup.md) for exactly what
> is and is not implemented before you attempt an install.

Hive can authenticate with either a personal access token or a GitHub App. Use a GitHub App for production hives because installation tokens are scoped to selected repositories and can author PRs as the app bot when `github.app_authored_prs` is enabled.

## Personal access token (PAT) scopes

The PAT path is `HIVE_GITHUB_TOKEN` (or `github.token` in `hive.yaml`); the App path is `github.app_id`/`github.key_file` as described below. If you set `app_id`/`key_file` this section does not apply — the App permission table further down does.

Hive never validates token scopes at startup. A PAT with missing scopes fails **at request time** with generic GitHub 403 responses (fine-grained PATs typically say `Resource not accessible by personal access token`). The visible symptoms are agents reporting no actionable work, advisory digests not appearing on the tracking issue, empty fleet-stats widgets, or failed issue/PR writes in agent logs — none of which name the missing scope. If you see unexplained 403s, check the scopes below first.

### Classic PATs

Classic scopes are coarse, so one scope covers every ACMM tier:

| ACMM tier | Minimum classic scopes | Why |
| --- | --- | --- |
| L1–L2 (advisory) | `repo` (private repos) or `public_repo` (public repos only) | Read issues/PRs/contents; post advisory digest comments to the pinned tracking issue (an issues **write**, even though the tier is otherwise read-only). |
| L3–L4 (issue filing) | same | Create, label, comment on, and close issues. |
| L5–L6 (PR/merge) | same, plus `workflow` if agent PRs may touch `.github/workflows/` | Push branches, open/update/merge PRs, read checks/statuses. GitHub rejects workflow-file pushes without `workflow`. |

Add `read:org` when Hive should identify org members and contributor roles (recommended for org-owned hives).

### Fine-grained PATs

Fine-grained PATs are supported — the token is sent as a plain bearer, same as a classic PAT. Grant the token access to every repository the hive works on, with repository permissions mirroring the App table below:

| Permission | L1–L2 (advisory) | L3–L4 (issues) | L5–L6 (PR/merge) |
| --- | --- | --- | --- |
| Metadata | Read | Read | Read |
| Contents | Read | Read | Read and write |
| Issues | Read and write | Read and write | Read and write |
| Pull requests | Read | Read | Read and write |
| Checks | Read | Read | Read |
| Actions | Read | Read | Read |
| Commit statuses | Read | Read | Read |

Organization permission: **Members: Read** where contributor/owner identification is configured. Note that Issues read-and-write is needed even at advisory tiers because digests are posted as issue comments.

## Create the app

In GitHub, open **Settings → Developer settings → GitHub Apps → New GitHub App** (or the equivalent organization settings page). On **GitHub Enterprise**, do this on your enterprise host (`https://<your-ghe-host>/settings/apps/new`), not github.com — the app, its install page (`https://<your-ghe-host>/github-apps/<app-slug>`), and the source control host Hive is configured for must all be the same host.

Recommended values:

- **GitHub App name**: any unique operator-owned name.
- **Homepage URL**: your project or Hive dashboard URL.
- **Setup URL** (optional): `https://<hive-host>/gh-setup`. `<hive-host>` means
  **the address you type into your browser to reach this hive** — not where the
  hive process runs. GitHub redirects *your browser* here; it never fetches this
  URL itself, and neither does the hive. See
  [Choosing a Setup URL](#choosing-a-setup-url) before setting it, and leave it
  blank if the hive is not reachable from the browser you administer it with —
  Hive discovers `installation_id` on its own.
- **Redirect on update**: enabled — but only alongside a Setup URL that is
  actually reachable. It re-fires the redirect on every repository add or
  removal, so an unreachable Setup URL produces a dead browser tab every time
  you change the installation, not just once at install.
- **Webhook**: inactive unless you separately configure webhook channels; the dashboard setup flow does not require webhooks.
- **Device Flow**: enabled, so dashboard login can use the app's client ID.
- **Visibility**: private for an organization-specific app; public only if you intentionally operate one app for many unrelated owners.

Repository permissions used by the dashboard setup UI are:

| Permission | Level | Why |
| --- | --- | --- |
| Metadata | Read-only | Required by GitHub. |
| Contents | Read/write | Clone, branch, and push agent changes. |
| Issues | Read/write | Enumerate, comment on, create, and close issues. |
| Pull requests | Read/write | Create, update, approve/merge, and inspect PRs. |
| Checks | Read-only | Monitor CI status. |
| Actions | Read-only | Inspect workflow runs. |
| Workflows | Read/write | Let trusted-tier agents push branches that modify `.github/workflows/`. Without it GitHub rejects any such push server-side ("refusing to allow a GitHub App to create or update workflow … without workflows permission") no matter what the token requests. Hive degrades gracefully — trusted-tier token minting retries without this permission and logs a warning — but CI-fix PRs from agents stay impossible until it is granted **and re-accepted on each installation** (an existing install must approve the new permission under Settings → Integrations → the app → Review request). |

Organization permission:

| Permission | Level | Why |
| --- | --- | --- |
| Members | Read-only | Identify contributors and owners where configured. |

### The optional Visual Hive App (#4030)

Visual Hive needs two grants the Hive App deliberately does **not** hold — Actions
at write (to dispatch its installed workflow) and Commit statuses at write (to
publish the provenance-bound setup authorization status). Rather than widen the
Hive App, those grants live in a separate, optional, KubeStellar-owned App, so
enabling Visual Hive never changes what an installation that will never use it
has approved.

| App | App ID | Forge |
| --- | --- | --- |
| `kubestellar-viz-hive` | 4729416 | github.com |
| `kubestellar-viz-hive-ghe` | 5945 | github.ibm.com |

Its permission set is exactly:

| Permission | Level | Why |
| --- | --- | --- |
| Actions | Read/write | Dispatch the installed Visual Hive workflow. |
| Commit statuses | Read/write | Publish the setup authorization status. |
| Metadata | Read-only | Required by GitHub. |

Nothing else — no Contents, Workflows, Issues, Pull requests or Checks — and no
webhooks. Install it on **selected repositories**, not all repositories.

These identities are declared in `src/pkg/config/provenance.go` and are inert on
this branch: nothing resolves a key for them, delivers one, or authenticates as
them. Registering them ahead of the feature only means a config naming one is
described accurately instead of reported as an unknown App.

#### Reading the granted Actions and Commit-statuses permissions

App auth classification turns solely on the Issues permission, and an
installation that has not been granted the two Visual Hive permissions is
healthy — its `state` is `ok`. That is correct, and it is also why those grants
have to be reported separately: without it, an installation that has approved
them and one that has not are indistinguishable.

Every credential verdict now logs what is actually granted, including when the
verdict is `ok`:

```
github app credential verdict owner=<org> state=ok grants="actions=read statuses=none" visual_hive_execution_grants=false
```

`grants` reports the level GitHub returned for each permission (`none` when
there is no grant), and `visual_hive_execution_grants` is true only when both
are at `write`. Emitting it for a healthy verdict is the point: an installation
that has not approved a permission update is the one that still looks fine, and
GitHub keeps an App on its old permissions until an org owner accepts, so a
partly approved fleet is otherwise invisible.

Verdicts are computed where a GitHub call has failed and on the dashboard's
**Re-check** button, not on every eval cycle, so this adds no API calls to a
healthy hive. To read a specific installation's grants on demand — for example
to confirm the demo/canary install — press Re-check on that hive and read the
line above from its logs.

## Install and configure Hive

1. After creating the app, note the **App ID**, **Client ID**, and app slug from the app page/URL.
2. Generate a private key and mount it into the hive, for example `./secrets/gh-app-key.pem` on the host mounted as `/secrets/gh-app-key.pem`.
3. Install the app on the organization/repositories Hive manages.
4. Configure Hive:

```yaml
github:
  app_id: <app-id>
  installation_id: <installation-id>  # optional; auto-discovered (see below)
  app_slug: <app-slug>
  key_file: /secrets/gh-app-key.pem
  oauth_client_id: <client-id>
```

For GitHub Enterprise, also set `api_url`/`base_url` or the supported `forge` value so install URLs and API calls target the same host.

### Signed commits on agent PRs (`app_signed_commits`)

Agents commit with plain `git` in their pane and cannot sign: a GitHub App has no account to hold a GPG or SSH key. A base branch whose ruleset **requires signed commits** therefore blocks every agent PR from merging, however many approvals it has. GitHub does sign commits it creates itself — commits authored through the `createCommitOnBranch` GraphQL mutation are GPG-signed by GitHub and shown as **Verified**, and a GitHub App may author them directly.

```yaml
github:
  app_signed_commits: true   # opt-in; needs an installed App
```

With it on, the PR-request watcher (the choke point every `hive-open-pr` request passes through) re-authors the head branch before it opens the PR: it reads the `base...head` diff, builds one commit through the mutation with the App installation token — same tree, the agents' original commit messages and DCO trailers as the message — force-updates the branch to it, and then opens the PR. The PR arrives with a single commit, signed by GitHub, authored by `<app_slug>[bot]`, that a `required_signatures` rule accepts.

What it does not do, and falls back on (the PR still opens on the agent's own commits, the reason lands in the request's `.result.json` as `signed_skipped` and in the hive log at WARN):

- changes the mutation cannot express: executable bits, symlinks, submodule pointers;
- changes above 20 MiB of file content, or past the compare API's 300-file list;
- a head branch that moved (the agent pushed again) between reading the diff and updating the ref — nothing is replaced that the signed commit does not carry;
- a head branch that already has an open PR — that PR is reused untouched, as before.

It covers the PR as opened. A commit an agent pushes to the branch afterwards is plain git again and unsigned. Leave it off (the default) on hives whose base branches do not require signatures; it rewrites agent branches for no benefit there.

## Choosing a Setup URL

The Setup URL is a **browser redirect target**, not a callback GitHub's servers
make. After an install — and, with **Redirect on update** enabled, after every
repository add or removal — GitHub sends whatever browser you were using to
that address. So the only question that matters is:

> Can the browser I administer this hive from open that URL?

That is a property of how *you* reach the hive, not of where the hive runs:

| How you reach the dashboard | Correct Setup URL |
| --- | --- |
| Desktop session on the hive machine | `http://localhost:3001/gh-setup` |
| SSH tunnel (`ssh -L 3001:localhost:3001 you@hive`) | `http://localhost:3001/gh-setup` — the tunnel makes `localhost` genuinely the hive |
| Another machine on the network | `http://<hive-lan-name-or-ip>:3001/gh-setup` |
| Public hostname behind TLS | `https://<hive-host>/gh-setup` |
| Not reachable from your browser at all | **leave it blank** — see below |

`localhost` is the common trap. It is correct only for the first two rows, and
it fails silently everywhere else: set it while sitting at the hive machine and
it works, then administer the hive from a laptop and every install-update
redirect lands on the laptop's own port 3001, which is nothing at all.

### Headless, NATed, and remotely administered hives

**The Setup URL is optional. A hive that is not reachable from anywhere should
leave it blank and turn Redirect on update off.** Nothing is lost.

The callback's only job is to save you from pasting an `installation_id`, and
Hive already resolves that itself: it asks GitHub `GET /orgs/{org}/installation`
authenticated with the **App JWT**, falling back to walking
`GET /app/installations`. That runs when the hive starts, whenever App
credentials are (re)loaded, and when you press **Re-check** on the dashboard's
Forge App panel — adopting and persisting an unambiguous match, so it corrects a
missing *or wrong* `installation_id` without any redirect. An ambiguous or
absent result leaves your configuration untouched rather than guessing.

Every one of those calls is **outbound, hive to GitHub**. A headless Fedora
CoreOS host, a VM on a NAT network, or any server you only reach over SSH needs
no inbound reachability for GitHub App authentication to work. The
preconditions are the ones App auth already has: the private key mounted at
`key_file`, and `project.org` set so Hive knows whose installation to look for.

If you do leave a Setup URL configured that your browser cannot reach, the
resulting dead redirect is cosmetic. Adding a repository modifies the existing
installation rather than creating a new one, so the `installation_id` in that
redirect is one the hive already holds — the failed page does not mean App
authentication is broken.

## `/gh-setup` flow

When the app's Setup URL points at `https://<hive-host>/gh-setup`, GitHub redirects back with `setup_action` and, for install/update, `installation_id`.

Hive accepts `setup_action=install` and `setup_action=update`, verifies that:

- a real `github.app_id` is configured,
- a private key file is resolvable,
- the installation ID can mint/verify a GitHub App installation, and
- the installation account matches the configured project org.

If verification succeeds, Hive persists `github.installation_id`, reinitializes the GitHub client, and redirects the browser to `/?ghSetup=ok`. `setup_action=request` records a pending approval redirect but does not configure an installation.

The setup endpoint is intentionally public because GitHub opens it in a browser that may not have a Hive session. The query string is not trusted; verification is done with the app key and GitHub API.

## Rotation and recovery

To rotate a private key, generate a new key in GitHub, mount it at the configured `key_file`, restart Hive, then delete the old key in GitHub after the new one is confirmed working. If an installation was replaced, use `/gh-setup` again or update `github.installation_id` and restart/reload the hive.

## Hub-distributed App keys (hosted fleet) — operator runbook

On a hosted fleet the hub — not the hive owner — is the App-key authority. The hub keeps one PEM per cluster at `/data/saas/app-keys/<clusterID>.pem` (owner-only file modes, on the same PVC as the hub's other secrets) and reconciles it to every spoke on that cluster over the heartbeat. A claimed hive whose cluster has no stored key is delivered `key_delivered=false` at claim time and then receives nothing on any beat — it stays `Degraded` on `key-missing` forever until an operator uploads a key. **The hive owner cannot see, supply, or fix this key**; every owner-facing surface deliberately stays silent for the operator-side states (`key-missing`, `key-invalid`, `no-app-assigned`).

### Uploading (or replacing) a cluster's App key

The upload endpoint is the ONLY way key material enters the hub. Admin-gated:

```
PUT /api/saas/admin/cluster-app-keys/{clusterID}
Content-Type: application/json

{"private_key": "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----", "app_id": 123456}
```

- `private_key` (required) — the GitHub App's PEM private key. Validated before it is persisted; a non-PEM or unparsable key is rejected with 400 and nothing is stored.
- `app_id` (optional) — the App's numeric ID; when supplied it is persisted alongside in `clusters.json` so the cluster carries a complete identity.
- An unknown `{clusterID}` returns 404.

The response echoes back only the key's **fingerprint** (never the key), so you can verify the right key landed:

```json
{"cluster_id": "oke-frankfurt-1", "app_id": 123456, "has_key": true, "fingerprint": "sha256:..."}
```

Compare that fingerprint with one computed locally from the PEM you meant to upload. Delivery to the spokes then happens automatically on the next heartbeats — no restart needed.

### Checking which clusters hold a key

```
GET /api/saas/admin/cluster-app-keys
```

returns `[{cluster_id, app_id, has_key, fingerprint}, ...]` for every cluster. A cluster with `has_key: false` will strand the first App-requiring hive claimed onto it — check this *before* pointing a pool at a new cluster.

### The `app-creds-undelivered` fleet alert

When a claimed, online hive reports an operator-side App-credential state, the hub raises a **critical** fleet alert (type `app-creds-undelivered`) in the dashboard's "Attention needed" panel. The alert names the hive and its cluster, shows how long it has been stranded, and carries the exact `PUT` remedy above. One alert per hive; it clears automatically once the key is delivered and the spoke reports healthy. The states it covers:

- `key-missing` — no key ever reached the spoke (usually: the cluster has no stored PEM — upload one).
- `key-invalid` — a key is present but GitHub rejects the JWTs it signs (it belongs to a different App — replace it with the correct PEM).
- `no-app-assigned` — the hive still carries the placeholder `app_id` and was never assigned a real App (assign the cluster's App and upload its key).
