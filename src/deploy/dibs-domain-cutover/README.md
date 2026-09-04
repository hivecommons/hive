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

Before applying, compare each staged object with the live object and preserve any
cluster-local annotations, labels, ingress class, service port, or issuer details
that differ from these captured assumptions. For the Certificate, first confirm
`dibs.hivecommons.dev` is not already present, then apply the patch instead of
replacing the whole Certificate object.
