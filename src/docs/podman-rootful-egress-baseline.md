# Rootful Podman egress-gate baseline

## Result

Rootful Podman is a clean baseline for Hive's forced-proxy egress gate. It
behaves exactly as the enforcing standalone path expects, and in the same three
states the rootless spike measured:

| Case | Container | Gate installed | Exit |
| --- | --- | --- | --- |
| Default rootful | refuses to start | no | **77** (`EX_NOPERM`) |
| Rootful + `--cap-add NET_ADMIN` | starts and stays up | **yes** | still running at the cap |
| Rootful + `HIVE_PROXY_ADVISORY_OK=true` | starts and stays up | no — advisory accepted | still running at the cap |

Rootful does **not** hand out `CAP_NET_ADMIN` by default, so the fail-closed
branch is reached for a real reason rather than by accident:

```
default    CapBnd=00000000800405fb   NET_ADMIN=no
--cap-add  CapBnd=00000000800415fb   NET_ADMIN=yes
```

Bit 12 (`0x1000`) is `CAP_NET_ADMIN` — the same bit the entrypoint tests.

This makes the rootless result in
[podman-rootless-startup-spike.md](podman-rootless-startup-spike.md)
comparable: rootless matched rootful on all three cases, so rootless is not
weaker than the baseline on this configuration.

## Environment

| | |
| --- | --- |
| Podman | 5.8.4, **rootful** |
| OCI runtime | crun 1.28 |
| Network backend | netavark (bridge; the container has its own netns) |
| cgroups | v2 |
| Kernel / host | 7.1.4-200.fc44.x86_64, Aurora 44.20260815.1 (Kinoite), SELinux enforcing |
| Image | `ghcr.io/hivecommons/hive:v4-latest`, digest `sha256:654d835632ab1e813a3157c901319056f85c62417cc63e95e2764ee29c94adfa` |
| Architecture | `amd64` only |

The image digest differs from the one the rootless spike recorded
(`sha256:e1479d76…`); `v4-latest` moved between the two runs. The comparison
above is therefore across two builds of the same tag, not one artifact.

### Storage safety

Rootful Podman on a workstation is usually where the long-lived services live,
so every Podman call in the probe is pinned to a throwaway `--root`/`--runroot`
under `/var/tmp`. The host's rootful store was never addressed, and the probe
refuses to run if the store path resolves to `/var/lib/containers`,
`/run/containers`, or the host's reported `graphRoot`. Cleanup removes only the
containers the probe created, by exact name; it never prunes.

Verified before the first container started: the host store listed 11
containers, the probe's store listed 0.

## Reproducing

`src/deploy/probe_podman_rootful_netadmin.sh` runs the whole matrix and must be
run as root:

```bash
sudo src/deploy/probe_podman_rootful_netadmin.sh --somark
```

It creates its own throwaway store by default, removes only the containers it
created, and exits non-zero if any case stops matching the contract recorded
here. A store passed with `--store` is kept, so repeated runs do not re-pull the
image. It stops with `EX_CONFIG` (78) and names the missing prerequisite if
Podman is absent, is not rootful, or the image cannot be pulled — it never falls
back to Docker.

No Docker configuration was read or changed by any part of this spike.

## Evidence: what the gate actually installs

Under `--cap-add NET_ADMIN` the entrypoint reports:

```
[entrypoint] iptables (iptables-nft): outbound :443 -> :18443 (proxy UID 1001 + egress mark 0x1112 exempt)
[entrypoint] Dropping to dev user (ambient CAP_NET_ADMIN granted for proxy SO_MARK egress-gate)
```

and the chain in the container's own network namespace is:

```
-N HIVE_PROXY
-A HIVE_PROXY -m owner --uid-owner 0 -j RETURN
-A HIVE_PROXY -m owner --uid-owner 1001 -j RETURN
-A HIVE_PROXY -m mark --mark 0x1112 -j RETURN
-A HIVE_PROXY -p tcp -m tcp --dport 443 -j REDIRECT --to-ports 18443
```

Both the redirect and the capability propagation across the privilege drop are
therefore observed, not inferred: the ambient-`CAP_NET_ADMIN` line is printed by
the process that has already dropped to `dev`.

## `SO_MARK` isolated

The rootless spike could not say which exemption carried the proxy's own
traffic, because the owner-UID rule and the mark rule were both in the chain.
Deleting the two owner `RETURN`s leaves the mark as the only exemption — the
OpenShift/OVN shape, where `xt_owner` is absent.

| | owner rules present | owner `RETURN`s removed |
| --- | --- | --- |
| unmarked dial as UID 1001 (control) | `http_code=200` | `http_code=000` |
| agent request through the proxy | `http_code=200` | **`http_code=200`** |

The control is what makes this readable. An exec'd `curl` carries no packet
mark — `SO_MARK` is set by the proxy's own dialer in
`src/pkg/proxy/somark_linux.go`, and setting it needs `CAP_NET_ADMIN`, which an
exec'd process does not inherit. So the control measures the *owner* rule only:
its `000` after deletion proves the owner exemption is genuinely gone and the
`REDIRECT` is live. The agent request, by contrast, is redirected *into* the
proxy and forces the proxy's own upstream dial — the one that does carry the
mark. That still returning 200 is the mark path working alone.

**This does not retire the #2678 regression history.** That failure was
`SO_MARK` not reliably sticking to the proxy's sockets on OKE, which is a
property of that platform, not of the rule. The result here is that the mark
exemption is correct and sufficient *on this kernel and network backend*. Both
exemptions should stay.

## What remains unproven

- **IPv6 — now ANSWERED, and the bypass was real.** The gate was IPv4-only: the
  entrypoint had no `ip6tables` path at all. On this configuration the container
  got no IPv6 address, so there was nothing to bypass here. Measured since on a
  dual-stack container network: 5 agent connections to `:443` over IPv6 produced
  **0** redirects while 5 over IPv4 produced **5**, in the same run. See
  [The IPv4-only egress gate is bypassable over IPv6](podman-ipv6-egress-bypass.md).
  *Fixed in #4327:* the entrypoint now closes the IPv6 family with an
  `ip6tables` filter-table `REJECT` carrying the same three exemptions (the
  proxy listens on `127.0.0.1` only, so a v6 `REDIRECT` had nowhere to
  deliver); `src/deploy/probe_podman_ipv6_egress.sh` observes the dual-stack
  case on a Linux host.
- **Kernels without `xt_owner`.** Deleting the owner rules emulates the shape
  but not the kernel. This host has `xt_owner`.
- **Restart, reboot, and recreate.** Single `podman run` only; nothing about
  whether the gate re-installs reliably across a restart.
- **The gateway and the pod topology.** One container in isolation — nothing
  about publishing 3001, withholding 7681, or two containers sharing a network.
- **`arm64`.** `amd64` only.
- **A locally built image.** The published `v4-latest` was probed, not a
  `src/Dockerfile` build.
- **An agent setting its own mark.** Agents drop to unprivileged UIDs without
  ambient `CAP_NET_ADMIN`, so they should not be able to stamp `0x1112` and
  exempt themselves. That is argued from the capability model here, not measured.

## References

- [Rootless Podman startup and exit-77 behavior](podman-rootless-startup-spike.md) — the rootless counterpart (#4199).
- [Podman support matrix](podman-support-matrix.md) — the support decision this baseline feeds: rootful + enforcing is the **supported** reference cell.
- `src/deploy/probe_podman_rootful_netadmin.sh` — the probe that produces every number above.
- `src/deploy/entrypoint.sh` — the gate itself, and the `#2678` regression history in its comments.
- `src/pkg/proxy/somark_linux.go` — where `SO_MARK` is stamped.
