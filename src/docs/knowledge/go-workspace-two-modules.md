---
title: src/ is a two-module Go workspace
type: reference
layer: project
state: approved
tags: [repo:hivecommons/hive, go, crux, workspace]
source: AGENTS.md (moved 2026-11-07, hivecommons/hive#17047)
scope:
  paths: [src/**/*.go, src/go.work, src/go.mod, src/crux/go.mod]
  labels: [area/core, area/scheduler]
  lanes: []
---

`src/` is a two-module Go workspace (`src/go.work`): the root module and the Crux agent runtime at `src/crux/` (`github.com/hivecommons/hive/crux`, packages `crux`, `crux/agent`, `crux/inference`, …).

From `src/`, `./...` is the root module only; the Crux module is `./crux/...`. CI covers both. Design background: `src/docs/design/crux.md`.
