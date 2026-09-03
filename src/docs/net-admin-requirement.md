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
unenforced egress. You'll see:

```
[entrypoint] FATAL: could not establish forced proxy egress (iptables redirect). …
[entrypoint] FATAL: refusing to start. Grant NET_ADMIN + install iptables, or set HIVE_PROXY_ADVISORY_OK=true to deliberately run in advisory mode.
```

Installing that redirect itself needs `CAP_NET_ADMIN` (netfilter chain
manipulation), so on a container whose bounding set lacks it, this is really
the *same* missing-capability condition — just hit at the point where the
entrypoint has decided it cannot safely continue rather than degrade. The
entrypoint exits with a distinct code for exactly this case, **77** (sysexits.h's
`EX_NOPERM`, "permission denied"), instead of the generic `1` every other
startup failure in this script uses — so a supervisor, `docker inspect
--format '{{.State.ExitCode}}'`, or a Kubernetes `lastState.terminated.exitCode`
can identify "grant the capability" programmatically, without parsing the log.
Any other cause of the same FATAL (missing `iptables` binary, persistent
netfilter lock contention) still exits `1`, since granting `NET_ADMIN` would
not fix those.

The escape hatch is the same as always: set `HIVE_PROXY_ADVISORY_OK=true` to
start anyway in advisory-only mode (see [security-model.md](security-model.md#forced-proxy-egress-and-cap_net_admin)),
or grant the capability per the section below for the full gate.

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
[`overlays/openshift-netadmin`](https://github.com/kubestellar/hive/tree/v4/src/deploy/kustomize/overlays/openshift-netadmin)
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
