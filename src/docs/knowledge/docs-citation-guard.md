---
title: Fixing src/docs file:line citation drift
type: pattern
layer: project
state: approved
tags: [repo:hivecommons/hive, docs, citations, ci]
source: AGENTS.md (moved 2026-11-07, hivecommons/hive#17047)
scope:
  paths: [src/docs/**, src/pkg/hub/**, src/pkg/dashboard/**, src/scripts/check-docs-citations.py, src/scripts/check-api-reference-citations.sh]
  labels: [kind/documentation, documentation]
  lanes: []
---

Do not hand-edit `src/docs` file:line citations (including `src/docs/api-reference.md` and `src/docs/api-reference/*.md`).

When CI's citation guard reports drift, run `python3 src/scripts/check-docs-citations.py --fix` first. It runs `check-api-reference-citations.sh --fix` for `api-reference.md` and re-anchors other `.go` citations by their backticked Go declaration anchor. Fall back to a manual edit only for citations it still reports as `DRIFT` / `UNRESOLVED`, and run it only when CI tells you citations drifted.

The required `changelog-fragment-guard` check runs the guard (non-fix) when a PR edits a hub or dashboard `server.go` or their `static/` pages, so the PR that causes drift goes red.
