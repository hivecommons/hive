# dibs.hivecommons.dev domain cutover runbook

Issue [#5925](https://github.com/hivecommons/hive/issues/5925) moves `dibs` to
`https://dibs.hivecommons.dev` and demotes `https://dibs.kubestellar.io` to a
redirect. This is repo-side preparation only: do not touch live DNS, Kubernetes,
cert-manager, or releases until an operator intentionally executes the sequence.

## Current repo inventory

- `dibs.kubestellar.io` appears only in hub SSO comments/tests under `src/pkg/hub`;
  no canonical docs link was found in `README.md`, `docs/`, `src/docs/`,
  `dashboard/`, `examples/`, `discord/`, `config/`, or `.github/`.
- Searching `src/` for `dibs` found the hub-side `/api/saas/dibs/repos` feed and
  SSO bridge only. No `dibs` URL environment default or constant is configured in
  this repository.
- `hivecommons/infra` contains shared CI/Prow files only, and
  `hivecommons/hive-redirect` is a GitHub Pages redirect for the hub entry point.
  The live `dibs/dibs` Ingress and `hive-hub/hive-wildcard-tls` Certificate were
  not found in git, so staged Kubernetes manifests live under
  `src/deploy/dibs-domain-cutover/`.

## Ordered operator sequence

1. **DNS:** In Cloudflare, create `dibs.hivecommons.dev` as an A record pointing
   to `157.151.252.29`. Leave it **DNS only / grey cloud**.
2. **Let's Encrypt hold:** Wait until the certificate quota window has headroom,
   expected around **2026-09-10**. Do not trigger cert-manager re-issuance before
   then; an early 429 can push the retry window out further.
3. **Certificate SAN:** After the hold, confirm the SAN is not already present,
   then apply the JSON Patch in
   `src/deploy/dibs-domain-cutover/01-hive-wildcard-tls-certificate.yaml` so
   `hive-hub/hive-wildcard-tls` includes `dibs.hivecommons.dev` in `dnsNames`
   without replacing the rest of the Certificate:

   ```sh
   kubectl -n hive-hub get certificate hive-wildcard-tls -o jsonpath='{.spec.dnsNames}'
   kubectl -n hive-hub patch certificate hive-wildcard-tls --type=json --patch-file src/deploy/dibs-domain-cutover/01-hive-wildcard-tls-certificate.yaml
   ```
4. **Dual-host ingress:** After the Certificate is ready, review and apply
   `src/deploy/dibs-domain-cutover/02-dibs-ingress-dual-host.yaml` so the
   `dibs/dibs` Ingress serves both hosts while verification runs. The staged
   Ingress keeps `dibs.kubestellar.io` on its existing `hive-tls-hc` secret and
   relies on the ingress-nginx default `hive-hub/hive-wildcard-tls` certificate
   for `dibs.hivecommons.dev` after the SAN update is ready.
5. **Verify the new canonical host:**

   ```sh
   curl -sI https://dibs.hivecommons.dev
   kubectl -n dibs get certificate,ingress
   ```

   Confirm the HTTP response is successful, the certificate chain is a valid
   Let's Encrypt certificate for `dibs.hivecommons.dev`, and a browser already
   signed in to the hub is recognized by `dibs` on the new host.
6. **Redirect the legacy host:** Once the new host is proven, remove
   `dibs.kubestellar.io` from the application Ingress and review/apply
   `src/deploy/dibs-domain-cutover/03-dibs-kubestellar-redirect.yaml`. Keep the
   old per-host TLS secret/certificate until the redirect has been verified.
7. **Verify the redirect:**

   ```sh
   curl -sI https://dibs.kubestellar.io
   kubectl -n dibs get certificate,ingress
   ```

   Confirm the legacy host returns a 308 redirect to `https://dibs.hivecommons.dev`
   and that the new host still serves successfully.

## Rollback

- If DNS fails before the Certificate step, remove or correct the Cloudflare A
  record and stop; no cluster rollback is needed.
- If certificate issuance fails, remove `dibs.hivecommons.dev` from the
  `hive-wildcard-tls` `dnsNames` list and wait for cert-manager to settle before
  retrying.
- If the dual-host application Ingress fails, restore the previous `dibs/dibs`
  Ingress with only `dibs.kubestellar.io` and keep the per-host `hive-tls-hc`
  secret in place.
- If the redirect fails, delete or revert the redirect Ingress and restore
  `dibs.kubestellar.io` on the application Ingress. Do not delete the old cert
  until the redirect has been observed working and no clients depend on direct
  serving from the legacy host.
