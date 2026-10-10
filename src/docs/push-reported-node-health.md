# Push-reported cluster node health

Push-reported clusters are clusters the hub cannot query with `kubectl`; spokes
must push cluster node health in their authenticated heartbeat. Each spoke needs
both:

- a configured cluster identity (`hub.cluster_id` or `HIVE_CLUSTER_ID`) matching
  the hub's `/data/saas/clusters.json` entry, such as `vllm-d`
- read-only cluster-scoped access to core Nodes and NodeMetrics

For an existing vLLM-d hosted spoke namespace, an operator with cluster-admin on
vLLM-d can apply the one-time fix below. Replace `NS` if needed; repeat for each
hosted namespace on the cluster. Do not grant `nodes/proxy` or cluster-wide
`pods list` to tenant ServiceAccounts.

```bash
CLUSTER_ID=vllm-d
NS=hive-hosted-hosted-available-vllmd-260731-nq4a
SA=$(kubectl --context vllm-d -n "${NS}" get deploy hive -o jsonpath='{.spec.template.spec.serviceAccountName}')
SA=${SA:-hive-sa}
kubectl --context vllm-d apply -f - <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: hive-node-health-reader-${NS}
rules:
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list"]
- apiGroups: ["metrics.k8s.io"]
  resources: ["nodes"]
  verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: hive-node-health-reader-${NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: hive-node-health-reader-${NS}
subjects:
- kind: ServiceAccount
  name: ${SA}
  namespace: ${NS}
YAML
kubectl --context vllm-d -n "${NS}" set env deploy/hive HIVE_CLUSTER_ID="${CLUSTER_ID}"
kubectl --context vllm-d -n "${NS}" rollout restart deploy/hive
```

On OpenShift clusters where `metrics.k8s.io` is served by the monitoring stack,
the NodeMetrics list can require an additional operator-managed monitoring
viewing role. If the hub shows `node_health_error` mentioning
`metrics.k8s.io` after the Node RBAC is present, grant the tenant ServiceAccount
the cluster's approved read-only metrics role, such as `cluster-monitoring-view`,
instead of broadening the Hive role above.
