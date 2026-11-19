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
| Node kernel **≥ 6.2** with Landlock **active** | OpenShell's sandbox qualification fails below it; a new-enough kernel built without Landlock in the LSM list fails the same way | `kubectl get nodes -o jsonpath='{.items[*].status.nodeInfo.kernelVersion}'`, then from a privileged debug pod `cat /sys/kernel/security/lsm` must contain `landlock`. Of the clusters hive runs on, LKE (Debian 13, 6.12) and OKE on OL9/UEK8 (6.12) qualify; OKE UEK7 (5.15) and RHCOS 9.x (5.14) do not. |
| The **Agent Sandbox** controller (`sandboxes.agents.x-k8s.io`) | the gateway's Kubernetes driver creates `Sandbox` objects; the chart's preflight refuses to install without the API | `kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox.yaml`, then `kubectl api-resources \| grep agents.x-k8s.io` |
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
  --set server.telemetryEnabled=false \
  --set server.workspaceStorageClass=<fast or RWX class> \
  --set 'gatewayConfig.openshell\.drivers\.kubernetes.allowed_pod_labels={app.kubernetes.io/managed-by}' \
  --set 'gatewayConfig.openshell\.drivers\.kubernetes.allowed_pod_annotations={hive.dev/*}'
```

`gatewayConfig` is rendered to TOML: its top-level keys are table names
written as dotted strings (`openshell.drivers.kubernetes:` in a values file,
escaped as above on the command line), not nested maps; nesting
`openshell: {drivers: …}` breaks the config and crash-loops the gateway.

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
- **`allowed_pod_labels` / `allowed_pod_annotations`** under
  `[openshell.drivers.kubernetes]`. The gateway rejects any template label or
  annotation it was not told about; hive labels sandboxes
  `app.kubernetes.io/managed-by=hive-openshell` and annotates them
  `hive.dev/policy-profile` when a profile is set.
- **`server.workspaceStorageClass`**. The gateway provisions a 2Gi
  `workspace-<sandbox>` PVC for every sandbox even though hive mounts its own
  claim. On a block-storage default class that provision + attach added ~20 s
  to every kick on LKE (27 s vs 9 s warm start, [#17643](https://github.com/hivecommons/hive/issues/17643));
  point it at an NFS/RWX or otherwise fast class.
- **Sandbox uid**. The gateway runs every sandbox as `runAsUser`/`fsGroup`
  `10001` by default, overriding the image's `USER`. The kick workspace hive
  creates on the shared claim must be writable by that uid: either set
  `server.sandboxUid`/`server.sandboxGid` to the uid crux runs as, or make the
  claim's workspace directories group-writable for 10001.
- **`server.telemetryEnabled=false`** on the gateway. Hive's threat model
  treats sandbox metadata as spoke-private; keep upstream telemetry off.
- **A bearer token** for hive. Upstream authenticates clients by mTLS, OIDC
  JWT, or a trusted reverse proxy; hive's client sends a static bearer token,
  so "token" here means a JWT from the OIDC issuer the gateway trusts (see the
  upstream access-control page). Hand it to the hive pod as an environment
  variable (a Secret-backed `env` entry on the hive Deployment); name that
  variable in `agent_sandbox.openshell.token_env`. The token never goes in
  `hive.yaml`. `allowUnauthenticatedUsers` is for throwaway clusters only.
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
   Sandbox names are `hk-<agent slug>-<base36 unix>` (the gateway caps names at
   19 characters) and the claim is mounted as volume `hive-workspace`
   (`workspace` is reserved by the gateway).
5. Agent images must ship a glibc `sh`: debian/ubuntu/fedora/EL-based images
   and hive's own image work, but busybox/musl images (alpine) fail at
   `spawn sandbox entrypoint process '/bin/sh': Permission denied`.

## Not covered

Hive does not preflight any of the above ([#17644](https://github.com/hivecommons/hive/issues/17644))
and has no Podman-driver path for standalone hosts yet. The hub provisioner
does not install the gateway for hosted spokes; that is a follow-up. The
prototype numbers are on [#17643](https://github.com/hivecommons/hive/issues/17643):
on LKE with the settings above, cold start 10.0 s vs 6.0 s for the Job runtime
(1.67×), warm 9.1 s vs 3.8 s (2.4×), +38 Mi per sandbox, egress policy
enforced.
