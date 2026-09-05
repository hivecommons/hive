# HiveCommons organization migration tracker

Hive has transferred public project infrastructure from the historical
`kubestellar` namespace to the `hivecommons` organization. This page is the
sequenced umbrella tracker for the migration on the v5 edge line: it records
what has moved, the remaining breakages, and the order in which maintainers are
closing them.

## Operator promise

- Existing `https://github.com/kubestellar/hive` clone URLs continue to work via
  GitHub redirects after the transfer to `https://github.com/hivecommons/hive`.
  Update local remotes with:

  ```sh
  git remote set-url origin https://github.com/hivecommons/hive.git
  ```

- The canonical Go module is `github.com/hivecommons/hive` on both v4 and v5
  after [#5815](https://github.com/hivecommons/hive/pull/5815) and
  [#5816](https://github.com/hivecommons/hive/pull/5816), closing
  [#5778](https://github.com/hivecommons/hive/issues/5778).
- The transferred GHCR packages are `ghcr.io/hivecommons/hive`,
  `ghcr.io/hivecommons/hive-hub`, `ghcr.io/hivecommons/hive-contributor`, and
  `ghcr.io/hivecommons/pr-verifier`; old `ghcr.io/kubestellar/*` image
  references remain compatibility references while the migration is completed.
- Any future deprecation of `kubestellar` package names will be announced in the
  changelog, release-channel docs, upgrade guide, and dashboard/operator notices
  before enforcement.
- Rollback guidance stays digest-based: operators can pin immutable short-SHA
  tags while both namespaces are kept equivalent.

## Phase plan

| Phase | Status | Owner | Exit criteria |
| --- | --- | --- | --- |
| P0 — org, sites, and repository bootstrap | Done | Maintainers | `hivecommons` organization artifacts, profile README, `hivecommons.dev`, LICENSE files, and public site/docs repositories are updated. |
| P1 — repository transfer and redirects | Done, with exit caveats | Maintainers | `kubestellar/hive` transferred to `hivecommons/hive` and old GitHub URLs redirect. Remaining transfer fallout is tracked by [#5774](https://github.com/hivecommons/hive/issues/5774), [#5810](https://github.com/hivecommons/hive/issues/5810), and [#5814](https://github.com/hivecommons/hive/issues/5814). |
| P2 — Go module rename | Done | Maintainers | v4 [#5815](https://github.com/hivecommons/hive/pull/5815) and v5 [#5816](https://github.com/hivecommons/hive/pull/5816) switched the module to `github.com/hivecommons/hive`; [#5778](https://github.com/hivecommons/hive/issues/5778) is closed. |
| P3 — GHCR namespace transfer and workflow repair | In progress | Release maintainers | `hive`, `hive-hub`, `hive-contributor`, and `pr-verifier` packages exist under `ghcr.io/hivecommons/*`; workflow writes and verifier pulls still need [#5810](https://github.com/hivecommons/hive/issues/5810) and [#5814](https://github.com/hivecommons/hive/issues/5814) closed. |
| P4 — documentation and install sweep | In progress | Docs maintainers | Duplicate XL sweep PRs [#5776](https://github.com/hivecommons/hive/pull/5776) and [#5777](https://github.com/hivecommons/hive/pull/5777) were closed as superseded; the redo landed as [#5818](https://github.com/hivecommons/hive/pull/5818) and [#5819](https://github.com/hivecommons/hive/pull/5819), with residual doc links tracked below. |
| P5 — web-domain cutover | In progress | Hub maintainers + docs maintainers | `https://hive.hivecommons.dev` is documented as the canonical hosted hub, `https://hive.kubestellar.io` remains a legacy redirect, operator-facing `HIVE_HUB_URL`/`HIVE_HUB_PUBLIC_URL`/`HIVE_HUB_SPOKE_DOMAIN` guidance is published, and redirect retirement criteria are announced before enforcement. `dibs` is tracked by [#5925](https://github.com/hivecommons/hive/issues/5925); see [Moving a public hostname](#moving-a-public-hostname). |
| P6 — kubestellar package freeze/deprecation | Not started | Maintainers | A dated deprecation notice exists, operators have a documented migration window, and rollback/pinning instructions are tested after open breakages are closed. |

## Ordered closeout checklist

1. **App bot push permissions** — Open in
   [#5774](https://github.com/hivecommons/hive/issues/5774); fix branch/PR
   [#5821](https://github.com/hivecommons/hive/pull/5821) is investigating the
   post-transfer GitHub App installation and write-token path.
2. **GHCR namespace and workflow writes** — Open in
   [#5810](https://github.com/hivecommons/hive/issues/5810); the canonical
   namespace is `ghcr.io/hivecommons/*`, but any remaining workflow pushes to
   `ghcr.io/kubestellar/*` must stop failing after the transfer.
3. **`:latest` multi-arch parity** — Open in
   [#5803](https://github.com/hivecommons/hive/issues/5803); restore `latest`
   to the same multi-architecture coverage as named release tags.
4. **Reference sweep** — Redo completed by
   [#5818](https://github.com/hivecommons/hive/pull/5818) on v4 and
   [#5819](https://github.com/hivecommons/hive/pull/5819) on v5 after duplicate
   XL PRs [#5776](https://github.com/hivecommons/hive/pull/5776) and
   [#5777](https://github.com/hivecommons/hive/pull/5777) were closed as
   superseded.
5. **Retired `pr-verifier` image** — Open in
   [#5814](https://github.com/hivecommons/hive/issues/5814); PR content
   verification must pull from the transferred `ghcr.io/hivecommons/pr-verifier`
   package instead of the retired `ghcr.io/kubestellar/pr-verifier` reference.
6. **Hosted hub web domain** — In progress under
   [#5951](https://github.com/hivecommons/hive/issues/5951); the canonical hub
   is `https://hive.hivecommons.dev`, while `https://hive.kubestellar.io`
   remains a legacy redirect during the cutover. Operator docs must call out
   `HIVE_HUB_URL`, `HIVE_HUB_PUBLIC_URL`, and `HIVE_HUB_SPOKE_DOMAIN` updates
   before any redirect retirement.
7. **Public hostnames** — In progress under
   [#5925](https://github.com/hivecommons/hive/issues/5925) for `dibs`; the hub
   moved to `hive.hivecommons.dev` on 2026-09-04. Read
   [Moving a public hostname](#moving-a-public-hostname) before moving another
   one — the order is not cosmetic.
8. **Residual doc links and operator notices** — In progress under this tracker:
   keep README, install, release-channel, backup/restore, Podman, upgrade, and
   dashboard/operator notice text aligned as each preceding item closes.

## Moving a public hostname

A hostname move looks like branding and is not. Two constraints decide the
order, and both fail quietly if the order is wrong.

### Sibling sign-in breaks the moment the hub's registrable domain changes

First-party sibling products have no login of their own. `dibs` resolves the
visitor by forwarding the browser's `hive_hub_user` cookie to the hub's
`GET /api/saas/whoami` server-to-server ([#4171](https://github.com/hivecommons/hive/issues/4171)).
That only works while the browser sends the cookie to the sibling at all, and it
does so solely because of the cookie's `Domain` attribute — which the hub derives
from **the registrable domain of its own canonical host** (`HIVE_HUB_PUBLIC_URL`
→ `sessionCookieDomain`, `src/pkg/hub/saas.go`).

So the bridge has a precondition:

> The hub and the sibling product must share a registrable domain.

When the hub became `hive.hivecommons.dev`, the session cookie became
`Domain=.hivecommons.dev`, and `dibs.kubestellar.io` stopped receiving it. The
symptom is not an error: the sibling simply renders every visitor as signed out.
Moving the sibling under `hivecommons.dev` is what restores the precondition,
which is why [#5925](https://github.com/hivecommons/hive/issues/5925) is a repair
and not a rename. The invariant is pinned by
`src/pkg/hub/sibling_host_migration_test.go`.

**The old host must REDIRECT, not keep serving the application.** Leaving it
dual-serving is the tempting shortcut once the new host works, and it cannot be
made to work: a browser ignores a `Set-Cookie` whose `Domain` does not cover the
host that sent it (RFC 6265 §5.3), so a hub on `hivecommons.dev` can never scope
a cookie to `kubestellar.io`. There is no second domain to configure. A redirect
works precisely because it is not dual-serving — the browser ends up issuing its
request to the new host, which is inside the cookie's scope.

`HIVE_HUB_LEGACY_COOKIE_DOMAIN` does not rescue this either. It expires the
stale cookie on the old domain so a browser converges to one session; it never
writes a session there.

### Certificate issuance is rate-limited, and the limit is shared

Let's Encrypt caps `hivecommons.dev` at 50 certificates per rolling 168 hours,
across every host under it. Prefer **adding the new host as a SAN on the existing
wildcard `Certificate`** over minting a per-host one: either way it is one
issuance against the same quota, and consolidating keeps the object count down.

Note that the fleet wildcard covers `*.hive.hivecommons.dev`,
`*.lke.hive.hivecommons.dev` and `hive.hivecommons.dev` — a name one level up,
such as `dibs.hivecommons.dev`, is **not** covered and needs the SAN addition.

Check headroom before triggering a re-issue. A 429 does not just fail; it pushes
the retry deadline further out.

### Order

1. Add the DNS record for the new host (A record to the ingress load balancer,
   proxy disabled), and confirm it resolves.
2. When the issuance window has headroom, add the new host to the wildcard
   `Certificate`'s `dnsNames` and let it re-issue.
3. Add the new host to the service's `Ingress`, keeping the old host alongside it
   for the cutover.
4. Verify the new host serves `200` with a valid certificate, and — for a sibling
   product — that a signed-in browser is recognised there.
5. Demote the old host to a redirect to the new one. Only then drop its per-host
   certificate.

Do not delete the old host's certificate before the redirect is proven, and do
not recreate names in the old organization: the redirects are what keep rollback
viable.

## Communications checklist

Every phase that changes an operator-facing URL, package, tag, or default should
update all applicable locations:

- `CHANGELOG.md`/`changelog.d/` release note;
- `README.md` quick-start and install examples;
- `UPGRADE.md` when a major-version boundary or package rename is involved;
- `src/docs/release-channels.md` for channel and mirror semantics;
- `src/docs/operator-reference.md`, Podman, backup/restore, and manual
  provisioning docs for image and manifest examples;
- dashboard or fleet notice text when a running hive should alert its owner; and
- this umbrella tracker.

## Current known gaps

- [#5774](https://github.com/hivecommons/hive/issues/5774) keeps agent PR flows
  at risk until GitHub App write permissions are verified after the transfer.
- [#5810](https://github.com/hivecommons/hive/issues/5810) and
  [#5814](https://github.com/hivecommons/hive/issues/5814) keep CI/package
  verification sensitive to old GHCR namespace assumptions.
- [#5803](https://github.com/hivecommons/hive/issues/5803) keeps `:latest`
  unsafe for non-arm64 operators until multi-arch publishing is restored.
- No package-name deprecation date is set; `kubestellar` references remain
  supported until this tracker is updated with a dated freeze plan.
- [#5925](https://github.com/hivecommons/hive/issues/5925) keeps `dibs` on
  `dibs.kubestellar.io`, a different registrable domain from the hub's, so hub
  sign-in does not carry to it — see
  [Moving a public hostname](#moving-a-public-hostname).
