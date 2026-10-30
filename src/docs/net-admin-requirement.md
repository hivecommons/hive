# Self-Hosted Hive and `CAP_NET_ADMIN`

**For self-hosted operators:** Hive normally refuses to start if it cannot
install the traffic rules that force agents through its checking proxy. It
needs the Linux `NET_ADMIN` capability, working netfilter support, and the
iptables tools. Merely removing an old binary's file capability does not
remove that startup requirement.

If you explicitly set `HIVE_PROXY_ADVISORY_OK=true`, startup may continue with
network enforcement **advisory-only**: agents can bypass the proxy. This is a
security opt-out, not a way to grant a missing capability, and it does not
automatically lower your configured ACMM level.

## Full gate vs. degraded mode

| What the runtime can establish | Default result | What to do |
|---|---|---|
| Required IPv4 redirect and IPv6 gate, plus the proxy's own connection exemption | Enforced egress: agents' HTTPS traffic passes through the proxy | Use the [full-gate setup](#how-to-get-the-full-gate) for your runtime. |
| Required traffic rules, but the proxy cannot mark its own connections | Separate **self-exemption degradation**; it relies on the optional owner-UID exemption | Inspect runtime support. Where owner matching is absent, proxy connections may fail; this is not permission to bypass required traffic rules. |
| Required traffic rules cannot be installed; no explicit opt-out | **Refuses startup**: exit 77 for missing capability or required modules; other rule-install failures generally exit 1 | Match the [error signatures](#which-exit-77-do-i-have-the-check-order-since-6003). |
| Required traffic rules cannot be installed and `HIVE_PROXY_ADVISORY_OK=true` | Starts with **unenforced, advisory-only egress** and a warning | Accept this loss of enforcement deliberately, or fix the runtime. Never treat it as the full gate. |

**Missing `NET_ADMIN` does not mean “starts in degraded mode.”** Installing
the redirect also requires that capability, so the default result is refusal
to start. Rootless Podman is subject to the same decision.

For a failed deployment, go directly to [exit-77 diagnosis](#when-the-container-refuses-to-start-exit-77)
or [capability grants](#how-to-get-the-full-gate). The kernel explanation below
is reference, not a prerequisite for choosing a deployment.

## Why `NET_ADMIN` matters

The hive process runs as a **non-root** user (`dev`). The MITM proxy uses
`CAP_NET_ADMIN` to `setsockopt(SO_MARK)` on its **own** upstream dials, which is
how the proxy's traffic is exempted from the **forced-proxy-egress** gate: the
entrypoint installs an iptables `REDIRECT` of all outbound `:443` through the
proxy (and, since #4319, an `ip6tables` `REJECT` of outbound IPv6 `:443` with
the same exemptions, so a dual-stack network cannot carry agent traffic around
the redirect — the proxy listens on `127.0.0.1` only, so the IPv6 family is
closed rather than redirected), and on OpenShift/OVN — where the `-m owner` UID match is unavailable —
the `SO_MARK` packet mark is the **only** self-exemption. `setsockopt(SO_MARK)`
requires `CAP_NET_ADMIN` in the calling process's **effective** set.

The entrypoint delivers that capability by reading its own capability bounding
set (`CapBnd` in `/proc/self/status`) at the privilege drop and, **only when
`CAP_NET_ADMIN` (capability number 12) is present**, dropping to `dev` via
`setpriv --ambient-caps +net_admin …` so the hive process inherits `NET_ADMIN`
in its effective + ambient set. When the bounding set lacks it, the entrypoint
does **not** attempt the raise (it would error) and drops to `dev` via plain
`gosu`, printing a one-line notice that the SO_MARK egress exemption is
unavailable.

> This is a **capability bounding-set** distinction, not a seccomp or AppArmor
> filter — `--security-opt seccomp=unconfined` / `apparmor=unconfined` do not
> change it.

The **managed fleet** is configured to request the full gate: its pods are granted
`NET_ADMIN` via an SCC / PodSecurity grant (see
`src/deploy/k8s/deployment.yaml`), so the entrypoint's ambient raise fires.

### Historical file-capability failure

Earlier builds gave `/usr/local/bin/hive` a `cap_net_admin+ep` file capability.
If the runtime's **bounding set** (the maximum capabilities it can grant) did
not include it, the kernel refused to execute the binary with `Operation not
permitted` (EPERM). That file capability was removed (#3760/#3794). The
entrypoint now raises the capability at runtime only when permitted.
This fixes that old execute-time failure, **not** the separate fail-closed
traffic-rule check.

Reference vocabulary: an **effective** capability is usable now; an **ambient**
capability survives the controlled privilege drop. `SO_MARK` marks a socket's
packets so the proxy can exempt its own connections. Netfilter is Linux's
packet-filtering system. **SCC** is OpenShift's Security Context Constraint;
**OVN** is its network layer; **OKE** is Oracle Kubernetes Engine; **RHCOS** is
Red Hat CoreOS. These platform details do not change the startup decision.

## When the container refuses to start (exit 77)

Self-exemption degradation is about the **SO_MARK self-exemption** only. A
*separate*, earlier check —
whether the `iptables` **REDIRECT** that forces agent egress through the proxy
could be installed at all — is fail-closed: without it, the whole ACMM
capability model is advisory-only (an agent holding a raw token could bypass
the proxy entirely), so the entrypoint refuses to start rather than run with
unenforced egress. The same rule applies when a chain is created but a
non-optional append fails: the entrypoint logs the exact iptables stderr,
flushes the partial `HIVE_PROXY` chain, and then follows the explicit
fail-closed / `HIVE_PROXY_ADVISORY_OK=true` decision. You'll see:

```
[entrypoint] FATAL: could not establish forced proxy egress (iptables redirect). …
[entrypoint] FATAL: refusing to start. Grant NET_ADMIN + install iptables, or set HIVE_PROXY_ADVISORY_OK=true to deliberately run in advisory mode.
```

Installing that redirect itself needs `CAP_NET_ADMIN` (netfilter chain
manipulation), so on a container whose bounding set lacks it, this is really
the *same* missing-capability condition — just hit at the point where the
entrypoint has decided it cannot safely continue rather than degrade. The
entrypoint exits with a distinct code for exactly this case, **77** (sysexits.h's
`EX_NOPERM`, "permission denied"; `EXIT_NET_ADMIN_REQUIRED` in
`src/deploy/entrypoint.sh`), instead of the generic `1` every other
startup failure in this script uses — so a supervisor, `docker inspect
--format '{{.State.ExitCode}}'`, or a Kubernetes `lastState.terminated.exitCode`
can identify "the node/runtime cannot enforce egress here" programmatically,
without parsing the log. Exit 77 has exactly **two** causes — the missing
capability above and the missing kernel modules below; the FATAL lines name
which one you have. Any other cause of the same FATAL (missing `iptables`
binary, persistent netfilter lock contention) still exits `1`, since neither
fix below would help those.

The escape hatch is the same as always: set `HIVE_PROXY_ADVISORY_OK=true` to
start anyway in advisory-only mode (see [security-model.md](security-model.md#5-network-and-credentials)),
or grant the capability per the section below for the full gate.

### Kernel netfilter modules (the second exit-77 cause)

`CAP_NET_ADMIN` is necessary but not sufficient: the *node's kernel* must also
have the netfilter extension modules the gate's rules use. Since #6003, the
entrypoint probes them before building the real ruleset, in a throwaway
`HIVE_PROXY_PREFLIGHT` chain that is never hooked into `OUTPUT` (so it can
match no traffic even mid-probe) and is unconditionally torn down:

| Module | Used for | Missing ⇒ |
|---|---|---|
| `xt_mark` | packet-mark self-exemption (`-m mark`) — the only self-exemption on OpenShift/OVN | **FATAL, exit 77** |
| `xt_REDIRECT` | the `REDIRECT` target that forces `:443` through the proxy | **FATAL, exit 77** |
| `xt_owner` | owner-UID self-exemption (`-m owner`) | WARN only — optional by design; OpenShift/OVN runs without it and the mark exemption carries the proxy's egress |

When a required module is missing, the FATAL names it explicitly:

```
[entrypoint] ERROR: netfilter REDIRECT target unavailable on this node (kernel module xt_REDIRECT): …
[entrypoint] FATAL: this node's kernel is missing netfilter module(s) required by the forced-egress gate: xt_mark, xt_REDIRECT.
[entrypoint] FATAL: without them the HIVE_PROXY chain would redirect nothing, so agents holding raw tokens could reach the network unproxied while the spoke reported healthy. Refusing to start.
```

Granting `NET_ADMIN` does **not** fix this case — the capability is already
there; the kernel simply has nothing to grant access *to*. The durable fix is
loading the modules on the node so they survive a node rebuild — on
OpenShift/RHCOS, a MachineConfig writing an `/etc/modules-load.d/` drop-in
(for example `/etc/modules-load.d/hive-netfilter.conf` containing `xt_mark`,
`xt_REDIRECT` and `xt_owner`). A MachineConfig rolling-reboots every node in the pool,
though, which is often unacceptable on a shared or GPU cluster. The no-reboot
alternative is the node-prep DaemonSet shipped at
[`src/deploy/k8s/node-prep/hive-netfilter-modules.yaml`](../deploy/k8s/node-prep/hive-netfilter-modules.yaml):

**Who runs this: a cluster administrator**, from the Hive repository root.
It creates privileged node-level workloads; a hive owner with namespace-only
access must ask the cluster administrator, not try to grant these privileges.

```bash
kubectl apply -f src/deploy/k8s/node-prep/hive-netfilter-modules.yaml
kubectl -n hive-node-prep logs -l app=hive-netfilter-modules --prefix   # "loaded xt_REDIRECT" per node
```

It runs one tiny privileged pod per worker that `modprobe`s the modules into the
host kernel (via `chroot /host`). It re-checks every five minutes, and because
it restarts with the node, it reloads the modules after a reboot. Apply it once
per cluster, not per hive. Then delete the crashlooping hive pod so it
reschedules. Used on the vllm-d cluster (2026-09-24), where one worker lacked
`xt_REDIRECT` and six lacked `xt_owner`. Until one of the two is in place,
taint or label the node so hive pods are not scheduled onto it. `HIVE_PROXY_ADVISORY_OK=true` remains the explicit opt-out
here too: the spoke starts with the gate unenforced and logs a WARN saying
agents can bypass the proxy on this node.

Why fail closed instead of installing what does append? A node in this state
once booted a spoke with a half-built `HIVE_PROXY` chain that redirected
nothing: it went mute for hours — proxy read timeouts, failed heartbeat
collections, no `git_hash` reported, so the hub never upgraded it — while
reporting healthy. A crashloop is visible; a green-but-unenforced hive is not.

### Which exit 77 do I have? (the check order since #6003)

The two causes exit through different branches of `src/deploy/entrypoint.sh`,
in this order, so the log tells them apart without guesswork:

1. **Binary selection.** `iptables-nft` is preferred, plain `iptables` is the
   fallback. Neither present: the gate is skipped, `_iptables_ok` stays false,
   and the generic FATAL at the end exits `1` (or `77` if the bounding set
   also lacks `CAP_NET_ADMIN`, since that absence explains the failure by
   itself).
2. **Netfilter extension preflight** (added by #6003, before any part of the
   real ruleset exists). The throwaway `HIVE_PROXY_PREFLIGHT` chain is
   created, `xt_mark` and `xt_REDIRECT` are probed as required and `xt_owner`
   as optional, and the chain is torn down again.
   - A required module is missing: any stale `HIVE_PROXY` chain from an
     earlier boot of the same container is flushed and deleted, the
     `FATAL:` lines above are printed (module names, why it is fatal, the
     `/etc/modules-load.d/` fix, and `exiting 77`), and the entrypoint exits
     `77` **right there**. It never reaches the chain-creation retries and
     never prints `Grant NET_ADMIN`. With `HIVE_PROXY_ADVISORY_OK=true` the
     same condition is one `WARN:` line instead, and startup continues into
     the advisory-only path.
   - The preflight chain itself cannot be created (this is what a missing
     `CAP_NET_ADMIN` looks like: `Permission denied (you must be root)`):
     one `WARN: could not create preflight chain to probe netfilter
     extensions` line, no module verdict, and the entrypoint falls through
     to step 3 to diagnose it.
3. **Real chain and ruleset.** `HIVE_PROXY` is created with up to five
   jittered retries, the owner-UID exemptions are appended best-effort, and
   the packet-mark exemption, the `:443` `REDIRECT`, and the `OUTPUT` hook are
   appended as required rules (a failure here logs the iptables stderr and
   flushes the partial chain).
4. **The fail-closed decision.** If the IPv4 redirect or the IPv6 gate did
   not establish and `HIVE_PROXY_ADVISORY_OK` is not `true`, the entrypoint
   prints `could not establish forced proxy egress` and `refusing to start.
   Grant NET_ADMIN + install iptables/ip6tables, ...`. Then, if the bounding
   set lacks `CAP_NET_ADMIN`, it adds `CAP_NET_ADMIN is not in the
   container's capability bounding set` and exits `77`;
   otherwise it exits `1`.

So the signatures are:

| You see | Cause | Exit | Fix |
|---|---|---|---|
| `FATAL: this node's kernel is missing netfilter module(s) required by the forced-egress gate: ...` (no `Grant NET_ADMIN` line, no chain-creation retries) | required `xt_*` module not loaded on the node | 77 | load the modules on the node ([above](#kernel-netfilter-modules-the-second-exit-77-cause)); granting the capability changes nothing |
| `WARN: could not create preflight chain ...`, five `chain creation attempt n/5 failed` lines, then `Grant NET_ADMIN ...` and `CAP_NET_ADMIN is not in the container's capability bounding set` | bounding set lacks `CAP_NET_ADMIN` | 77 | grant the capability ([below](#how-to-get-the-full-gate)) |
| `Grant NET_ADMIN ...` with **no** bounding-set line | capability present, something else broke (no `iptables` binary, persistent netfilter lock contention, a required append failing for a non-module reason) | 1 | read the logged iptables stderr |

Both exit-77 causes are covered by tests that run from the checkout with no
cluster: `src/deploy/test_entrypoint_xt_module_preflight.sh` asserts that a
missing `xt_REDIRECT` or `xt_mark` exits exactly 77, names the module, leaves
no `HIVE_PROXY` chain behind, and that `HIVE_PROXY_ADVISORY_OK=true` turns it
into a warning; the Podman rootful and rootless CI lanes
(`probe_podman_rootful_netadmin.sh`, `probe_podman_rootless_netadmin.sh`)
assert the capability cause end to end against a real container, and the
same `--cap-add NET_ADMIN` case installing the `REDIRECT` rule shows those
runners have the modules.

## How to get the full gate

### Docker / Podman (rootful)

**Who runs this:** the host/container-runtime operator. These are capability
fragments, not complete installation commands; retain the configuration,
storage, and authentication options from your runtime quickstart.

```bash
docker run --cap-add NET_ADMIN ... ghcr.io/hivecommons/hive:<tag>
# or
podman run --cap-add NET_ADMIN ... ghcr.io/hivecommons/hive:<tag>
```

### Kubernetes / k3s

**Who runs this:** the workload operator edits the Deployment; a cluster
administrator must approve any required admission-policy exception.

Add `NET_ADMIN` to the hive container's `securityContext`:

```yaml
securityContext:
  capabilities:
    add:
      - NET_ADMIN
```

(The bundled `src/deploy/k8s/deployment.yaml` already declares this.) If the
namespace enforces Pod Security admission, note that the `baseline` and
`restricted` profiles deny `NET_ADMIN` — the namespace needs `privileged`
enforcement (or none) for the request to be honored.

### OpenShift

Declaring the capability is not enough on OpenShift: an SCC must *permit*
adding it, and no stock SCC (`anyuid` included) does — the pod is rejected at
admission (`unable to validate against any security context constraint`). A
cluster-admin applies the bundled
[`overlays/openshift-netadmin`](https://github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/openshift-netadmin)
overlay once:

```bash
oc apply -k src/deploy/kustomize/overlays/openshift-netadmin
```

It creates a dedicated `hive-netadmin` SCC — `restricted-v2` loosened by
exactly the capabilities the deployment declares, not `privileged` — plus the
`use` RBAC scoped to the `hive` ServiceAccount. To check ahead of time whether
your cluster already grants the capability (and to read the exit-77 signature
after the fact), see the
[Does your cluster grant NET_ADMIN?](manual-provisioning.md#does-your-cluster-grant-net_admin)
checks in the provisioning guide.

### Rootless Podman

Rootless Podman cannot grant the host/network capability needed here. If it
cannot establish the required egress rules, startup is refused by default.
Only the explicit `HIVE_PROXY_ADVISORY_OK=true` opt-out permits advisory-only
egress; it does not supply the missing enforcement or merely degrade the
proxy's self-exemption. Choose rootful execution with the required capability
for the full gate, or knowingly accept the advisory-only boundary described
in the [Podman quickstart](quickstart-podman.md).
