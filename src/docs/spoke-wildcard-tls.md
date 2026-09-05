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

## Renewal is now a single point of failure — and it is watched

Once spokes rely on the wildcard, its renewal covers every dashboard URL on the
domain: a failed renewal takes all of them down at once rather than one.

The hub therefore checks the certificate on every cluster-health build, and only
on clusters that have opted in — everywhere else spokes still carry their own
certificates and nothing here is load-bearing. The result rides in the
`/api/cluster-health` payload as `wildcard_tls` and renders on the cluster row
of the fleet-health panel. A healthy certificate is deliberately silent.

| `status` | What it means | What to do |
| --- | --- | --- |
| `ok` | Covers the domain, more than 21 days left | Nothing |
| `expiring` | Under 21 days left | Renewal is **overdue** — cert-manager starts renewing a 90-day certificate at 30 days out, so reaching 21 means the DNS-01 path or the ACME quota is stuck |
| `expired` | Past `notAfter` | Every wildcard-served spoke on the cluster is failing TLS validation now |
| `missing` | Opted in, and the secret does not exist | Spokes provisioned without a `tls:` block are being served ingress-nginx's self-signed certificate. Either restore the secret or remove `wildcard_tls_secret` from the cluster |
| `domain_mismatch` | The certificate does not carry `*.<cluster domain>` | The opt-in is pointing at a certificate that cannot serve the hosts it caused `tls:` blocks to be dropped from |
| `not_served` | ingress-nginx is not advertising `--default-ssl-certificate=<wildcard_tls_secret>` | Configure the controller to serve the wildcard secret by default before relying on omitted `tls:` blocks |
| `unreadable` | The secret exists but its `tls.crt` will not parse | Look at the secret directly |

The 21-day threshold is "renewal is overdue", not "expiry is close". Warning at
cert-manager's own 30-day renewal point would fire on every healthy renewal on
every opted-in cluster, and an alert that is normally firing is one nobody
reads.

### This is also the only check on the opt-in itself

`wildcard_tls_secret` is an operator assertion, and the provisioner cannot
verify it: it has to decide without a cluster round-trip, and guessing wrong
takes the cluster down. The health build already talks to every cluster on a
timer, so the assertion is verified there instead — where being wrong costs a
warning rather than an outage. `missing` and `domain_mismatch` are exactly that
assertion turning out to be false, and neither is visible from `clusters.json`.

The check is read-only: one `kubectl get secret` per opted-in cluster per health
build. A cluster the hub cannot reach reports **nothing** rather than a
reassuring `ok` — the same unknown-is-not-healthy rule the stuck-pod and
leaked-namespace signals on that panel follow.

Still not automated, and still worth doing by hand before enabling this
fleet-wide: watching the DNS-01 solver itself. This check sees the certificate
that is installed, so it reports a renewal that failed — it cannot see one that
is *about* to fail for a Cloudflare credential reason.
