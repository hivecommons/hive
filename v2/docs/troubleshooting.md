# Hive troubleshooting

This guide covers the current containerized Go service (branch `v4`; code under the `v2/` directory). The older `docs/troubleshooting.md` page is for the legacy v1 systemd supervisor.

## Find the running service logs

Docker Compose deployments define `hive` and `gateway` services in `v2/docker-compose.yaml`. The Hive process healthcheck calls `http://127.0.0.1:3002/api/health`; the gateway publishes `3001` and proxies to Hive. Start with:

```bash
cd v2
docker compose ps
docker compose logs hive --tail=200
docker compose logs gateway --tail=100
```

Kubernetes deployments use namespace `hive`, Deployment `hive`, Service `hive`, and probes on `/api/health` and `/api/livez` in `v2/deploy/k8s/deployment.yaml`:

```bash
kubectl -n hive get deploy,pod,svc,pvc
kubectl -n hive logs deploy/hive --tail=200
kubectl -n hive describe pod -l app.kubernetes.io/name=hive
```

## Config fails to load or save

Hive reads `/etc/hive/hive.yaml` by default, or `HIVE_CONFIG`/`--config` when set. Startup logs `failed to load config` when `config.LoadWithDashboardOverlay` fails. Common validation strings in `v2/pkg/config/config.go` include:

- `project.org is required`
- `at least one agent must be configured`
- `github.token, github.app_id or github.forge is required`
- `agent <name>: invalid caveman_mode ...`
- `agent <name>: replicas must be between 1 and 5`
- `governor mode <mode> cadence for <agent>: ...`

In Kubernetes, edit the ConfigMap/Secret source and restart the pod; dashboard edits are stored in the `/data` PVC overlay. In Docker Compose, edit the bind-mounted `v2/hive.yaml` or the dashboard overlay and restart `hive`.

## GitHub credentials are missing or invalid

When no token or App credentials are usable, v2 starts the dashboard but disables write-capable GitHub work. The code logs these exact messages from `v2/cmd/hive/main.go` depending on the state:

- `no GitHub token configured (set github.token or github.app_id in config) — starting in dashboard-only mode`
- `GitHub App configured without credentials — hive starting in dashboard-only mode. Install the app and provide installation_id + key to enable agents.`
- `persisted user token is invalid or expired`

Check the configured `github:` block, the `HIVE_GITHUB_TOKEN` secret/env var, or the GitHub App `app_id`, `installation_id`, and `key_file`. For App setup, use the dashboard banner or `/gh-setup`; details are in [GitHub App setup](github-app-setup.md).

## Agents are stuck, paused, or need CLI login

Agents still run in tmux sessions managed by the v2 agent manager, but there is no v1 `AGENT_READY_MARKER` or `bin/supervisor.sh` loop. The v2 login detector scans recent tmux pane output for configured regexes and, on a match, logs `login required detected`, pauses the agent, and sends a notification that says to attach to `hive-<agent>` and run the backend login command.

Useful checks from inside the Hive container/pod:

```bash
tmux ls
tmux capture-pane -t hive-scanner -p -S -80
tmux attach -t hive-scanner
```

Detach from tmux with `Ctrl+B`, then `D`. If the dashboard shows an agent as paused, resume it from the dashboard/API after completing the CLI login (`claude login`, `copilot auth login`, `gemini auth login`, or the backend-specific command shown in the notification).

## Dashboard auth and access problems

The dashboard config lives under `dashboard:`. `dashboard.auth_token` protects non-public dashboard/API paths; health/liveness, snapshot/style, contribute, leaderboard, user-auth negotiation, SSO, the login provider picker (`/login`, `/login/{provider}`), and GitHub App setup paths are intentionally public in `isPublicPath`. `dashboard.authorized_users` controls direct-route user login allowlists; `dashboard.hub_proxied` means trusted hub/nginx headers identify the caller.

If API calls fail, check whether the request is going through the gateway on port `3001` or directly to Hive on `3002`, then inspect the response and Hive logs. Dashboard handlers return concrete messages such as `X-Hive-Role header required`, `insufficient access`, `owner access required`, and `only the owner can back up this hive` for role/header failures.

## Health endpoints

Use the same endpoints as the probes:

```bash
curl -fsS http://127.0.0.1:3002/api/health
curl -fsS http://127.0.0.1:3002/api/livez
```

`/api/livez` is deliberately process-focused: the Kubernetes manifest notes that stale hub heartbeat state belongs in deeper health reporting and should not crash-loop a healthy pod.
