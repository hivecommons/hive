# ClankeR contributor relay

ClankeR lets a contributor lend their local AI CLI subscription to a hive. The relay connects to `/contribute`, receives one task at a time, runs the selected CLI in the contributor's environment, and reports completion/PR metadata back to the hive.

## Basic setup

From a checkout of this repository:

```bash
export HIVE_HUB=wss://hive.example.com/contribute
just contribute-setup claude
just contribute-hive
```

`compose-contributor.yaml`, `Dockerfile.contributor`, and the `just contribute-hive` recipe are the reference container path. Native mode is available through `just contribute-hive <backend> local` when a container runtime is not desired.

## Docker Compose workflow

The containerized path is `v2/compose-contributor.yaml` plus `v2/Dockerfile.contributor`; the `just contribute-hive` recipe wraps it. From the repository root you can also run Compose directly after `just contribute-setup` has written `${HOME}/.config/hive/contributor.env`:

```bash
export AGENT_BACKEND=claude
docker compose -f v2/compose-contributor.yaml up --build
```

The compose file mounts local contributor state read-only into the container:

- `${HOME}/.config/hive` for Hive registration/config.
- `${HOME}/.claude` and `${HOME}/.config/claude-code` for Claude-family CLI auth.

Important environment variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `HIVE_HUB` | value from `contributor.env`, else public hub default | WebSocket hub(s) to subscribe to. Use comma-separated URLs for multi-hub mode. Direct Compose reads the registered value from the mounted config file. |
| `HIVE_REGISTRATION_TOKEN` | value from `contributor.env` | Registration token(s), positional with `HIVE_HUB` when multiple hubs are listed. Required; run `just contribute-setup` / `just contribute-register` first. |
| `AGENT_BACKEND` | `claude` | CLI/backend to run (`claude`, `copilot`, `goose`, `bob`, `codex`, `litellm`, etc., depending on image support and credentials). |
| `AGENT_MODEL` | unset | Optional model override passed to the contributor agent. |
| `CONTRIBUTOR_MODE` | `interactive` | `interactive` keeps a tmux/TTY session. `headless` is for one-shot/no-TTY task delivery. |

To change hubs for direct Compose, re-run the registration/setup flow for the target hub or edit `${HOME}/.config/hive/contributor.env` so `HIVE_HUB` and `HIVE_REGISTRATION_TOKEN` stay matched.

Backend credentials stay local to the contributor container. For example, `AGENT_BACKEND=bob` needs `BOBSHELL_API_KEY` in the container environment, while LiteLLM-style backends need their endpoint/key variables. Use `just contribute-check <backend>` before registering to catch missing CLIs or obvious auth gaps.

## Multi-hub subscription

A single relay can subscribe to multiple hives. Register with each hive first, then provide matching comma-separated lists:

```bash
export HIVE_HUB='wss://hive-a.example.com/contribute,wss://hive-b.example.com/contribute'
export HIVE_REGISTRATION_TOKEN='token-from-hive-a,token-from-hive-b'
just contribute-hive
```

The lists are positional: the first token belongs to the first hub, the second token belongs to the second hub, and so on. If the counts differ, the relay refuses to start rather than sending a token to the wrong hub.

The relay keeps a WebSocket and heartbeat for each subscribed hub, but shares one CLI/tmux session and works on only one task at a time. It rotates to another hub when the active hub has no assignable work. A task that is blocked on human action stays with its owning hub; the relay does not mix task state across hubs.

## Acting as a spoke agent role

Set `HIVE_AGENT_ROLE` to request a delegated role, or use the **Acting as** control in `/contribute` where available:

```bash
export HIVE_AGENT_ROLE=quality
just contribute-hive
```

The hive may override the request with an owner-assigned role. See [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md) for tier, grant, and allow-list requirements.


## Kubernetes contributor workload

`just contribute-k8s` emits a complete Kubernetes workload for a long-lived contributor relay: Namespace, ConfigMap, Secret, and Deployment. It prints YAML to stdout by default, or writes a file when an output path is supplied:

```bash
just contribute-setup claude
just contribute-k8s                          # default namespace hive-contributor
just contribute-k8s my-namespace relay.yaml  # write a manifest
just contribute-k8s my-namespace relay.yaml v2  # pin image tag
kubectl apply -f relay.yaml
kubectl -n my-namespace rollout status deploy/hive-contributor
```

The generated pod sets `CONTRIBUTOR_MODE=headless` because Kubernetes pods have no TTY; interactive tmux mode would stall. Headless mode is currently verified for `claude`, `litellm`, `copilot`, `codex`, and `goose`. The Deployment has one replica per registered contributor identity and uses readiness/liveness probes that read the relay's headless status file (`waiting`, `working`, `done` pass; missing/failed state fails).

The generated Secret contains the registration token and `GH_TOKEN` as Kubernetes Secret data. Treat it as sensitive cluster-readable material and prefer a pinned image tag/digest for repeatable operation.

Fixes #3024.
