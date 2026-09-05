# Dashboard route and health checks

Hive reports dashboard health from both the spoke and hub vantage points. The newer checks replace the old assumption that the public hub can always probe every spoke URL directly.

This page covers *reachability* — HTTP probes of the dashboard URL and route. For the per-hive green/amber/red/unknown *output* verdict on `/fleet` (the WHY chip and remediation hints), see [fleet health](fleet-health.md): a hive can pass every probe here and still be red there.

## Kubernetes RBAC

Apply the dashboard route RBAC manifest with the other Kubernetes resources:

```bash
kubectl apply -f deploy/k8s/dashboard-route-rbac.yaml
```

It grants the `hive` ServiceAccount permission to list same-namespace `networking.k8s.io` Ingresses and OpenShift `route.openshift.io` Routes. Without this RBAC, the spoke reports route existence as `unknown` with an RBAC error; Hive treats unknown as non-fatal.

> **This manifest is for self-hosted spokes only.** **Hosted** hives provisioned by the hub get an equivalent Role automatically — `hive-route-reader`, emitted by the provisioning template. Do not apply this manifest into a hosted hive's namespace: it hardcodes namespace `hive` and ServiceAccount `hive`, neither of which matches a hosted spoke (namespace `hive-hosted-<id>`, ServiceAccount `hive-sa` on SCC clusters or `default` elsewhere). The two Roles grant the same same-namespace read access to the same two resources; only the names and subjects differ. See [Provisioning a Hosted Hive](manual-provisioning.md#hive-route-reader--why-the-dashboard-link-503s-without-it).

> **The `unknown` above is non-fatal, but a second consequence is not.** The same read backs `SpokeServedHost`, which is how the spoke learns the external hostname its own Route/Ingress actually serves and reports it to the hub as `dashboard_url`. Denied that read, the spoke falls back to synthesising `<hiveID>.<hub host>` — correct only for spokes fronted by the hub's own wildcard domain, and a guaranteed **503** anywhere else, because the wildcard hands the name to the hub's router, which has no backend for a spoke on another cluster. The dashboard link breaks silently: DNS resolves, so nothing looks wrong until someone clicks it.

## Heartbeat fields

Each heartbeat may include:

- `public_url_self_check` — the spoke dials its own listener through `127.0.0.1:3002` while preserving the advertised Host header. HTTP statuses below 500 are OK; repeated 5xx responses become fail; DNS/timeouts are unknown rather than dead-link alarms.
- `route_exists` — the spoke confirms an Ingress or OpenShift Route for the advertised dashboard host in its namespace. Values are `found`, `missing`, or `unknown`.

Both probes run at most every five minutes and use short timeouts. Missing fields mean an older spoke or a non-Kubernetes/local deployment and are treated as unknown.

## Hub-side probing

The hub still probes dashboard URLs, but only treats a failed public probe as critical for hub-fronted domains. Private-network or cluster-local hives can be unreachable from the public hub while healthy from their own users' path, so a fresh heartbeat plus an OK/unknown spoke self-check suppresses false dead-link alerts.

A dashboard URL is considered healthy from the hub when it returns 200, a redirect, 401, or 403. 5xx, 404, and transport errors are failures.

## Alert matrix and hysteresis

Hive raises URL health alerts conservatively:

| Signal | Alert behavior |
| --- | --- |
| Spoke listener self-check `fail` | Critical after three consecutive spoke failures. |
| `route_exists: missing` | Critical; there is no Ingress/Route for the advertised host. |
| Hub-fronted URL fails and heartbeat is stale/offline | Critical once hub-side failure thresholds are met. |
| Hub-fronted URL fails but heartbeat is fresh and spoke self-check is OK/unknown | Informational/private-vantage signal, not a dead-link page. |
| DNS blip, timeout, missing RBAC, or old spoke | Unknown; no alert by itself. |

Hub-side hysteresis requires consecutive failures, a minimum hive age, and repeated dashboard evaluations before a critical URL alert reaches the Attention panel. Cluster-wide outages are rolled up instead of paging every hive individually.


## Spoke deep-health agent and token checks

The spoke `HealthSummary` treats quiet-by-design agents as idle, not failed. A stopped agent whose config is `on_demand: true`, whose ACMM pack marks it on-demand, or whose current governor-mode cadence is paused/off-schedule is reported in the agents check as `idle (on-demand)` or `idle (off-schedule)` rather than `down`. Paused agents remain a separate non-failing bucket; only expected-active agents that are stopped or failed count as `down`.

The token check no longer emits a bare `zero consumed` warning for every zero-token hive. It reports the best available reason, such as all agents paused, no agents due in the current governor mode, no model calls recorded, metering disabled or misconfigured, a parser/sink error, or live-capture/open sessions whose usage has not been accounted yet. All-paused and no-due windows are skipped instead of warning.
