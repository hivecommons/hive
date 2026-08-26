# GitHub App setup

> **Terminology:** the dashboard and docs call this the **Forge App** — the app Hive installs on your forge (your source control system, e.g., GitHub, GitHub Enterprise, GitLab, or Gitea). On **GitHub.com and GitHub Enterprise (GHE)** the Forge App **is a GitHub App**; this page covers creating and installing it. Dashboard controls live under **Governor Config → Forge App**.

Hive can authenticate with either a personal access token or a GitHub App. Use a GitHub App for production hives because installation tokens are scoped to selected repositories and can author PRs as the app bot when `github.app_authored_prs` is enabled.

Visual Hive is optional. A hosted installation uses two deliberately separate
GitHub Apps:

- the existing **Hive App** remains the only repository lifecycle writer; and
- the optional **Visual Hive App** may only dispatch the managed evidence
  workflow and publish the provenance-bound setup status.

Installing or omitting the Visual Hive App does not change ordinary Hive. Hive
continues operating when the optional App is absent or unavailable, while Visual
Hive setup and dispatch fail closed.
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
- **Setup URL**: `https://<hive-host>/gh-setup`.
- **Redirect on update**: enabled.
- **Webhook**: inactive unless you separately configure webhook channels; the dashboard setup flow does not require webhooks.
- **Device Flow**: enabled, so dashboard login can use the app's client ID.
- **Visibility**: private for an organization-specific app; public only if you intentionally operate one app for many unrelated owners.

Repository permissions used by the ordinary Hive dashboard and lifecycle are:

| Permission | Level | Why |
| --- | --- | --- |
| Metadata | Read-only | Required by GitHub. |
| Contents | Read/write | Clone, branch, and push agent changes. |
| Issues | Read/write | Enumerate, comment on, create, and close issues. |
| Pull requests | Read/write | Create, update, approve/merge, and inspect PRs. |
| Checks | Read-only or existing higher grant | Monitor CI check runs. |
| Actions | Read-only or existing higher grant | Inspect Actions; retain any higher grant already required by non-Visual-Hive Hive features. |
| Workflows | Read/write | Create and update the two managed workflow files. |
| Commit statuses | Read-only or existing higher grant | Inspect statuses; retain any higher grant already required by non-Visual-Hive Hive features. |
| Checks | Read-only | Monitor CI status. |
| Actions | Read-only | Inspect workflow runs. |
| Workflows | Read/write | Let trusted-tier agents push branches that modify `.github/workflows/`. Without it GitHub rejects any such push server-side ("refusing to allow a GitHub App to create or update workflow … without workflows permission") no matter what the token requests. Hive degrades gracefully — trusted-tier token minting retries without this permission and logs a warning — but CI-fix PRs from agents stay impossible until it is granted **and re-accepted on each installation** (an existing install must approve the new permission under Settings → Integrations → the app → Review request). |

Organization permission:

| Permission | Level | Why |
| --- | --- | --- |
| Members | Read-only | Identify contributors and owners where configured. |

## Create the optional Visual Hive App

Create a second App only when the owner elects to install Visual Hive. It needs
no OAuth flow, device flow, setup URL, or webhook. Install it on selected
repositories, beginning with a non-production demo repository.

Repository permissions are exactly:

| Permission | Level | Why |
| --- | --- | --- |
| Metadata | Read-only | Bind the exact repository identity. |
| Actions | Read/write | Dispatch the Hive-managed Visual Hive workflow. |
| Commit statuses | Read/write | Publish the exact setup-authorization status. |

Do not grant this App any other repository permission. Hive rejects an extra
read or write grant rather than allowing the optional App's scope to grow
silently or become a second lifecycle writer.

The App private key stays on the Hub. Configure the Hub process, not a spoke:

```text
HIVE_VISUAL_HIVE_GITHUB_APP_ID=<visual-hive-app-id>
HIVE_VISUAL_HIVE_GITHUB_APP_KEY_FILE=/secrets/visual-hive-app-key.pem
```

Opt in only the selected hosted Hive spoke:

```text
HIVE_VISUAL_HIVE_GITHUB_APP_ENABLED=true
```

This per-instance switch is deliberately separate from the Hub's fleet-level
key configuration. A spoke without the switch creates no Visual Hive token-
recipient state and makes no optional-App broker request, so ordinary Hive
operation is unchanged.

On an authenticated heartbeat, the Hub verifies the exact selected repository,
installation, bot identity, and granted permissions. It mints a repository-
scoped installation token lasting about one hour and seals it to the spoke's
persistent wrapping key. The private App key never leaves the Hub, and the
installation token remains memory-only on the spoke. Renewal does not restart
Hive or create a second controller.

## Install and configure Hive

1. After creating the app, note the **App ID**, **Client ID**, and app slug from the app page/URL.
2. Generate a private key and mount it into the hive, for example `./secrets/gh-app-key.pem` on the host mounted as `/secrets/gh-app-key.pem`.
3. Install the app on the organization/repositories Hive manages.
4. Configure Hive:

```yaml
github:
  app_id: <app-id>
  installation_id: <installation-id>  # optional when /gh-setup can complete it
  app_slug: <app-slug>
  key_file: /secrets/gh-app-key.pem
  oauth_client_id: <client-id>
```

The optional App installation must be approved by the organization or repository
owner before hosted Visual Hive preflight can pass. Hive reports both App
identities and their exact permission digests and fails before any setup mutation
when the optional App is missing, over-privileged, under-privileged, expired, or
bound to another repository. No permission change to the existing Hive App is
required for Visual Hive.

For GitHub Enterprise, also set `api_url`/`base_url` or the supported `forge` value so install URLs and API calls target the same host.

## `/gh-setup` flow

When the app's Setup URL points at `https://<hive-host>/gh-setup`, GitHub redirects back with `setup_action` and, for install/update, `installation_id`.

Hive accepts `setup_action=install` and `setup_action=update`, verifies that:

- a real `github.app_id` is configured,
- a private key file is resolvable,
- the installation ID can mint/verify a GitHub App installation, and
- the installation account matches the configured project org.

If verification succeeds, Hive persists `github.installation_id`, publishes a
new in-process core App runtime snapshot, and redirects the browser to
`/?ghSetup=ok`. Ordinary Hive remains available while the snapshot is missing
or invalid. An installed Visual Hive controller becomes ready in the same
process only after the independent Visual Hive App lease is also valid; a pod
restart is not required.
`setup_action=request` records a pending approval redirect but does not
configure an installation.

The setup endpoint is intentionally public because GitHub opens it in a browser that may not have a Hive session. The query string is not trusted; verification is done with the app key and GitHub API.

## Rotation and recovery

To rotate the ordinary Hive App key, generate a new key in GitHub, mount it at
the configured `key_file`, and reload the Hive configuration (or complete
`/gh-setup`) before deleting the old key. Rotate the Visual Hive App key on the
Hub at its separate key path. The same verified App and installation may rotate
keys and installation tokens without replacing a spoke process. A different
App, installation, repository, or active Visual Hive binding is rejected and
must use the managed uninstall/rebind path.

The initial broker supports public GitHub. Other forges remain ordinary-Hive
only until an equivalent least-privilege token broker is implemented for that
forge; Hive does not fall back to the core App.

### Existing Visual Hive installations

An older hosted contract may bind workflow dispatch and its exact setup status
to the ordinary Hive App. Hive never rewrites that identity in place: the
authorization workflow on the current default branch still expects the old
writer. Use the supported two-phase uninstall while that exact legacy binding
is available, review and merge its cleanup PR, then finalize. A fresh preflight
and setup binds the optional App. The legacy writer may authorize only its own
cleanup; it cannot activate a new Visual Hive controller or dispatch new
production evidence after the separate-App release is selected.
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
