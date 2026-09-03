# HiveCommons organization migration tracker

Hive has transferred public project infrastructure from the historical
`kubestellar` namespace to the `hivecommons` organization. This page is the
sequenced umbrella tracker for the migration on the v4 stable line: it records
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
| P5 — kubestellar package freeze/deprecation | Not started | Maintainers | A dated deprecation notice exists, operators have a documented migration window, and rollback/pinning instructions are tested after open breakages are closed. |

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
6. **Residual doc links and operator notices** — In progress under this tracker:
   keep README, install, release-channel, backup/restore, Podman, upgrade, and
   dashboard/operator notice text aligned as each preceding item closes.

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
