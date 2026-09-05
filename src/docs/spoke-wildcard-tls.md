# Serving spokes from the fleet wildcard certificate

Issue [#5977](https://github.com/hivecommons/hive/issues/5977). A wildcard
certificate covering `*.<cluster domain>` can serve every hosted spoke on a
cluster, replacing one certificate per hive. This page is how to turn that on
for a cluster, and what has to be true first.

## What was wrong

A fleet wildcard existed and served nothing. Every provisioned spoke Ingress
carried its own `tls:` block naming a per-namespace secret, and **an explicit
`tls:` block always wins over the ingress controller's
`--default-ssl-certificate`** — so the wildcard was inert while each hive kept
minting its own certificate. Measured on 2026-09-04: ~195 Ingress TLS references
and ~60 `Certificate` objects across two clusters, doing what one wildcard
already covered.

That is not just waste. Let's Encrypt caps issuance at **50 per week per
registered domain**, and with ~263 wildcard-covered hostnames on those clusters,
every hostname change costs a certificate. The 2026-09-03 rate-limit incident is
what happens when enough of them change at once.

## Turning it on for a cluster

### Prerequisites — both must hold first

The provisioner cannot see either of these, and getting them wrong is not a soft
failure. **If the tls: block is omitted on a cluster whose controller has no
`--default-ssl-certificate`, ingress-nginx serves its built-in self-signed
certificate and every spoke dashboard on that cluster fails TLS validation at
once.**

1. The wildcard secret exists on the cluster:

   ```sh
   kubectl -n hive-hub get secret hive-wildcard-tls
   ```

2. The ingress-nginx controller serves it by default:

   ```sh
   kubectl -n ingress-nginx get deployment ingress-nginx-controller \
     -o jsonpath='{.spec.template.spec.containers[*].args[*]}' \
     | tr ' ' '\n' | grep default-ssl-certificate
   ```

   Expect `--default-ssl-certificate=hive-hub/hive-wildcard-tls`. If nothing
   comes back, **stop** — set the flag (and copy or issue the wildcard) before
   going further. Note that issuing a second wildcard on another cluster costs
   one ACME issuance against the same weekly cap.

### The switch

Add `wildcard_tls_secret` to the cluster's entry in `clusters.json`:

```json
{
  "id": "hive-oke",
  "domain": "hive.hivecommons.dev",
  "ingress_type": "nginx",
  "wildcard_tls_secret": "hive-hub/hive-wildcard-tls"
}
```

Empty or absent (the default) keeps the historical per-host behaviour, which
always works. The value is an operator **assertion** that the two prerequisites
above hold; the hub does not verify it.

Newly provisioned spokes on that cluster then get Ingresses with no `tls:` block
and no `cert-manager.io/cluster-issuer` annotation. Both halves matter: the
`tls:` block is what beats `--default-ssl-certificate`, and the annotation is
what makes cert-manager's ingress-shim mint the Certificate in the first place.

## What is decided per host, not per cluster

Coverage is evaluated for each spoke's own hostname, so a host the wildcard does
not cover keeps its own certificate even on an opted-in cluster.

A wildcard matches **exactly one label** (RFC 6125 §6.4.3), so
`*.hive.hivecommons.dev` covers `hosted-acme-ab12.hive.hivecommons.dev` but
**not**:

| Host | Why not |
| --- | --- |
| `hive.hivecommons.dev` | the apex is not matched by `*.` |
| `a.b.hive.hivecommons.dev` | deeper than one label |
| `dibs.hivecommons.dev` | one level *above* the wildcard's scope — see [#5925](https://github.com/hivecommons/hive/issues/5925), it needs its own SAN |
| `hive.kubestellar.io` | different domain |

OpenShift Route clusters are skipped entirely: Routes terminate TLS with edge
termination and no secret, so they never minted a per-host certificate through
this path, and `--default-ssl-certificate` is an ingress-nginx concept that does
not apply to them.

## Existing spokes

This change affects **newly provisioned and re-reconciled** spokes. Spokes
already carrying a `tls:` block keep it until the provisioner rewrites their
manifest. Before deleting any now-unused `hive-tls` Certificate or secret,
verify the host actually serves the wildcard by SNI:

```sh
openssl s_client -connect <host>:443 -servername <host> </dev/null 2>/dev/null \
  | openssl x509 -noout -issuer -subject -ext subjectAltName
```

A spoke serving the wildcard shows the wildcard's SANs. One still on its own
certificate shows a single host — deleting its secret would take it down, and
cert-manager would mint a replacement rather than falling through to the
wildcard (confirmed during the #5977 investigation).

## Renewal is now a single point of failure

Once spokes rely on the wildcard, its renewal covers every dashboard URL on the
domain: a failed renewal takes down all of them at once rather than one. The
DNS-01 path and the wildcard's expiry both deserve monitoring before this is
enabled fleet-wide — that is tracked in #5977 rather than implemented here.
