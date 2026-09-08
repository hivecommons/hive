# Self-Hosted Hive and `CAP_NET_ADMIN`

A self-hosted / unmanaged Hive spoke **runs without** the `NET_ADMIN` Linux
capability — the container starts normally either way. But `NET_ADMIN` is what
activates the **full forced-proxy-egress security gate**. Grant it to get the
complete gate; without it the spoke runs in a **degraded (best-effort) egress
mode** described below.

> Historical note: earlier builds shipped `/usr/local/bin/hive` with a
> `cap_net_admin+ep` **file capability**. The kernel refuses to `execve()` a
> binary whose file capabilities it cannot grant from the container's bounding
> set, and `NET_ADMIN` is not in the default bounding set for docker / podman /
> containerd / k3s — so those builds **crash-looped** on self-hosted spokes with
> a bare `exec ... Operation not permitted` (EPERM). That file capability has
> been **removed** (see #3760/#3794): the binary now carries no
> `security.capability` xattr and execs everywhere. `NET_ADMIN` is instead raised
> at runtime as an **ambient** capability by the entrypoint, gated on the
> bounding set actually having it. The crash-loop is gone; what remains is the
> choice between the full gate and the degraded mode.

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

The **managed fleet** always gets the full gate: its pods are granted
`NET_ADMIN` via an SCC / PodSecurity grant (see
`src/deploy/k8s/deployment.yaml`), so the entrypoint's ambient raise fires.

## Full gate vs. degraded mode

| Deployment | `NET_ADMIN`? | Behavior |
|---|---|---|
| Managed k8s / OpenShift (SCC or `securityContext`) | yes | Entrypoint raises it ambiently → SO_MARK works → **full forced-proxy-egress gate**. |
| Self-hosted docker / podman **with** `--cap-add NET_ADMIN` | yes | Same as above → **full gate**. |
| Self-hosted docker / podman **without** the cap | no | Container **runs** (no crash). SO_MARK exemption unavailable → egress gate is **degraded / best-effort**: on OKE the owner-UID exemption still applies, but on OpenShift/OVN the proxy's own dials are not mark-exempted. |
| Rootless podman | effectively no | Rootless cannot grant `NET_ADMIN` meaningfully, so it runs in the **degraded mode** above. |

"Degraded" means the proxy's own upstream dials rely solely on the owner-UID
iptables exemption; where that match is unavailable the proxy dials **unmarked**
(the Go proxy logs this once and continues — it does not fail). Agents' forced
egress through the proxy is still installed; only the proxy's *self*-exemption is
best-effort.

## When the container refuses to start (exit 77)

The degraded mode above is about the **SO_MARK self-exemption** only, and it
never stops the container from starting. A *separate*, earlier check —
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
start anyway in advisory-only mode (see [security-model.md](security-model.md#forced-proxy-egress-and-cap_net_admin)),
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
(for example `/etc/modules-load.d/hive-netfilter.conf` containing `xt_owner`
and `xt_REDIRECT`). Until then, taint or label the node so hive pods are not
scheduled onto it. `HIVE_PROXY_ADVISORY_OK=true` remains the explicit opt-out
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

```bash
docker run --cap-add NET_ADMIN ... ghcr.io/hivecommons/hive:<tag>
# or
podman run --cap-add NET_ADMIN ... ghcr.io/hivecommons/hive:<tag>
```

### Kubernetes / k3s

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

Rootless Podman cannot grant `NET_ADMIN` in a way the kernel will honor, so a
rootless self-hosted spoke runs in the **degraded egress mode** above. The
container still starts and operates; it simply lacks the SO_MARK self-exemption.
