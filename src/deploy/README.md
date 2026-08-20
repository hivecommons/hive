# v2 deployment scripts

This directory contains Kubernetes/LXC manifests plus runtime helper scripts used by the Hive container. The dashboard terminal pane uses two small helpers that are easy to miss:

## Dashboard TTY helpers

### `ttyd-tmux.sh`

`ttyd` serves the browser terminal websocket, but agent tmux sessions may be owned by per-agent UIDs. Because tmux only lets the socket owner attach, `ttyd-tmux.sh <session>` finds the matching `/tmp/tmux-*/<session>` socket, derives its numeric UID/GID, and uses `su-exec` to attach as that owner. While attached it enables tmux mouse mode (so the scroll wheel drives copy-mode scrollback instead of just reflowing the viewport — issue #3694) and raises the session `history-limit` (default `50000`, override with `HIVE_TTYD_HISTORY_LIMIT`) for panes created later in the session. Both options are restored to their previous values on detach. Note that tmux reads `history-limit` at pane creation, so the attach-time raise cannot deepen an already-created pane — the authoritative deep-scrollback depth is set when the agent manager creates the session (`newSessionCommands` in `src/pkg/agent/manager.go`, default `50000`, override with `HIVE_TMUX_HISTORY_LIMIT`).

Use it indirectly through the dashboard terminal link. If a terminal pane says no tmux socket was found, check that the agent session name exists and that the socket is present under `/tmp/tmux-*` in the container.

### `hive-panes.sh`

`hive-panes [lines]` prints the last N raw-output lines from every other agent's pluk JSONL log in `/var/run/pluk/logs`. It skips the caller named by `HIVE_PROXY_AGENT`, strips ANSI/control sequences, and never attaches to another tmux session. Agents can run it for read-only peer awareness when diagnosing fleet activity. See [Agent peer-awareness logging](../docs/agent-logging.md) for what pluk is, the JSONL format, and when logs are (or aren't) available.

## Other files

- `entrypoint.sh` — container startup, config layering, proxy/agent setup, and long-lived process supervision.
- `k8s/` — namespace, deployment, service, PVC, Secret, ConfigMap, and route/RBAC manifests.
- `inference/` — sample in-cluster OpenAI-compatible inference deployment and RBAC.
- `docker-compose.architect.yaml`, `hive-quickstart.yaml`, `hive-level*.yaml`, `architect-only.yaml`, `hive.yaml` — example deployment/configuration manifests.
- `blue-green-deploy.sh`, `bootstrap-lxc.sh`, `create-lxc.sh` — operational scripts for non-Kubernetes deployments.
- `test_*.sh` — shell tests for entrypoint/runtime deployment behavior.

## Deployment contract tests

Three guards cover different parts of the standalone stack and deliberately do
not overlap. All three run in CI and none of them starts a container.

| Test | Covers |
|---|---|
| `test_standalone_service_contract.sh` | The `hive` and `gateway` services: published-port boundary (3001 published, 7681 never), `NET_ADMIN`, `/data` on a named volume, health checks and readiness ordering, read-only config and secret mounts. |
| `test_watchtower_socket_contract.sh` | The opt-in auto-update profile: Docker-socket containment, profile gating, the proxy's ports/networks/API sections. |
| `test_supply_chain_pins.sh` | Build inputs: base-image and toolchain digest pins. |

```bash
bash src/deploy/test_standalone_service_contract.sh
bash src/deploy/test_watchtower_socket_contract.sh
bash src/deploy/test_supply_chain_pins.sh
```

`test_standalone_service_contract.sh` is also the snapshot the Podman runtime
work in #4188 has to keep passing: it is a focused subset, not a claim of
Docker/Podman parity, and further invariants belong in separate small issues.
