# Dedicated CI runner cluster on Linode (LKE) — `hive-ci`

Moves the `hivecommons/hive` self-hosted GitHub Actions runners off the shared
`vllm-d` OpenShift cluster onto a cluster that exists only for CI. The parent
[README](../README.md) records why the shared cluster kept failing: no IPv6
egress and blocked apt mirrors (#6648), `No space left on device` on nodes the
runners share with inference workloads (#9629), a cephfs RWX Go cache that
corrupted under concurrent writers (#9338 follow-ups), and a flapping node
(`worker-2`, CRI-O down) that held 8 runners at a time.

Everything in this directory is applied by an operator; no workflow in this
repository touches it.

| Item | Value |
| --- | --- |
| Cluster | `hive-ci`, region `us-ord` (Chicago), Kubernetes 1.36, standard tier, **HA control plane** |
| Pool `arc-system` | 3 × `g8-dedicated-8-4` (4 vCPU / 8 GB), label+taint `hive-role=system` |
| Pool `runners` | 4 × `g8-dedicated-64-32` (initial) (32 vCPU / 64 GB / 655 GB NVMe), label `hive-ci-runner=true`, autoscaler 3–12 |
| ARC | `gha-runner-scale-set-controller` 0.14.2 in `arc-systems`; scale set `hive-runners-lke` in `arc-hive` |
| Runner budget | `minRunners: 4`, `maxRunners: 100` (≈9 runners per node by request → 11–12 nodes at peak; 4 nodes ≈ 36) |
| Cost | ≈ $3.5k/mo at 4 runner nodes, ≈ $2.8k/mo at the 3-node autoscaler floor, ≈ $9.6k/mo if pegged at 12 |

## Design

- **Compute Optimized (1:2) dedicated CPU.** `-race` shards and golangci-lint
  are CPU-bound; shared-CPU steal time is what made the vllm-d shards flaky.
  Per node: 32 vCPU ÷ (1.5 CPU runner + 0.25 CPU dind requested) ≈ 16
  scheduled runners, each able to burst to its 6+4 CPU limit. Requests are
  deliberately below steady-state use because CI jobs are bursty; with 4
  nodes that is ~64 concurrent runners before the pool autoscaler is needed
  (at the original 3 CPU request the pool capped at ~36 and jobs sat
  `Pending: Insufficient cpu`).
- **No shared RWX volume.** Go promises safe concurrent cache use only on a
  local filesystem. `GOMODCACHE` and the runner tool cache are per-node
  `hostPath`s under `/var/lib/hive-ci/`; `GOCACHE` stays per-job
  (`HIVE_GO_BUILD_CACHE=job`). `cache-prune-daemonset.yaml` bounds the
  hostPaths. Module zips and tool archives are content-addressed, so duplication
  across nodes is safe.
- **`ephemeral-storage` request == limit** (25 Gi runner + 20 Gi dind), so the
  scheduler packs at most ~14 pods' worth of disk on a 655 GB node and eviction
  never races a job.
- **Registry pull-through cache** (`registry-cache.yaml`) on the system pool;
  dind is started with `--registry-mirror` pointing at it. Base-image pulls
  happen once per cluster, not once per job, and Docker Hub rate limits stop
  mattering.
- **Spread and drain.** `maxSkew: 1` across hostnames; the system pool is
  tainted so runners can never evict the controller or the registry.
- **Egress-only firewall** (`terraform/main.tf`): inbound drop except the LKE
  control-plane range, outbound DNS + 80/443 only.
- **Toolchain image unchanged.** The runners still use
  `ghcr.io/hivecommons/hive-ci-runner:*-toolchain-*` from the parent README, so
  `ci-install-tool.sh` keeps its zero-network path. Linode has working IPv4 and
  IPv6 egress, so the apt-mirror ConfigMap from #6648 is not needed here.

## 1. Create the cluster

Either click through Cloud Manager (Kubernetes → Create: label `hive-ci`,
Chicago, 1.36, HA control plane on, App Platform off, the two pools above —
the create wizard has no autoscaler control; enable it afterwards on the
cluster page → `runners` pool → **Autoscale Pool** → min 3 / max 12) or:

```sh
export LINODE_TOKEN=...
cd src/deploy/ci-runners/lke/terraform
terraform init
terraform apply -var 'control_plane_acl_cidrs=["<your-ip>/32"]'
terraform output -raw kubeconfig | base64 -d > ~/.kube/hive-ci.yaml
```

If the pools were created in the UI, `terraform import linode_lke_cluster.hive_ci <cluster-id>`
brings them under management; the labels and taints below are then applied
by the next `terraform apply`.

## 2. Kubeconfig, labels, taints

```sh
CLUSTER_ID=$(linode-cli lke clusters-list --json | jq -r '.[] | select(.label=="hive-ci") | .id')
linode-cli lke kubeconfig-view "$CLUSTER_ID" --json | jq -r '.[0].kubeconfig' | base64 -d > ~/.kube/hive-ci.yaml
KUBECONFIG=~/.kube/config:~/.kube/hive-ci.yaml kubectl config view --flatten > /tmp/kc && mv /tmp/kc ~/.kube/config
kubectl config rename-context "lke${CLUSTER_ID}-ctx" hive-ci

# Pool IDs → node labels/taints (skip if Terraform created the pools).
linode-cli lke pools-list "$CLUSTER_ID" --json | jq -r '.[] | "\(.id) \(.type) \(.count)"'
SYS_POOL=<id of the g8-dedicated-8-4 pool>; RUN_POOL=<id of the g8-dedicated-64-32 pool>
kubectl --context hive-ci label nodes -l lke.linode.com/pool-id=$SYS_POOL hive-role=system
kubectl --context hive-ci taint nodes -l lke.linode.com/pool-id=$SYS_POOL hive-role=system:NoSchedule
kubectl --context hive-ci label nodes -l lke.linode.com/pool-id=$RUN_POOL hive-ci-runner=true
kubectl --context hive-ci get nodes -L hive-role,hive-ci-runner
```

Labels set with `kubectl` are lost when the autoscaler replaces a node; the
Terraform pool `labels`/`taint` blocks (or the LKE pool-update API) make them
sticky. Do that before enabling the autoscaler in anger.

## 3. Cloud Firewall

`terraform apply` creates and attaches `hive-ci-nodes`. Without Terraform:
Cloud Manager → Firewalls → Create, use the **Kubernetes** inbound preset,
outbound `DROP` with `53/UDP`, `80,443/TCP` to `0.0.0.0/0` and `::/0`, attach
every `lke<id>-*` Linode. Re-attach when the autoscaler adds a node (Terraform
does this on the next apply).

## 4. ARC controller and system services

```sh
kubectl --context hive-ci create namespace arc-systems
kubectl --context hive-ci create namespace arc-hive
helm --kube-context hive-ci upgrade --install arc -n arc-systems \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller --version 0.14.2 \
  --set nodeSelector.hive-role=system \
  --set 'tolerations[0].key=hive-role,tolerations[0].value=system,tolerations[0].effect=NoSchedule' \
  --set replicaCount=2
kubectl --context hive-ci -n arc-systems apply -f src/deploy/ci-runners/lke/registry-cache.yaml
kubectl --context hive-ci -n arc-systems rollout status deploy/registry-cache
```

The controller's service account name is `arc-gha-rs-controller` when the
release is called `arc`; `hive-runners-lke-values.yaml` references it.

## 5. GitHub credentials and the scale set

Copy the existing App/PAT secret from vllm-d rather than minting a new one —
the scale set authenticates to the same repository:

```sh
kubectl --context vllm-d -n arc-v2-hive get secret hive-github-token -o json \
  | jq 'del(.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,.metadata.managedFields) | .metadata.namespace="arc-hive"' \
  | kubectl --context hive-ci apply -f -
kubectl --context hive-ci -n arc-hive create serviceaccount hive-runner
helm --kube-context hive-ci upgrade --install hive-runners-lke -n arc-hive \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set --version 0.14.2 \
  -f src/deploy/ci-runners/lke/hive-runners-lke-values.yaml
kubectl --context hive-ci -n arc-hive apply -f src/deploy/ci-runners/lke/cache-prune-daemonset.yaml
```

Verify:

```sh
kubectl --context hive-ci -n arc-hive get autoscalingrunnersets,pods -o wide
unset GITHUB_TOKEN && gh api repos/hivecommons/hive/actions/runners --paginate \
  | jq -r '.runners[] | select(.name|startswith("hive-runners-lke")) | "\(.name) \(.status) busy=\(.busy)"'
```

Four idle `hive-runners-lke-*` runners, spread one per runner node, is the
expected steady state.

## 6. Cut-over

Every self-hosted job in `.github/workflows/` picks its runner from the
repository variable `HIVE_RUNNER_LABELS`; the scale-set name is the label, so
no workflow file changes:

```sh
unset GITHUB_TOKEN && gh variable set HIVE_RUNNER_LABELS --repo hivecommons/hive --body '["hive-runners-lke"]'
```

1. Watch one full `v2 Tests` run go green on the new pool
   (`gh run list --workflow v2-tests.yml --limit 3`; job logs show the
   `hive-runners-lke-*` runner name).
2. Drain the old cluster:
   `helm --kube-context vllm-d upgrade hive-runners -n arc-v2-hive oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set --version 0.14.2 --reuse-values --set maxRunners=0`
3. After a quiet day: `helm --kube-context vllm-d uninstall hive-runners -n arc-v2-hive`,
   then `helm uninstall arc-v2 -n arc-v2-systems`, and remove the
   `hive-ci-runner=true` labels from the vllm-d nodes.
4. Optionally rename: uninstall the LKE release, set
   `runnerScaleSetName: hive-runners` in the values file, reinstall, and set
   `HIVE_RUNNER_LABELS` back to `["self-hosted","hive"]`.

Rollback at any point before step 3 is the same variable set back to
`["self-hosted","hive"]`; the vllm-d runners pick up the queue on the next job.

## 7. Operating

| Signal | Where |
| --- | --- |
| Queue depth vs capacity | `gh run list --status queued`; `kubectl -n arc-hive get autoscalingrunnersets` (`currentRunners`, `pendingEphemeralRunners`) |
| Node pressure | `kubectl --context hive-ci top nodes`; autoscaler events in `kubectl get events -A --field-selector reason=TriggeredScaleUp` |
| Disk | `kubectl --context hive-ci -n arc-hive logs ds/hive-ci-cache-prune`; `df` inside any runner (`/var/lib/hive-ci` on the node) |
| Registry cache hit rate | `kubectl -n arc-systems logs deploy/registry-cache | grep -c 'blob unknown'` should trend down |

Runner image upgrades follow the parent README (`ci-runner-image.yml`), then
edit the two `image:` lines in `hive-runners-lke-values.yaml` and
`helm upgrade`. Kubernetes upgrades: LKE HA control plane upgrades in place;
recycle the runner pool during a quiet hour (`linode-cli lke pool-recycle`) —
ephemeral runners finish their job before the node drains because the pods
carry the default 5-minute grace period and ARC marks them non-schedulable.
