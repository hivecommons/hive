# Installing Agent Substrate for `sandbox.runtime: substrate`

Cluster-side notes for the opt-in Substrate sandbox runtime
([ADR-0026 phase 2](../docs/adr/0026-sandbox-runtime-selection.md#implementation-notes-phase-2-agent-substrate),
epic [#17695](https://github.com/hivecommons/hive/issues/17695); operator view
in [sandbox-isolation.md](../docs/sandbox-isolation.md#running-sandboxed-kicks-in-a-substrate-actor)).
Hive installs nothing: Substrate is a cluster-admin install, once per cluster,
and hive only talks to its Control API and router. Upstream is the source of
truth for the install — [agent-substrate/substrate → docs/guides/operator-install.md](https://github.com/agent-substrate/substrate/blob/main/docs/guides/operator-install.md),
the [API guide](https://github.com/agent-substrate/substrate/blob/main/docs/reference/api-guide.md)
and [sandbox classes](https://github.com/agent-substrate/substrate/blob/main/docs/reference/sandbox-classes.md) —
and no manifests are vendored here; pin the release you install and record it
alongside the hive config.

## Prerequisites to check before installing

| Requirement | Why | How to check |
|---|---|---|
| Kubernetes with `ClusterTrustBundle`, `ClusterTrustBundleProjection` and `PodCertificateRequest` feature gates and `certificates.k8s.io/v1beta1` enabled | actor and client identities are pod certificates (SPIFFE); the podcert controller does not start without them. Still off by default as of Kubernetes 1.36 | `kubectl get --raw /metrics \| grep 'kubernetes_feature_enabled{name="PodCertificateRequest"'` and `kubectl api-resources --api-group=certificates.k8s.io`. Managed control planes (OKE, LKE) need the provider to enable them; OpenShift needs a `FeatureGate` CR |
| Standard Linux nodes with **cgroup v2** for the `gvisor` class | `ateworker-gvisor` runs `runsc` inside the worker pod; there is no kernel floor beyond ≥ 5.8 | `kubectl get nodes -o jsonpath='{.items[*].status.nodeInfo.kernelVersion}'`; `stat -fc %T /sys/fs/cgroup` on a node → `cgroup2fs` |
| `/dev/kvm` on nodes for the `microvm` class | Kata on Cloud Hypervisor guests; `atelet` advertises the `ate.dev/kvm` device only where it exists | `kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.ate\.dev/kvm}{"\n"}{end}'` after install |
| Nodes trusted to run worker pods with `SYS_ADMIN` and `NET_ADMIN` | workers are `privileged: false` but keep those two for mounts and networking | on OpenShift, an SCC granting them to the `ate-system` worker ServiceAccount |
| A CNI that **enforces** egress `NetworkPolicy` | Substrate's `atenet` dataplane carries actor egress; the worker pod itself must not be a side door | create a deny-all policy in a scratch namespace and confirm a connection is refused |
| PostgreSQL and S3-compatible object storage | Control API state; actor snapshots (`snapshot_on_idle`) | `ate-setup` deploys bundled ones for a lab; use managed services in production |
| Cluster-admin for the install | CRDs (`workerpools`, `sandboxconfigs`, `egresspolicies`, …), ValidatingAdmissionPolicies, RBAC, PKI | — |

### Which hive clusters qualify (survey 2026-11-11)

| cluster | kernel / Kubernetes | `gvisor` | `microvm` |
|---|---|---|---|
| hive-oke | OL9, UEK8 6.12, k8s 1.34.10 | yes, once the podcert feature gates are enabled on the OKE control plane | needs KVM-capable shapes (bare metal or nested virtualisation) |
| lke648397, hive-ci | Debian 13, 6.12 | yes; confirm the feature gates — LKE exposes no control-plane flags | KVM to confirm per plan |
| vllm-d, a-ks-wec2 | RHCOS 9.x, 5.14 | possible: SCC for the worker capabilities + `FeatureGate` CR (TechPreviewNoUpgrade) | only on KVM-capable nodes |

## Install

Build or download `ate-setup` from a pinned release of
`github.com/agent-substrate/substrate`, write the install document, and deploy:

```yaml
# substrate-install.yaml
apiVersion: install.ate.dev/v1alpha1
kind: SubstrateInstall
namespace: ate-system
atenet:
  dataplane: envoy
images:
  repo: <your mirror>/substrate
  tag: <pinned release>
```

```shell
ate-setup deploy ate-system --config substrate-install.yaml \
  --credential-provider='{"name":"k8s.io"}' \
  --ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/<hive namespace>/sa/<hive ServiceAccount>
```

`--ingress-allowed-spiffe-ids` must name hive's own pod identity: that is what
lets hive open `CONNECT` tunnels through `atenet-router` to the bridge in each
actor. Unknown keys in the document are rejected, not ignored.

Then create the pieces hive's runtime depends on, in Substrate terms:

- **An atespace** for hive (`sandbox.substrate.atespace`, default `default`),
  and a principal for hive's token (`token_env` / `token_file`) that may
  create and delete `ActorTemplate`, `Actor` and `EgressPolicy` and list
  workers in it. `kubectl ate` shows the exact RBAC verbs for the release.
- **A `WorkerPool`** with `spec.sandboxClasses[].name: gvisor` (or `microvm`)
  resolving the default `SandboxConfig` (`gvisor-default`), labelled so that
  `sandbox.substrate.worker_selector` matches it; hive only uses pools whose
  workers report `ACTIVE` and advertise a sandbox class. Taint the node pool
  `ate.dev/sandboxClass=<class>:NoSchedule` to reserve it; worker pods
  tolerate it.
- **Projected files in the hive pod**: a ServiceAccount token with audience
  `api.ate-system.svc` (→ `token_file`), the Control API CA via
  `clusterTrustBundle` with signer `servicedns.podcert.ate.dev/identity`
  (→ `ca_file`), and a `podCertificate` with signer
  `podidentity.podcert.ate.dev/identity` (→ `router_client_bundle`) plus the
  router CA (→ `router_ca_file`). The authentication guide upstream has the
  volume stanza.
- **A digest-pinned sandbox image** containing `crux` (for the
  `substrate-bridge` entrypoint) and the agent CLI, set as
  `sandbox.substrate.image`; tags are refused at boot and at kick. Publishing
  it is tracked in [#17697](https://github.com/hivecommons/hive/issues/17697).
- **Egress**: hive writes its own `EgressPolicy` per actor (`github.com`,
  `api.github.com`, `allow_hosts`). Do **not** also configure Substrate's
  credential injection for GitHub — actors run credential-free and hive
  brokers pushes after the kick (ADR-0026, #17695).

## Verify

```shell
kubectl -n ate-system get deploy,ds                      # ateapi, atenet-router, podcert controller, atelet ready
kubectl get crd | grep ate.dev                            # workerpools, sandboxconfigs, egresspolicies, …
kubectl get workerpool -A                                 # at least one pool with ACTIVE workers
kubectl get sandboxconfig gvisor-default -o yaml | grep defaultVersion
kubectl get nodes -o custom-columns=NAME:.metadata.name,KVM:.status.allocatable.ate\.dev/kvm
kubectl ate create actor smoke -a <atespace> --template <any template> && kubectl ate delete actor smoke -a <atespace>
```

From the hive side, start with `runtime: substrate` and watch the boot log: the
preflight warns when the Control API is unreachable, has no matching `ACTIVE`
worker, or its workers report no sandbox class. Then run one kick and confirm
in the router log that the actor's SPIFFE id made exactly the allowed
connections and that a connect to any other host was refused — that denial is
the point of the runtime. Latency and image numbers go back into this page
from the prototype ([#17697](https://github.com/hivecommons/hive/issues/17697));
the fuller prover (CRDs, operator, feature gates, KVM) is
[#17698](https://github.com/hivecommons/hive/issues/17698).

## Upgrades

Substrate is pre-1.0; follow its version-skew policy (N ↔ N+1 only) and pin
the release and the sandbox image by digest. A `SandboxConfig` upgrade adds a
`runsc` version and keeps the previous one so suspended actors still restore.
Hive's generated client (`crux/sandbox/substrateapi`) tracks the fields it
uses; bump it with `generate.sh` against the matching upstream tag when the
Control API changes.
