# Network and port requirements

This page is derived from the Go code under `src/` and the deployment manifests. Treat it as the operator-facing firewall matrix for standalone Docker Compose, Kubernetes spokes, and hub deployments.

## Inbound listeners

| Port | Where it binds | External? | Purpose |
| ---: | --- | --- | --- |
| 3001 | Docker gateway (`deploy/nginx.conf`); hub mode (`HIVE_MODE=hub`, `HIVE_HUB_PORT` override) | Yes for standalone dashboard or hub | Public dashboard front door, hub UI/API, `/contribute`, `/api/heartbeat`, SSE, and WebSocket proxy paths. |
| 3002 | Go dashboard in Kubernetes examples (`deploy/k8s/configmap.yaml` + Service) | Expose through Ingress/Route only | Dashboard/API process in Kubernetes. The Service targets 3002; TLS should terminate before it. |
| 7681 | `ttyd` and Docker stream proxy | Usually no; expose only to trusted operators | Web terminal for `/terminal`/tmux access. In Docker Compose it is published; in Kubernetes it is a Service port. |
| 18443 | Go process, loopback/agent namespace | No | MITM GitHub proxy used by iptables redirection for agent GitHub egress. Do not expose. |
| 18444 | Go process, loopback/agent namespace | No | Inference translator for OpenAI-compatible gateway backends. Do not expose. |
| 9000 | `cmd/apiproxy` default | Only if you run `apiproxy` separately | Standalone API proxy/debug tool, not part of the default hive container. |

Notes:

- `hive.yaml.example` defaults `dashboard.port` to 3001 for local/source runs. The Kubernetes ConfigMap sets it to 3002 because the cluster Service/Ingress owns the public edge. The Go default is 3002 (`defaultDashboardPort`), and the container deployments assume it: the Compose `hive` healthcheck and the Podman Quadlet unit's `HealthCmd=` both probe 3002 **first**, so a container install that keeps the example's 3001 never reports healthy — see [the Quadlet page](podman-standalone-quadlet.md#traps-measured-not-guessed). Both then probe 3001 as well, because the container serves on two ports and certifying only the Go API let a hive report healthy while the auth proxy on 3001 — the port the gateway dials — was refusing connections ([#4476](https://github.com/hivecommons/hive/issues/4476)). Only the 3002 half is coupled to `dashboard.port`; the 3001 half comes from `HIVE_PROXY_PORT`.
- Docker Compose publishes 3001 and 7681 from the `gateway` container and only `expose`s 3001/3002/7681 on the `hive` container.
- The hub is also a Go HTTP server on 3001 unless `HIVE_HUB_PORT` is set. Contributor relay defaults to `wss://hive.hivecommons.dev:3001/contribute`.

## HTTP/WebSocket paths that must pass through proxies

Reverse proxies, Routes, and Ingresses must preserve HTTP upgrade headers and long read timeouts for:

- `/api/events` — dashboard Server-Sent Events.
- `/api/contribute/ws` and hub `/contribute/ws` — contributor relay WebSocket.
- `/terminal` — ttyd WebSocket terminal.
- `/gh-setup` — GitHub App Setup URL callback.
- `/api/heartbeat` — spoke-to-hub heartbeat POSTs.

## Outbound egress

| Destination | Required by | Notes |
| --- | --- | --- |
| GitHub web/API host (`github.com`/`api.github.com` or configured GHE `base_url`/`api_url`) | Issue/PR enumeration, App token minting, git operations, `/gh-setup` verification | Agents may be routed through the local MITM proxy; the hive process itself also calls GitHub APIs. |
| Hub URL (`hub.url`) | Spokes | Heartbeats and callback polling. Must be reachable from firewalled spokes even when the hub cannot initiate inbound connections. |
| Model backends and CLI auth endpoints | Agents | Claude/Copilot/Codex/Antigravity CLIs and OpenAI-compatible gateways (`vllm`, `llm-d`, `litellm`, OpenRouter, custom). |
| Notification endpoints | Optional | `ntfy`, Slack webhooks, and Discord webhooks/bot API only when configured. |
| `discord.com` / Discord webhook hosts | Optional | Required when `notifications.discord.webhook`, `notifications.discord.factory_webhook`, or the Discord bot integration is configured. |
| Container/image registries | Deployment/auto-update | Pull hive images, watchtower updates, and any inference backend images. |
| npm/GitHub for caveman mode | Optional | `npx github:JuliusBrussee/caveman#...` runs when `caveman_mode` is enabled for supported backends. |
| Kubernetes API | In-cluster spokes/hub | Used for health/provisioning/route discovery when those features are configured. |
| Login provider issuers | Hub with multi-provider login | GitHub OAuth endpoints, plus the OIDC issuer/JWKS hosts for each configured provider (e.g. `accounts.google.com`, `login.microsoftonline.com`, `sso.redhat.com`, or your IBMid/custom issuer). |
| OCI Object Storage (`objectstorage.<region>.oraclecloud.com`) | Hub running DR backups | Only when the opt-in backup CronJob is deployed; see [backup-restore.md](backup-restore.md). |

## Egress through a corporate forward proxy

Some environments cannot reach a destination directly — the common case is an
enterprise GitHub instance that is only reachable through a corporate HTTP
forward proxy. Hive can chain selected hosts through such a proxy with
`CONNECT`, on every path that carries TLS egress:

- the control-plane GitHub clients (App JWT / installation-token minting, REST
  and GraphQL calls);
- in-process `git` runs (knowledge sources, ad-hoc worktree provisioning),
  expressed as scoped `GIT_CONFIG_*` entries;
- the in-process MITM proxy that all agent `:443` traffic is redirected into.
  Agents keep `HTTPS_PROXY=http://127.0.0.1:<mitm-port>`; the MITM proxy dials
  the upstream proxy for matching hosts and speaks TLS (inspected flows) or
  splices bytes (passthrough flows) over the tunnel. Agents are never handed
  the corporate proxy URL.

Configure it in `hive.yaml`:

```yaml
network:
  upstream_proxy:
    url: http://proxy.corp.example:3128      # userinfo → Proxy-Authorization: Basic
    hosts: ["ghe.corp.example", "*.ghe.corp.example"]
    no_proxy: ["10.0.0.0/8"]
```

or via environment (`HIVE_UPSTREAM_PROXY_URL`, comma-separated
`HIVE_UPSTREAM_PROXY_HOSTS`, `HIVE_NO_PROXY`), which overrides the file block.
When neither is set, standard `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` apply to
every host. `hosts` patterns: exact host, `*.domain` (subdomains only),
`.domain` (domain and subdomains), IP literal, CIDR, or `*`; empty means every
host not listed in `no_proxy`. Hosts outside the rules keep the direct path.
Limitation: for `git`, only exact-host patterns can be expressed as per-URL
`http.<url>.proxy` config; wildcard/suffix/CIDR rules apply to API and MITM
traffic but not to in-process `git` unless `hosts` is empty.

Hosted hubs set this per cluster in `/data/saas/clusters.json` so new pool
slots on that cluster are provisioned with the env vars above:

```json
{
  "id": "corp-cluster",
  "upstream_proxy": {
    "url": "http://proxy.corp.example:3128",
    "hosts": ["ghe.corp.example", "*.ghe.corp.example"]
  }
}
```

Existing hive Deployments are not reconciled; patch them once with
`kubectl -n <hive-ns> set env deploy/hive -c hive HIVE_UPSTREAM_PROXY_URL=... HIVE_UPSTREAM_PROXY_HOSTS=...`.

### GitHub Enterprise behind a TLS reverse proxy (hostAliases)

Some networks expose an enterprise GitHub not through a CONNECT forward proxy
but through a **TLS reverse proxy** pinned to a fixed IP: the proxy serves a
certificate valid for the GHE hostnames on :443 and forwards to GHE, while
`CONNECT` is rejected (e.g. openresty answering `405 Not Allowed`) and direct
egress to GHE times out. `upstream_proxy` cannot express that; the fix is a
Pod `hostAliases` entry that resolves the GHE hostnames to the proxy IP so TLS
SNI/verification and every path (API, git smart-HTTP, MITM) keep working.

Hosted hubs declare it per cluster in `/data/saas/clusters.json`:

```json
{
  "id": "corp-cluster",
  "host_aliases": [
    { "ip": "129.34.104.21", "hostnames": ["github.ibm.com", "api.github.ibm.com"] }
  ]
}
```

Entries are validated at load (the IP must parse, every entry needs at least
one hostname; hostnames are lowercased). An invalid `host_aliases` block is
logged and ignored — it never drops the cluster. New pool slots on the cluster
are provisioned with the matching `spec.template.spec.hostAliases`, and a
throttled reconcile sweep (at most 3 patches per 15 minutes, since each one
rolls that hive's pod) converges existing hives. Unrelated hand-applied
aliases for other IPs are preserved. Non-hub (standalone) deployments add the
same `hostAliases` block to their own pod spec.

## Firewall recommendations

- Public edge: expose only the dashboard/hub front door (3001 for Docker/hub, or your HTTPS Ingress/Route for Kubernetes). Keep 3002 and 7681 cluster/private unless you have an explicit operator access path.
- Internal-only: never publish 18443 or 18444 outside the pod/container network.
- Kubernetes: use a `ClusterIP` Service and terminate HTTPS at an Ingress/Route. Open 3002 from the Ingress/Route controller to the Service, and 7681 only if terminal access is required.
- Docker Compose: restrict published 7681 to trusted networks or remove that port mapping if operators do not need web terminal access.
- GitHub App callbacks: configure DNS and HTTPS so GitHub can reach `https://<hive-host>/gh-setup` when using the Setup URL flow.
