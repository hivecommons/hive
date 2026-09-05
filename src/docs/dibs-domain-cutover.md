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

## Measured state

Re-measured 2026-09-04 by `src/deploy/dibs-domain-cutover/verify.sh` from
outside the cluster (`pass=4 warn=2 fail=2`):

- **Step 1 is already done.** `dibs.hivecommons.dev` resolves to
  `157.151.252.29`. The issue body recorded it as not resolving at all; that is
  no longer true, so nobody needs to create the A record.
- **Step 3 has not happened.** The certificate served for the new host is a
  real Let's Encrypt one (`CN=YR1`) but its SANs are still
  `*.hive.hivecommons.dev`, `*.lke.hive.hivecommons.dev`, `hive.hivecommons.dev`
  only — so `https://dibs.hivecommons.dev` currently fails hostname
  verification. This is the expected state while the Let's Encrypt hold is on.
- **The SSO precondition already holds on the new name.** The hub at
  `hive.hivecommons.dev` scopes `hive_hub_user` to `.hivecommons.dev`, which
  covers `dibs.hivecommons.dev` and does not cover `dibs.kubestellar.io` —
  confirming both that the move repairs the bridge and that dual-serving cannot.
- `dibs.kubestellar.io` answers `200` on `/` and `401` on an authenticated deep
  path. Either way it is still being served by the application, not redirected.

## Ordered operator sequence

0. **Preflight — do this now, not on the day.** Every assumption the staged
   manifests rest on is checkable read-only, at zero certificate cost:

   ```sh
   bash src/deploy/dibs-domain-cutover/preflight.sh
   ```

   Run it as soon as you have cluster access, well before the hold expires. The
   expensive failure in this sequence is not "a step fails" — it is "a step
   fails *after* the certificate has been re-issued", because that spends the
   window the whole plan is waiting for and the next attempt is a week out. The
   preflight exits `78` when something would block, and it treats a check it
   could not run as a warning, never as a pass.

   It verifies, in particular, the assumption step 4 depends on and that nothing
   else in this repo does: that the ingress-nginx controller's
   `--default-ssl-certificate` really is `hive-hub/hive-wildcard-tls`. If it is
   not, `dibs.hivecommons.dev` is served ingress-nginx's **self-signed** default
   certificate — a host that answers `200` while failing certificate validation,
   discovered only at step 5, with the issuance already gone. Every other
   Ingress in this repo names a `secretName` explicitly
   (`src/pkg/hub/saas_provision.go`); this one cannot, because an Ingress may
   only reference a Secret in its **own** namespace and the wildcard lives in
   `hive-hub`. That is why the default-certificate path is load-bearing here and
   worth confirming in advance.

   It also estimates the Let's Encrypt registered-domain window with a read-only
   crt.sh query for recent `hivecommons.dev` certificates. That is not an ACME
   probe and does not trigger issuance; it is only a headroom check before the
   date-gated Certificate patch is applied.

   **crt.sh not answering is not headroom, and it does not look like an error.**
   When crt.sh cannot service a query it replies `HTTP 200` with a body of
   `[]` — measured 2026-09-04, six consecutive times for `hivecommons.dev`,
   while that domain has a live Let's Encrypt wildcard and (per the issue) ~50
   certificates minted the previous afternoon. Read naively that is "0
   certificates this week, headroom 50": a confident green on the one gate
   protecting the irreversible step. The preflight now treats *no rows at all*
   for the domain as an unanswered query and warns; zero rows **inside the
   window**, out of rows it did receive, is a real answer and still passes.

   So if check 6 reports `crt.sh returned NO certificates at all`, retry it
   later or count the window from the CA's own rate-limit tooling. Do not read
   it as a go.

   Its counterpart, `verify.sh`, answers the question that comes AFTER — "did
   it work?" — and is run at steps 5 and 7 below. Keep them distinct: the
   preflight protects the issuance, the verifier protects the conclusion.

1. **DNS:** In Cloudflare, create `dibs.hivecommons.dev` as an A record pointing
   to `157.151.252.29`. Leave it **DNS only / grey cloud**.

   Already satisfied as of 2026-09-04 — the name resolves to that address (see
   [Measured state](#measured-state)). Confirm rather than re-create:

   ```sh
   dig +short A dibs.hivecommons.dev
   ```

   Run this from a host with a working resolver. `dig +short` prints its
   `;; communications error ...` diagnostics to **stdout** and signals an
   unreachable resolver only through its exit status, so a machine with no
   resolver produces output that reads like an answer. Both scripts now check
   that exit status and report the lookup as *unchecked* rather than as a wrong
   or missing record — but by hand, at a terminal, the raw output is still
   easy to misread as "the record points somewhere unexpected".
2. **Let's Encrypt hold:** Wait until the certificate quota window has headroom
   (the preflight's crt.sh count must be below the limit), expected around
   **2026-09-10**. Do not trigger cert-manager re-issuance before then; an early
   429 can push the retry window out further.
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
   for `dibs.hivecommons.dev` after the SAN update is ready — which is the
   assumption step 0 exists to confirm, and the one that fails silently if it
   does not hold.
5. **Verify the new canonical host.** A `200` is necessary and not
   sufficient here — both failure modes this step exists to catch still answer
   `200`. Run the verifier, which performs every check below and fails on the
   ones a human reading `curl` output by eye tends to wave through:

   ```sh
   bash src/deploy/dibs-domain-cutover/verify.sh
   ```

   At this point checks 1-4 must pass and check 5 will report the dual-host
   window as OPEN — that is the expected shape here, not a problem. The
   verifier exits `78` if anything is actually wrong, and treats a check it
   could not run as a warning, never as a pass.

   What it runs, if you would rather do it by hand:

   ```sh
   # Fails outright on an invalid chain, rather than reporting a cheerful 200
   # served under ingress-nginx's self-signed default certificate.
   curl -sSI https://dibs.hivecommons.dev

   # Name the issuer and the SANs, so "there is a certificate" is not mistaken
   # for "there is the RIGHT certificate".
   openssl s_client -connect dibs.hivecommons.dev:443 \
     -servername dibs.hivecommons.dev </dev/null 2>/dev/null \
     | openssl x509 -noout -issuer -subject -ext subjectAltName

   kubectl -n dibs get certificate,ingress
   ```

   Then the acceptance test that actually matters: **a browser already signed in
   to the hub must be recognized by `dibs` on the new host.** A signed-out `dibs`
   also renders fine and also returns `200`, so nothing above distinguishes a
   working SSO bridge from the broken one this cutover exists to repair — see
   [Moving a public hostname](hivecommons-migration.md#moving-a-public-hostname).
6. **Redirect the legacy host.** Once the new host is proven, remove
   `dibs.kubestellar.io` from the application Ingress **first**, then apply
   `src/deploy/dibs-domain-cutover/03-dibs-kubestellar-redirect.yaml`. Keep the
   old per-host TLS secret/certificate until the redirect has been verified.

   The order is not stylistic. Both Ingresses live in the `dibs` namespace and
   would claim the same host and path, and ingress-nginx resolves a duplicate
   host+path claim in favour of the **older** Ingress, logging a warning and
   nothing more. Apply the redirect while the application Ingress still lists
   the host and the redirect is simply inert: `curl` keeps returning `200`, the
   objects all look applied, and the obvious reading — "the redirect has not
   landed yet" — sends you to re-apply it rather than to the host it is losing
   to. `preflight.sh` check 4 reports this collision if it already exists.

   ```sh
   # Confirm exactly one Ingress claims the legacy host before applying.
   kubectl -n dibs get ingress \
     -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.rules[*].host}{"\n"}{end}'
   ```
7. **Verify the redirect, on a deep path.** The redirect annotation relies on
   `$request_uri` to carry the path and query across, and a redirect that drops
   them passes a `/`-only check while silently breaking every deep link into
   `dibs`. Re-run the verifier — this time every check should pass:

   ```sh
   bash src/deploy/dibs-domain-cutover/verify.sh
   ```

   A `pass=N warn=0 fail=0` report here is the finish line. Anything less is
   not: in particular the verifier fails a `308` whose `Location` has lost the
   path or query, and fails a duplicate host claim that leaves the redirect
   inert.

   What it runs, if you would rather do it by hand:

   ```sh
   # Root: expect 308 to https://dibs.hivecommons.dev/
   curl -sI https://dibs.kubestellar.io | grep -Ei '^(HTTP|location)'

   # Deep path AND query: the Location must carry both across.
   curl -sI 'https://dibs.kubestellar.io/ideas/42?ref=x' | grep -Ei '^(HTTP|location)'

   kubectl -n dibs get certificate,ingress
   ```

   Confirm the status is `308`, the `Location` is
   `https://dibs.hivecommons.dev/ideas/42?ref=x` (not the bare host), and that
   the new host still serves successfully.

## The dual-host window is a verification window, not a resting state

Between steps 4 and 6 both hosts serve the application, which reads like a safe
place to pause. It is not, and the reason is the point of the whole issue: the
hub session cookie is scoped to `hivecommons.dev`, so a visitor arriving on
`dibs.kubestellar.io` is **not** signed in there and cannot be made to be —
a browser ignores a `Set-Cookie` whose `Domain` does not cover the host that
sent it, so no configuration scopes a `hivecommons.dev` session onto
`kubestellar.io` (see
[Moving a public hostname](hivecommons-migration.md#moving-a-public-hostname),
pinned by `src/pkg/hub/sibling_host_migration_test.go`).

That is today's already-broken behaviour rather than a regression this sequence
introduces — but it means the legacy host is knowingly serving a signed-out
experience for as long as the window is open, and only step 6 ends it. Keep the
window short, and do not treat "both hosts answer" as the finish line.

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
