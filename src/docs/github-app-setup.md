# GitHub App setup

> **Terminology:** the dashboard and docs call this the **Forge App** — the app Hive installs on your forge (your source control system, e.g., GitHub, GitHub Enterprise, GitLab, or Gitea). On **GitHub.com and GitHub Enterprise (GHE)** the Forge App **is a GitHub App**; this page covers creating and installing it. Dashboard controls live under **Governor Config → Forge App**.

Hive can authenticate with either a personal access token or a GitHub App. Use a GitHub App for production hives because installation tokens are scoped to selected repositories and can author PRs as the app bot when `github.app_authored_prs` is enabled.

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

Repository permissions used by the dashboard setup UI are:

| Permission | Level | Why |
| --- | --- | --- |
| Metadata | Read-only | Required by GitHub. |
| Contents | Read/write | Clone, branch, and push agent changes. |
| Issues | Read/write | Enumerate, comment on, create, and close issues. |
| Pull requests | Read/write | Create, update, approve/merge, and inspect PRs. |
| Checks | Read-only | Monitor CI status. |
| Actions | Read-only | Inspect workflow runs. |

Organization permission:

| Permission | Level | Why |
| --- | --- | --- |
| Members | Read-only | Identify contributors and owners where configured. |

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

For GitHub Enterprise, also set `api_url`/`base_url` or the supported `forge` value so install URLs and API calls target the same host.

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
