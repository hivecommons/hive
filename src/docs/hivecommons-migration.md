# HiveCommons organization migration tracker

Hive is moving public project infrastructure from the historical
`kubestellar` namespace toward the `hivecommons` organization. This page is the
public tracker for that migration on the v4 stable line: it states what has
moved, what still points at `kubestellar`, and what operators should expect
before any breaking transfer occurs.

## Operator promise

- Existing `github.com/kubestellar/hive` clone URLs and
  `ghcr.io/kubestellar/*` image references remain valid throughout v4.
- `ghcr.io/hivecommons/*` image tags are mirrors of the same manifest digests as
  the `kubestellar` packages while dual-publish is active.
- Any future deprecation of `kubestellar` package names will be announced in the
  changelog, release-channel docs, upgrade guide, and dashboard/operator notices
  before enforcement.
- Rollback guidance stays digest-based: operators can pin the immutable short-SHA
  tag in either org while both namespaces publish the same digest.

## Phase plan

| Phase | Status | Owner | Exit criteria |
| --- | --- | --- | --- |
| P0 — org, sites, and repository bootstrap | Done | Maintainers | `hivecommons` organization and public site/docs repositories exist, with transferred test repos (`pluk`, `hotshot`) proving redirects and permissions. |
| P1 — image dual-publish and retention | In progress | Release maintainers | v4 publishes `kubestellar` and `hivecommons` image tags for Hive, contributor, and hub images; pruning keeps moving tags and rollback short-SHA tags in both orgs. |
| P2 — documentation and install sweep | Pending | Docs maintainers | README, install, operator, release-channel, backup/restore, Podman, and upgrade pages clearly distinguish canonical v4 URLs from hivecommons mirrors. |
| P3 — v5 edge dual-publish | Pending | Release maintainers | The v5/edge workflow publishes equivalent hivecommons images before v5 becomes the recommended development line. |
| P4 — repository transfer and redirects | Pending | Maintainers | Branch protections, default branch, GitHub App/webhook settings, package permissions, and issue/PR redirects are verified in a public transfer checklist. |
| P5 — kubestellar package freeze/deprecation | Not started | Maintainers | A dated deprecation notice exists, operators have a documented migration window, and rollback/pinning instructions are tested. |

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
- the public roadmap row that links back to this tracker.

## Current known gaps

- The v4 line has dual-publish documentation, but many examples still use only
  `github.com/kubestellar/hive` and `ghcr.io/kubestellar/hive`.
- The v5 edge workflow needs the same mirror coverage before v5 is presented as
  the next-generation line.
- No package-name deprecation date is set; `kubestellar` references remain
  supported until this tracker is updated with a dated freeze plan.
