# backup-exec-restriction overlay

This optional kustomize overlay deploys a [Kyverno](https://kyverno.io/) `ClusterPolicy` that closes the
gap between the backup CronJob's cluster-wide `pods/exec` RBAC grant and the
application's actual access pattern (only `hive-hosted-*` namespaces).

## Background (issue [#4062](https://github.com/hivecommons/hive/issues/4062))

The `hive-hub-backup` ClusterRole must grant `pods/exec: create` cluster-wide
because Kubernetes RBAC cannot express wildcard-namespace rules. The backup
binary (`pkg/hubbackup/collect.go`) only ever targets `hive-hosted-<id>`
namespaces, but the RBAC permission is broader: a compromised backup pod could
exec into pods in any namespace, including infrastructure workloads.

This overlay enforces the intended restriction at the admission layer.

## Scope of this overlay

This overlay is **policy-only**: it renders the Kyverno `ClusterPolicy` and
nothing else. It does not include `src/deploy/k8s` — that base contains the hub
Deployment/Service/PVC but *not* `backup-cronjob.yaml`, so including it would
have pulled in unrelated manifests while still omitting the workload this
policy constrains. Apply the backup CronJob yourself, first.

## Prerequisites

- Kyverno ≥ 1.11 installed and running in the cluster.
- `src/deploy/k8s/backup-cronjob.yaml` already applied (it provides the
  `hive-hub-backup` ServiceAccount this policy matches on).

## Apply

```sh
kubectl apply -f src/deploy/k8s/backup-cronjob.yaml

# Standalone (no kustomize build needed)
kubectl apply -f src/deploy/kustomize/overlays/backup-exec-restriction/kyverno-backup-exec-restriction.yaml

# Or via kustomize (equivalent — the overlay renders only the policy)
kubectl apply -k src/deploy/kustomize/overlays/backup-exec-restriction
```

## Availability trade-off: `failurePolicy: Ignore`

The policy matches `Pod/exec` `CONNECT` requests cluster-wide. With
`failurePolicy: Fail`, any Kyverno outage (upgrade, crashloop, webhook cert
rotation) would make the API server reject the admission review and block
**every** `kubectl exec` in the cluster — including the exec needed to debug
Kyverno. The policy therefore fails **open**.

While Kyverno is unavailable, the backup SA's exec is bounded by RBAC plus the
application-level namespace guard compiled into the backup binary
(`pkg/hubbackup/collect.go`), which no admission outage can bypass. Treat this
policy as defence-in-depth, never as the sole control.

## What this changes

| Before | After |
|--------|-------|
| `hive-hub-backup` SA can exec into pods in **any** namespace | Exec requests are admitted only when the target namespace matches `hive-hosted-*` |
| Namespace enforcement: application-only (binary, not admission) | Namespace enforcement: application + Kyverno admission webhook (when Kyverno is up) |

## Known limitations

- `pods: get,list` in the ClusterRole remains **cluster-wide**: this policy only
  constrains `pods/exec`. A compromised backup pod can still enumerate pods
  fleet-wide (a reconnaissance vector, not an execution one).
- Enforcement is fail-open (see above), so it is hardening, not a boundary.
- Requires Kyverno ≥ 1.11; clusters without Kyverno rely solely on the
  application-level guard.

## Testing

After applying, verify the policy blocks an out-of-scope exec:

```sh
# Should be DENIED (wrong namespace)
kubectl auth can-i create pods/exec \
  --namespace default \
  --as system:serviceaccount:hive-hub:hive-hub-backup

# Should be ALLOWED (correct namespace)
kubectl auth can-i create pods/exec \
  --namespace hive-hosted-example \
  --as system:serviceaccount:hive-hub:hive-hub-backup
```

Note: `kubectl auth can-i` checks RBAC only; Kyverno enforcement is visible
in `kubectl describe clusterpolicy restrict-backup-exec-to-hive-namespaces`
and in Kyverno audit logs.
