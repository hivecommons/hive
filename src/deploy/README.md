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
- `quadlet/` — Podman Quadlet units for the standalone deployment (ADR-0017). `hive.container` and `hive-data.volume` start the Hive service under `systemd`, rootful or rootless; `hive.network` and `hive-gateway.container` put the authenticating gateway in front of it with the same published-port split as the Compose stack — 3001 published, 7681 never. See [the operator guide](../docs/podman-standalone-quadlet.md). Docker Compose remains the default runtime and is unaffected.
- `probe_podman_volume_persistence.sh` — characterises the `hive-data` named volume under SELinux enforcing (#4376): its label and MCS category, the copy-up ownership change, survival across container removal and unit recreation, and the named-volume/bind-mount contrast. Needs an enforcing host and reports `78` rather than a vacuous pass elsewhere; its static section (no `:z`/`:Z` on the volume line, `:Z` on the config and secret bind mounts) runs anywhere. See [the characterisation](../docs/podman-volume-persistence.md).
- `test_*.sh` — shell tests for entrypoint/runtime deployment behavior.

## Deployment contract tests

Four guards cover different parts of the standalone stack and deliberately do
not overlap. All four run in CI and none of them starts a container.

| Test | Covers |
|---|---|
| `test_standalone_service_contract.sh` | The `hive` and `gateway` services: published-port boundary (3001 published, 7681 never), `NET_ADMIN`, `/data` on a named volume, health checks and readiness ordering, read-only config and secret mounts. |
| `test_watchtower_socket_contract.sh` | The opt-in auto-update profile: Docker-socket containment, profile gating, the proxy's ports/networks/API sections. |
| `test_supply_chain_pins.sh` | Build inputs: base-image and toolchain digest pins. |
| `test_quadlet_port_boundary.sh` | The Podman units in `quadlet/`: the same published-port boundary as the row above (gateway publishes 3001 and only 3001, no unit publishes 7681), the shared network both containers join, the gateway's `After=hive.service` ordering, and the network's `DisableDNS`/`Internal`/`IPv6` defaults. |

```bash
bash src/deploy/test_standalone_service_contract.sh
bash src/deploy/test_watchtower_socket_contract.sh
bash src/deploy/test_supply_chain_pins.sh
bash src/deploy/test_quadlet_port_boundary.sh
```

`test_standalone_service_contract.sh` parses `docker-compose.yaml` and
`test_quadlet_port_boundary.sh` parses the Quadlet units; neither can see the
other runtime's assets, which is why the boundary is asserted twice rather than
once.

`test_standalone_service_contract.sh` is also the snapshot the Podman runtime
work in #4188 has to keep passing: it is a focused subset, not a claim of
Docker/Podman parity, and further invariants belong in separate small issues.
