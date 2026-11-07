---
title: Changelog fragment — exact exemption set and the no-changelog label
type: pattern
layer: project
state: approved
tags: [repo:hivecommons/hive, changelog, ci, pr-convention]
source: AGENTS.md (moved 2026-11-07, hivecommons/hive#17047)
scope:
  paths: [src/**, changelog.d/**]
  labels: []
  lanes: []
---

Every PR with a user-visible `src/` code change needs a `changelog.d/<category>-<slug>.md` fragment (`added|changed|deprecated|fixed|security`) whose body is a single `- ` bullet.

Exempt automatically — no fragment and no label needed:

- changes outside `src/` (root-level docs, `changelog.d/`, `.github/`);
- `src/docs/`;
- `*_test.go` files;
- `src/{deploy,scripts}/test[-_]*.sh` scripts.

`src/scripts/check-changelog-fragment.sh` is the source of truth for the exact set.

Other test-adjacent changes (`testdata/` fixtures, non-`_test.go` test helpers) and non-user-visible `src/` code changes (refactors, dependency bumps) need the `no-changelog` label instead of a fragment.

`changelog.d/` is at the repository root, not under `src/` — `src/changelog.d/` is never compiled and the guard rejects it.
