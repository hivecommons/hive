# Dashboard route and health checks

Hive reports dashboard health from both the spoke and hub vantage points. The newer checks replace the old assumption that the public hub can always probe every spoke URL directly.

## Kubernetes RBAC

Apply the dashboard route RBAC manifest with the other Kubernetes resources:

```bash
kubectl apply -f deploy/k8s/dashboard-route-rbac.yaml
```

It grants the `hive` ServiceAccount permission to list same-namespace `networking.k8s.io` Ingresses and OpenShift `route.openshift.io` Routes. Without this RBAC, the spoke reports route existence as `unknown` with an RBAC error; Hive treats unknown as non-fatal.

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

