# ADR-0026: Sandbox runtime selection

Status: Accepted (phase 1 OpenShell implemented, runtime opt-in; measured on LKE in #17643: cold 1.67× / warm 2.4× the Job runtime, egress enforced — #17642, hivecommons/hive#17646; phase 2 Agent Substrate implemented, runtime opt-in — #17695, hivecommons/hive#17741)

## Context

By default Hive agents share one container, separated by Unix user. The opt-in
sandbox ([sandbox-isolation.md](../sandbox-isolation.md), #2804) runs a kick in a
rootless Podman container or, with `sandbox.runtime: job`, a Kubernetes Job
(#6311). Both are ordinary containers: the boundary is the runtime's default
seccomp profile and namespaces, and network confinement in `restricted` mode is
whatever policy the operator supplies.

The landscape refresh (#13806) named two external runtimes built for that gap.
#14092 asked for a spike comparing them against the sandbox contract. This
record covers that comparison as a paper evaluation, read from each project's
own documentation on 2026-10-25:

- **NVIDIA OpenShell** ([repo](https://github.com/NVIDIA/OpenShell), v0.1.5,
  2026-10-20).
- **OpenSandbox** ([repo](https://github.com/opensandbox-group/OpenSandbox),
  1.1.1, 2026-10-09; 1.2.0-rc.2 on 2026-10-21). It is a Linux Foundation project
  under AAIF, contributed by Alibaba.

**Not done in this record:** the prototype run on a hive spoke (#14092 step 2).
Startup latency, image and boot size, and proxy and attribution behaviour are
**not measured** here. [Prototype protocol](#prototype-protocol) lists what to
measure; #17643 tracks running it. The runtime shipped as opt-in before those
numbers existed (see [Implementation notes](#implementation-notes-phase-1)):
nothing changes for an operator who does not select it, and the measurement
needs the code to exist.

### The contract a runtime must satisfy

These are the requirements from `sandbox-isolation.md`. The seam is the
`sandbox.Launcher` interface (`src/crux/sandbox/sandbox.go`):
`Run(ctx, LaunchSpec) (Result, error)`. Podman and `crux/kubejob` already
implement it. A new runtime is one more `sandbox.runtime` value behind that
interface.

| # | Requirement | Where hive enforces it today |
|---|---|---|
| C1 | Task in: a prepared workspace and `.hive/kick-prompt.txt`, a command, an env allowlist | `LaunchSpec` |
| C2 | The workspace is credential-free and no GitHub token enters | `SanitizedEnv`, credential-name filter |
| C3 | Pushes go through the push broker, outside the sandbox | `crux/pushbroker` reads commits back from the workspace |
| C4 | The proxy still mediates GitHub | ADR-0002 iptables redirect in the hive pod; agent identified by socket UID |
| C5 | Transcript and exit code come back; a timeout kills the run | `Result`, `.hive/sandbox-transcript.log` |
| C6 | Hosted spokes (OKE) need no privileged pods | namespace-scoped RBAC from the hub provisioner |

## Evaluation

### NVIDIA OpenShell

**Model.** A gateway is the control plane. A compute driver (Docker, Podman,
Kubernetes or MicroVM) creates two parts per sandbox:

- a **workload**, which holds the agent and has no network of its own;
- a trusted **supervisor**, which carries every TCP connection and DNS lookup
  the workload makes.

Inside the workload, the agent runs as one non-root user with no Linux
capabilities. Landlock confines files. Seccomp user notification hands each
`connect` and DNS query to the supervisor, which checks it against policy and
opens the real connection. Policy covers files, processes, network endpoints
and HTTP methods. A prover flags proposed changes that would widen
credentialed reach.

| # | Fit | Notes |
|---|---|---|
| C1 | Yes | `sandbox create` from an OCI image, `sandbox upload` for the workspace, `sandbox exec`, which returns the command's exit code. The Go SDK is `github.com/NVIDIA/OpenShell/sdk/go`. Workspace transport changes: hive's shared-PVC `subPath` model becomes an upload/download, or a mount set in the sandbox template. |
| C2 | Yes, stronger | OpenShell's own premise is that the workload never holds credentials. Hive attaches no GitHub provider, so there is nothing to inject. |
| C3 | Yes | Unchanged. The broker reads commits from the returned workspace. |
| C4 | Changes shape | The supervisor terminates TLS and opens the upstream connection. In the Kubernetes driver the supervisor is **its own pod**, so the hive pod's iptables redirect never sees the traffic, and ADR-0002's socket-UID identification has no meaning across pods. See [Proxy and attribution](#proxy-and-attribution). |
| C5 | Yes | Exec exit code, gateway logs and OCSF JSON export. If the supervisor connection drops, the workload freezes, so the run fails closed. |
| C6 | Yes, with cluster prerequisites | No privileged pod. The workload has no added capabilities, and the gateway meets the Restricted Pod Security Standard. The cluster needs three things: (a) the [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) controller and CRDs, a cluster-scoped install; (b) a CNI that **enforces** ingress and egress `NetworkPolicy`, because without one the workload can bypass the supervisor; (c) node kernels with Landlock ABI 3 (Linux ≥ 6.2) and the seccomp user-notification operations it probes for. A node that lacks them fails closed at sandbox start. |

**Licensing and cadence.** Apache-2.0, DCO, public RFC board. There have been
six stable `0.1.x` releases since 0.1.0 on 2026-09-25: stable builds are
promoted from nightly pre-releases that pass qualification. It is pre-1.0, so
expect API movement. Anonymous telemetry is **on by default**. Hive must set
`OPENSHELL_TELEMETRY_ENABLED=false`, or `server.telemetryEnabled=false` in
Helm.

### OpenSandbox

**Model.** A lifecycle server (Python, `opensandbox-server`) creates sandboxes
on Docker or Kubernetes, the latter through an operator. Each sandbox runs
`execd` for commands and files. Isolation is chosen per server profile:

- `""`: plain runc;
- `gvisor`;
- `kata`: a RuntimeClass;
- `firecracker`: Kata-FC, or the "Fast Sandbox" Fastlet path.

Network policy is an **egress sidecar** in the sandbox's network namespace. It
proxies DNS, sets nftables allow sets and can run an optional transparent
mitmproxy, which the Credential Vault uses.

| # | Fit | Notes |
|---|---|---|
| C1 | Yes | Lifecycle API with `commands.run` and `files.write_files`. Volumes, including Kubernetes PVCs, can be set at creation. The Go SDK's import path is still `github.com/alibaba/OpenSandbox/sdks/sandbox/go`. Hive would run and upgrade one more service, the Python lifecycle server. |
| C2 | Yes | The Credential Vault keeps real secrets in the sidecar. Hive would use none. |
| C3 | Yes | Unchanged. |
| C4 | Changes shape | The sidecar owns transparent interception in the pod and rejects a second interceptor in the same namespace. It can chain to an upstream proxy over HTTP/1 CONNECT, but only with its own transparent MITM enabled. Hive's proxy would then see a re-originated TLS session from the sidecar, not the agent. |
| C5 | Yes | Sandbox state, exec results and OpenTelemetry metrics. |
| C6 | **Only at the weakest isolation level** | On Kubernetes the egress sidecar holds the pod's `NET_ADMIN`. That is the same capability class the hive pod already needs, and it is not privileged. But with runc that buys FQDN egress policy and nothing more. The boundary is the same container boundary `sandbox.runtime: job` already has. gVisor is **rejected** with egress policy, because its netstack cannot take the nat redirect. Kata needs a cluster RuntimeClass and KVM on the nodes. Fast Sandbox's Fastlet container is **privileged with `/dev/kvm`** and needs bare metal or nested virtualization, plus XFS reflink storage. Its headline numbers (64 ms P50 create) come from that path. |

**Licensing and cadence.** Apache-2.0, Linux Foundation, release images signed
with Cosign. A new line ships every two weeks. Only the latest line is
supported (no LTS): a line reaches end-of-life when the next one ships, apart
from a 72-hour emergency-CVE window. The project is post-1.0, but that pace is
an upgrade obligation on every spoke.

### Proxy and attribution

Both runtimes move the agent's outbound TCP to a component outside the hive
pod's network namespace. ADR-0002's two mechanisms rest on the hive pod: the
iptables redirect, and agent identification by socket owner UID. Neither
reaches a sandbox on these runtimes.

For a sandboxed kick this matters less than it first appears. C2 already keeps
every GitHub token out of the sandbox, so nothing inside can authenticate a
GitHub write. The proxy's job for a sandboxed kick reduces to "unauthenticated
reads only". OpenShell can express that directly: a network policy that allows
`GET` on `api.github.com` and git fetch on `github.com`, and denies the rest. The
prover can check it in CI. The push broker's own traffic leaves from the hive
pod and stays under ADR-0002.

Attribution moves from UID to sandbox: one OpenShell sandbox per kick, named
for the agent and kick. OpenShell's logs carry the sandbox name and the calling
executable. Hive's existing `audit: sandbox agent kicked` line already keys on
the agent.

## Decision

**Accepted:** adopt **OpenShell** as a third `sandbox.runtime` value
(`openshell`), behind the existing `sandbox.Launcher` interface. Use the
Kubernetes driver on hosted spokes and the Podman driver on standalone hosts.
The existing `podman` and `job` runtimes stay as they are, and the default
isolation mode does not change (out of scope for #14092).

Reasons:

1. **It meets C6 without privileged pods and still raises the boundary.** The
   workload has no capabilities, and Landlock, seccomp mediation and a
   network-less workload are confirmed before the agent starts. OpenSandbox
   meets C6 only with runc, where it adds egress policy but no stronger
   boundary than the current Job runtime. Its stronger modes need KVM, and
   Fast Sandbox needs privileged pods.
2. **No new long-lived service per spoke beyond the gateway**, and the gateway
   speaks gRPC with a Go SDK. OpenSandbox adds a Python lifecycle server plus a
   per-pod sidecar image.
3. **Default-deny egress is the runtime's contract**, not a policy the operator
   has to supply. That closes the "`restricted` depends on an operator-provided
   network policy" gap listed in `sandbox-isolation.md`.

**Do not adopt OpenSandbox now.** Revisit it for self-hosted spokes on
KVM-capable nodes, where Kata gives a VM boundary without privileged pods, if
the OpenShell prototype fails its gates.

### Preconditions the implementation epic must verify first

These are blocking. If any one fails, the recommendation falls back to "no
change".

- The hosted OKE node pool's kernel passes OpenShell's Landlock ABI 3 and
  seccomp qualification. Oracle UEK7 is 5.15 and fails the Landlock ABI 3
  floor; a 6.2+ kernel such as UEK8 is required. Check `uname -r` on the pool,
  then run a real `sandbox create`.
- The hosted cluster's CNI **enforces** egress `NetworkPolicy`. Flannel does
  not; Calico or Cilium do. Kubernetes accepts unenforced policies silently, so
  test with a denied connection, not by reading the manifest.
- Someone with cluster-admin can install the Agent Sandbox controller and CRDs
  once per cluster. Per-spoke isolation then maps to OpenShell workspaces in
  `operator` namespace mode, one pre-provisioned namespace per spoke, so a spoke
  still needs only namespace-scoped RBAC.

### Prototype protocol

Run one real kick for the same agent and task on hive's own spoke, under
`job`, `openshell` (Kubernetes driver), and OpenSandbox (runc + egress sidecar)
for comparison. Record these numbers and attach them to this ADR before it
moves to `Accepted`:

| Measure | How |
|---|---|
| Cold start to agent's first command | Kick timestamp until the first transcript line, median of 5 |
| Warm start | Same, with images already on the node |
| Image and boot size | Pulled bytes for sandbox, supervisor and sidecar images |
| GitHub write refused | A `POST` to `api.github.com` from inside the sandbox fails with no token present |
| GitHub read allowed | `gh api repos/<repo>` read and `git fetch` succeed |
| Direct egress refused | A raw TCP connect to an arbitrary IP fails |
| Attribution | The refused write appears in runtime logs with the sandbox and kick name |
| Broker path intact | The kick's commits arrive as a PR through `pushbroker`, unchanged |

## Implementation notes (phase 1)

Phase 1 landed in hivecommons/hive#17646 (epic #17642) as
`sandbox.runtime: openshell`, `src/crux/sandbox/openshell.go`, configured
under `agent_sandbox.openshell` — see
[sandbox-isolation.md](../sandbox-isolation.md#running-sandboxed-kicks-in-an-openshell-sandbox)
for the operator view and [deploy/openshell.md](../../deploy/openshell.md) for
the cluster side. Writing it against the real SDK moved four assumptions in
this record:

| This record assumed | What the SDK (v0.1.8) offers | What phase 1 does |
|---|---|---|
| Go SDK at `github.com/NVIDIA/OpenShell/sdk/go` | that path is deprecated upstream in favour of `github.com/ai-openshell/sdk-go` | pins `github.com/ai-openshell/sdk-go v0.1.8` (tagged). +3 modules per go.mod (`sdk-go`, `x/oauth2`, `x/sync`), grpc 1.83.2 → 1.85.0, no CGO, ~3.6 MB on the `hive` binary |
| Workspace copied in and out through the SDK | `Files().Upload` is single-file and the default transport is optional on the gateway | the hive data PVC is mounted at a `sub_path` through the gateway's Kubernetes `driver_config`, exactly as the `job` runtime shares it. Needs `allow_driver_config = true` on the gateway and the claim labelled `openshell.ai/sandbox-attachable` |
| `policy_profile` selects a gateway-side policy | no create-time policy-profile API; OpenShell "profiles" are credential-provider profiles | the policy is derived by hive (`OpenShellNetworkPolicy`); `policy_profile` is recorded on the sandbox as the annotation `hive.dev/policy-profile` for gateway admission to act on |
| A static egress allowlist to translate | none exists: ADR-0002 redirects all `:443` to the proxy and decides per request | rules for `github.com` (read-write, git fetch-pack) and `api.github.com` (read-only), plus `agent_sandbox.openshell.allow_hosts`; attached to an overridable default binary set because OpenShell allows nothing for an unlisted binary |

The preconditions above are still the operator's to verify; phase 1 refuses a
kick when `gateway_url` or `workspace_claim` is missing and warns at boot, but
cannot see the kernel, the CNI or the gateway settings from inside the hive
pod (#17644 adds a gateway preflight). The kernel survey for the clusters hive
runs on, read on 2026-11-10: LKE (`lke648397`, `hive-ci`) run 6.12 on Debian 13
with Calico and qualify; OKE UEK7 (5.15), and the RHCOS 9.x OpenShift clusters
(5.14) do not.

## Implementation notes (phase 2: Agent Substrate)

Phase 2 (#17695, hivecommons/hive#17741) adds a **fourth** runtime,
`sandbox.runtime: substrate`, behind the same `sandbox.Launcher` seam. It is
[Agent Substrate](https://github.com/agent-substrate/substrate) (CNCF Sandbox,
donated by Google, Apache-2.0): a Kubernetes-hosted *actor* runtime where an
`ActorTemplate` (image, command, env, limits, volumes) is instantiated as an
`Actor` through a gRPC Control API (`ate.control.v1`), scheduled on a
`WorkerPool` whose pods run the workload under gVisor (`runsc`, class
`gvisor`) or in a KVM microVM (Kata on Cloud Hypervisor, class `microvm`). All
egress leaves through `atenet-router` under an `EgressPolicy`, and every actor
holds a SPIFFE identity issued through Kubernetes pod certificates. It was not
in the 2026-10-25 comparison; the write-up that led here is the Substrate
vs. crux research note, and the operator view is in
[sandbox-isolation.md](../sandbox-isolation.md#running-sandboxed-kicks-in-a-substrate-actor)
and [deploy/substrate.md](../../deploy/substrate.md). Like OpenShell it is
opt-in and inert unless selected.

### How it maps onto the contract

| Contract item | Substrate | What phase 2 does |
|---|---|---|
| Kernel-enforced confinement | gVisor user-space kernel or a microVM; worker pods are `privileged: false` but keep `SYS_ADMIN`/`NET_ADMIN` | selects the class through `worker_selector`; no change to hive |
| Default-deny egress | `atenet-egress` `EgressPolicy` per actor; rules are TLS-passthrough or terminate per host:port, no per-method read-only mode | `SubstrateEgressPolicy`: `github.com` and `api.github.com` on 443 plus `sandbox.substrate.allow_hosts`, TLS passthrough; everything else (and all UDP) denied |
| Credentials never in the sandbox | zero-credential model: actors have a SPIFFE id, credentials are injected at the router | actors run credential-free: no `HTTPS_PROXY`/MITM broker inside the actor and no GitHub credential enters it; hive does every push and GitHub write after the kick; Substrate's injection is **not** used (see the amendment below) |
| Workspace in, commits out | `durableDir` volumes and object-storage snapshots; no exec/attach API | an in-actor HTTP **bridge** (`crux substrate-bridge`) reached through the router's mTLS `CONNECT` tunnel: push tar → `POST /run` → pull tar; the hive-side workspace stays authoritative |
| Cleanup and timeouts | `DeleteActor` / `SuspendActor` / `DeleteActorTemplate` | one template + actor per kick, both deleted on success, failure and timeout (fresh bounded context, `ExitCode -1`, 16 MiB cap, as `job`/`openshell`) |

### Deviations from Substrate's reference model

| Substrate assumes | Phase 2 does |
|---|---|
| long-lived actors and reusable templates | per-kick `ActorTemplate` + `Actor`, named `hive-kick-<agent>-<unix>` |
| Go client `go.ate.dev/substrate` | **not imported**: it needs Go 1.27 (hive is on 1.26) and a very large module graph. A trimmed, wire-compatible proto subset is generated in-repo (`crux/sandbox/substrateapi`, field numbers pinned by tests); zero new Go modules, +578 KB on the `crux` binary |
| image by tag or digest | digest-pinned `sandbox.substrate.image` only, enforced at boot (`AgentSandboxGateWarnings`) and at kick |
| no `policy_profile` concept | passed to the actor as `HIVE_POLICY_PROFILE`; Substrate has no annotations |
| snapshot/resume as the normal lifecycle | `snapshot_on_idle: true` suspends (snapshot to object storage) instead of deleting; only the latest suspended actor per agent per hive process is retained (the previous one is deleted on the next suspend; actors from before a restart are not reclaimed); resume-on-next-kick is not implemented (#17697) |
| `api.github.com` read-only | TLS passthrough like `github.com`; the actor holds no token and hive does every write after the kick |

### Amendment (2026-11-12, #17695): credential-free actors

Decision by the hive owner on #17695, option A:

- Substrate actors run **credential-free**. There is no `HTTPS_PROXY` or MITM
  broker inside the actor and no GitHub credential enters it; the actor's
  egress is governed only by Substrate's `EgressPolicy`.
- Hive brokers pushes **post-kick**: it performs git pushes and GitHub writes
  itself, from the workspace the bridge returns.
- **Phase 3 (future)**: SVID-authenticated broker delegation over the atenet
  mTLS channel, so hive can delegate scoped GitHub writes to the actor without
  a credential entering it. It is gated on hosted clusters serving the podcert
  feature gates (`PodCertificateRequest` / `ClusterTrustBundle`,
  `certificates.k8s.io/v1beta1`) and is tracked as a child issue of #17695.

This supersedes the earlier statement that hive's credential brokering is
unchanged for Substrate. The podman, `job` and `openshell` runtimes keep
their proxy model (ADR-0002).

### Cluster prerequisites and which hive clusters qualify

Substrate's real requirement is **not** a kernel floor: the gVisor class runs
`runsc` inside the worker pod on any standard Linux node with cgroup v2; the
microVM class needs `/dev/kvm` (nested virtualisation on cloud shapes). Both
need the operator (`ate-setup deploy ate-system`: Control API, `atelet`
DaemonSet, `atenet-router`, podcert controller, PostgreSQL, snapshot object
storage) and Kubernetes with the `ClusterTrustBundle`,
`ClusterTrustBundleProjection` and `PodCertificateRequest` feature gates — still
off by default as of 1.36 — because actor identities are pod certificates.
Survey on 2026-11-11:

| cluster | kernel / Kubernetes | `gvisor` class | `microvm` class |
|---|---|---|---|
| hive-oke | OL9, UEK8 6.12, k8s 1.34.10 | yes once the feature gates are on (OKE control plane: needs a support request or a self-managed control plane) | needs KVM-capable (bare-metal or nested-virt) shapes |
| lke648397, hive-ci | Debian 13, 6.12 | yes; LKE exposes no control-plane flags, so the podcert gates are the blocker — check with `kubectl get --raw /metrics \| grep kubernetes_feature_enabled` | KVM to confirm per plan |
| vllm-d, a-ks-wec2 | RHCOS 9.x, 5.14 | possible: the kernel is fine, but the worker's `SYS_ADMIN`/`NET_ADMIN` needs an SCC and the gates a `FeatureGate` CR (TechPreviewNoUpgrade) | only on KVM-capable nodes |

### When to choose it over OpenShell

- Choose **OpenShell** when the cluster already runs a ≥ 6.2 kernel with an
  enforcing CNI and you want per-binary, per-method egress policy and the
  shared-PVC workspace with no image changes.
- Choose **Substrate** when you want a stronger boundary than Landlock +
  seccomp (gVisor or a VM), snapshot/suspend of idle agents, or SPIFFE
  identities for the sandboxed workload, and you can run its operator,
  PostgreSQL and object storage. Its egress policy is coarser (host:port, no
  read-only API mode) and the workspace is copied, not shared.

## Consequences

- Hive gains a sandbox boundary that is checked before the agent runs, without
  privileged pods. The cost is three cluster prerequisites: the controller, an
  enforcing CNI, and a 6.2+ kernel. Those turn into hub provisioning and
  preflight work.
- For sandboxed kicks under `openshell`, ADR-0002's iptables and UID mechanism
  is replaced by OpenShell network policy. ADR-0002 still governs the hive pod
  and tmux agents. A change to the OpenShell policy file for hive is a policy
  change and goes through the same maintainer-approval gate as proxy rules.
- OpenShell is pre-1.0 and ships often. Pin the gateway, supervisor and sandbox
  images by digest, upgrade through Renovate, and run its prover in CI against
  hive's policy.
- Telemetry must be disabled explicitly. A preflight check should refuse a
  gateway that reports it enabled.
- Substrate (phase 2) adds a second operator-side stack (Control API,
  atelet, atenet-router, PostgreSQL, object storage) and a copied workspace;
  the hive-side credential model is untouched. Its preflight (#17698) and
  prototype (#17697) follow the OpenShell ones.
- If the prototype fails a gate, this record is rewritten to decide "no
  change" with the measured reason, and the Podman and Job runtimes remain the
  only ones.

Refs #2804, #6311, #13806, #14092, #17642, #17695.
