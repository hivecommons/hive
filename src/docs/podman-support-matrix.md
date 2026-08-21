# Podman support matrix: rootful/rootless × enforcing/advisory

What this page is: the support statement for running **standalone Hive itself**
under Podman, for each combination of rootful/rootless and enforcing/advisory.
It publishes a decision; it does not re-run or restate the measurements. Every
cell cites the spike that measured it, and anything not measured is named as
unmeasured rather than filled in by reasoning from the ruleset.

Scope note: this is Hive-under-Podman (#4188). The contributor relay's own
Podman support is separate. Nothing here changes the entrypoint, adds deployment
assets, or touches Docker — **Docker Engine and Docker Compose remain the
default and fully supported runtime**, unchanged by this document.

## The matrix

| | **Enforcing** (`--cap-add NET_ADMIN`, gate installs) | **Advisory** (`HIVE_PROXY_ADVISORY_OK=true`, gate not installed) |
| --- | --- | --- |
| **Rootful** | **Supported** — #4200 ([PR #4304](https://github.com/kubestellar/hive/pull/4304)) | **Supported as a deliberate choice, unenforced** — #4200 ([PR #4304](https://github.com/kubestellar/hive/pull/4304)) |
| **Rootless** | **Experimental** — #4199 ([PR #4280](https://github.com/kubestellar/hive/pull/4280)) | **Supported as a deliberate choice, unenforced** — #4199 ([PR #4280](https://github.com/kubestellar/hive/pull/4280)) |

There is no fifth state hiding behind the table. The remaining combination —
neither `CAP_NET_ADMIN` nor the advisory opt-in — is not a support level at all:
the container **refuses to start** and exits **77** (`EX_NOPERM`). That was
measured on both rootful and rootless, and it is the same fail-closed contract
in both. See [Fail-closed is the third state](#fail-closed-is-the-third-state).

Read the advisory column carefully. It is **not** a weaker grade of the
enforcing column and it is not a fallback — see
[Advisory mode is a choice, not a fallback](#advisory-mode-is-a-choice-not-a-fallback).

## Why each cell reads the way it does

### Rootful + enforcing — supported

The [rootful egress-gate baseline](podman-rootful-egress-baseline.md) (#4200)
measured, in one run: the `HIVE_PROXY` chain installed in the container's own
nat table; ambient `CAP_NET_ADMIN` surviving the privilege drop to `dev`, which
is what the proxy's `SO_MARK` exemption depends on; and the `SO_MARK` exemption
isolated from the owner-UID exemption by deleting the owner `RETURN`s and
re-testing with a control. That is the full enforcing claim, observed rather
than inferred.

This is the reference cell. The other three are positioned against it.

### Rootless + enforcing — experimental

The [rootless startup spike](podman-rootless-startup-spike.md) (#4199) measured
that rootless Podman with `--cap-add NET_ADMIN` **does** install the gate, and
that the gate really intercepts: an agent-UID TLS session came back with
`issuer=CN=Hive ACMM Proxy CA`, while the exempt proxy UID reached the real
upstream. That removed the assumption that rootless cannot enforce.

It did not make rootless first-class, and the spike says so itself. Three things
that are settled for rootful are not settled for rootless:

- **`SO_MARK` in isolation.** On rootless, the owner-UID `RETURN` and the mark
  `RETURN` were both in the chain and which one carried the proxy's dial was
  never separated. The isolation was done under rootful only (#4200). On a
  kernel without `xt_owner`, the unseparated path is the *only* path.
- **`slirp4netns`.** Only `pasta` was exercised. The rootless helper terminates
  the container's traffic, so it is a plausible source of difference — and it is
  a rootless-only variable with no rootful counterpart.
- **Restart, reboot, and recreate.** Single `podman run`. Applies to every cell
  (see [Carried-forward gaps](#carried-forward-gaps)), but it bites hardest
  where the posture is otherwise unproven.

**What would promote this cell to supported:** the `SO_MARK` isolation repeated
under rootless, one `slirp4netns` lane measured against the `pasta` result, and
gate re-installation observed across a container restart. That list is the
rootless spike's own, and it is the whole list — not an open-ended bar.

Until then, an operator choosing rootless + enforcing gets a posture that has
been observed to work on one configuration and has not been shown to hold on the
variations rootless specifically introduces.

### Rootful + advisory and rootless + advisory — supported as a deliberate choice, unenforced

Both spikes measured this case, and they measured the same thing. The container
starts and reaches steady state; the entrypoint prints

```
[entrypoint] WARN: proxy egress enforcement is ADVISORY-ONLY (HIVE_PROXY_ADVISORY_OK=true set).
  Agents can bypass the MITM proxy — capability model is NOT enforced.
```

and the agent audit record carries the reduced posture explicitly
(`"mode":"ADVISORY"`). Advisory mode is never silent and never claims to be
enforcing.

These cells read the same on rootful and rootless because in advisory mode the
gate is never installed, so none of the rootless-specific unknowns above are in
play. Nothing about the redirect, the ambient capability, or `SO_MARK` matters
when there is no chain.

That produces an ordering worth stating plainly, because it looks backwards:
**rootless + advisory is graded above rootless + enforcing.** It is not a
security ranking. Advisory mode makes no enforcement claim, so there is nothing
left unproven about it; rootless enforcing makes the full claim on evidence that
does not yet reach it. The grades measure how well the evidence covers the
claim, not how safe the deployment is.

One known wart, from #4199 and not fixed here: in advisory mode the entrypoint
reports `no xt_owner on this kernel`, which is a misdiagnosis — the `-C` probe
fails because the chain was never created, not because the kernel lacks the
module. The same kernel loaded owner rules in the enforcing case. The posture is
correct; the diagnostic wording outruns its check.

## Advisory mode is a choice, not a fallback

Advisory mode requires the operator to set `HIVE_PROXY_ADVISORY_OK=true`. It is
never entered automatically, and a missing `CAP_NET_ADMIN` does **not** degrade
into it — that path exits 77 instead. Nothing in this matrix should be read as
"try enforcing, fall back to advisory."

The consequence of the choice, spelled out: **agents can bypass the MITM proxy,
and the ACMM capability model is not enforced.** An agent holding a raw token
reaches the network directly. Every capability decision that depends on egress
passing through the proxy becomes a suggestion.

That is a defensible choice for a single-operator local deployment where the
agents and their credentials are already trusted. It is not a defensible choice
for a deployment running untrusted or third-party agents, or where the
capability model is doing real access-control work. Choosing it is the
operator's call; being surprised by it is not an available outcome, because the
warning is printed on every start and recorded in the audit line.

## Root-mode differences that are not about enforcement

The matrix above grades one axis: whether the egress-gate claim was measured for
a cell. An operator choosing a root mode is deciding more than that, and #4478
found a difference that the units themselves cannot show — it is invisible in
the asset, identical in both modes as written, and means two different things
depending on which manager reads it.

| | Rootful (system manager) | Rootless (user manager) |
| --- | --- | --- |
| `[Install] WantedBy=default.target` resolves to | the **boot transaction** | the *user* manager's default target, which logind reaches after the system boot has finished |
| A Hive that never becomes healthy | **holds the boot** for `TimeoutStartSec` — 5min `hive.service`, 2min `hive-gateway.service` | delays nothing but Hive |
| Measured | [lifecycle page](podman-quadlet-lifecycle.md#rootful-hive-is-inside-the-system-boot-transaction-rootless-is-not) (#4413, #4478) | same |

Both units carry the same two lines, and the Quadlet generator installs
`default.target.wants/hive.service` in **both** modes. On two real reboots of one
host, systemd's own `FinishTimestampMonotonic` on the rootful boot was **549µs**
after `hive-gateway.service` went active — the boot was not declared finished
until Hive was serving — while the rootless boot finished at 9.2s with Hive not
healthy until 18.5s.

The failure case was inferred from that and is now measured directly, in a
throwaway systemd container rather than by rebooting a host into a broken state
(`src/deploy/probe_boot_transaction_coupling.sh`): a `Type=notify` unit reached
through `default.target.wants/` that never sends READY held the boot for
**20.188s** of a 20s `TimeoutStartSec`, against **0.133s** for the same unit with
the same timeout left out of `default.target.wants/`.

**This is not a support grade and does not move a cell.** It costs availability,
not enforcement, and it is recorded here because it is the kind of difference
that decides which mode an operator picks and is otherwise not written down
anywhere they would look. Whether the units should change — `DefaultDependencies=`,
different ordering, or a shorter rootful `TimeoutStartSec` — is open in #4478 and
is deliberately not decided by this page.

## Fail-closed is the third state

| Case | Container | Gate installed | Exit |
| --- | --- | --- | --- |
| No `CAP_NET_ADMIN`, no advisory opt-in | refuses to start | no | **77** (`EX_NOPERM`) |
| `--cap-add NET_ADMIN` | starts and stays up | **yes** | still running at the cap |
| `HIVE_PROXY_ADVISORY_OK=true` | starts and stays up | no — advisory accepted | still running at the cap |

Measured identically under rootful (#4200) and rootless (#4199). Neither mode
hands out `CAP_NET_ADMIN` by default — rootful does not either, so the
fail-closed branch is reached for a real reason rather than by accident.

## The configuration all of this was measured on

Every cell above rests on one host shape. It is stated here so that a reader can
tell whether their deployment is inside or outside the evidence:

| | |
| --- | --- |
| Podman | 5.8.4 (rootful and rootless runs) |
| OCI runtime | crun 1.28 |
| Network backend | netavark; rootless helper `pasta` |
| cgroups | v2 |
| Kernel / host | 7.1.4-200.fc44.x86_64, Aurora (Fedora) 44, SELinux enforcing |
| Architecture | `amd64` only |
| Image | published `ghcr.io/kubestellar/hive` tags, not a local `src/Dockerfile` build |

This document does **not** set minimum supported Podman versions or a lifecycle
choice. That is a separate slice under #4188; 5.8.4 is what was measured, not a
floor that has been decided.

## Carried-forward gaps

These are open against every cell unless noted. They are carried by reference —
each already has a home, and none of them is closed by this document.

| Gap | Status | Where it lives |
| --- | --- | --- |
| **IPv6 egress-gate bypass** | Measured real (#4319, [PR #4321](https://github.com/kubestellar/hive/pull/4321)) and **fixed** in [PR #4327](https://github.com/kubestellar/hive/pull/4327), which closes the v6 family with an `ip6tables` filter-table `REJECT` carrying the same three exemptions. Residual: the fix was observed on a rootful, ULA-only, amd64/netavark dual-stack network — **no rootless dual-stack measurement exists**, and no globally routable IPv6 path was available to either run. | [IPv6 egress-gate bypass](podman-ipv6-egress-bypass.md) |
| **`arm64`** | Unmeasured everywhere. `amd64` only in both spikes. A hosted `ubuntu-24.04-arm` lane is identified but not built. | [Podman CI runner map](podman-ci-runner-map.md), #4336 |
| **`slirp4netns`** | Unmeasured. Only `pasta` was exercised on rootless; rootless has no other network-helper evidence. | #4199 / [rootless spike](podman-rootless-startup-spike.md) |
| **Restart, reboot, recreate** | **Narrowed, not closed.** The LIFECYCLE is now measured in both root modes — stop/start/restart/recreate (#4377) and an actual reboot of each (#4413), written up on the [lifecycle page](podman-quadlet-lifecycle.md). What those runs did not do is inspect the egress chain afterwards, so the part of this gap that belongs to this page — that the GATE re-installs reliably across a restart — is still unmeasured, and both spikes remain a single `podman run`. | #4199, #4200; [lifecycle page](podman-quadlet-lifecycle.md) (#4377, #4413) |
| **Kernels without `xt_owner`** | Unmeasured. Deleting the owner `RETURN`s under rootful emulates the shape but not the kernel. Both exemptions stay in the chain regardless. | [rootful baseline](podman-rootful-egress-baseline.md) |
| **`SO_MARK` on OKE-shaped platforms** | Not retired. The #2678 regression was the mark not sticking to the proxy's sockets on that platform — a property of the platform, not of the rule. The rootful isolation does not speak to it. | `src/pkg/proxy/somark_linux.go`, entrypoint comments |
| **Gateway and pod topology** | Unmeasured. One container in isolation in both spikes — nothing about publishing 3001, withholding 7681, or two containers sharing a network. | the gateway/network slice under #4188 |
| **Locally built image** | Unmeasured. Published tags only; a `src/Dockerfile` build was never probed. | #4199, #4200 |
| **Agent setting its own mark** | Argued from the capability model, not measured. Agents drop to unprivileged UIDs without ambient `CAP_NET_ADMIN`, so they should not be able to stamp `0x1112`. | [rootful baseline](podman-rootful-egress-baseline.md) |

A cell being **supported** means the enforcement claim for that cell was
measured — not that these gaps are closed for it.

## CI lanes that would keep this honest

The matrix is a snapshot of two manual spikes. The lanes mapped in the
[Podman CI runner map](podman-ci-runner-map.md) are what would make it a
standing guarantee rather than a dated observation: rootful (#4335) and rootless
(#4334) on hosted amd64, and the `arm64` build/pull-and-startup lane (#4336).
Neither supported cell is currently defended by CI.

## References

- [Rootful Podman egress-gate baseline](podman-rootful-egress-baseline.md) — #4200, [PR #4304](https://github.com/kubestellar/hive/pull/4304).
- [Rootless Podman startup and exit-77 behavior](podman-rootless-startup-spike.md) — #4199, [PR #4280](https://github.com/kubestellar/hive/pull/4280).
- [The IPv4-only egress gate is bypassable over IPv6](podman-ipv6-egress-bypass.md) — #4319, [PR #4321](https://github.com/kubestellar/hive/pull/4321); fixed in [PR #4327](https://github.com/kubestellar/hive/pull/4327).
- [`CAP_NET_ADMIN` requirement](net-admin-requirement.md)
- [Security model — operator guide](security-model.md)
- [Podman CI runner map](podman-ci-runner-map.md) — #4211.
- [ADR-0017: Quadlet `.container`/`.pod` units as the Podman persistent lifecycle](adr/0017-podman-quadlet-lifecycle.md) — the mechanism a supported cell gets installed with.
- [Quadlet lifecycle: stop, start, restart, recreate, and boot persistence](podman-quadlet-lifecycle.md) — #4377, #4413, and the boot-transaction difference in #4478.
