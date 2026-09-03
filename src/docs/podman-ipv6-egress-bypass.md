# The IPv4-only egress gate is bypassable over IPv6

## Result

**The bypass is real and was observed.** On a container with a working IPv6
address, agent traffic to `:443` over IPv6 never meets the forced-proxy
redirect. The gate installs and reports success; it simply does not exist in the
IPv6 tables.

The measurement, from one run with the gate installed normally
(`--cap-add NET_ADMIN`), all connections made as the agent UID (2001):

| From agent UID 2001, to `:443` | Connections | `HIVE_PROXY` REDIRECT hits |
| --- | --- | --- |
| over **IPv4** | 5 | **5** |
| over **IPv6** | 5 | **0** |

The IPv4 column is the control required by
[#4319](https://github.com/hivecommons/hive/issues/4319): it rules out "the gate
failed to install" as an explanation for the IPv6 column, in the same run,
against the same endpoint, seconds apart.

Independently, the gate was confirmed to be genuinely intercepting rather than
merely counting packets. From the agent UID over IPv4:

```
issuer=CN=Hive ACMM Proxy CA          # agent UID 2001 — intercepted
issuer=C=GB, O=Sectigo Limited, ...   # proxy UID 1001 — exempt, real upstream cert
```

So on this host the enforcement is working exactly as designed for IPv4, and is
absent for IPv6.

## Why it is silent

There is no `ADVISORY-ONLY` equivalent for this, because from the entrypoint's
point of view nothing failed. `src/deploy/entrypoint.sh` contains **zero**
references to `ip6tables` (verified: `grep -c ip6tables` → `0`), so it never
attempts an IPv6 rule and never has one to report failing.

Inside the running container, with the gate up:

```
# iptables-nft -t nat -S
-A OUTPUT -j HIVE_PROXY
-A HIVE_PROXY -m owner --uid-owner 0 -j RETURN
-A HIVE_PROXY -m owner --uid-owner 1001 -j RETURN
-A HIVE_PROXY -m mark --mark 0x1112 -j RETURN
-A HIVE_PROXY -p tcp -m tcp --dport 443 -j REDIRECT --to-ports 18443

# ip6tables-nft -t nat -S
-P PREROUTING ACCEPT
-P INPUT ACCEPT
-P OUTPUT ACCEPT
-P POSTROUTING ACCEPT
```

The IPv6 nat table is empty. Not misconfigured — empty.

## Environment

| | |
| --- | --- |
| Podman | 5.8.4, **rootful** |
| OCI runtime | crun |
| Network backend | netavark |
| cgroups | v2 |
| Kernel / host | 7.1.4-200.fc44.x86_64, Aurora 44.20260815.1 (Kinoite), SELinux enabled |
| Image | `ghcr.io/hivecommons/hive:stable`, digest `sha256:8f1d4da08bb4439f5bc3da91e20161cb58483567d0f1e53e570675ad4d334d4b` |
| Architecture | `amd64` only |

Container network, created for this run and removed afterwards:

```bash
podman network create --ipv6 \
  --subnet 10.243.19.0/24 --subnet fd00:4319::/64 hive-4319-net
```

Every container, image and network went into a throwaway store with a private
graphroot and runroot (`--root`/`--runroot`). The host's own Podman storage was
not written to and was verified untouched afterwards.

## Reproducing

```bash
# 1. Dual-stack network.
podman network create --ipv6 --subnet 10.243.19.0/24 --subnet fd00:4319::/64 hive-4319-net

# 2. A TLS endpoint that listens on BOTH families, with its own self-signed cert.
#    nginx: `listen 443 ssl;` and `listen [::]:443 ssl;`
podman run -d --name hive-4319-endpoint --network hive-4319-net \
  -v ./certs:/etc/nginx/certs:ro,Z -v ./conf:/etc/nginx/conf.d:ro,Z nginx:1-alpine

# 3. Hive, with the gate installed normally.
podman run -d --name hive-4319-gate --network hive-4319-net --cap-add NET_ADMIN \
  -v ./hive.yaml:/etc/hive/hive.yaml:ro,Z ghcr.io/hivecommons/hive:stable

# 4. Read the REDIRECT counter, drive traffic from the AGENT uid, read it again.
podman exec hive-4319-gate iptables-nft -t nat -L HIVE_PROXY -n -v | awk '/REDIRECT/{print $1}'
for i in 1 2 3 4 5; do
  podman exec -u 2001 hive-4319-gate curl -6 -sk -m 8 -o /dev/null https://hive-4319-endpoint/
done
podman exec hive-4319-gate iptables-nft -t nat -L HIVE_PROXY -n -v | awk '/REDIRECT/{print $1}'
```

### Read the counter, not the response body

The obvious check — "did the request reach the endpoint?" — **does not work**,
and nearly produced the wrong answer here.

The Hive proxy relays to the intended upstream, so a request that WAS redirected
still returns the origin's content. An early IPv4 attempt came back with the
endpoint's own body and its own certificate, which reads exactly like a bypass;
the rule counter showed it had been redirected all along. `curl -k` hides the
substituted certificate, and for a host the proxy tunnels rather than terminates
there is no substituted certificate to see in the first place.

The netfilter rule counter is the discriminator that cannot be fooled: it counts
what the kernel matched, not what the application received.

## The fix slice

Not implemented here — [#4319](https://github.com/hivecommons/hive/issues/4319)
is scoped to determining whether the bypass is reachable, and it is.

The good news for whoever takes it: **the IPv6 counterpart is a direct mirror.**
All three exemptions and the redirect load unmodified under `ip6tables-nft`,
verified in the running container:

```
-m owner --uid-owner 1001 -j RETURN            OK
-m mark --mark 0x1112 -j RETURN                OK
-p tcp --dport 443 -j REDIRECT --to-ports 18443  OK
```

So the answers #4319 asked for:

- **Does the IPv6 chain need the same three exemptions?** Yes, all three, and
  for the same reasons — the root and proxy UID `RETURN`s and the `SO_MARK`
  `RETURN` are not IPv4-specific concepts.
- **What are the `SO_MARK` and `--uid-owner` equivalents?** They are the same
  spellings. `xt_owner` and the packet-mark match are family-independent;
  `ip6tables-nft` accepts `-m owner --uid-owner` and `-m mark --mark` verbatim.
  `REDIRECT --to-ports` also exists in the IPv6 nat table.

`ip6tables-nft` is already present in the image (`/usr/sbin/ip6tables-nft`), so
no image change is required.

Two things the fix must decide that this measurement does not settle:

1. **Failure policy when IPv6 rules cannot be installed.** The current
   `_iptables_ok` gate fails closed on IPv4. A host with IPv6 disabled entirely
   has no IPv6 nat table to write to, and that must not become a boot failure
   for the many hives that have no IPv6 at all — so "IPv6 rules failed" needs to
   be distinguishable from "there is no IPv6 here".
2. **Whether the proxy itself accepts IPv6 connections on `:18443`.** Redirect
   without a listener that answers on that family converts a silent bypass into
   a silent outage.

> **Postscript — the fix slice has since landed
> ([#4327](https://github.com/hivecommons/hive/pull/4327)).** It answers
> question 2 in the negative — the proxy listens on `127.0.0.1:18443` only —
> so the entrypoint closes the IPv6 family with a filter-table
> `REJECT --reject-with tcp-reset` carrying the same three exemptions, rather
> than the mirrored `REDIRECT`. Question 1 is settled with a vacuous pass when
> the kernel has no IPv6 stack (`/proc/sys/net/ipv6` absent) and the same
> fail-closed treatment as IPv4 otherwise.
> `src/deploy/probe_podman_ipv6_egress.sh` re-observes both families live.

## Limits

- **Global routability was not available and is therefore not proven.** This
  host has only ULA IPv6 (`fd00::/8`, plus a Tailscale `fd7a::/48` address), no
  default IPv6 route, and no IPv6 path to the internet — confirmed from inside
  the container (`curl -6 https://api.github.com` → `http=000`). What is proven
  is that the gate does not match IPv6 traffic **at the netfilter layer**, which
  is a property of the ruleset and the packet's family, not of the destination's
  routability. What is NOT proven by direct observation is an agent reaching a
  real public MITM-ed endpoint over IPv6, because no such path exists here.
- **One image, one architecture, one network backend.** `amd64`, netavark,
  rootful, a single `stable` digest.
- **A container-local endpoint, not a public one.** The bypassed connection went
  to another container on the same network.
- **No claim about how often containers get IPv6 in practice.** Whether a given
  deployment is exposed depends on whether its network hands out IPv6 at all.
  This says what happens when it does.

## References

- [Rootful Podman egress-gate baseline](podman-rootful-egress-baseline.md) —
  where this gap was first recorded under *What remains unproven*.
- [`src/deploy/entrypoint.sh`](https://github.com/hivecommons/hive/blob/v4/src/deploy/entrypoint.sh)
  — the `HIVE_PROXY` chain construction, IPv4 only.
- [`CAP_NET_ADMIN` requirement](net-admin-requirement.md)
