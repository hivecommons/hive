# GitHub App setup

Hive can authenticate with either a personal access token or a GitHub App. Use a GitHub App for production hives because installation tokens are scoped to selected repositories and can author PRs as the app bot when `github.app_authored_prs` is enabled.

## Create the app

In GitHub, open **Settings → Developer settings → GitHub Apps → New GitHub App** (or the equivalent organization settings page).

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
| Checks | Read-only | Monitor CI check runs. |
| Actions | Read/write | Inspect and dispatch the managed Visual Hive workflows. |
| Workflows | Read/write | Create and update the two managed workflow files. |
| Commit statuses | Read/write | Publish and verify exact setup-authorization statuses. |

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

When an existing App registration gains either write permission, GitHub marks
the installation update as pending. The organization or repository owner must
approve that permission update before hosted Visual Hive preflight can pass.
Hive reports the exact granted permission set and fails before any setup
mutation while a required permission is absent.

For GitHub Enterprise, also set `api_url`/`base_url` or the supported `forge` value so install URLs and API calls target the same host.

## `/gh-setup` flow

When the app's Setup URL points at `https://<hive-host>/gh-setup`, GitHub redirects back with `setup_action` and, for install/update, `installation_id`.

Hive accepts `setup_action=install` and `setup_action=update`, verifies that:

- a real `github.app_id` is configured,
- a private key file is resolvable,
- the installation ID can mint/verify a GitHub App installation, and
- the installation account matches the configured project org.

If verification succeeds, Hive persists `github.installation_id`, publishes a
new in-process App runtime snapshot, and redirects the browser to
`/?ghSetup=ok`. Ordinary Hive remains available while the snapshot is missing
or invalid. An installed Visual Hive controller becomes ready in the same
process after a valid snapshot appears; a pod restart is not required.
`setup_action=request` records a pending approval redirect but does not
configure an installation.

The setup endpoint is intentionally public because GitHub opens it in a browser that may not have a Hive session. The query string is not trusted; verification is done with the app key and GitHub API.

## Rotation and recovery

To rotate a private key, generate a new key in GitHub, mount it at the configured
`key_file`, and reload the Hive configuration (or complete `/gh-setup`) before
deleting the old key. The same verified App and installation may rotate keys and
installation tokens without replacing the process. A different App,
installation, repository, or active Visual Hive binding is rejected and must use
the managed rebind path.
