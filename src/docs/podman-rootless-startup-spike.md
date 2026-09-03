# Rootless Podman startup and exit-77 behavior

## Result

Rootless Podman with `--cap-add NET_ADMIN` **does** install Hive's forced-proxy
egress gate, and the gate demonstrably intercepts agent traffic. The
fail-closed contract also holds: without the capability the container exits 77
rather than running unenforced.

That is stronger than #4188's blocking design gate assumed. It is not, on its
own, a decision that rootless Podman is first-class — see
[What remains unproven](#what-remains-unproven). It does mean option 1
("rootful first, rootless experimental") is not forced by the egress gate on
this configuration.

| Case | Container | Gate installed | Exit |
| --- | --- | --- | --- |
| Default rootless | refuses to start | no | **77** (`EX_NOPERM`) |
| Rootless + `--cap-add NET_ADMIN` | starts and stays up | **yes** | still running at the cap |
| Rootless + `HIVE_PROXY_ADVISORY_OK=true` | starts and stays up | no — advisory accepted | still running at the cap |

Started and enforcing are different questions, and the probe reports them
separately in every case. The `--cap-add NET_ADMIN` run initially exited 1 with
`validating config: project.org is required` — a config error **after** a gate
that had already installed correctly. Reading that exit code as a gate failure
would have been wrong.

## Environment

| | |
| --- | --- |
| Podman | 5.8.4, rootless |
| OCI runtime | crun 1.28 |
| Network backend | netavark, rootless helper `pasta` |
| cgroups | v2 |
| Kernel / host | 7.1.4-200.fc44.x86_64, Aurora (Fedora) 44.20260815.1, SELinux enforcing |
| Image | `ghcr.io/hivecommons/hive:v4-latest`, digest `sha256:e1479d76c453cdd8271be76913a1544d5b70fd3569adebdd388bdd4632b4a263` |
| Architecture | `amd64` only |

Every container, image, and volume went into a throwaway store with a private
graphroot and runroot. The host's own Podman storage was not written to and was
verified untouched afterwards.

## Reproducing

`src/deploy/probe_podman_rootless_netadmin.sh` runs the whole matrix:

```bash
src/deploy/probe_podman_rootless_netadmin.sh --bypass
```

CI runs exactly this, per pull request, on `ubuntu-latest`:
`.github/workflows/podman-rootless-lane.yml` (#4334). A case that stops
matching the contract recorded here fails that job.

It creates its own throwaway store by default, removes only the containers it
created, by name, and exits non-zero if any case stops matching the contract
recorded here. It deletes its own store with `podman unshare rm -rf`, because
files the containers wrote are owned by mapped subordinate UIDs and a plain
`rm -rf` cannot remove them. A store passed with `--store` is kept, and needs
the same `podman unshare rm -rf` when you are done with it. It stops with `EX_CONFIG` (78) and names the missing
prerequisite if Podman is absent, is not rootless, or the image cannot be
pulled — it never falls back to Docker.

## Capability bounding set

CAP_NET_ADMIN is capability 12, so the entrypoint tests `CapBnd & 0x1000`:

```
default          CapBnd=00000000800405fb   NET_ADMIN=no
--cap-add        CapBnd=00000000800415fb   NET_ADMIN=yes
```

Rootless Podman does not grant NET_ADMIN by default, and `--cap-add NET_ADMIN`
does put it in the bounding set. The capability is scoped to the container's
user and network namespaces, which is exactly why it is sufficient here: the
netfilter tables it manipulates belong to the container's own network
namespace.

Confirmed directly, outside the entrypoint:

```
$ podman run --rm --cap-add NET_ADMIN --entrypoint /bin/sh IMAGE \
    -c 'iptables-nft -t nat -N HIVE_PROBE && echo OK'
OK
$ podman run --rm --entrypoint /bin/sh IMAGE -c 'iptables-nft -t nat -N HIVE_PROBE'
iptables v1.8.11 (nf_tables): Could not fetch rule set generation id:
Permission denied (you must be root)
```

## Case 1 — default rootless: exit 77

```
[entrypoint] WARN: iptables chain creation attempt 1/5 failed: iptables v1.8.11 (nf_tables):
  Could not fetch rule set generation id: Permission denied (you must be root) — retrying
  ... attempts 2/5 through 5/5 ...
[entrypoint] ERROR: iptables chain creation failed after 5 attempts: ...
[entrypoint] FATAL: could not establish forced proxy egress (iptables redirect). The ACMM
  capability model would be advisory-only, allowing agents with raw tokens to bypass the
  MITM proxy.
[entrypoint] FATAL: refusing to start. Grant NET_ADMIN + install iptables, or set
  HIVE_PROXY_ADVISORY_OK=true to deliberately run in advisory mode.
[entrypoint] FATAL: CAP_NET_ADMIN is not in the container's capability bounding set —
  exiting 77 (EX_NOPERM) rather than 1.
```

Exit status 77. The whole run was 21 log lines. The five retries cost real
startup time for a condition that the bounding-set probe already knows is
hopeless — a small improvement candidate, not a defect.

## Case 2 — rootless + `--cap-add NET_ADMIN`: the gate installs

```
[entrypoint] iptables (iptables-nft): outbound :443 -> :18443
  (proxy UID 1001 + egress mark 0x1112 exempt)
[entrypoint] Dropping to dev user (ambient CAP_NET_ADMIN granted for proxy SO_MARK egress-gate)
```

The rules really are in the container's nat table:

```
-N HIVE_PROXY
-A OUTPUT -j HIVE_PROXY
-A HIVE_PROXY -m owner --uid-owner 0 -j RETURN
-A HIVE_PROXY -m owner --uid-owner 1001 -j RETURN
-A HIVE_PROXY -m mark --mark 0x1112 -j RETURN
-A HIVE_PROXY -p tcp -m tcp --dport 443 -j REDIRECT --to-ports 18443
```

The ambient capability survived the privilege drop, which is what the proxy's
`SO_MARK` exemption depends on. The Go process, running as uid 1001:

```
Uid:     1001  1001  1001  1001
CapEff:  0000000000001000
CapAmb:  0000000000001000
```

Bit 12 set in both — so this is not the #3874 silent-no-op path.

### Bypass resistance

From the agent UID (2001), which no `RETURN` rule exempts:

```
$ podman exec -u 2001 ... openssl s_client -connect api.github.com:443
subject=CN=api.github.com
issuer=CN=Hive ACMM Proxy CA
```

The agent's TLS session was terminated by Hive's own MITM proxy, not GitHub.
`curl` from the same UID fails closed on certificate validation
(`self-signed certificate in certificate chain`) rather than reaching GitHub.

From the exempt proxy UID (1001):

```
http_code=200 remote=140.82.114.5:443
```

So the redirect intercepts unexempt traffic and the self-exemption works.

## Case 3 — deliberate advisory mode

```
[entrypoint] WARN: proxy egress enforcement is ADVISORY-ONLY (HIVE_PROXY_ADVISORY_OK=true set).
  Agents can bypass the MITM proxy — capability model is NOT enforced.
[entrypoint] NOTICE: CAP_NET_ADMIN is not in the bounding set — the forced-proxy egress
  exemption (SO_MARK) is unavailable. Owner-UID backstop: ABSENT (no xt_owner on this kernel)
  — the proxy has NO self-exemption; outbound :443 from the proxy will loop back into the
  redirect. ...
```

The container started and reached steady state, and the agent audit line
records the reduced posture explicitly:

```json
{"msg":"audit: agent started","name":"probe","backend":"claude","mode":"ADVISORY"}
```

Advisory mode is never silent and never claims to be enforcing, which matches
what #4188 requires of it.

### One diagnostic is wrong in this mode

`no xt_owner on this kernel` is a misdiagnosis here. The same kernel loaded
`-m owner --uid-owner` rules without complaint in case 2. The check behind that
message is:

```sh
if iptables -t nat -C HIVE_PROXY -m owner --uid-owner "$PROXY_UID" -j RETURN 2>/dev/null
```

In advisory mode the `HIVE_PROXY` chain was never created, because creating it
is exactly what failed. So the `-C` probe fails for the obvious reason and the
message blames the kernel. The code comment above it says the intent was to
"report what is actually in the chain" — the wording just outruns the check.

Two candidate fixes, neither implemented here: say "chain absent — no
self-exemption is in place" when the chain does not exist, or note that in
advisory mode there is no chain for the proxy to be exempt from and skip the
backstop clause entirely. Worth a separate issue.

## What remains unproven

The result above is one configuration. None of the following is settled:

- **`SO_MARK` specifically.** Both a UID `RETURN` and a mark `RETURN` were in
  the chain, and the proxy's dial succeeded. Which rule matched was not
  isolated, so the mark path is untested on its own. Isolating it means
  removing the owner rule and re-testing.
- **Kernels without `xt_owner`.** There the mark is the only exemption, so the
  untested path becomes the only path. This host is not that host.
- **`slirp4netns`.** Only `pasta` was exercised. The rootless helper terminates
  the container's traffic and is a plausible source of difference.
- **Restart, reboot, and recreate.** Single `podman run` only; nothing about
  whether the gate re-installs reliably across a restart.
- **The gateway and the pod topology.** One container in isolation. Nothing
  here covers publishing 3001, withholding 7681, or two containers sharing a
  network.
- **`arm64`.** `amd64` only.
- **A locally built image.** The published `v4-latest` was probed, not a
  `src/Dockerfile` build.
- **Rootful Podman.** Out of scope by design — that is #4200.

A support-matrix decision needs at least the `SO_MARK` isolation, the
`slirp4netns` lane, and restart behavior. This spike removes the assumption
that rootless cannot enforce; it does not finish the argument.

## References

- [`src/deploy/entrypoint.sh`](https://github.com/kubestellar/hive/blob/v4/src/deploy/entrypoint.sh) — `EXIT_NET_ADMIN_REQUIRED`, the bounding-set probe, and the fail-closed branch.
- [Podman support matrix](podman-support-matrix.md) — the support decision this spike feeds: rootless + enforcing started as **experimental** on this spike's evidence and was promoted to **supported** by #4487, which measured the three promotion criteria this spike named.
- [NET_ADMIN requirement](https://github.com/kubestellar/hive/blob/v4/src/docs/net-admin-requirement.md)
- [Security model](https://github.com/kubestellar/hive/blob/v4/src/docs/security-model.md)
