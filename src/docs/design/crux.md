# Crux: the agent runtime boundary (#13536)

Status: **partly shipped (v5).** Stage 1 is in progress in this repo: the
boundary is named, the architecture ratchet and the tmux funnel are live,
`crux.Runtime` exists, the dashboard is on it, the hub and spoke are on the
crux value types, and the remaining Stage 1 item (CI path filters) is
tracked on the RFC. Stages 2 and 3 are design only.

## What Crux is

Crux is the agent runtime inside Hive, named so it can be reasoned about as a
component with its own surface rather than as "the parts of the binary that
run agents". It covers: running an agent from a definition (engine, model,
prompt, tools, limits); lifecycle (start, observe, nudge, stall and crash-loop
recovery, stop); the engine adapters; model discovery and gateway; policy
enforcement at the tool boundary; sandbox and Kubernetes-job isolation; and
telemetry. Hive is Crux's first consumer, not its definition.

The name is "the point everything turns on". Hive turns on Crux.

## Inside the boundary

`pkg/agent` plus its ring. Measured on v5 at `2dbe5f6` (Oct 22, 2026):

| Area | Packages |
|---|---|
| Runtime core | `pkg/agent` (manager, kick files, engine adapters: aider, bob, claude, codex, copilot, gemini, goose) |
| Agent definition | `pkg/agentmode`, `pkg/agentparse`, `pkg/agentsmd`, `pkg/skillreg` |
| Engines and gateway | `pkg/claude`, `pkg/inference`, `pkg/openrouter` |
| Policy at the tool boundary | `pkg/proxy` (MITM), `pkg/toolapprove` |
| Isolation | `pkg/sandbox`, `pkg/kubejob` |
| Observation | `pkg/watchdog`, `pkg/trajectory`, `pkg/logscrub` |

Crux's outbound coupling is already small: `pkg/agent` imports `pkg/github`
for one symbol (`AppAuth`), reads two `pkg/config` fields, and reaches pluk
through `exec.LookPath`, which is already a process boundary.

## Outside the boundary (stays in Hive)

The governor, GitHub and forge integration (`pkg/github`, `pkg/forgestate`),
review, evidence, the dashboard (`pkg/dashboard`), the hub and spokes
(`pkg/hub`, `pkg/hub/spoke`), TUI, Discord/Slack, Linear, and `cmd/hive`.
These consume Crux; none of them is part of it.

## The problem Stage 1 fixes

Inbound coupling. Everything reaches into `pkg/agent` directly:

- `pkg/dashboard` references 68 distinct `agent.<Symbol>` identifiers,
  `cmd/hive` 37, `pkg/hub` 18, `pkg/hub/spoke` 15, `pkg/proxy` 14, plus
  `loginscan`, `ioscan`, `discord` (AST count over Go files including tests;
  the RFC's grep-based 109/26 also matched local variables named `agent`).
- tmux is the assumed primary isolation and is driven from `pkg/agent`'s
  `tmuxCmd`; outside the runtime, `pkg/tui` invoked `tmux attach` directly
  until the funnel landed.
  (The RFC's "dashboard 17, hub 5" counted files that *mention* tmux in
  comments, JSON field names and messages, not files that invoke it.)
- `pkg/config/config.go` is one 9,580-line file; agent settings are not a
  separable struct.

Co-change tells the same story: of 4,659 v5 commits Aug 22 to Oct 22, 291
touch `pkg/agent` and 106 of those (36%) also touch dashboard, hub, TUI,
governor, review or `cmd/hive`. A repo split today would mean about 1.7
two-repo changes per day, which is why Stage 1 happens in this repo.

## Staged plan

**Stage 1: boundary, in repo (this record).**

1. Architecture ratchet (shipped):
   `src/internal/testutil/crux_boundary_ratchet_test.go` counts, per package
   outside `pkg/agent/...`, the distinct `agent.<Symbol>` identifiers and the
   non-test files that invoke tmux. Baselines are exact and fail closed in
   both directions: a count above its baseline fails the PR gate and names
   the offenders; a count below it also fails and names the baseline to
   lower, so removed coupling cannot grow back into stale headroom. Baselines
   are lowered in the same change that removes coupling; they are never
   raised.
2. tmux funnel (shipped): `pkg/agent/tmux` is the one package outside the
   manager that names the tmux binary (`LookPath`, `Command`, `Attach`,
   `HasSession`); `pkg/tui` attaches through it and the ratchet's tmux
   baseline is zero everywhere else. Operations are added as call sites
   move behind the funnel, not ahead of them.
3. `crux.Runtime` (dashboard shipped, hub on the crux value types): `src/pkg/crux/runtime.go`
   is the exported runtime surface, grouped as `Lifecycle`, `Observer`,
   `Kicker`, `Definitions`, `Fleet` and `Credentials`; every method is
   annotated with the consumer that needs it and `*agent.Manager` satisfies
   it directly, so the adapter is zero-cost. The observation value types
   (`Process`, `ProcessState`, `KickDispatch`, `KickOutcome`, `KickLogInfo`,
   `RestartEvent`, `SubAgentModel`, `StartFailureClass` and their constants)
   are aliases of the `pkg/agent` definitions for now: Stage 1 renames the
   seam, Stage 2 moves the definitions behind it. `pkg/dashboard` holds a
   `crux.Runtime` in `Dependencies.AgentMgr` and went from 68 to 51 distinct
   `agent.<Symbol>` references; what remains is audit, commit-trailer,
   provider-status, Copilot-token and agent-mode helpers plus the test-only
   manager constructors, none of which a lifecycle interface covers.
   `pkg/hub` holds no manager at all: its 18 references were the provider
   status value types (`ProviderStatus`, `ProviderAgentEvidence`,
   `RollUpProviderStatus`, `BackendAuthState`, the `BackendAuth*`,
   `ProviderAuth*` and `Provider*` availability constants). Those now live in
   `pkg/crux/types`, a leaf package that `pkg/agent` imports and aliases
   (the alias direction is reversed for them: Crux defines, the runtime
   implementation re-exports) and that `pkg/crux` re-exports as
   `crux.<Name>`, so consumers never see the leaf. It is a separate package
   only because `pkg/crux` still aliases `agent.AgentProcess` and therefore
   imports `pkg/agent`, which cannot import it back; it folds into
   `pkg/crux` once `Process` is its own type. That move took `pkg/hub` to 1
   (`AgentAvailableAtACMMLevel`), `pkg/hub/spoke` 15 → 5, `pkg/dashboard`
   51 → 39 and `cmd/hive` 37 → 33. `pkg/crux` and everything under it is
   inside the boundary for the ratchet's purposes.
4. Agent configuration as its own struct and file (shipped):
   `AgentRuntimeConfig` and `AgentRuntimeGovernorConfig` in
   `src/pkg/config/agent_config.go`, embedded inline in `Config` and
   `GovernorConfig` so every `hive.yaml` key keeps its position.
5. CI path filters so agent-only PRs skip hub/rest/dashboard shards and vice
   versa, with the aggregate required check kept fail-closed.

**Stage 2: nested module.** `src/crux/` as its own Go module via `go.work`,
own tags (`crux/vX.Y.Z`), own coverage floor and CI lane; same repo, same PRs,
same v5/v6 branches. Ship a `crux` binary (`run <def>`, `ps`, `logs`, `kick`,
`stop`) that runs an agent definition with policy and telemetry and no
governor, GitHub or dashboard.

**Stage 3: platform.** `crux serve` with an API (gRPC or HTTP), tenancy, auth,
quotas; Hive and hub become clients; the hub-to-runtime contract becomes
explicit and versioned; move to `hivecommons/crux`. Only when a second
consumer is real (candidates: Nous / Experiment Lab, Spektacular, Dibs,
external optimizer and formal-verification lanes, enterprise agent services).

## Not doing

- Repo split before Stage 1: the co-change tax lands before any second
  consumer exists and doubles v5/v6 top-up work.
- Rewriting engines or the proxy. This is a boundary, not a rewrite.
- Shortening the PR gate by splitting: `pkg/agent` is a 429 s serial test
  monolith already sliced five ways; the gate is bounded by the slowest shard,
  so only path filters (Stage 1, item 5) change anything, and they work
  in-repo.

## Naming

**Crux.** One syllable; it means the thing. CLI `crux run claude.yaml`,
`crux ps`, `crux kick`, `crux serve`; module `github.com/hivecommons/crux`.
Collision check (Oct 22, 2026): `hivecommons/crux` is free. The word is used
by unrelated projects (redbadger/crux, Google CrUX, CRUX Linux; dormant npm,
PyPI and crates packages) and no company holds it as a software mark in this
field, so the risk is discoverability, not legal.

Rejected for trademark reasons: Honeycomb (honeycomb.io), Nuc (Intel, licensed
to ASUS), Krux (Salesforce), Grit (grit.io), Kiln (Kiln-AI), Keel (keel-hq).
Also considered: Hatch, Flit, Nest, Flow, and the bee words Forager, Brood,
Comb, Hum, Osmia, Apis.

## References

- RFC: [hivecommons/hive#13536](https://github.com/hivecommons/hive/issues/13536)
- Ratchet: `src/internal/testutil/crux_boundary_ratchet_test.go`
- Prior extractions from Hive: pluk, promptargs, rationguard, hotshot,
  spektacular, dibs
- Related: #13532 (control arm)
