# Crux: the agent runtime boundary (#13536)

Status: **Stage 1 shipped (v5); Stage 2 blocked on `pkg/config`.** The
boundary is named, the inbound and outbound architecture ratchets and the
tmux funnel are live, `crux.Runtime` exists, the dashboard is on it, Crux
owns its value types and the hub, spoke, dashboard and `cmd/hive` reference
them as `crux.<Name>`, CI path filters select Crux-only shards for Crux-only
PRs, and the `crux` CLI ships. Stage 2 (nested module) is measured but not
started: the ring's outbound closure is about 55 Hive packages, almost all
through `pkg/config`; see [Outbound edges](#outbound-edges). Stage 3 is
design only.

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
| Surface and CLI | `pkg/crux`, `pkg/crux/cli`, `cmd/crux` |

The same list, as directories, is `CRUX_DIRS` in
`.github/scripts/crux-scope.sh` (CI path filter) and `cruxModulePrefixes` in
`src/internal/testutil/crux_outbound_ratchet_test.go`; the two must agree.

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

Not started. It was blocked until the outbound ratchet's `pkg/config` rows
were gone, which prerequisite 1 delivered, and until `pkg/agent` and the
sibling ring packages imported nothing from Hive, which prerequisite 2
delivered; what remains is the proxy and `cmd/crux` (prerequisite 3). A nested module whose packages import `pkg/config` needs the root
module to `replace` the nested one *and* the nested one to `replace` the
root: a module cycle. It builds, but nothing can consume
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
3. `pkg/proxy`'s edges and `cmd/crux`'s, measured on v5 after prerequisite 2
   (distinct non-test `<pkg>.<Symbol>` selectors):

   | Edge | Non-test | Symbols | Likely shape |
   |---|---|---|---|
   | `proxy → github` | 1 | `AgentProxyToken` | move the token type to `pkg/crux/types`, `pkg/github` aliases |
   | `proxy → inferencehealth` | 3 | `GatewayStatus`, `NewStore`, `Store` | a status-sink interface the host injects, or the store joins the ring if nothing else imports it |
   | `proxy → ioscan` | 3 | `CanaryLeak`, `CanaryRegistry`, `NewCanaryRegistry` | a canary-registry interface the host injects (`ioscan` stays outside, as above) |
   | `proxy → issueshape` | 2 | `BodyHasMisEscapedNewlines`, `UnsubstitutedTemplatePlaceholder` | pure functions: move to a `pkg/crux` leaf, `issueshape` aliases |
   | `proxy → tokens` | 4 | `BackendCopilot`, `InferenceSink`, `NewInferenceSink`, `SessionEntry` | a usage-sink interface the host injects |
   | `cmd/crux → config` | 5 | `AgentConfig`, `Config`, `Load`, `LoadAgentOverrides`, `PoliciesConfig` | stays: this is the process boundary that loads `hive.yaml` for `cli.DefinitionLoader`; the nested module's `cmd/crux` is a host-side binary or gets a `crux/config`-only loader |
   | `cmd/crux → defsrc` | 3 | `DefinitionKind`, `MergeAllowedFields`, `ParseDefinition` | the portable-YAML parser could move to a leaf; `ApplyToConfig(*config.Config)` and the governor `Cadence` stay |

   13 non-test symbols on the proxy, 8 on `cmd/crux`. The proxy is both a
   ring member and an inbound consumer (14 `agent.<Symbol>` references);
   whether it stays in the ring is decided when those edges are cut, not
   before.
4. Then the move: `git mv` of the ring under `src/crux/`, `src/crux/go.mod`,
   `src/go.work`, one `replace` in the root module, and the shards, floors
   and `crux-scope.sh` repointed in the same PR.

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
outbound ratchet records them after Stage 2 prerequisites 1 and 2: no ring
package imports `pkg/config`, and `pkg/agent` and the sibling ring packages
import nothing from Hive. Entries marked *(test)* appear only in `_test.go`
files.

| Ring package | Outside Hive imports |
|---|---|
| `cmd/crux` | `config`, `defsrc` (the `DefinitionLoader` implementation; moved here from `pkg/crux/cli`, see [Configuration leaf](#configuration-leaf)) |
| `pkg/agent` | `internal/testutil` *(test)* |
| `pkg/inference` | `internal/testutil` *(test)* |
| `pkg/proxy` | `github`, `inferencehealth`, `ioscan`, `issueshape`, `tokens` |

Before prerequisite 2 the table also had `pkg/agent → effects, fleetreport,
gitidentity, jev, outputschema, pushbroker`, `pkg/inference → watsonx`,
`pkg/toolapprove → ioscan`, `pkg/trajectory → jsonextract` and
`pkg/watchdog → fleetreport, rotation`; the per-edge decisions are under
[Staged plan](#staged-plan).

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
from `src/`:

```sh
cd src && go test ./pkg/agent/... ./pkg/crux/... ./cmd/crux \
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
