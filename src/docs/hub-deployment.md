# Self-hosted hub deployment

Hive can run either as a spoke dashboard or as the hub. The same Go binary serves both roles; `HIVE_MODE=hub` selects the hub server in `cmd/hive/main.go`.

Use a self-hosted hub when you need a private registry, private SaaS provisioning, air-gapped control, or a hub URL other than `https://hive.hivecommons.dev`. If you only need a hosted spoke, use the hosted Hive Hub instead.

## What `Dockerfile.hub` builds

`src/Dockerfile.hub` builds only `./cmd/hive`, installs `kubectl`, creates `/data` and `/etc/hive`, sets `HIVE_MODE=hub`, and runs:

```sh
hive --config /etc/hive/hive.yaml
```

The hub listen port is still controlled by `HIVE_HUB_PORT` in the binary and defaults to `3001`. The Dockerfile exposes port `80`, so set `HIVE_HUB_PORT=80` if your container platform expects the process to bind that exposed port.

## Required storage

Use a persistent volume mounted at `/data`. The production/manual-provisioning docs describe the live hub as namespace `hive-hub`, PVC `hive-hub-data-rwx`, mounted at `/data`.

The hub stores live SaaS state under `/data`:

| Path | Purpose |
|---|---|
| `/data/saas/hives/<id>/meta.json` | Per-hive SaaS record used by auth, upgrades, assignments, and dashboard links. |
| `/data/hub-registry.json` | Fleet registry populated by spoke heartbeats. |
| `/data/saas/hub-secret.key` | Optional fallback heartbeat bearer secret when `HIVE_HUB_SECRET` is not set on the hub. |
| `/data/saas/clusters.json` | Cluster definitions used by automated provisioning. |

Use ReadWriteMany storage for hosted spoke PVCs. The provisioning template in `pkg/hub/saas_provision.go` requests `ReadWriteMany` and its rolling update strategy relies on both old and new pods being able to mount `/data` during a surge.

## Minimal hub environment

| Variable | Required | Notes |
|---|---:|---|
| `HIVE_MODE=hub` | Yes | Selects hub mode. |
| `HIVE_HUB_PORT` | No | Defaults to `3001`. Set to `80` for the published hub image contract if needed. |
| `HIVE_CONFIG` or `--config` | No | Defaults to `/etc/hive/hive.yaml`; `Dockerfile.hub` passes `--config /etc/hive/hive.yaml`. |
| `HIVE_HUB_SECRET` | Strongly recommended | Master secret from which heartbeat bearers and per-hive keys derive. If unset, the hub reads `/data/saas/hub-secret.key` when present. The hub additionally maintains a rotation generation set alongside it — see [security-model.md](security-model.md#master-key-rotation); rotate via the admin endpoint, never by swapping the master in place. |
| `HIVE_HUB_OAUTH_CLIENT_ID` / `HIVE_HUB_OAUTH_CLIENT_SECRET` | Enables **GitHub** login | These two enable the GitHub provider specifically. Human login is multi-provider: Google, IBMid, Red Hat, Microsoft Entra ID, and a generic OIDC slot are each enabled independently via `HIVE_HUB_OIDC_<PROVIDER>_CLIENT_ID` (+ `_CLIENT_SECRET`, `_ISSUER`, `_SCOPES`, `_DISPLAY` — see [env-vars.md](env-vars.md#hub-login-providers)). With two or more providers configured the hub shows a provider picker at `/login`; with exactly one it redirects straight into it; with none it logs OAuth disabled and registers no login routes. A provider with a client id but a missing/invalid issuer is silently skipped (check the `hub login enabled providers=` startup log line). |
| `HIVE_GITHUB_TOKEN` or GitHub App config | Required for operations that call GitHub | Same GitHub auth rules as a spoke. |
| `HIVE_HUB_SLACK_BOT_TOKEN`, `HIVE_NTFY_SERVER`, `HIVE_NTFY_TOPIC` | No | Optional hub notifications. |
| `HIVE_BACKUP_*`, `OCI_*`, `HIVE_KUBECONFIG_DIR` | No | Required only for disaster-recovery backup and OCI/FSS provisioning flows. See `backup-restore.md`. |

## Hub config shape

The hub still reads `hive.yaml`. The hub-specific code mainly uses the `hub` block, GitHub credentials, dashboard/auth settings, and any provisioning-related data under `/data/saas`. A minimal seed keeps the normal project and GitHub validation satisfied:

```yaml
project:
  org: your-org
  repos: [your-repo]
  primary_repo: your-repo

github:
  token: ${HIVE_GITHUB_TOKEN}

dashboard:
  auth_token: ${HIVE_DASHBOARD_TOKEN}

hub:
  enabled: true
  url: https://hive.example.com
```

Keep secret values in Kubernetes Secrets or environment variables; do not write token values directly into `hive.yaml`.

## Registering spokes to your hub

A spoke points at the hub with its config `hub.url` or the `HIVE_HUB_URL` override. Protected hubs require the same heartbeat secret on every spoke:

```yaml
hub:
  enabled: true
  url: https://hive.example.com
```

```yaml
env:
  - name: HIVE_HUB_URL
    value: https://hive.example.com
  - name: HIVE_HUB_SECRET
    valueFrom:
      secretKeyRef:
        name: hive-secrets
        key: HIVE_HUB_SECRET
```

Without `HIVE_HUB_SECRET`, a protected hub returns `401 unauthorized`, the spoke logs `hub heartbeat rejected status=401`, and the hub shows the hive offline. This matches the gotcha in `manual-provisioning.md`.

## Hosted spoke provisioning model

The hub's SaaS provisioner creates or records:

1. Namespace, RBAC, Secret, ConfigMap, Service, route/ingress, Deployment, and an RWX `hive-data` PVC.
2. A mounted `/data` volume where the spoke writes runtime config and state.
3. `HIVE_ID`, `POD_NAMESPACE`, `HIVE_LEVEL`, `HIVE_HUB_URL`, `HIVE_HUB_SECRET`, optional `HIVE_AUTHORIZED_USERS`, and inference endpoint env vars in the spoke Deployment.
4. `/data/saas/hives/<id>/meta.json` on the hub PVC. Heartbeats can be accepted without this record, but hub-driven upgrades and dashboard links need it.

For heartbeat-only clusters where the hub cannot run `kubectl`, follow the manifest-level workflow in `manual-provisioning.md`; this guide intentionally keeps the hub setup consistent with that battle-tested path.

## API surface to know

The hub registers heartbeat, registry, SaaS, contributor, OAuth, webhook, backup, and callback-related routes from the Go hub packages. Operators normally interact through the dashboard, but the important external contracts are:

- `POST /api/heartbeat` — spoke heartbeat, bearer-authenticated by `HIVE_HUB_SECRET` when configured.
- `POST /api/saas/hives` — hub-admin SaaS hive creation path.
- `/api/saas/auth-check` — ingress auth check used by hub-proxied spoke dashboards.
- `/contribute` and `/api/contribute/*` — contributor relay registration and WebSocket flow.
- `/api/registry` and leaderboard/fleet views — public or authenticated hub registry views.

See `api-reference.md` and the dashboard OpenAPI file for route details, and `manual-provisioning.md` for known failure modes.
