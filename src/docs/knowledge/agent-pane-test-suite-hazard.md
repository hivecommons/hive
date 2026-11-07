---
title: Running the test suite inside an agent pane kills the agent's own session
type: gotcha
layer: project
state: approved
tags: [repo:hivecommons/hive, testing, tmux, crux-agent, dashboard]
source: AGENTS.md (moved 2026-11-07, hivecommons/hive#17047)
scope:
  paths: [src/crux/agent/**, src/pkg/dashboard/**]
  labels: []
  lanes: []
---

Parts of `crux/agent` and `pkg/dashboard` exercise real process management: tmux servers, `/proc` sweeps, and SIGKILL by uid. Run as an agent user, those tests can kill the agent's own tmux server, CLI and terminal.

This is how the scanner lost its session repeatedly (hivecommons/hive#9416). It is the reason `AGENTS.md` says never to run this repository's test suite from inside an agent pane — the rule has no safe exception for "just this package".
