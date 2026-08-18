# Hive troubleshooting

This guide covers the current containerized Go service (branch `v4`; code under the `src/` directory). Hive runs as an in-process supervisor: the governor loop, health checks, login detection, and notifications all run inside the `hive` process — there is no separate systemd supervisor, `bin/supervisor.sh` loop, `hive-healthcheck.service` timer, or `/etc/hive/agent.env` file. Those, along with `AGENT_LOOP_PROMPT`, `AGENT_READY_MARKER`, `AGENT_AUTO_APPROVE_PHRASE`, and `install.sh`/`uninstall.sh`, belong to the legacy v1 host install and do not apply here.

Start most investigations from the dashboard and the service logs, then drop to the sections below for a specific failure mode.

## Find the running service logs

Docker Compose deployments define `hive` and `gateway` services in `src/docker-compose.yaml`. The Hive process healthcheck calls `http://127.0.0.1:3002/api/health`; the gateway publishes `3001` and proxies to Hive. Start with:

```bash
cd v2
docker compose ps
docker compose logs hive --tail=200
docker compose logs gateway --tail=100
```

Kubernetes deployments use namespace `hive`, Deployment `hive`, Service `hive`, and probes on `/api/health` and `/api/livez` in `src/deploy/k8s/deployment.yaml`:

```bash
kubectl -n hive get deploy,pod,svc,pvc
kubectl -n hive logs deploy/hive --tail=200
kubectl -n hive describe pod -l app.kubernetes.io/name=hive
```

The process also writes `hive.log` under the configured logs directory (default `/data/logs`, from `governor.logging.dir`, falling back to `data.logs_dir`), and tees the same lines to stdout — so `docker compose logs` / `kubectl logs` show everything the file does. The older per-agent `AGENT_LOG_FILE` heartbeat file from v1 is not used.

## Config fails to load or save

Hive reads `/etc/hive/hive.yaml` by default, or `HIVE_CONFIG`/`--config` when set. Startup logs `failed to load config` when `config.LoadWithDashboardOverlay` fails. Common validation strings in `src/pkg/config/config.go` include:

- `project.org is required`
- `at least one agent must be configured`
- `github.token, github.app_id or github.forge is required`
- `agent <name>: invalid caveman_mode ...`
- `agent <name>: replicas must be between 1 and 5`
- `governor mode <mode> cadence for <agent>: ...`

In Kubernetes, edit the ConfigMap/Secret source and restart the pod; dashboard edits are stored in the `/data` PVC overlay. In Docker Compose, edit the bind-mounted `src/hive.yaml` or the dashboard overlay and restart `hive`.

## GitHub credentials are missing or invalid

When no token or App credentials are usable, v2 starts the dashboard but disables write-capable GitHub work. The code logs these exact messages from `src/cmd/hive/main.go` depending on the state:

- `no GitHub token configured (set github.token or github.app_id in config) — starting in dashboard-only mode`
- `GitHub App configured without credentials — hive starting in dashboard-only mode. Install the app and provide installation_id + key to enable agents.`
- `persisted user token is invalid or expired`

Check the configured `github:` block, the `HIVE_GITHUB_TOKEN` secret/env var, or the GitHub App `app_id`, `installation_id`, and `key_file`. For App setup, use the dashboard banner or `/gh-setup`; details are in [GitHub App setup](github-app-setup.md).

## Agents are stuck, paused, or need CLI login

Agents run in tmux sessions named `hive-<agent>` managed by the v2 agent manager. There is no v1 `AGENT_READY_MARKER` or `bin/supervisor.sh` loop: the manager drives each agent by delivering a *kick* (its next work prompt) directly into the session and, once running, auto-dismisses the CLI's own startup consent screens.

The login detector (`scanForLoginRequired` in `src/cmd/hive/main.go`) scans recent tmux pane output for the regexes in `governor.sensing.login_patterns`. On a match it logs `login required detected`, pauses the agent, and sends a high-priority notification telling you to attach to `hive-<agent>` and run that backend's login command.

Useful checks from inside the Hive container/pod:

```bash
tmux ls
tmux capture-pane -t hive-scanner -p -S -80
tmux attach -t hive-scanner
```

Detach from tmux with `Ctrl+B`, then `D` — the session keeps running.

To recover a paused agent:

1. Complete the CLI login for that backend outside the pause. Attach to the session (`tmux attach -t hive-<agent>`) and run the login command shown in the notification: `claude login`, `copilot auth login`, `gemini auth login`, or the backend-specific command. The picker expects an interactive OAuth/browser flow, so complete it from a terminal you control rather than leaving the unattended session blocked on it.
2. Resume the agent from the dashboard, or `POST /api/resume/{agent}`.

If an agent returns to "needs login" immediately after resuming, the credentials themselves are the problem (expired token, revoked API key, or an account-level sign-out). Re-authenticate that backend's CLI as the agent user, then resume again so the fresh session is picked up.

## A permission prompt seems to block an agent

On the containerized runtime you normally do **not** need to configure an auto-approve phrase. Claude Code agents launch with `--dangerously-skip-permissions` (`src/pkg/agent/manager.go`), and the manager's `dismissInferencePrompts` routine polls the pane and auto-dismisses the startup consent screens (the "Bypass Permissions mode" dialog, the custom-API-key prompt, and generic "Enter to confirm" menus) dynamically, without a hardcoded phrase list.

If an agent still looks wedged on a prompt, capture the pane (`tmux capture-pane -t hive-<agent> -p -S -80`) and check the `hive` logs. A prompt whose selected default is negative (for example "No, exit") is navigated away from before Enter is sent; a genuinely novel prompt that the routine does not recognize is the case to report, along with the captured pane text.

## Notifications (ntfy / Slack / Discord) never arrive

Notifications are sent in-process by the notifier (`src/pkg/notify/notify.go`), configured under the top-level `notifications:` block in `hive.yaml`, not by a systemd healthcheck timer:

```yaml
notifications:
  ntfy:
    server: https://ntfy.sh
    topic: my-hive-alerts
```

The Docker Compose `NTFY_SERVER` / `NTFY_TOPIC` environment variables are just passthrough for this block and, when set, override the YAML values. Fields are `notifications.ntfy.server` and `notifications.ntfy.topic` (both required); Slack and Discord use `notifications.slack.webhook` and `notifications.discord.webhook`. See [Notifications](notifications.md) for the full schema.

To debug:

```bash
# 1. Does the outbound path work at all? (posting to the same topic Hive uses)
curl -d "test" "https://ntfy.sh/my-hive-alerts"

# 2. Is the block actually loaded? Grep the running config / logs.
kubectl -n hive logs deploy/hive | grep -i notif
```

A blank or misspelled `topic` is the most common cause of "posted but nothing arrives." Note that a notification only fires on an actual event — budget warning/exhausted, an SLA breach, a login-required detection, or a fix-loop escalation — so silence when nothing is wrong is expected. There is no periodic "log went stale" ping in v4.

## An agent writes a heartbeat but its work is obviously broken

Liveness is judged by the governor's in-process health check, so an agent that keeps its session alive but silently no-ops its actual work can still look healthy. Two defensive habits:

1. **Read the work counts, not just liveness.** If an agent reports "Issues triaged: 0" cycle after cycle in the logs, that is the signal — `kubectl -n hive logs deploy/hive | grep <agent>` or attach to the session.
2. **Cross-check an external surface.** Confirm the effect the agent is supposed to produce (a GitHub API query for the PRs/issues it claims to have handled) rather than trusting its self-reported state.

## Switching an agent's model

Model selection is a per-agent config field (`agent.<name>.model`), applied at **launch time** as the CLI's `--model` flag (`src/pkg/agent/manager.go`). There is no live in-session `/model` slash command sent over tmux; changing the model means changing the config and relaunching the agent. Do this through the supported paths, which handle the restart for you:

```bash
# CLI
hivectl agent model-set <agent> <model>
```

Or from the dashboard (which calls `POST /api/model/{agent}/{model}`). Both persist the new value, mark it operator-owned, and restart the agent session so the new `--model` takes effect (`handleModelSet` in `src/pkg/dashboard/api.go`).

Two gotchas:

- **Slug spelling is backend-specific.** The Claude CLI expects hyphens (`claude-opus-4-8`); Copilot uses dots (`claude-opus-4.8`). A mis-normalized slug is rejected by that backend. The dashboard offers candidate ids from live `/v1/models` discovery, with a static fallback list in `src/pkg/dashboard/cli_models.go`.
- **A change that is not operator-owned reverts on restart.** If the model appears to "come back" to the pack default, it was set through a path that did not mark it operator-owned. The `hivectl` and dashboard routes above set operator ownership precisely to prevent that reconciliation.

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

## Clean reset

There is no `uninstall.sh`/`install.sh` on the containerized runtime. To start from a clean state, remove the process and its `/data` state, then bring it back:

```bash
# Docker Compose (deletes the named /data volume — see Backup & restore first)
docker compose down -v
docker compose up -d

# Kubernetes (deletes the PVC-backed state)
kubectl -n hive delete deploy/hive
kubectl -n hive delete pvc -l app.kubernetes.io/name=hive
# then re-apply the manifests
```

`/data` holds the dashboard config overlay, persisted tokens, logs, and other state; deleting it discards dashboard edits and cached credentials. Back up first if you need any of it — see [Backup & restore](backup-restore.md).
