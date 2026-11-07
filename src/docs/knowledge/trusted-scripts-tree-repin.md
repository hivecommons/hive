---
title: Editing src/scripts/ requires re-pinning TRUSTED_SCRIPTS_TREE
type: gotcha
layer: project
state: approved
tags: [repo:hivecommons/hive, ci, governance, scripts]
source: AGENTS.md (moved 2026-11-07, hivecommons/hive#17047)
scope:
  paths: [src/scripts/**, .github/scripts/integration-governance.cjs]
  labels: []
  lanes: []
---

A PR that edits anything under `src/scripts/` must re-pin `TRUSTED_SCRIPTS_TREE` in `.github/scripts/integration-governance.cjs` to `git rev-parse HEAD:src/scripts` (of the post-merge tree); otherwise `build-and-test` fails.
