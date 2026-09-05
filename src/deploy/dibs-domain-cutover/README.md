# dibs.hivecommons.dev cutover manifests (#5925)

These manifests stage the repo-side assets for moving `dibs` from
`dibs.kubestellar.io` to `dibs.hivecommons.dev`. The live `dibs/dibs` Ingress
and `hive-hub/hive-wildcard-tls` Certificate were not found in `hivecommons/hive`,
`hivecommons/infra`, or `hivecommons/hive-redirect`; the current cluster objects
appear to have been applied by hand. Do not apply any file here until the issue's
operator sequence is ready.

## Files

- `01-hive-wildcard-tls-certificate.yaml` stages a JSON Patch for the
  cert-manager `Certificate` `hive-hub/hive-wildcard-tls`, appending
  `dibs.hivecommons.dev` without replacing the existing SAN list. It is
  intentionally held until Let's Encrypt quota headroom returns around
  2026-09-10.
- `02-dibs-ingress-dual-host.yaml` stages the interim `dibs/dibs` Ingress shape:
  `dibs.hivecommons.dev` serves the application while `dibs.kubestellar.io`
  remains on the same application during verification. The new host intentionally
  has no per-Ingress TLS secret; ingress-nginx should serve it with the controller
  default SSL certificate, `hive-hub/hive-wildcard-tls`, after the held SAN
  update is applied.
- `03-dibs-kubestellar-redirect.yaml` stages the final ingress-nginx 308 redirect
  from `dibs.kubestellar.io` to `dibs.hivecommons.dev`.

- `preflight.sh` checks — read-only, at zero certificate cost — the assumptions
  the three manifests above encode: the controller default certificate, the
  Certificate's namespace/name and current SANs, drift between the live
  `dibs/dibs` Ingress and the staged copy, host collisions between the app and
  redirect Ingresses, the DNS record, and Let's Encrypt registered-domain
  headroom via a read-only crt.sh query. Run it EARLY; every check is free before
  the Let's Encrypt hold expires and expensive after issuance is spent. Contract
  tests: `bin/test_dibs_cutover_preflight.sh`.

- `verify.sh` checks — also read-only — whether the cutover actually WORKED,
  which is a different question and one that no status code answers on its own:
  every failure mode this sequence has still returns a `200`. It reads the
  issuer and SANs off the certificate the new host actually serves, computes
  whether the hub session cookie's `Domain` can reach that host at all (the
  signed-out failure this whole cutover repairs), and tests the legacy
  redirect on a path **with a query string**, because a redirect that drops
  `$request_uri` passes a bare-root check and breaks every deep link. Run it
  twice: after step 4 (checks 1-4 must pass; check 5 will report the dual-host
  window as OPEN) and again after step 6 (everything should pass). Contract
  tests: `bin/test_dibs_cutover_verify.sh`.

Before applying, compare each staged object with the live object and preserve any
cluster-local annotations, labels, ingress class, service port, or issuer details
that differ from these captured assumptions. `preflight.sh` performs that
comparison for the fields it knows about, and warns when the live Ingress carries
annotations that `02-dibs-ingress-dual-host.yaml` — a full object, not a patch —
would drop on apply. For the Certificate, first confirm `dibs.hivecommons.dev` is
not already present, then apply the patch instead of replacing the whole
Certificate object.
