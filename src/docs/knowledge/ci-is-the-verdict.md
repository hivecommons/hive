---
title: Why agents do not run go test locally — CI is the verdict
type: decision
layer: project
state: approved
tags: [repo:hivecommons/hive, testing, ci, policy]
source: AGENTS.md (moved 2026-11-07, hivecommons/hive#17047)
scope:
  paths: [src/**]
  labels: [kind/failing-test, kind/flake, kind/test]
  lanes: []
---

The rule in `AGENTS.md` is "do not run `go test`, `go build ./...`, `go vet`, `golangci-lint` or the docs/citation guards locally". The reasons:

- CI runs the full matrix with the right toolchain, caches and coverage gates; a local run duplicates it and burns tokens and pod CPU/disk.
- Inside the hive pod, `go test` and `go vet` are blocked for agents by the Go shim, so the local run cannot succeed anyway.
- The hive opens requested PRs; CI is the sole verdict and records failures for agents to iterate on (`gh pr checks <n>`, then the failing job log).
