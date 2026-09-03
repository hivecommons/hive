# Hive troubleshooting

This guide covers the current containerized Go service (branch `v4`; code under the `src/` directory). Hive runs as an in-process supervisor: the governor loop, health checks, login detection, and notifications all run inside the `hive` process — there is no separate systemd supervisor, `bin/supervisor.sh` loop, `hive-healthcheck.service` timer, or `/etc/hive/agent.env` file. Those, along with `AGENT_LOOP_PROMPT`, `AGENT_READY_MARKER`, `AGENT_AUTO_APPROVE_PHRASE`, and `install.sh`/`uninstall.sh`, belong to the legacy v1 host install and do not apply here.

Start most investigations from the dashboard and the service logs, then drop to the sections below for a specific failure mode.

## Find the running service logs

Docker Compose deployments define `hive` and `gateway` services in `src/docker-compose.yaml`. The Hive process healthcheck calls **both** of the container's listeners — `http://127.0.0.1:3002/api/health` (the Go API) and `http://127.0.0.1:3001/api/health` (the Node auth proxy the gateway actually reaches). The gateway publishes `3001` and proxies to Hive. Start with:

```bash
docker compose -f src/docker-compose.yaml ps
docker compose -f src/docker-compose.yaml logs hive --tail=200
docker compose -f src/docker-compose.yaml logs gateway --tail=100
```

Kubernetes deployments use namespace `hive`, Deployment `hive`, Service `hive`, and probes on `/api/health` and `/api/livez` in `src/deploy/k8s/deployment.yaml`:

```bash
kubectl -n hive get deploy,pod,svc,pvc
kubectl -n hive logs deploy/hive --tail=200
kubectl -n hive describe pod -l app.kubernetes.io/name=hive
```

Podman (Quadlet) deployments run the same two containers as **systemd units**: `hive.service` and `hive-gateway.service`, generated from `src/deploy/quadlet/` (plus `hive-network.service` and `hive-data-volume.service`). Ask systemd first, not `podman logs`:

```bash
# rootless — drop --user for a rootful install
systemctl --user status hive.service hive-gateway.service
journalctl --user -u hive.service -n 200 --no-pager
journalctl --user -u hive-gateway.service -n 100 --no-pager
```

**Why the unit and not the container.** `hive.container` sets `Notify=healthy`, so the unit stays in `activating` until `/api/health` answers — the unit's state, not the container's, is what tells you whether Hive is *serving*. And a start that times out is deleted: the generated `ExecStart` carries `--rm`, so a failed start leaves no container for `podman logs` to read, while the journal still holds why. `podman ps` / `podman logs hive` remain useful once the unit is `active`, for output the process wrote after it came up.

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

When no token or App credentials are usable, Hive starts the dashboard but disables write-capable GitHub work. The code logs these exact messages from `src/cmd/hive/main.go` depending on the state:

- `no GitHub token configured (set github.token or github.app_id in config) — starting in dashboard-only mode`
- `GitHub App configured without credentials — hive starting in dashboard-only mode. Install the app and provide installation_id + key to enable agents.`
- `persisted user token is invalid or expired`

Check the configured `github:` block, the `HIVE_GITHUB_TOKEN` secret/env var, or the GitHub App `app_id`, `installation_id`, and `key_file`. For App setup, use the dashboard banner or `/gh-setup`; details are in [GitHub App setup](github-app-setup.md). Note the dashboard calls this the **Forge App** — the app for your forge (your source control system, e.g., GitHub, GitHub Enterprise, GitLab, or Gitea) — under Governor Config → Forge App. GitLab, Gitea, and Forgejo are **not supported for running a hive** today; see [Forge setup: GitLab, Gitea, and Forgejo](forge-app-setup.md).

## Hosted hive disappeared or its URL times out

Hosted hives that never complete setup or go inactive are **reaped on a timer**: the hive vanishes from the hub's Usage view and the old `https://<id>.hive.kubestellar.io` URL times out permanently. This is expected reclamation, not an outage. Recovery:

1. **Request a new hive** from the hub's `/get-started` wizard (hosted hub: `https://hive.kubestellar.io/get-started`, the **Request a hive** button). The old URL will not come back.
2. **Install the Forge App immediately** on the new hive — the GitHub App on GitHub.com, or the same app on your GHE host for enterprise. See [GitHub App setup](github-app-setup.md) and the [getting-started guide's Step 0](getting-started.md#step-0--before-you-start-do-this-first).
3. An installed Forge App plus regular heartbeats keeps the new hive from being reaped again.

On GitHub Enterprise, a 404 from the install link usually means the hive is pointed at github.com instead of your GHE host (or vice versa) — check which source control host is configured under Governor Config → Forge App.

## Agents are stuck, paused, or need CLI login

### Start on the agent card, not in tmux

Since v4.1.0 ([#5594](https://github.com/hivecommons/hive/issues/5594)) the dashboard agent card states **every** reason an agent will not run at once, so read it before reaching for `tmux`. Under the card state (and on the ops-center detail panel) is a blockers line with three segments:

- **`session`** — `up` (live tmux session), `down` (no live session; `↻ restart` asks the supervisor to respawn it), or `disabled` (disabled in config; the governor never starts it — enable it with ⚙️).
- **`scheduling`** — the governor cadence for the current mode when the agent is kickable, or **every** live reason it is not, joined together (for example `paused + off in surge mode`). All of the listed reasons must be cleared; fixing one is not enough. On-demand agents show `on demand`.
- **`next kick`** — an ETA (`in 12m`, `due now`) rather than a wall-clock time, or `never` while any scheduling blocker exists.

Two more card behaviors remove the old one-reason-per-click treasure hunt:

- **Zero cadence is named.** An enabled, governor-kickable agent with no cadence in *any* mode that has never been kicked shows a `⏱ never scheduled — set cadences` chip; clicking it opens the agent's Cadences tab. This is the per-agent form of the fleet-level "never kicked" banner — both are driven by the same predicate, so they cannot name different agents.
- **A paused agent's primary action is always its pause toggle.** With a live session the button is `▶ resume`, and its tooltip names anything resuming will *not* clear. If the session is also down, the one button reads `▶ start & resume` and clears both flags in a single click — client-side chaining of the two existing endpoints (`POST /api/resume/{agent}` first, so the fresh session is never born paused, then `POST /api/restart/{agent}`). No new API surface; scripts can chain the same two calls. A paused agent never offers a bare Start.

If the card says the agent should be running (session up, scheduling shows a cadence, next kick has an ETA) and it still misbehaves, *then* drop into the session as described below.

### Inspecting the session

Agents run in tmux sessions named `hive-<agent>` managed by Hive's agent manager. There is no v1 `AGENT_READY_MARKER` or `bin/supervisor.sh` loop: the manager drives each agent by delivering a *kick* (its next work prompt) directly into the session and, once running, auto-dismisses the CLI's own startup consent screens.

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
2. Resume the agent from the dashboard, or `POST /api/resume/{agent}`. If the pause outlived its session, the card offers a single `▶ start & resume` instead — see [Start on the agent card, not in tmux](#start-on-the-agent-card-not-in-tmux).

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

## An agent session completes but no branch or PR appears

The agent ran, the session ended cleanly, the fleet view shows it healthy — and there is no PR and no branch on the remote. Often the agent's own summary says so plainly, in words like "branch committed locally but push failed due to git authentication issue."

This is not the agent deciding no work was needed. The work was done; it could not be published. The fleet view cannot tell you which, because from the governor's point of view the session *is* healthy — the agent hit an auth error, correctly refused to manipulate git credentials, and wrote an honest summary. See [kubestellar/hive#5343](https://github.com/hivecommons/hive/issues/5343).

There are two distinct causes with the same symptom, and they are distinguishable.

### First: confirm the work exists and is unpublished

```sh
# Does the branch exist on the remote at all?
gh api "repos/<owner>/<repo>/git/ref/heads/<branch>" 2>&1 | head -3

# Did the agent commit it locally? (per-agent HOME, not the dev user's)
ls -d /data/home/agents/<agent>/* 2>/dev/null
```

A 404 from the first command plus commits in the agent's working copy is this scenario. If the branch *is* on the remote, the problem is downstream — go to [`hive-open-pr`](hive-open-pr.md#diagnosing-a-pr-request-that-never-opens) instead.

### Cause 1 — the credential helper is not reachable from the agent's UID

The helper is invoked per-UID, and agents do **not** share the dev user's `$HOME`: each per-agent UID runs with its own `$HOME` under `/data/home/agents/<name>`, which has no `.gitconfig`. `git config --global` writes to the *caller's* `$HOME`, so wiring the helper that way makes it invisible to every agent. The helper is therefore wired **system-wide in `/etc/gitconfig`**, written from the entrypoint's root phase — see [#5343](https://github.com/hivecommons/hive/issues/5343) for the original defect and [#5352](https://github.com/hivecommons/hive/pull/5352) for the fix.

Check the layer that actually matters, from a process with no per-user config — this is the same probe the entrypoint runs at boot:

```sh
# Inside the hive container. Empty output = the helper is invisible to agents.
HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent \
  git config --get-regexp '^credential\.' | grep git-credential-hive.sh

# Or ask as the agent UID directly:
su -s /bin/sh hive-<agent> -c 'git config --get-regexp credential'

# The file that supplies it — must exist and be world-readable (0644).
ls -l /etc/gitconfig
```

Each should list `/usr/local/bin/git-credential-hive.sh`. The boot log records the same verdict, so you can also just read it back:

```sh
kubectl -n hive logs deploy/hive | grep 'git credential helper'
```

- `git credential helper VERIFIED reachable without a per-user .gitconfig` — this cause is ruled out. Go to cause 2.
- `WARN: git credential helper is NOT reachable ...` — this is your cause. Every agent on this hive will commit branches it cannot push. Restart the hive so the entrypoint's root phase rewrites `/etc/gitconfig`; if that phase never ran (a boot that could not become root), only the dev user's global config exists and no agent will ever push.

Also confirm the agent's own scoped token is present and readable by its UID — the helper needs it:

```sh
su -s /bin/sh hive-<agent> -c 'test -r "$HIVE_AGENT_TOKEN_CACHE" && echo readable || echo MISSING'
```

Never print the file's contents.

### Cause 2 — the credential went stale mid-task (silent refresh failure)

Contributor-relay tasks are pushed with a scoped token the hub re-mints periodically, a few minutes before its TTL expires. When a re-mint **fails**, the hub logs a warning and keeps the old token; the relay is told nothing. See [kubestellar/hive#5447](https://github.com/hivecommons/hive/issues/5447).

The signature is different from cause 1, and it is a timing signature:

| | Cause 1 (helper unreachable) | Cause 2 (refresh failed) |
| --- | --- | --- |
| Which agents | **all** agents on the hive | usually one long-running task |
| When the push fails | the **first** push of any task | roughly an hour in, after earlier pushes in the *same* task succeeded |
| Boot-log probe | `WARN: ... NOT reachable` | `VERIFIED reachable` |
| Where it is recorded | entrypoint boot log | hub log only — nothing agent-side |

So: **a task whose earlier pushes worked and whose later ones did not is cause 2, not cause 1.** Confirm from the hub log:

```sh
kubectl -n hive logs deploy/hive | grep -iE 'token.*(refresh|mint)'
```

A short task that never pushes successfully at all is cause 1.

### If neither fits

Read what the PR-request watcher itself concluded. It probes the repository and then the head ref before blaming a push, and writes its verdict to the request's result file — the shapes and where to find them are in [`hive-open-pr`](hive-open-pr.md#diagnosing-a-pr-request-that-never-opens).

```sh
kubectl -n hive logs deploy/hive | grep 'pr-request watcher'
```

## An agent says "Please run /login" but logging in changes nothing

Check whether the same line carries **`API Error: 403`**. If it does, the agent is
**already logged in** and `/login` cannot help.

Claude Code prefixes *every* API error with its login hint, so an upstream refusal
renders like this:

```
● Please run /login · API Error: 403 {"type":"error","error":{"type":"api_error",
  "message":"inference backend returned 403: {"error":{"message":"team not allowed
  to access model. This team can only access models=['gemini-2.5-pro',
  'gcp/gemini-3.1-pro-preview', 'aws/claude-sonnet-4-6', ...]"}}
```

The status is the tell:

| status | meaning | fix |
| --- | --- | --- |
| **401** | authentication — the caller is not identified | `/login` |
| **403** | authorization — the caller *is* identified and is not permitted | not `/login` |

The usual cause on an inference backend (`litellm`, `vllm`, `llm-d`) is that the
agent's **model id does not match what the gateway entitles**, exactly. Read the
allowed list out of the error and compare it character by character with the
agent's configured model — separators and prefixes both matter:

```
configured:  claude-sonnet-4.6
entitled:    aws/claude-sonnet-4-6
                 ^^^^         ^
```

`.` versus `-`, and a missing `aws/` prefix, are each enough for a 403. For plain
`vllm`/`llm-d` backends hive passes model ids through **verbatim** on purpose — the
server is the only thing that knows its own naming — so fix drift there in the
agent's `model:` setting.

For **`litellm`** gateways this drift now self-heals
([#4400](https://github.com/hivecommons/hive/issues/4400)): hive learns the key's
entitled model set (from a key-info probe, or from the first team-scope 403 itself)
and, when the configured id differs from **exactly one** entitled id only by
separator (`.`/`-`), case, or provider-prefix drift, forwards that exact entitled id
instead. The first request after a fresh start may still 403 (that 403 is what
teaches the entitled set); subsequent requests resolve. An id that matches nothing —
or matches more than one entitled id — is still sent verbatim, so correct the
agent's `model:` setting to an id from the allowed list in that case.

**Why one agent fails and another with "the same" model does not:** they are not the
same string. Compare the two agents' configured `model:` values directly rather than
the model each was *meant* to use — a single separator differs and only one of them
matches the gateway. (Note the dashboard's model dropdown matches tolerantly, so both
agents can *display* the same selection while their stored ids differ.)

Before [#4400](https://github.com/hivecommons/hive/issues/4400) hive read that line as
a login prompt: it badged the agent 🔑, and — because a valid token was on disk —
auto-restarted it straight back into the same 403, which looked like the agent
crash-looping. Hive no longer treats a 403 as a login signal; a 401 still is.

## The terminal looks frozen — no new output, and reopening it doesn't help

You are almost certainly **scrolled back**, not looking at a halted agent.

The browser terminal is a live `tmux` attach, and the mouse wheel scrolls by entering tmux's copy-mode. **While a pane is in copy-mode it stops following live output.** Copy-mode is state held by the tmux server, not the browser, so closing the tab and reopening it re-attaches to the same scrolled-back pane and still shows nothing new — which is what makes it look like the agent died.

Look at the right-hand end of the status bar:

```
[SCROLLBACK 812/4837 lines back - not following live output - press q to resume]   now 14:22:07
[live]                                                                             now 14:22:07
```

Press **`q`** (or `Esc`) to leave copy-mode and resume following output.

**The timestamp there is a clock, not a content timestamp.** It is the time *now*, which is why it never lines up with any particular line of scrollback. The `812/4837 lines back` counter is your scroll position: how many lines back from live you are, out of the total retained history.

**If you still see an unlabelled black-on-yellow box in the top-right of the pane** (e.g. `12:41 [812/4837]`), your hub predates the fix that hides it. That box is tmux's built-in copy-mode marker: the `[812/4837]` is the same scroll position, and the time next to it is the moment the **top visible line** was written — a reference point nothing on screen points at, which is why it never seems to correspond to the top or the bottom consistently. Current hubs hide that marker and carry the position, labelled, in the status bar instead.

If the status bar shows `[live]` and output really has stopped, the agent is idle between kicks — check its next scheduled kick on the dashboard before assuming a fault.

## The dashboard says the next kick is later, but the agent is visibly working now

The agent-card **last kick** / **next kick** fields describe when work is *started*, not how long it runs. A kick sends one prompt into the agent's CLI; the resulting work pass then runs as long as it needs — often hours for a deep quality or scan pass. So an agent visibly busy at 01:47 with `last kick 8:12 PM` and `next kick 2:12 AM` is not off schedule: it is still working through the pass that began at 20:12. (These fields were labelled "last run" / "next run" before [#4399](https://github.com/hivecommons/hive/issues/4399), which invited exactly this misreading.)

Every kick path — scheduled cadence, manual restart, crash-resume, CEL event triggers — records itself in `last kick` and the 🕘 *past kicks* archive, so a timestamp that has *not* moved is positive evidence that no new kick happened.

## An agent runs, but not the way I expect — why did it do that?

Agents are told to act, not narrate: every policy carries an "Output Rules —
Terse Mode" block, and on inference backends the agent manager appends an
explicit `EXECUTE, DO NOT NARRATE` instruction to each kick. That rule earns
its keep — weak models otherwise answer a kick with a plan for someone else to
run — but it means the log shows *what* an agent did and never *why*, which is
the one thing you need when the behaviour is wrong rather than absent.

**Turn on explain mode for that one agent.** It asks the agent to record the
reason for each tool call without relaxing the rule for anything else:

```yaml
agents:
  scanner:
    backend: claude
    explain_mode: brief     # off | brief | full
```

`brief` adds one `EXPLAIN:` line before each tool call. `full` adds a closing
block covering the goal as understood, the approach chosen, and the
alternatives rejected. To turn it on for every agent at once, set the hive-wide
default in Settings → Governor → General (`governor.explain_mode` in
`hive.yaml`); `HIVE_EXPLAIN_MODE` on the deployment is the fallback when that is
unset. Either applies only to agents that leave the field unset.

The explanation lands in the agent's ordinary log behind an `EXPLAIN:` prefix,
so it is a read-time choice rather than a second stream:

```sh
# Just the reasoning
curl -s "$HIVE/api/agents/<name>/log?explain=only"

# The log as it would read with explanation off
curl -s "$HIVE/api/agents/<name>/log?explain=hide"
```

`grep EXPLAIN:` works the same way on a log you have already downloaded.

Two things worth knowing before you reach for it:

- **It costs tokens on every kick**, which is why it is per-agent and off by
  default. Turn it on for the agent you are debugging, not for the fleet.
- **The agent still has to act.** A response containing only explanation is a
  failure, and the prose-only watchdog still fires — explain mode does not
  license narrating instead of working.

Full reference, including the tri-state inheritance rules:
[agent-configuration.md](agent-configuration.md#explain-mode-debugging-agent-behaviour).

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
curl -fsS http://127.0.0.1:3002/api/health   # the Go API
curl -fsS http://127.0.0.1:3001/api/health   # through the auth proxy — what the gateway reaches
curl -fsS http://127.0.0.1:3002/api/livez
```

Run both of the first two. They are the two halves of the container health probe, and they fail independently: a hive whose auth proxy refused to start answers the first and refuses the second ([#4476](https://github.com/hivecommons/hive/issues/4476)). Neither needs a credential — only mutating methods are authenticated.

`/api/livez` is deliberately process-focused: the Kubernetes manifest notes that stale hub heartbeat state belongs in deeper health reporting and should not crash-loop a healthy pod.

## Podman (Quadlet) deployments: failure modes Docker does not have

These are specific to running Hive as systemd units. Everything else in this guide applies unchanged; the install-side counterpart is the **Traps** section of [podman-standalone-quadlet.md](podman-standalone-quadlet.md).

Commands are shown rootless. For a rootful install drop `--user` from `systemctl`/`journalctl`, and read `%E/hive` as `/etc/hive` rather than `~/.config/hive`. The examples below use `$CONF` for that configuration directory:

```bash
CONF=~/.config/hive     # rootful: CONF=/etc/hive
```

### It will not start at all

Run the preflights before reading anything else — they diagnose most of the causes, and they are read-only:

```bash
export HIVE_DEPLOY_RUNTIME=podman   # WITHOUT this they exit 0 having checked nothing
bin/hive-podman-preflight.sh        # engine, root mode, cgroups
bin/hive-podman-preflight-ids.sh    # subordinate IDs, graphroot, networking
HIVE_SRC_DIR="$CONF" bin/hive-podman-preflight-host.sh   # SELinux labels, config, secrets, port 3001
```

Docker is the default runtime, so with `HIVE_DEPLOY_RUNTIME` unset all three print `Podman preflight: skipped` and return success — which reads exactly like a pass.

### `systemctl start` hangs, then fails five minutes later

The unit sits in `activating` for the whole `TimeoutStartSec` (300s for `hive.service`, 120s for the gateway) and then gives up. `Notify=healthy` is doing its job: it holds the unit until the healthcheck passes, so a healthcheck that will never pass costs the full budget in silence.

The measured cause ([#4367](https://github.com/hivecommons/hive/issues/4367)) is a **port mismatch between the config and the unit's `HealthCmd`**: the unit's first probe is `http://127.0.0.1:3002/api/health`, while `src/hive.yaml.example` ships `dashboard.port: 3001` for local source runs. Install the example unchanged and Hive serves on 3001, the probe never answers, and `--rm` deletes the container that held the evidence.

```bash
grep -A1 '^dashboard:' "$CONF/hive.yaml"    # must be 3002, or absent (3002 is the default)
journalctl --user -u hive.service -n 100 --no-pager
```

The other frequent cause is a missing `HIVE_DASHBOARD_TOKEN` in `%E/hive/hive.env`. The Node auth proxy refuses to start without one unless the hive is hub-hosted, and `Notify=healthy` turns that refusal into the same silent `activating` wait. The journal carries the `[SECURITY]` line:

```bash
journalctl -u hive.service | grep '\[SECURITY\]'
```

**This one used to look different, and older notes may still describe it that way.** Until [#4476](https://github.com/hivecommons/hive/issues/4476) the probe read the Go API alone, so `hive.service` went `active` with its container `healthy` while the proxy was dead; the only red arrived 120s later on `hive-gateway.service`, as an nginx `connect() failed (111: Connection refused)` naming neither the port nor the variable. The probe now covers both listeners, so the wait and the journal line are on the same unit.

### `systemctl is-failed` says `activating`, not `failed`

Do not key monitoring on `failed` for these units. Measured in [#4378](https://github.com/hivecommons/hive/issues/4378): `Restart=always` moves the unit from a `TimeoutStartSec` expiry straight to `activating/auto-restart` and into the next attempt, so `is-failed` reports `activating` at every point during a bad update and `ActiveState` never reaches `failed`. An alert keyed on `failed` does not fire.

What does move is `Result=timeout` during the auto-restart window, and `NRestarts` climbing — though not immediately, so a single sample that reads `NRestarts=0` has not shown the unit is healthy:

```bash
systemctl --user show hive.service -p ActiveState -p SubState -p Result -p NRestarts
```

For boot persistence specifically, `systemctl is-enabled` is not evidence either — it reports `generated` regardless. Use `bin/hive-podman-lifecycle-probe.sh check`; see [podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md).

### `systemctl cat hive.service` does not show my drop-in

Expected. Quadlet merges `hive.container.d/*.conf` into `hive.container` **before** generating the service, so `systemctl cat` prints the merged result and never names the drop-in file that produced it. To see what is actually in force, read the generated `ExecStart` and the drop-in directory separately:

```bash
systemctl --user cat hive.service | grep -m1 '^ExecStart='
ls -l ~/.config/containers/systemd/hive.container.d/
```

This matters most when an image pin is not taking effect — [podman-quadlet-update-rollback.md](podman-quadlet-update-rollback.md) covers the pin, and `bin/hive-podman-update.sh status` prints the pinned digest, the running container's digest, and the unit state together.

### SELinux denials on `/data` or the secrets directory

On an enforcing host a mislabelled bind mount fails in ways that do not name SELinux — and a private-category denial records **no AVC at all**. The bind mounts carry `:Z` and the named volume deliberately carries **no** relabel suffix; adding `:Z` to the volume is the trap.

Do not weaken SELinux to test the theory. The measured behaviour, including the restore, is in [podman-selinux-avc-evidence.md](podman-selinux-avc-evidence.md) and [podman-volume-persistence.md](podman-volume-persistence.md); `bin/hive-podman-preflight-host.sh` reports the labels and the secrets group-traverse check directly.

### `tar: can't open '/backup/…': Permission denied` restoring or migrating an archive

Rootless, and the archive was written by a root process — anything from Docker's daemon, or anything taken under `sudo`. The `:z` on the `/backup` mount is supposed to relabel it to `container_file_t`, but relabelling needs ownership or `CAP_FOWNER`, and a rootless user has neither over a root-owned file. Podman skips it without saying so and the container is denied. The file's mode is a red herring: it is usually `0644` and perfectly readable from your own shell.

`sudo chown "$(id -u):$(id -g)" <archive>` and re-run the identical command. Not `chmod 777`, which changes nothing here, and not `setenforce 0`. Full mechanism and the Docker → Podman procedure it belongs to: [backup-restore.md](backup-restore.md#docker--podman-migration).

### Auto-update rolled back, or is not updating

Auto-update is opt-in and off by default. If it rolled back, the published image is bad and **will be retried on the next timer firing** — the unit's own state stays green throughout. If it reports success but changes nothing, a digest pin is probably in force. Both are covered in [podman-auto-update.md](podman-auto-update.md).

## Clean reset

There is no `uninstall.sh`/`install.sh` on the containerized runtime. To start from a clean state, remove the process and its `/data` state, then bring it back:

```bash
# Docker Compose (deletes the named /data volume — see Backup & restore first)
docker compose -f src/docker-compose.yaml down -v
docker compose -f src/docker-compose.yaml up -d

# Kubernetes (deletes the PVC-backed state)
kubectl -n hive delete deploy/hive
kubectl -n hive delete pvc -l app.kubernetes.io/name=hive
# then re-apply the manifests

# Podman / Quadlet (rootless; drop --user for a rootful install)
systemctl --user stop hive-gateway.service hive.service
#  ⚠ DESTRUCTIVE — this line deletes all persisted Hive state. Everything
#    else here is reversible; this is not. Back up FIRST (see below).
podman volume rm hive-data
systemctl --user start hive-gateway.service
```

The Podman volume removal is the counterpart of `down -v`, and it is separated onto its own line for the same reason: stopping and starting the units is routine, and `podman volume rm hive-data` is not. `systemctl stop` alone does **not** delete the volume — the units can be stopped and started freely without losing state. To remove the units and every labelled resource as well, use [`bin/hive-podman-teardown.sh`](../../bin/hive-podman-teardown.sh), which selects by the `io.kubestellar.hive.*` ownership labels.

`/data` holds the dashboard config overlay, persisted tokens, logs, and other state; deleting it discards dashboard edits and cached credentials. Back up first if you need any of it — see [Backup & restore](backup-restore.md).

### "backup encryption key is not configured on this hive, so a backup would be unencrypted; refusing"

Expected, and deliberate: the archive carries this hive's GitHub App private keys, so hive refuses to build one without a key. Set one in **Governor Config → Security → Backup → Set key** (`openssl rand -hex 32`) and retry — no deployment or cluster access is needed. `HIVE_BACKUP_KEY` on the deployment still works as a fallback for self-hosted hives. Escrow the key: it is not inside the archive, so a backup without it cannot be restored.
