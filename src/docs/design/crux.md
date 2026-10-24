# Crux: the agent runtime boundary (#13536)

Status: **Stage 1 shipped (v5); Stage 2 prerequisites done, module cut
(item 4) not started.** The boundary is named, the inbound and outbound
architecture ratchets and the tmux funnel are live, `crux.Runtime` exists,
the dashboard is on it, Crux owns its value types and the hub, spoke,
dashboard and `cmd/hive` reference them as `crux.<Name>`, CI path filters
select Crux-only shards for Crux-only PRs, and the `crux` CLI ships. The
ring's outbound closure, about 55 Hive packages when this record began, is
now zero, in test code too: the outbound baseline is empty (the ring's tests
use their own `pkg/crux/testutil` copy of `Eventually*` and
`SkipUnlessRequired`); see [Outbound edges](#outbound-edges). What is left of Stage 2 is the move, and
its scope and open questions are under
[Step 4: the module cut](#step-4-the-module-cut). Stage 3 is design only.

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
| Engines and gateway | `pkg/claude`, `pkg/inference`, `pkg/openrouter`, `pkg/watsonx` |
| Policy at the tool boundary | `pkg/proxy` (MITM), `pkg/toolapprove`, `pkg/pushbroker` |
| Isolation | `pkg/sandbox`, `pkg/kubejob` |
| Observation | `pkg/watchdog`, `pkg/trajectory`, `pkg/logscrub` |
| Stdlib-only leaves | `pkg/crux/types`, `pkg/crux/config`, `pkg/crux/effects`, `pkg/crux/gitidentity`, `pkg/crux/jev`, `pkg/crux/jsonextract`, `pkg/crux/outputschema` |
| Surface and CLI | `pkg/crux`, `pkg/crux/cli` |

The same list, as directories, is `CRUX_DIRS` in
`.github/scripts/crux-scope.sh` (CI path filter) and `cruxModulePrefixes` in
`src/internal/testutil/crux_outbound_ratchet_test.go`; the two must agree.
`cmd/crux`, the `crux` binary's `main`, is a host in the root module like
`cmd/hive` (Stage 2, prerequisite 3): it holds the Hive-backed
`cli.DefinitionLoader` and reaches the runtime only through `pkg/crux`.

The RFC's claim that Crux's outbound coupling is "one `pkg/github` symbol and
two `pkg/config` fields" was wrong. Measured on v5 at `437843c` (Oct 23,
2026) with `go list -deps` plus an AST walk over selector expressions,
`pkg/agent` alone imported 19 Hive packages outside the ring in non-test
code, used 60 distinct `pkg/config` symbols, and `config.AgentConfig`
appears in about 105 of its test files. The transitive closure of the ring
is about 55 Hive packages. What is true is that pluk is reached through
`exec.LookPath`, which is already a process boundary. The measured edge set
and what was done about it is in [Outbound edges](#outbound-edges).

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
3. `crux.Runtime` (shipped): `src/pkg/crux/runtime.go` is the exported
   runtime surface, grouped as `Lifecycle`, `Observer`, `Kicker`,
   `Definitions`, `Fleet` and `Credentials`; every method is annotated with
   the consumer that needs it and `*agent.Manager` satisfies it directly, so
   the adapter is zero-cost. `pkg/dashboard` holds a `crux.Runtime` in
   `Dependencies.AgentMgr`; `pkg/hub` holds no manager at all.

   The value types are Crux's own. Everything on the surface except
   `Process` — `ProcessState` and the `State*` values, `KickDispatch` and
   the `KickPhase*` values, `KickOutcome` and the `KickOutcome*` values,
   `KickLogInfo`, `KickRecord`, `RestartEvent`, `SubAgentModel`,
   `StartFailureClass` and its values, `TurnLoss`, `TurnInterruption`, and
   the provider-status vocabulary (`ProviderStatus`,
   `ProviderAgentEvidence`, `RollUpProviderStatus`, `BackendAuthState`, the
   `BackendAuth*`, `ProviderAuth*` and `Provider*` constants) — is DEFINED
   in `pkg/crux/types`, a stdlib-only leaf that `pkg/agent` imports and
   re-exports as aliases and that `pkg/crux` re-exports as `crux.<Name>`.
   Consumers never see the leaf. It is a separate package only because
   `pkg/crux` still aliases `agent.AgentProcess` as `crux.Process` and so
   imports `pkg/agent`, which cannot import it back; it folds into
   `pkg/crux` once `Process` is its own type (Stage 2). The governor-side
   audit vocabulary (lease stages, commit trailers, artifact links) that
   `pkg/agent` used to re-export from `pkg/agentaudit` is no longer
   re-exported; consumers reference `agentaudit` directly.

   Ratchet counts over that sequence: `pkg/dashboard` 68 → 51 (onto
   `crux.Runtime`) → 39 (provider status) → 19 (audit eviction);
   `pkg/hub` 18 → 1 (`AgentAvailableAtACMMLevel`); `pkg/hub/spoke` 15 → 5
   → 3; `cmd/hive` 37 → 33 → 22; `pkg/loginscan` 7 → 4. What remains in
   every consumer is the manager and its constructors (`Manager`,
   `NewManager`, `ProjectContext`, `NewRingBuffer`), agent-mode and
   capability parsing, UID maps, Copilot-token helpers and pane
   classification: implementation the test-only constructors and
   `cmd/hive`'s wiring need, not observation. `pkg/crux` and everything
   under it is inside the boundary for the ratchet's purposes.
4. Agent configuration as its own struct and file (shipped):
   `AgentRuntimeConfig` and `AgentRuntimeGovernorConfig` in
   `src/pkg/config/agent_config.go`, embedded inline in `Config` and
   `GovernorConfig` so every `hive.yaml` key keeps its position.
5. CI path filters (shipped, #13541): `.github/scripts/crux-scope.sh`
   classifies a PR's diff as `crux-only`, `none`, `mixed` or `full` against
   the ring's directory list; `v2-tests.yml` consults the verdict only when
   dependency analysis already selected a partial package set, skipping the
   hub slices and the dashboard shuffle lane on a crux-only diff and the
   agent slices on a diff that touches no Crux package. Changes to
   `go.mod`, `src/internal/`, `pkg/config`, workflows or CI scripts force
   `full`. The aggregate required check stays fail-closed.
6. A `crux` CLI (shipped): `src/cmd/crux` with `run`, `ps`, `logs`, `kick`,
   `stop`, each mapped onto `crux.Runtime` methods. See [CLI](#cli) below.
7. Outbound ratchet (shipped): `src/internal/testutil/crux_outbound_ratchet_test.go`
   lists, per ring package, the Hive packages outside the ring it imports
   (non-test and test), exact and fail-closed in both directions like the
   inbound ratchet. The edges it froze and the ones it removed are in
   [Outbound edges](#outbound-edges).

**Stage 2: nested module.** `src/crux/` as its own Go module via `go.work`,
own tags (`crux/vX.Y.Z`), own coverage floor and CI lane; same repo, same PRs,
same v5/v6 branches. The `crux` binary that this stage was going to ship
already exists (item 6), so the stage is the module cut alone.

Prerequisites done, move not started. The move was blocked until the
outbound ratchet's `pkg/config` rows were gone, which prerequisite 1
delivered, until `pkg/agent` and the sibling ring packages imported nothing
from Hive, which prerequisite 2 delivered, and until the proxy's edges were
cut and `cmd/crux` was scoped, which prerequisite 3 delivered. The reason
the order mattered: a nested module whose packages import `pkg/config`
needs the root module to `replace` the nested one *and* the nested one to
`replace` the root: a module cycle. It builds, but nothing can consume
`github.com/hivecommons/hive/crux` without also pulling in all of Hive, so
tags on it mean nothing, and every import path under `pkg/agent/...` would
move for the ring's consumers and for the test shards, coverage floors and
`crux-scope.sh` that key on `./pkg/agent`. Doing the move before the config
cut buys the churn without the benefit. Prerequisites, in order:

1. **Done** (#13834, #13868, #13883). An agent-runtime configuration
   leaf: `config.AgentConfig` (35 methods, embedding review-model,
   rotation and provider-budget types) and the `AgentRuntimeConfig` /
   `AgentRuntimeGovernorConfig` structs from item 4 moved into the ring
   as the stdlib-only `pkg/crux/config` (package name `config`, so a
   consumer's migration was an import-path swap); `pkg/config` keeps them
   as aliases so `hive.yaml` loading and every key position are
   unchanged. No ring package imports `pkg/config` any more; the one
   `config` row left in the outbound baseline is `cmd/crux`, the process
   boundary that loads `hive.yaml` for the CLI's `DefinitionLoader`. What
   moved, what was aliased and what was inverted is in
   [Configuration leaf](#configuration-leaf).
2. **Done** (#13916, #13933, #13937). The remaining `pkg/agent` edges, plus
   the sibling ring edges that fell cheaply: 27 non-test symbols across the
   six `pkg/agent` edges measured after prerequisite 1, and 7 more across
   `watchdog`, `toolapprove`, `trajectory` and `inference`. Each symbol was
   moved, aliased or inverted by one rule: a value type or pure function
   with no Hive dependency moves into a stdlib-only `pkg/crux/<name>` leaf
   and the Hive package aliases it; a service with Hive dependencies becomes
   a narrow interface the host injects; a whole package that is really
   runtime-side and imported only by the ring joins the ring.

   | Edge | Symbols | Decision | Where |
   |---|---|---|---|
   | `agent → effects` | `Boundary` (1) | moved whole | `pkg/crux/effects`; `pkg/effects` aliases every symbol (#13916) |
   | `agent → gitidentity` | `AgentIdentity`, `AppLinkedAgentIdentity` (2) | moved whole | `pkg/crux/gitidentity`; the two host callers repointed, `pkg/gitidentity` removed (#13916) |
   | `agent → jev` | `DefaultEndpoint`, `EndpointEnvVar`, `ModeEnvVar`, `SkillFile`, `SkillMarkdown`, `SkillRelDir` (6) | moved: environment contract + `jev-decide` skill | `pkg/crux/jev`; `pkg/jev` keeps the server and CLI and aliases the six (#13916) |
   | `agent → outputschema` | `AgentReport`, `AgentReportFilePrefix`, `AgentReportFileSuffix`, `Validate` (4) | moved: schema + validator; `proof.Provenance` inverted to a type | `pkg/crux/outputschema`; the receipt handle is `pkg/crux/types.ProofProvenance`, `pkg/convergence/proof` and `pkg/outputschema` alias; report directory / path helpers stay host side (#13916) |
   | `trajectory → jsonextract` | `Object` (1) | moved whole | `pkg/crux/jsonextract`; `pkg/jsonextract` aliases (#13916) |
   | `agent → fleetreport`, `watchdog → fleetreport` | `Observe`, `Clear`, seven `Class*` (9 non-test across the two packages) | inverted | `pkg/crux/types.ConditionReporter` (`Observe`, `Clear`); the classes are `types.Class*` constants and `fleetreport` aliases them. `Manager.SetConditionReporter` (pane text redacted before it leaves the ring) and `watchdog.WithConditionReporter`; `cmd/hive` injects `fleetreport.DefaultReporter{}`, which delegates to the package tracker so `SetDefaultTrackerForTest` still swaps it (#13933) |
   | `agent → pushbroker` | `Broker`, `DefaultRemote`, `GitHubAppMinter`, `PushEnv`, `Result`, `TokenMinter` (6) | joined the ring; its one Hive edge inverted | `pkg/pushbroker` is a ring package. Its `*github.AppAuth` field became `pushbroker.TokenScoper`; `cmd/hive`'s `appMinter` adapts and leaves a nil interface for a nil credential (#13933) |
   | `inference → watsonx` | `DefaultMinter`, `ProjectIDHeader` (2) | joined the ring | `pkg/watsonx` is stdlib-only and imported only by `pkg/inference` (#13933) |
   | `watchdog → rotation` | `Prober` (1) | inverted | `watchdog.UsageProber` (`Provider`, `ProbeError`); `cmd/hive` adapts each `rotation.Prober`, and the two Claude-credential probe tests moved with the adapter (#13937) |
   | `toolapprove → ioscan` | `ScanInput`, `ScanOutput`, `SeverityHigh` (3) | inverted | `toolapprove.TextScanner`; `pkg/ioscan.ToolApproveScanner` implements it and `cmd/hive` and `pkg/turn` install it at init (`SetDefaultTextScanner`). A desk with no text scanner fails closed (`ErrNoTextScanner`). `ioscan` could not join the ring: `scheduler`, `github`, `config` and `dashboard` import it (#13937) |

   `pkg/agent`, `pkg/inference`, `pkg/toolapprove`, `pkg/trajectory` and
   `pkg/watchdog` import nothing outside the ring in non-test code; the two
   `internal/testutil` *(test)* rows are the ratchets themselves. The ring
   prefix list grew by `pkg/pushbroker` and `pkg/watsonx` in
   `crux_outbound_ratchet_test.go`, `crux-scope.sh` and the v2-tests
   boundary comment together.
3. **Done** (#13941, #13942, and the `cmd/crux` re-scope that followed).
   `pkg/proxy`'s edges and `cmd/crux`'s, measured on v5 after prerequisite 2
   as distinct non-test `<pkg>.<Symbol>` selectors, cut by the same three
   rules as prerequisite 2. The inventory in the tracker named
   `github.AgentProxyToken` as the `proxy → github` edge; on inspection that
   type was already behind a function field, and the real edge was the REST
   rate-limit accounting (`CoreRateLimitSnapshot`, `RecordRESTRequest`,
   `RecordInjectedRESTRequest`).

   | Edge | Symbols | Decision | Where |
   |---|---|---|---|
   | `proxy → inferencehealth` | `GatewayStatus`, `NewStore`, `Store` (3) | moved whole | `pkg/crux/inferencehealth`; `pkg/inferencehealth` aliases every symbol. Its one test that reads `fleetreport` stayed Hive side in the alias package (#13941) |
   | `proxy → issueshape` | `BodyHasMisEscapedNewlines`, `UnsubstitutedTemplatePlaceholder` (2) | moved whole | `pkg/crux/issueshape`; `pkg/issueshape` aliases (#13941) |
   | `proxy → ioscan` | `CanaryLeak`, `CanaryRegistry`, `NewCanaryRegistry` (3) | inverted; the leak record moved | `proxy.CanaryScanner` (`ScanEgress`), installed by `Proxy.SetCanaryScanner`, which `cmd/hive` already called with the `ioscan` registry. `CanaryLeak` is `pkg/crux/types.CanaryLeak` and `ioscan` aliases it. No scanner means no egress inspection; the host always installs one (#13942) |
   | `proxy → tokens` | `BackendCopilot`, `InferenceSink`, `NewInferenceSink`, `SessionEntry` (4) | inverted | `proxy.UsageSink` (`Record`, `RecordWithCache`, `RecordCopilotUsage`), installed by `Proxy.SetTokenSink`; `*tokens.InferenceSink` satisfies it unchanged and `cmd/hive` injects it. A nil sink records nothing, matching the nil-pointer tolerance the concrete type had (#13942) |
   | `proxy → github` | `CoreRateLimitSnapshot`, `RecordRESTRequest`, `RecordInjectedRESTRequest` (3) | inverted | `proxy.GitHubRESTAccounting` (`RecordRequest`, `RecordInjected`, `CoreSnapshot`), installed by `Proxy.SetGitHubRESTAccounting`; `cmd/hive/proxy_rest_accounting.go` adapts the process-global `pkg/github` store. Without it the proxy still forwards and reports no core-bucket snapshot (#13942) |
   | `cmd/crux → config`, `cmd/crux → defsrc` | `AgentConfig`, `Config`, `Load`, `LoadAgentOverrides`, `PoliciesConfig`; `DefinitionKind`, `MergeAllowedFields`, `ParseDefinition` (8) | re-scoped: `cmd/crux` left the ring set | Every one of the eight is the `cli.DefinitionLoader` implementation: `hive.yaml`, Hive's per-agent defaults, the portable `AgentDefinition`. That is host knowledge, and `main` has no layer above it to be injected from, so `cmd/crux` is to the `crux` binary what `cmd/hive` is to `hive`: a host composition root in the root module, built against the ring. It left `cruxModulePrefixes` (which `crux-scope.sh` and the v2-tests boundary comment never listed it in) and the inbound ratchet keeps measuring it at zero `agent.<Symbol>` references, so it can reach the runtime only through `pkg/crux` |

   The proxy stayed in the ring. It is still an inbound consumer (14
   `agent.<Symbol>` references: `Manager`, agent-mode and capability
   parsing, UID maps, the Linear credential), which is fine inside one
   module and is the first question step 4 has to answer, below.
4. Then the move. Scoped under [Step 4: the module cut](#step-4-the-module-cut).

### Step 4: the module cut

The shape, in one PR series on v5, each PR mergeable alone:

1. **Decide `pkg/proxy`'s home.** Its 14 inbound `agent.<Symbol>`
   references are the ring's only intra-ring dependency on the manager from
   a package that is not the manager, `pkg/crux` or `pkg/crux/cli`. Either
   it moves with the ring and those references become `crux.<Name>` on the
   exported surface (`crux.Runtime` already carries the manager; agent mode,
   capabilities and the UID map would need re-exports), or it stays in the
   root module as a host-side component that imports the nested module. The
   proxy is imported by exactly one non-ring package (`cmd/hive`) and the
   runtime does not import it, so either is consistent; the deciding
   question is whether a second Crux consumer wants the gateway. Default:
   it moves, because a runtime without its credential gateway is not a
   runtime a second consumer can run.
2. **`src/crux/go.mod`.** Module path `github.com/hivecommons/hive/crux`,
   same Go version as the root. Its third-party closure today is `cel-go`
   (and `antlr4-go`, `cel.dev/expr`, `genproto`, `protobuf` under it),
   `go-qrcode`, OpenTelemetry (`otel`, `otel/trace`, `otel/metric`,
   `otel/log`, `auto/sdk`), `go.yaml.in/yaml/v3` and `gopkg.in/yaml.v3`,
   `golang.org/x/exp` and `golang.org/x/text`. Nothing in that list is
   Hive-specific, which is the point; the versions are pinned to the root's
   `go.sum` at the cut.
3. **The move.** `git mv` of every package in `cruxModulePrefixes` to
   `src/crux/<pkg>` (`pkg/agent` → `crux/agent`, `pkg/crux/types` →
   `crux/types`, `pkg/crux/cli` → `crux/cli`, and so on; `pkg/crux` itself
   becomes the module root package), with the import-path rewrite applied
   by `gofmt -r` or `sed` in the same commit. The alias packages in the
   root (`pkg/config`, `pkg/effects`, `pkg/jev`, `pkg/outputschema`,
   `pkg/jsonextract`, `pkg/inferencehealth`, `pkg/issueshape`, `pkg/ioscan`,
   `pkg/github`, `pkg/agentaudit`, `pkg/fleetreport`) are what make this a
   path swap and not a symbol migration for the 35 non-ring packages that
   import a ring package today.
4. **`src/go.work`** listing `.` and `./crux`, and in the root `go.mod` a
   single `replace github.com/hivecommons/hive/crux => ./crux` with a
   `require` on a pseudo-version so `go build` works without the workspace
   (CI caches, `go install`, downstream `go get`). No `replace` in the
   nested module in the other direction, ever; the outbound ratchet becomes
   `go list -deps` over the nested module asserting no
   `github.com/hivecommons/hive/pkg/...` or `.../internal/...` import, which
   is the same test with the module boundary doing the walk. The two
   test-only `internal/testutil` rows went first (pre-cut): `Eventually*`
   (3 helpers, 9 uses) and `SkipUnlessRequired`/`SkipfUnlessRequired` (21
   uses) copied into `pkg/crux/testutil`, which the move renames to
   `crux/internal/testutil`.
5. **CI.** `crux-scope.sh`'s `CRUX_DIRS` becomes the single prefix
   `src/crux/`; the v2-tests agent slices and the crux-only `go list` run
   from `src/crux` (`working-directory`) against `./...`; the per-package
   coverage floors keyed `agent`, `crux`, `proxy`, `watchdog`, … are
   re-keyed to the new paths with the same values, and the module gets its
   own floor line in the aggregate. `scripts/check-go-ratchet-completeness.sh`
   has to learn to walk both modules. `golangci-lint` runs per module.
6. **Tags.** `crux/vX.Y.Z` tags on the same commits as Hive's, cut by the
   release workflow alongside, so `go get
   github.com/hivecommons/hive/crux@crux/v5.N.0` resolves.

Still open before the series starts:

- The proxy decision (1), which also decides whether `pkg/tokens`,
  `pkg/ioscan` and `pkg/github` are adapted from `cmd/hive` (as now) or
  from a thin root-module package the proxy's host wiring moves into.
- The `crux` binary's home. `cmd/crux` is a root-module host (prerequisite
  3) and keeps building against the nested module, so the nested module
  ships no binary at the cut. A `crux/cmd/crux` that reads only
  `crux/config`-shaped definitions (no `hive.yaml`, no Hive presets) is a
  Stage 3 question, not a step 4 one.
- The dashboard, hub and spoke import `pkg/agent` for the test-only
  constructors (`NewManager`, `ProjectContext`, `NewRingBuffer`); they
  keep working through the `replace`, but a second consumer's `go get`
  of the nested module should not need them, so they are a reason to
  finish `crux.Process` as its own type and fold `pkg/crux/types` into the
  module root in the same series.
- `go.work` and the module cache in CI: the `setup-go` cache key is the
  root `go.sum`; it has to include `crux/go.sum` or the agent slices
  download the closure cold.

**Stage 3: platform.** `crux serve` with an API (gRPC or HTTP), tenancy, auth,
quotas; Hive and hub become clients; the hub-to-runtime contract becomes
explicit and versioned; move to `hivecommons/crux`. Only when a second
consumer is real (candidates: Nous / Experiment Lab, Spektacular, Dibs,
external optimizer and formal-verification lanes, enterprise agent services).

## CLI

The Stage 2 plan called for a `crux` binary; it shipped in Stage 1 instead
(`src/cmd/crux`, logic in `src/pkg/crux/cli`, operator page
[crux-cli.md](../crux-cli.md)) because it is the cheapest proof that
`crux.Runtime` is a sufficient surface: every subcommand is written against
the interface, and the only `*agent.Manager` reference is the constructor in
`pkg/crux/cli/runtime.go`. Putting the logic under `pkg/crux/` keeps it inside
the ratchet boundary, so it adds no baseline row.

| Subcommand | `crux.Runtime` methods |
|---|---|
| `run` | `Definitions.AddAgent`, `Lifecycle.Start`, `Observer.GetBufferOutput` (stream), `Lifecycle.Stop` (on interrupt) |
| `ps` | `Observer.AllStatuses` |
| `logs` | `Observer.ResolveAgent`, `Observer.GetOutput` / `GetBufferOutput` / `FullLogHeader`+`CaptureFullLog` |
| `kick` | `Observer.ResolveAgent`, `Kicker.SendKick` |
| `stop` | `Observer.ResolveAgent`, `Lifecycle.Stop` |

Definitions are the two formats hive already reads (`kind: AgentDefinition`
via `defsrc`, or an `agents_dir` overlay via `config.LoadAgentOverrides`),
and `--config hive.yaml` builds the runtime from `cfg.EnabledAgents()` the
way `cmd/hive` does. No new format.

What writing it surfaced: the runtime is in-process. `Lifecycle.Stop` and
`Kicker.SendKick` consult the Manager's in-memory state, and `Runtime` has no
adoption method, so a second process cannot stop or kick an agent that a
first process started. The CLI therefore makes `crux run` the long-lived
command and has it serve a unix control socket (`<workdir>/crux.sock`) that
`ps`/`logs`/`kick`/`stop` forward to when present. That socket is a
single-work-dir convenience and is not the Stage 3 API; what Stage 3 has to
add to `Runtime` (or beside it) is exactly the piece the socket papers over:
a way for a process other than the launcher to own or adopt an agent.

## Configuration leaf

`pkg/crux/config` is the stdlib-only home of the agent runtime's
configuration types (Stage 2 prerequisite 1). It was cut from `pkg/config`
by declaration closure: every type the ring reads, every method on those
types (Go cannot attach a method to an alias of another package's type, so
a type's methods travel with it), and the constants and defaults they
reference. `pkg/config` imports it as `cruxconfig` and re-exports each
symbol in `pkg/config/crux_aliases.go` (`type AgentConfig =
cruxconfig.AgentConfig`, `const JevModeAssist = cruxconfig.JevModeAssist`,
`var InferenceBackends = cruxconfig.InferenceBackends`), the direction
`pkg/crux/types` established in #13770: Crux owns, Hive aliases. Loading,
validation that needs YAML or ACMM packs, overlays and `Config` itself stay
in `pkg/config`; the two pieces of `AgentConfig` that needed a host-only
type were reshaped rather than moved — `ReviewModelsConfig.Select` (uses
`pkg/modelfamily`) became `config.SelectReviewModel`, and
`AgentConfig.ContinuousInMode` takes a `ContinuousCadence` interface that
the governor's `Cadence` satisfies instead of the `Cadence` type.

The `pkg/config` symbols each ring package used before the cut, measured on
v5 at `1fe983e` (Oct 24, 2026) by an AST walk over `config.<X>` selectors
(distinct symbols; *test* counts only `_test.go` files):

| Ring package | Non-test | Test-only | Files | Not in the leaf |
|---|---|---|---|---|
| `pkg/agent` | 58 | 21 | 13 + 112 test | `GovernorConfig`, `LoggingConfig` *(test)* |
| `pkg/crux` | 1 | 0 | 1 + 1 test | — |
| `pkg/crux/cli` | 5 | 0 | 3 + 2 test | `Config`, `Load`, `LoadAgentOverrides`, `PoliciesConfig` |
| `pkg/inference` | 7 | 5 | 1 + 3 test | `Config`; `GovernorConfig` *(test)* |
| `pkg/proxy` | 9 | 4 | 3 + 2 test | `OriginLaneFromAgent`, `WithIssueOriginLabel`; `LoadWithOverrides` *(test)* |
| `pkg/toolapprove` | 7 | 4 | 4 + 3 test | `AutoMergeConfig`, `Config`, `PlanAutoApproveForLevel`, `PlanningConfig` |
| `pkg/watchdog` | 1 | 3 | 1 + 1 test | — |

111 distinct symbols across the ring; 99 are in the leaf, 12 are not. The
12 are the whole-`Config` loaders (`Config`, `GovernorConfig`,
`LoggingConfig`, `PoliciesConfig`, `Load`, `LoadAgentOverrides`,
`LoadWithOverrides`), the ACMM plan-approval policy (`AutoMergeConfig`,
`PlanningConfig`, `PlanAutoApproveForLevel`, which drags the embedded pack
YAML) and the review origin-label vocabulary (`OriginLaneFromAgent`,
`WithIssueOriginLabel`). These are the inversion work for the migration
PRs: a ring package that takes `*config.Config` narrows to the
`AgentRuntimeConfig` / `AgentRuntimeGovernorConfig` value or to a small
interface the host fills in, the same shape as `agent.SpanStarter`.

Migration so far (the `config` row leaves a package's outbound baseline in
the same change that swaps its imports):

| Ring package | Moved | Inverted |
|---|---|---|
| `pkg/agent` | import-path swap in 13 + 112 test files; `GovernorConfig.ResolvedGateways` / `ResolveGateway` / `ValidateBackend` and `LegacyLiteLLMGatewayName` moved onto `AgentRuntimeGovernorConfig` (they read only `Gateways` and `LiteLLM`; `GovernorConfig` embeds it, so `cfg.Governor.ResolveGateway` still promotes); the kick-transcript test drives `NormalizeKickLogSource` instead of `LoggingConfig` | — |
| `pkg/crux` | import-path swap; `crux.ProjectContext` re-exports `agent.ProjectContext` so a host can build one without naming `pkg/agent` | — |
| `pkg/inference` | import-path swap; `GovernorConfig.ExplicitLiteLLMGateway` / `ResolveLiteLLMRemoteRoute` moved onto `AgentRuntimeGovernorConfig` (they read `Gateways` and `LiteLLM`) | `ResolveLiteLLMRoute`, `ExplicitLiteLLMGateway` and `ResolveWatsonxGateway` take `*config.AgentRuntimeGovernorConfig` instead of `*config.Config`; cmd/hive passes `&cfg.Governor.AgentRuntimeGovernorConfig` |
| `pkg/proxy` | import-path swap; the origin-label vocabulary (`origin_labels.go`: `OriginLaneFromAgent`, `WithIssueOriginLabel`, `CanonicalOriginLabel`, `OriginLabelRenames` and the `*OriginLabel*` constants) moved into the leaf | the one test that loaded a full `hive.yaml` (`LoadWithOverrides`) moved to `cmd/hive/agent_repo_scope_test.go` |
| `pkg/toolapprove` | import-path swap (`ToolApprovalConfig`, `SelfMergeMinACMMLevel`, `WildcardMatch` were already in the leaf) | `DeskFromConfig` takes the `ToolApprovalConfig` block, not `*Config`; `ACMMLevelOf` is `Config.ACMMLevelOrZero` on the host. The ACMM-pack plan gates (`PlanAutoApproveForLevel`, `PlanningConfig.PlanFromLabelEnabled`) are behind `toolapprove.PlanPolicy`, installed by `Desk.SetPlanPolicy`; cmd/hive's `planPolicyFromConfig` wires the real predicates and `cmd/hive/approvalparity_test.go` carries the per-level parity tests against them (the leaf-side tests pin that the desk follows its policy and fails closed without one). A desk with no policy sends plan kinds to the operator lane, which is why the host wires it unconditionally in `buildApprovalDesk`. |
| `pkg/watchdog` | import-path swap (`WatchdogConfig` was already in the leaf) | — |
| `pkg/crux/cli` | import-path swap | `cli.DefinitionLoader` (`LoadConfig`, `LoadDefinitionFile`) on `cli.Env`, passed to `cli.Main`. The Hive-backed implementation — `config.Load`, `config.LoadAgentOverrides`, `Config.ApplyAgentDefaults`, `defsrc.ParseDefinition` / `MergeAllowedFields`, cmd/hive's `ProjectContext` derivation — is `cmd/crux/definitions.go`. `pkg/defsrc` stays outside: beyond the portable-YAML parser it holds `ApplyToConfig(*config.Config)` and the governor `Cadence`, so moving it in would drag `Config`. |

## Outbound edges

Imports from ring packages to Hive packages outside the ring, as the
outbound ratchet records them after Stage 2 prerequisites 1, 2 and 3 and
the step 4 pre-cut: none. The baseline is empty in both directions (test
and non-test). The last two rows were `pkg/agent → internal/testutil`
*(test)* and `pkg/inference → internal/testutil` *(test)*: the `Eventually*`
and `Skip[f]UnlessRequired` helpers those tests used copied into
`pkg/crux/testutil`, a ring package that becomes `crux/internal/testutil`
at the module cut; `internal/testutil` keeps the canonical copy and the
tests of the contract.

Before the pre-cut the table had those two test-only rows; before
prerequisite 3 the table also had `pkg/proxy → github,
inferencehealth, ioscan, issueshape, tokens` and `cmd/crux → config,
defsrc`; before prerequisite 2, `pkg/agent → effects, fleetreport,
gitidentity, jev, outputschema, pushbroker`, `pkg/inference → watsonx`,
`pkg/toolapprove → ioscan`, `pkg/trajectory → jsonextract` and
`pkg/watchdog → fleetreport, rotation`; the per-edge decisions are under
[Staged plan](#staged-plan). `cmd/crux` is no longer a ring package; it is
a host in the root module (prerequisite 3) and the inbound ratchet measures
it instead.

Edges removed in #13785, and the shape each cut took, because the same
shapes applied to the rows that followed:

| Edge | Cut |
|---|---|
| `pkg/agent → pkg/github` (`CreatePRResult`, `AgentTokenCachePath`) | Type moved into the stdlib-only leaf `pkg/crux/types` (`PRResult`, `AgentTokenCachePath`); `pkg/github` aliases and delegates, so its callers did not change. |
| `pkg/agent → pkg/github` (test, `AppAuth`) | The sandbox wiring test uses a local `TokenMinter` fake. |
| `pkg/agent → pkg/agentaudit` | Runtime audit constants, `AuditSink` and `Fields` moved to `pkg/crux/types`; `agentaudit` aliases them. |
| `pkg/agent → pkg/tracing` | Inverted: `agent.SpanStarter` and `Manager.SetSpanStarter`; the default is the otel global tracer and `cmd/hive` injects `tracing.StartSpan`. |
| `pkg/agent → pkg/review` (test) | The test moved to `pkg/review` and reads `crux.FatalNetworkErrorPatterns()`. |

The rule the inbound ratchet imposes on these cuts: an inversion must not
add an `agent.<Symbol>` reference in a consumer, so seams are `Manager`
setters or `crux.<Name>` / `pkg/crux/types` re-exports, never new exported
`agent` symbols the host has to name.

## Running the Crux set alone

There is no separate module yet, so the ring is tested as a package list
from `src/` (`./cmd/crux` is a host that builds against it, like
`./cmd/hive`, and is tested with the rest of the root module):

```sh
cd src && go test ./pkg/agent/... ./pkg/crux/... \
  ./pkg/agentmode ./pkg/agentparse ./pkg/agentsmd ./pkg/claude \
  ./pkg/sandbox ./pkg/kubejob ./pkg/toolapprove ./pkg/skillreg \
  ./pkg/inference ./pkg/openrouter ./pkg/watchdog ./pkg/trajectory \
  ./pkg/logscrub ./pkg/proxy ./pkg/pushbroker ./pkg/watsonx
```

Both ratchets live in `./internal/testutil` and run with that package. In
CI, a PR whose diff is entirely inside the ring gets the `crux-only` verdict
from `crux-scope.sh` and runs only the agent slices (item 5). Two
`pkg/agent` tests fail on macOS because the tmux socket path under
`$TMPDIR` exceeds `sun_path`; they pass on Linux and in CI.

## Not doing

- Repo split before Stage 1: the co-change tax lands before any second
  consumer exists and doubles v5/v6 top-up work.
- Rewriting engines or the proxy. This is a boundary, not a rewrite.
- Shortening the PR gate by splitting: `pkg/agent` is a 429 s serial test
  monolith already sliced five ways; the gate is bounded by the slowest shard,
  so only path filters (Stage 1, item 5, shipped) change anything, and
  they work in-repo.

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
- Ratchets: `src/internal/testutil/crux_boundary_ratchet_test.go` (inbound),
  `src/internal/testutil/crux_outbound_ratchet_test.go` (outbound)
- CI scope: `.github/scripts/crux-scope.sh` (#13541)
- Prior extractions from Hive: pluk, promptargs, rationguard, hotshot,
  spektacular, dibs
- Related: #13532 (control arm)
