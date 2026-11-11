# Installing the OpenShell gateway for `sandbox.runtime: openshell`

Cluster-side notes for the opt-in OpenShell sandbox runtime
([ADR-0026](../docs/adr/0026-sandbox-runtime-selection.md), epic
[#17642](https://github.com/hivecommons/hive/issues/17642); operator view in
[sandbox-isolation.md](../docs/sandbox-isolation.md#running-sandboxed-kicks-in-an-openshell-sandbox)).
Hive installs nothing: the gateway is a cluster-admin install, once per cluster,
and hive only talks to it. This page points at the upstream install and lists
the settings hive's runtime depends on. Upstream is the source of truth for the
install itself — [NVIDIA/OpenShell → docs/kubernetes/setup](https://github.com/NVIDIA/OpenShell/blob/main/docs/kubernetes/setup.mdx)
and the [gateway configuration reference](https://github.com/NVIDIA/OpenShell/blob/main/docs/how-it-works/gateways/configuration.mdx) —
and no manifests are vendored here; pin the chart version you install and
record it alongside the hive config.

## Prerequisites to check before installing

| Requirement | Why | How to check |
|---|---|---|
| Node kernel **≥ 6.2** with Landlock ABI ≥ 3 | OpenShell's sandbox qualification fails below it | `kubectl get nodes -o jsonpath='{.items[*].status.nodeInfo.kernelVersion}'`. Of the clusters hive runs on, LKE (Debian 13, 6.12) qualifies; OKE UEK7 (5.15) and RHCOS 9.x (5.14) do not. |
| A CNI that **enforces** egress `NetworkPolicy` (Calico, Cilium) | the gateway proxies egress; the pod must not be able to go around it. Flannel accepts policies and ignores them | create a deny-all policy in a scratch namespace and confirm a connection is actually refused, not just that the object exists |
| Cluster-admin for the install | CRDs, the gateway's RBAC, PKI secrets | — |
| A **ReadWriteMany** claim for hive's data when sandboxes may land on other nodes | the workspace is shared by mounting the same PVC | same caveat as the Job runtime |

## Install

```shell
helm upgrade --install openshell \
  oci://ghcr.io/nvidia/openshell/helm-chart \
  --version <pinned chart version> \
  --namespace openshell --create-namespace \
  --set server.drivers.kubernetes.allowDriverConfig=true \
  --set-string 'server.env.OPENSHELL_TELEMETRY_ENABLED=false'
```

Then, for the workspace hive will use (hive's `agent_sandbox.openshell.namespace`,
default `default`), install the workspace chart with driver config allowed so
the gateway may read PVC metadata for admission:

```shell
helm upgrade --install openshell-workspace \
  oci://ghcr.io/nvidia/openshell/openshell-workspace \
  --version <same pinned version> \
  --namespace <sandbox namespace> \
  --set gateway.allowDriverConfig=true
```

Settings hive's runtime depends on, in gateway terms:

- **`allow_driver_config = true`** under `[openshell.drivers.kubernetes]` (Helm:
  `server.drivers.kubernetes.allowDriverConfig`). Hive passes the workspace
  mount as caller `driver_config`; with this off every kick fails at create.
- **Resource admission labels** on hive's data claim. The gateway only mounts a
  caller-selected PVC that carries, by default:

  ```yaml
  metadata:
    labels:
      openshell.ai/sandbox-attachable: "true"
      openshell.ai/sandbox-attachable-workspace: "<hive's openshell.namespace>"
  ```

  `kubectl label pvc hive-data openshell.ai/sandbox-attachable=true openshell.ai/sandbox-attachable-workspace=default`
  on the spoke's namespace is enough for the defaults.
- **`OPENSHELL_TELEMETRY_ENABLED=false`** on the gateway. Hive's threat model
  treats sandbox metadata as spoke-private; keep upstream telemetry off.
- **A bearer token** for hive. Create one per the upstream access-control page
  and hand it to the hive pod as an environment variable (a Secret-backed
  `env` entry on the hive Deployment); name that variable in
  `agent_sandbox.openshell.token_env`. The token never goes in `hive.yaml`.
- **Digest-pinned images**: the agent's `sandbox.image` should be
  `registry/image@sha256:…`. The gateway's own supervisor and sandbox runtime
  images are pinned by the chart version.
- **Workspace mode**: `shared` with `server.sandboxNamespace` set to the
  spoke's namespace keeps the sandbox pods, the hive pod and the claim in one
  namespace, which is what the shared PVC needs. `operator` mode (one
  pre-provisioned namespace per workspace) also works; mount the claim there.

## Verify

1. `kubectl -n openshell get pods` — the gateway is `Ready`.
2. Point hive at it (`gateway_url: https://openshell-gateway.openshell.svc:<port>`;
   `insecure_tls: true` only while the chart's self-signed PKI is in use and
   the CA is not yet trusted by the hive image) and restart or reload; the
   boot log must show no `AgentSandboxGateWarnings` line about `gateway_url`
   or `workspace_claim`.
3. From a debug pod with the claim mounted at `/data`, run the env-gated test:
   `OPENSHELL_GATEWAY_URL=… OPENSHELL_WORKSPACE_CLAIM=hive-data OPENSHELL_IMAGE=<digest> go test ./crux/sandbox -run TestOpenShellIntegration -v`.
   It writes through `/workspace`, reads the file back on the hive side and
   logs the round-trip latency.
4. Kick an openshell-runtime agent and read the sandbox log: a `POST` to
   `api.github.com` must be **denied** and `git fetch` must succeed. Sandboxes
   are labelled `app.kubernetes.io/managed-by=hive-openshell` and are deleted
   when the kick ends; one that is still there after a finished kick is a bug.

## Not covered

Hive does not preflight any of the above ([#17644](https://github.com/hivecommons/hive/issues/17644))
and has no Podman-driver path for standalone hosts yet. The hub provisioner
does not install the gateway for hosted spokes; that is a follow-up once the
prototype numbers ([#17643](https://github.com/hivecommons/hive/issues/17643))
are in.
