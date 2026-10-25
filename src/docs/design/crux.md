# Crux: the agent runtime boundary (#13536, continued in #14138)

Status: **Stage 1 shipped (v5); Stage 2 step 4 shipped: the ring is the
nested Go module `github.com/hivecommons/hive/crux` at `src/crux/`
(#13950), joined to the root module by `src/go.work`; `crux.Process` is
its own type and `crux/types` is folded into the root (#13982, #13996);
the first cut of Stage 3's `crux serve` has landed (#13959).** The
boundary is named, the inbound and outbound architecture ratchets and the
tmux funnel are live, `crux.Runtime` exists, the dashboard is on it, Crux
owns its value types and the hub, spoke, dashboard and `cmd/hive`
reference them as `crux.<Name>`, CI path filters select Crux-only shards
for Crux-only PRs, and the `crux` CLI ships. The inbound ratchet is
empty: `cmd/hive` holds `crux.Runtime` and constructs it through
`crux/runtime.New` (#14138 stage 1, see
[Stage 1: the Runtime seam](#stage-1-the-runtime-seam)), so no package in
the root module imports `crux/agent` any more. The ring's outbound closure, about 55 Hive
packages when this record began, is now zero, in test code too: the
outbound baseline is empty (the ring's tests use their own
`crux/internal/testutil` copy of `Eventually*` and `SkipUnlessRequired`);
see [Outbound edges](#outbound-edges). What the move did and how to work
in the two-module tree is under
[Step 4: the module cut](#step-4-the-module-cut); what is left is under
[After the cut](#after-the-cut) and tracked in #14138 (#13536 was the
Stage 1 RFC and is closed).

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
| Stdlib-only leaves | `pkg/crux/config`, `pkg/crux/effects`, `pkg/crux/gitidentity`, `pkg/crux/jev`, `pkg/crux/jsonextract`, `pkg/crux/outputschema` |
| Surface and CLI | `pkg/crux`, `pkg/crux/cli` |

The same list, as directories, is `CRUX_DIRS` in
`.github/scripts/crux-scope.sh` (CI path filter) and `cruxModulePrefixes` in
`src/internal/testutil/crux_outbound_ratchet_test.go`; the two must agree.
`cmd/crux`, the Hive-hosted `crux` binary's `main`, is a host in the root
module like `cmd/hive` (Stage 2, prerequisite 3): it holds the Hive-backed
`cli.DefinitionLoader` and reaches the runtime only through `pkg/crux`. The
module ships its own binary too, `crux/cmd/crux`, wired to `crux/defs`
(#14138; see [Definition loaders](#definition-loaders)).

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
   `pkg/crux` once `Process` is its own type (Stage 2); it did in #13963,
   and the definitions now live in the `crux` root package. The governor-side
   audit vocabulary (lease stages, commit trailers, artifact links) that
   `pkg/agent` used to re-export from `pkg/agentaudit` is no longer
   re-exported; consumers reference `agentaudit` directly.

   Ratchet counts over that sequence: `pkg/dashboard` 68 → 51 (onto
   `crux.Runtime`) → 39 (provider status) → 19 (audit eviction);
   `pkg/hub` 18 → 1 (`AgentAvailableAtACMMLevel`); `pkg/hub/spoke` 15 → 5
   → 3; `cmd/hive` 37 → 33 → 22; `pkg/loginscan` 7 → 4. What remained in
   every consumer at that point was the manager and its constructors
   (`Manager`, `NewManager`, `ProjectContext`, `NewRingBuffer`), agent-mode
   and capability parsing, UID maps, Copilot-token helpers and pane
   classification: implementation the test-only constructors and
   `cmd/hive`'s wiring need, not observation. Stage 2 then took every row
   but `cmd/hive` to zero and `cmd/hive` to 12; the sequence is under
   [After the cut](#after-the-cut). `pkg/crux` and everything under it is
   inside the boundary for the ratchet's purposes.
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

Shipped in #13950 (after the pre-cut #13948), one PR on v5. What it did,
against the plan this section carried before it:

1. **`pkg/proxy` moved with the ring** (the default). Its 14
   `agent.<Symbol>` references are now an intra-module import of
   `crux/agent`; nothing was re-exported through `crux.` for it and no
   consumer count changed. The inbound ratchet no longer walks `src/crux`,
   so the `pkg/proxy: 14` row is gone from its baseline. `pkg/tokens`,
   `pkg/ioscan` and `pkg/github` stay adapted from `cmd/hive` (the
   prerequisite-3 seams); no thin root-module wiring package was needed.
2. **`src/crux/go.mod`**: module `github.com/hivecommons/hive/crux`, the
   root's Go version, direct requires `cel-go`, `go-qrcode`, `otel`,
   `otel/trace` and `gopkg.in/yaml.v3`; `go.sum` seeded from the root's
   pins (so `cel-go` stays on the version the root uses rather than the
   latest, whose rename of a package would have broken the build) and
   tidied with `GOWORK=off`.
3. **The move**, with `git mv` so history follows the files:

   | before | after |
   |---|---|
   | `pkg/crux/*.go` | `crux/` — the module root package is `crux` |
   | `pkg/crux/{cli,config,effects,gitidentity,inferencehealth,issueshape,jev,jsonextract,outputschema,types}` | `crux/<same>` |
   | `pkg/crux/testutil` | `crux/internal/testutil` |
   | `pkg/{agent,agentmode,agentparse,agentsmd,claude,sandbox,kubejob,toolapprove,skillreg,inference,openrouter,watchdog,trajectory,logscrub,proxy,pushbroker,watsonx}` | `crux/<same>` |

   458 Go files changed import path and nothing else; the alias packages
   in the root (`pkg/config`, `pkg/effects`, `pkg/jev`, `pkg/outputschema`,
   `pkg/jsonextract`, `pkg/inferencehealth`, `pkg/issueshape`, `pkg/ioscan`,
   `pkg/github`, `pkg/agentaudit`, `pkg/fleetreport`) kept every non-ring
   caller's source unchanged. `cmd/crux` stayed in the root module as the
   host composition root; the nested module ships no binary.
4. **`src/go.work`** uses `.` and `./crux`; the root `go.mod` requires the
   module at the placeholder pseudo-version and carries the single
   `replace github.com/hivecommons/hive/crux => ./crux`, so `GOWORK=off`
   builds (the Dockerfiles, `go install`) resolve it too. `src/go.work.sum`
   is committed. The nested module never requires or replaces the root.
   The outbound ratchet walks `src/crux` as the module and asserts no
   `github.com/hivecommons/hive/pkg/...`, `.../cmd/...` or
   `.../internal/...` import; baseline empty.
5. **CI** addresses both modules from `src/` as `./... ./crux/...` (see
   [Working in the two-module tree](#working-in-the-two-module-tree) for
   why): the v2-tests agent slices run `./crux/agent`; the crux-only
   `go list` is `./crux/... ./internal/... ./pkg/config ./pkg/rotation`;
   the rest buckets, warm-build-cache, `existing-pkgs` and `no-test-pkgs`
   lists add `./crux/...`; the coverage floors are keyed `crux/<pkg>`
   (`floor_key` gained `*/crux/*` and `*/crux` cases, so `crux/config`
   can never alias `pkg/config`) with the same values the `pkg/<name>`
   keys had, and the gate prints an informational `CRUX_TOTAL` and fails a
   full run whose shards produced no Crux profiles. `coverage-hourly.yml`
   mirrors the floors; `v2-ci.yml` vets, `go-security-analysis.yml`
   `govulncheck`s and `golangci-lint`s both modules; `setup-go`'s cache key
   includes `src/crux/go.sum`. `crux-scope.sh` is `CRUX_DIRS=(src/crux)`
   with `src/go.work*` and `src/crux/go.{mod,sum}` as fail-safe paths;
   `affected-go-packages.sh` lists both modules and reads the root module
   path with `GOWORK=off go list -m`. The Dockerfiles copy
   `src/crux/go.mod`, `src/crux/go.sum` and `src/go.work` before
   `go mod download` (the `replace` target has to exist). There is no
   `scripts/check-go-ratchet-completeness.sh` in the tree; the ratchets
   themselves assert the directories they walk exist.
6. **Tags** did not ship with the cut; see [After the cut](#after-the-cut).

### Working in the two-module tree

`src/` is a Go workspace. The rules that follow from that, every one of
which bit a CI script during #13950:

- From `src/`, `./...` is the **root module only**. The Crux module is
  `./crux/...` (and `./crux/<pkg>`), addressable from `src/` because of
  `go.work`. Build, vet and test both with one invocation:
  `go build ./... ./crux/...`, `go vet ./... ./crux/...`,
  `go test ./... ./crux/...`. `cd crux && go test ./...` is the module on
  its own.
- `go list -m` in workspace mode prints both modules. A script that wants
  the root module path says `GOWORK=off go list -m`.
- `GOWORK=off` from `src/` is the Docker view: the root builds through the
  `replace`, the nested module's packages are still reachable as imports,
  but `./crux/...` is not a pattern the root module can list.
- `go mod tidy` runs per module, with `GOWORK=off` in each (workspace mode
  refuses to tidy). Keep the two `go.mod`s on the same Go version.
- Ring packages import each other and the standard library only; the
  outbound ratchet (`go test ./internal/testutil/ -run Crux`) fails any
  ring file that imports `github.com/hivecommons/hive/pkg/...` or
  `.../internal/...`. Test helpers the ring needs live in
  `crux/internal/testutil`; `internal/testutil` keeps the canonical copy.
- Root-module tests that read ring files by path (`internal/settingsparity`
  walked `src/pkg/crux/config`; the docs citation and link checkers) name
  `src/crux/...` now. `pkg/<ring>` paths in prose were left alone where
  they describe history.
- Relative paths inside ring tests kept their depth (`pkg/agent` and
  `crux/agent` are both two below `src/`), so `../../../bin` and
  `../../../config/backends.conf` still resolve.

### After the cut

Still Stage 2, in this order:

- **Tags** (plan item 6). `crux/vX.Y.Z` tags on the same commits as
  Hive's, cut by the release workflow, so a consumer can pin the module.
  One caveat the plan did not record: a module at `src/crux` of a
  repository is only `go get`-able if its module path ends in the
  directory path from the repo root, which for this tree would be
  `github.com/hivecommons/hive/src/crux`, not
  `github.com/hivecommons/hive/crux`. The root module has the same problem
  (`github.com/hivecommons/hive` lives in `src/`) and has never been
  fetched by path either, so this is not a regression; it is the thing to
  settle when the first consumer outside this repository appears
  (`src/` → repo root, or the Stage 3 move to `hivecommons/crux`, which
  fixes it by construction).
- **`crux.Process` as its own type**, so the dashboard, hub and spoke stop
  importing `crux/agent` for the test-only constructors (`NewManager`,
  `ProjectContext`, `NewRingBuffer`) and the inbound baseline for
  `agent.<Symbol>` can go to zero outside `cmd/hive`. The first slice of
  #13962 moves consumer test construction and setup controls behind
  `crux/cruxtest`, and spoke activity reads behind `crux.ActivityObserver`.
  The second slice routes agent-mode and capability parsing (`AgentMode`,
  `Mode*`, `ParseAgentMode`, `DefaultAgentMode`,
  `AgentAvailableAtACMMLevel`) through `crux`. The third slice exposes the
  Copilot-token helpers (`CopilotTokenSource*`, `CopilotUserTokenPath`,
  `GitHubTokenLogin`) and pane classification (`SanitizePaneText`,
  `RedactPaneTokens`, `PaneShowsBlockingPrompt`, `ClassifyAuthPane`) on
  `crux`, so `pkg/loginscan` no longer imports `crux/agent`. The final
  slice (#13962) made `crux.Process` its own struct: `crux/agent` copies
  its `AgentProcess` into one when it snapshots an agent (`GetStatus`,
  `GetStatusFast`, `AllStatuses` return `*crux.Process`), and
  `ProjectContext`, `RingBuffer`, the Copilot-token constants,
  `GitHubTokenLogin`, `DefaultAgentMode`, `AgentAvailableAtACMMLevel` and
  the fatal-network pane patterns are defined in `crux` and aliased or
  forwarded by `crux/agent`. The root package no longer imports
  `crux/agent`; the `Runtime` assertion and `agent.AsRuntime` (formerly
  `crux.FromManager`) live in `crux/agent`. The pane classifiers that share
  tables with the pane poller (`SanitizePaneText`, `ClassifyAuthPane`,
  `AuthPaneState`, `PaneShowsBlockingPrompt`) moved to the leaf
  `crux/paneclass`, which imports `crux/agent` so the root does not have to.
- **Fold `crux/types` into the module root.** Done (#13963). It was a
  sub-package so that the alias packages in the root could be written
  against a stdlib-only leaf before the cut; with the ring in its own module
  the leaf no longer needed to be separate from `crux`. Once the root
  stopped importing `crux/agent`, the `crux → crux/agent → crux/types`
  cycle was gone, and the last edge (the root imported `crux/watchdog` for
  `Process.WatchdogConditions`, while `crux/watchdog` imported `crux/types`
  for `ConditionReporter` and `ClassAgentCrashLoop`) was cut in the same
  change: `Condition`, `ConditionType`, `ConditionStatus` and their
  constants are defined in the root and aliased by `crux/watchdog`. The
  former leaf's files are now root files, every importer names `crux.<Name>`,
  and the root imports neither `crux/agent` nor `crux/watchdog`.
- **Inbound ratchet to one row.** After #13982 the inbound baseline was
  `cmd/hive` 22, `pkg/dashboard` 3. #14103 moved the dashboard's last
  three symbols into the root (`CanonicalizeCopilotModel` with its
  CLI-accepted model table, `BackendRequiresInteractiveAuth`; the third,
  `IsInferenceBackend`, was already `config.IsInferenceBackend` behind an
  alias) and dropped `crux/agent` from the dashboard import allowlist:
  dashboard 3 → 0. #14143 moved the pause vocabulary (`BreakerTrigger`,
  `PauseCausation`, `PauseTransitionEvent`) into `crux/pause.go`, had
  `cmd/hive` name `crux.ProjectContext` and `crux.DefaultAgentMode`
  directly, and renamed a loop variable called `agent` in
  `initAgentConfigDrivenSystems` that the ratchet's type-free AST walk had
  counted as five symbols: cmd/hive 22 → 12. The baseline is now the single
  row `cmd/hive` 12, and the twelve are classified, not pending:
  `Manager`, `NewManager`, `LinearCredential`, `AgentMintIssuer`,
  `StartPermissionsWatcher`, `WatchdogFleet`, `RunAgyTurn`,
  `ResumeHandle`, `ResolveReasoningEffort`, `ErrResumeBusy`,
  `ErrResumeSessionGone`, `PRCreator`. Each was the in-process manager
  itself or a value `cmd/hive` hands it (credential resolvers, the token
  minter, the PR creator, the fleet watchdog, the agy-turn subcommand).
  #14138 decided the sidecar plan, and its stage 1 took the row to zero:
  `cmd/hive` holds `crux.Runtime` plus `crux.Host`, the capability types
  are `crux.<Name>`, construction goes through `crux/runtime.New`, and the
  fleet watchdog through `crux/runtime.NewWatchdogFleet`. The baseline map
  is empty and the ratchet stays armed. The `AgentProcess` method split (13 methods,
  one mutex) that this record proposed is moot for the same reason: no
  consumer outside `crux/agent` names `AgentProcess` any more; they hold
  `*crux.Process` snapshots or build fixtures through `crux/cruxtest`.
- **`crux serve`** has shipped its first cut (#13959), the Go client and
  CLI remote mode (`crux --server` / `CRUX_SERVER`, #14090) and the serve
  docs ([crux-serve.md](../crux-serve.md)); the Stage 3 status block below
  has the detail. What remains of this record's list, in #14138's order:
  the sidecar stages after the seam (credential direction, the missing
  endpoint families, supervision, the flip); `crux/vX.Y.Z` tags and the
  `go get` path caveat above; a generic tool policy for the MITM proxy (the rules are
  GitHub-shaped today); and keeping this status section current.
- **Done: move `cmd/crux` into the nested module** (#14138). The module now
  ships `crux/cmd/crux`, buildable with `GOWORK=off` from `src/crux` alone
  (`make build` / `make smoke` there), with the same verbs, `--server`
  remote mode and `--version`. The root `cmd/crux` remains as a thin host
  wrapper: same `cli.Env`, Hive's loader. See
  [Definition loaders](#definition-loaders) for the split.

### Definition loaders

`crux/cli` programs against `cli.DefinitionLoader` (`LoadConfig`,
`LoadDefinitionFile`); two hosts implement it and nothing else in the module
knows which one it got:

| Host | Loader | Reads | Stays where |
|---|---|---|---|
| `crux/cmd/crux` (module) | `crux/defs.Loader` | `--defs` / `$CRUX_DEFS`: a directory of `*.yaml` or one file; each file a portable `AgentDefinition` (`kind: AgentDefinition`, merged through the same field allow-list as `pkg/defsrc`) or a bare `AgentConfig` overlay named by the file; defaults from `crux/config` (`id`, `role`, one replica, `enabled`, `clear_on_kick`) | in the module: stdlib + `yaml.v3` + `crux/config`, so the outbound ratchet stays empty |
| `cmd/crux` (root) | `hiveLoader` | `--config hive.yaml` through `pkg/config.Load` (enabled agents, `ProjectContext` from the project/github/policies blocks, well-known-agent defaults); the portable format through `pkg/defsrc` | in the root module: everything about *where* a Hive's definitions come from (repo-tree definition sources, the dashboard overlay, `agents_dir`) is host knowledge |

The root loader borrows the module's name rule and import defaults
(`defs.ValidateName`, `defs.DefaultBackend`, `defs.DefaultRestartStrategy`),
so a definition file loads to the same agent under either binary; the only
intentional difference is the Hive-only presentation table
(`applyKnownAgentDefaults`) and the `ProjectContext`, which a bare crux has
no source for.

**Stage 3: platform.** `crux serve` with an API, tenancy, auth, quotas; Hive
and hub become clients; the hub-to-runtime contract becomes explicit and
versioned; move to `hivecommons/crux`. The platform pieces wait until a
second consumer is real (candidates: Nous / Experiment Lab, Spektacular,
Dibs, external optimizer and formal-verification lanes, enterprise agent
services).

*Status: the process boundary has shipped and `cmd/hive` can consume it
behind `CRUX_MODE=sidecar` (stage 3 of the sidecar plan below); in-process
remains the default until stage 4–6 prove parity.*

- **Shipped: `crux serve` and the API** (operator page
  [crux-serve.md](../crux-serve.md)). The transport question the plan left
  open ("gRPC or HTTP") is settled as HTTP+JSON over a unix domain socket,
  with optional TCP behind a bearer token, and server-sent events for the
  two streams. The wire contract is `crux/api`, stdlib-only so a client
  does not link the runtime; the server is `crux/serve`; the socket path,
  env names and `ServeConfig` are in `crux/config`. The routes under `/v1`
  are the CLI verbs — list, get, run from a definition, logs (pane, buffer,
  full, or followed), kick, stop — plus `/v1/events`, which carries the
  lifecycle actions the API performed, state transitions the server's
  watcher observed (crash, restart, pause; there is no runtime hook for
  these, so it polls `AllStatuses`), and the kick observer relayed through
  `Kicker.SetKickObserver`. It fails closed: no TCP without a token, a
  token-less server answers only requests it saw arrive on its own unix
  socket, the socket is `0600` from the instant it is reachable in a
  directory that must be owned by the serving uid with no group/other bits,
  a token when set gates the socket too. Posting a definition applies the
  YAML loader's name gate and per-agent defaults, and a name the runtime
  already knows is a `409`. Agents start under the server's context, not the
  request's, because the runtime ties an agent's lifetime to the context
  `Start` received.
- **Shipped: a Go client and CLI remote mode** (#14090). `api.NewClient`
  speaks the contract; `crux --server ADDR` (or `CRUX_SERVER`) with `--token`
  (`CRUX_TOKEN`) makes `run`, `ps`, `logs`, `kick` and `stop` drive a
  server instead of an in-process runtime, which answers the question the
  CLI section below raised: a process other than the launcher now owns the
  agent by being the one process that holds the runtime, and everyone else
  is a client. The per-work-dir control socket of `crux run` stays as the
  single-work-dir convenience it was. `serve` itself ignores `CRUX_SERVER`
  so a stale address in a shell profile cannot stop a server from starting.
- **Shipped: every `Runtime` family over the API, and a client that is a
  `Runtime`** (#14138 stage 3; #14243, #14245). `api.RuntimeRoutes` maps
  each method to its route and a parity test in `crux/api/client` keeps
  the interface and the table equal; `crux/runtime.New` returns the client
  for `CRUX_MODE=sidecar`. Route table in
  [crux-serve.md](../crux-serve.md#runtime-families-14138-stage-3).
- **Not done, in this order** (tracked in #14138). *Hive as a client*: the
  sidecar branch of `cmd/hive` exists (stage 3) but is not the default;
  `pkg/dashboard` still takes the in-process manager, and the failure modes
  of the governor against an unreachable runtime are stage 4. *`Process` as
  its own type* and *folding `crux/types`* (Stage 2 leftovers above).
  *Tags* (`crux/vX.Y.Z`, above). *Tenancy, quotas, the hub contract, the
  move to `hivecommons/crux`*: when the second consumer exists.

### Hive as a client: the staged sidecar plan

Decided in #14138 ("/hive decision A"): `cmd/hive` consumes its own
`crux serve` API, in six stages so that no stage breaks a hosted hive and
in-process stays the default until parity is proven. The stages are (1) a
`Runtime` seam with two implementations selected by config, (2) the
credential direction, recorded before coding, (3) the endpoint families
the API lacks with a parity test, (4) failure semantics and supervision,
(5) the dashboard through the client, (6) the flip. The inbound ratchet
shrinks only when a stage flips; the outbound ratchet stays empty
throughout; no stage merges without its parity test green.

#### Stage 1: the Runtime seam

*Status: shipped.* The inventory below is what the host calls on the
runtime, by family. Each family is an
interface in the `crux` root; `crux.Runtime` composes them, so a consumer
depends on the narrowest one that serves it. `*agent.Manager` satisfies
`Runtime` (asserted in `crux/agent`); dashboard and hub were already on
the six dashboard families and gained nothing new to call.

| Family | Methods | Consumer today |
|---|---|---|
| `Lifecycle` | `Start`, `Stop`, `Restart`, `RestartWithBootstrap`, `Pause`, `PauseBy`, `Resume`, `IsPaused`, `ResetRestartCount` | dashboard, cmd/hive |
| `Observer` | `GetStatus`, `GetStatusFast`, `AllStatuses`, `ResolveAgent`, `GetOutput`, `GetBufferOutput`, `CaptureFullLog`, `FullLogHeader` | dashboard, cmd/hive, `crux` CLI |
| `Kicker` | `SendKick`, `SendKickAsync`, `ValidateKick`, `KickDispatchState`, `ListKickLogs`, `ReadKickLog`, `SetKickObserver` | dashboard, cmd/hive, CLI |
| `Definitions` | `AddAgent`, `RemoveAgent`, `UpdateConfig`, `ReconcileAgents`, `SetModelOverride`, `SetBackendOverride`, `PinCLI`, `PinModel`, `UnpinCLI`, `UnpinModel`, `ClearModeOverrides`, `SyncModeFiles`, `RefreshInferenceRoutes` | dashboard, cmd/hive |
| `Fleet` | `BreakerState`, `EngageBreaker`, `ReleaseBreaker`, `SetACMMLevel`, `GetACMMLevel`, `RelaunchBobAgentsAwaitingKey`, `SharedDevUID` | dashboard, cmd/hive |
| `Credentials` | `SubmitLoginCode`, `CopilotToken`, `CopilotTokenSource`, `SetCopilotToken`, `ActivateCopilotToken`, `ReloadClaudeToken`, `ClaudeCredentialCandidatePaths` | dashboard |
| `Resumer` (new) | `ResumeHandle`, `SendResumeKick`, `SessionID`, `SessionMissing`; sentinels `ErrResumeBusy`, `ErrResumeSessionGone`, `ErrResumeUnsupported`; value `ResumeHandle` | cmd/hive (PR follow-up) |
| `Supervisor` (new) | `GovernorMayStart`, `MarkStartupLaunchQueued`, `CheckAndRestartCrashedAgents`, `ConsentWedgedAgents`, `RefreshAgentTokens`, `RefreshAgentTokenFor`, `AgentCredentialRefreshable`, `AgentHasValidCredential`, `AgentCapabilities`, `EffectiveBackend`, `InvocationMetadata`, `RestartTelemetry`, `StartFailureState`, `ProviderErrorBackoffRemaining`, `PauseByCause`, `ArchiveAllKickLogs`, `CountAgentsWithModel`, `CrashOOMSuspected`, `BackendAuthAvailable`, `AgentAuthAvailable`, `SendGitHubAPIBudgetNudge`, `RecordAudit`, `TrajectoryAgents` | cmd/hive (governor, heartbeat, rotation, login scan, trajectory) |
| `Restorer` (new) | `SeedPauseState`, `SeedRestartTelemetry`, `SeedTurnLoss`, `SeedLastKick`, `SeedKickHistory`, `RestoreBreaker` | cmd/hive (snapshot restore) |
| `Background` (new) | `StartAgentTokenRefresh`, `StartCredentialWatchdog`, `StartCopilotSessionRefresh`, `StartTranscriptActivityRefresh` | cmd/hive (boot) |
| `Authorizer` (new) | `AuthorizePROpen`, `AuthorizePushBranch`, `AuthorizeIssueOpen`, `AuthorizeMerge`, `AuthorizeReviewRequest` | cmd/hive (request relays; method values handed to the watchers at boot) |

Two things `cmd/hive` calls are **not** runtime-instance methods and are
re-exported by the new `crux/runtime` package rather than put on the
interface: `RunAgyTurn` (the `hive agy-turn` subprocess; a sidecar runs it
as a `crux` subcommand) and `StartPermissionsWatcher` (a process-lifetime
loop over the agent home directories). `WatchdogFleet`, which reads the
manager's agent table under its lock, is built by
`crux/runtime.NewWatchdogFleet(rt, hooks)` and returns `ErrNotInProcess`
for any other implementation; its API shape is a stage 3 endpoint family.

**Hive-provided capabilities, deliberately not in `Runtime`.** The
runtime is handed these; it does not implement them, and a sidecar
runtime will receive them over a hive-side callback endpoint (stage 2
decides the shape) or publish them as API events (stage 3). The *types*
moved to the `crux` root so a host names them without importing
`crux/agent` (`crux/agent` aliases each): `AppTokenMinter`,
`AgentMintIssuer`, `LinearCredential`, `PRCreator`, `SpanStarter`. The
*installation* of them is `crux.Host`, a separate interface only the
in-process implementation satisfies (`crux/runtime.HostOf`), grouped by
what replaces it: credential capabilities (`SetAppAuth`, `SetAgentMint`,
`SetLinearCredentialResolver`, `SetBobAPIKeyResolver`,
`SetBobKeySourceResolver`, `SetSandboxPushMinter`, `SetSandboxPRClient`)
→ callback endpoint; launch-time resolvers (`SetBuildInfo`,
`SetGatewayBackendChecker`, `SetExplainModeDefaultResolver`,
`SetRepoAutoMergeEnabledResolver`, `SetKickLogSourceResolver`,
`SetInferenceCallbacks`, `SetSandboxConfig`, `SetSandboxMutationBoundary`,
`SetDeadSessionRecoveryOwner`) → definition fields or a callback;
observers (`SetKickOutcomeObserver`, `SetPauseTransitionObserver`,
`SetRelaunchObserver`, `SetPersistPauseCallback`,
`SetRecordPromptCallback`, `SetSandboxAuditCallback`, `SetAuditSink`,
`SetConditionReporter`, `SetSpanStarter`) → `/v1/events`.
`ResolveReasoningEffort` turned out to be pure (it reads only
`config.ReasoningEffortsByBackend`), so it moved to `crux/config` with
`AgyLaunchEffort` and `AgyDefaultEffort`; `crux/agent` forwards to it.

**Selection by config.** `crux/config.Mode` is `inprocess` (default) or
`sidecar`; `config.RuntimeConfig{Mode, Server, Token}` is read from
`CRUX_MODE`, `CRUX_SERVER`, `CRUX_TOKEN` by `RuntimeConfigFromEnv` and
validated at construction, so a typo fails boot rather than silently
running in-process. Like `ServeConfig` it is a process-level setting, not a
`hive.yaml` key; the key lands with stage 4, when an operator first has
supervision to configure. `crux/runtime.New(cfg, opts...)` returns the
in-process `*agent.Manager` as `crux.Runtime` for `inprocess` (options:
`WithDefinitions`, `WithLogger`, `WithProject`, `WithUIDMapPath`) and,
since stage 3, the `crux/api/client` runtime for `sidecar`
(`ErrModeUnavailable` is now only for a mode the build does not know). The
package is the one place outside `crux/agent` that names the manager on
the construction path; `crux/cli` and `crux/cruxtest` keep their own
constructors for now. `cmd/hive` holds the result as a local
`hostedRuntime` (`crux.Runtime` + `crux.Host`), fails boot if the selected
mode cannot provide the `Host` half, and passes the narrowest family on
(`crux.Host` to the installers, `crux.Runtime` elsewhere); its tests build
runtimes through `cruxtest.NewRuntime`. The inbound ratchet's `cmd/hive`
row went 12 → 0 and the baseline map is empty.

**Parity test.** `crux/cli.RuntimeVerbs` maps each CLI verb (`run`,
`ps`, `logs`, `kick`, `stop`) to the `Runtime` methods it calls;
`crux/cli`'s tests assert every listed method exists on `crux.Runtime`,
every verb the dispatcher routes to a runtime is listed, and the listed
methods are exactly the `rt.<Method>` selectors in each command body
(following helpers such as `stream`). Stage 3 adds the other half, every
verb and every `Runtime` method against an API route.

**What stage 3 needed from the API** to serve `Runtime` in full, from the
table above: the `Resumer` family (handle capture, resume kick with the
busy / gone / unsupported distinction as status codes); the `Authorizer`
gates, which sit on the relay request path and so need a synchronous
endpoint with a latency budget; the `Supervisor` reads and actions; `Restorer` as a bulk seed on connect; `Background` as
server-side loops (a no-op for the client); the `Fleet` watchdog
observe/restart and the dashboard's `Credentials` family; `/v1/events` for
the observer half of `Host`; and a `crux agy-turn` subcommand so the
`hive agy-turn` subprocess does not need the runtime linked in.

#### Stage 3: the endpoint families and the sidecar client

*Status: shipped in five lane-merged PRs; in-process stays the default.*

- **Endpoints** (#14243). `crux/api/runtime.go` holds the paths, wire
  types and codes for every family; `crux/serve/families.go` serves them
  against any `crux.Runtime`. Resume refusals are status codes (`409
  resume_busy`, `410 resume_session_gone`, `422 resume_unsupported`);
  `POST /v1/authorize` is synchronous and a transport error is a refusal;
  `Restorer` is one `POST /v1/agents/{name}/seed` body with optional
  fields; `Background` is the server's own loops. The watchdog's
  per-agent reads became a `watchdog.Source` interface (`AgentNames`,
  `Observe`, `LastProduction`, `SetConditions`) with its own routes, `501
  not_supported` when the runtime has none. When the runtime is a
  `crux.Host`, the server installs the eight observers and relays them on
  `/v1/events` with typed `data` (`agent.kick_outcome`,
  `agent.pause_transition`, `agent.relaunch`, `agent.pause_persist`,
  `agent.prompt`, `agent.sandbox_audit`, `runtime.audit`,
  `runtime.condition`). `cruxtest.Recorder` fakes it all.
- **Client** (#14245). `crux/api/client.Runtime` implements the full
  interface over `api.Client`; `crux/runtime.New` returns it for
  `sidecar`, dialing `$CRUX_SERVER` with `$CRUX_TOKEN`. One reconnecting
  `/v1/events` subscription feeds `SetKickObserver` and
  `SetEventObserver`. `Authorizer` fails closed on any error;
  `Background` is a no-op; `crux/runtime.NewWatchdogFleet` builds the
  fleet over any `WatchdogSourcer`, so the rotation watchdog runs
  unchanged against a sidecar.
  **Parity test**: `TestParityInterfaceVersusRouteTable` reflects over
  `crux.Runtime` and asserts every method is a key of `api.RuntimeRoutes`
  and vice versa, so a method cannot be added without a route.
- **Callback wiring** (#14246, #14248). `crux serve --hive-callback`
  (`$CRUX_HIVE_SOCKET`, `$CRUX_HIVE_TOKEN`) dials the hive callback
  endpoint and feeds the manager's seams from `hivecap.New` (text
  scanner, condition reporter, PR creator, Linear resolver, sandbox push
  minter); `Client.Status()` is watched into the
  `hive_callback_unavailable` condition and reported under
  `hive_callback` on `/v1/health`. On the hive side, `cmd/hive` in sidecar
  mode wraps the client in `sidecarHost`: the observer setters are fed
  from the event stream, the capability setters are inert because the
  callback carries them, and the policy setters (`SetBuildInfo`,
  `SetGatewayBackendChecker`, `SetSandboxConfig`, `SetSpanStarter`, …) are
  inert with a one-time log until stage 4. `bootAgents` serves
  `cruxhost.New(deps)` on `callback.Listen` before the rest of the wiring
  and exits if it cannot bind. `Usage` (the inference sink) stays
  `not_configured` until the proxy moves.
- **`crux agy-turn`** (#14247). The manager types its own executable into
  headless agy panes; under `crux serve` that is `crux`, so the subcommand
  exists there too.

**What stage 3 leaves for stage 4.** The policy setters above as routes
or configuration (the `hive.yaml` key for the runtime, so an operator can
set sandbox config, the gateway checker, explain-mode defaults, repo
auto-merge, dead-session recovery ownership); `SetAgentMint`, `SetAppAuth`
as callback-backed cache writers in `crux/agent` (today the sidecar's
agents have no mint issuer); the proxy move and with it the `Accounting`,
`Usage` and `Canary` seams fed from the callback; `SetSpanStarter`
(tracing across the boundary); the governor's behaviour against an
unreachable runtime (reconnect, degraded state, the
`hive_callback_unavailable` / runtime-unreachable pair of conditions);
`pkg/dashboard` on the client (stage 5); and a soak of a hosted hive in
sidecar mode before the flip (stage 6).

## Credentials and hive-provided capabilities

Status: **Stage 2 of the #14138 sidecar plan (decision A): done.** Design
in #14197; the callback contract (`crux/api/callback`), the runtime-side
adapters (`crux/hivecap`) and the hive-side mount (`pkg/cruxhost`) in
#14222 (coverage 94.8% / 99.1% / 100%, default floors). Stage 3 wired both
ends (see "What stage 3 wires" below): `crux serve --hive-callback` and the
sidecar branch of `cmd/hive`'s `bootAgents`.

The decision this section records: **the runtime never holds the GitHub
App private key, or any other hive-owned credential.** When hive runs the
runtime as a sidecar (`crux serve`), the runtime obtains short-lived scoped
tokens and every other hive-owned capability by calling back into hive over
a hive-side endpoint. In-process, the same capabilities are the direct
function calls `cmd/hive` already installs today. The seam interfaces the
Stage 2 inversions left behind are the contract in both modes; the sidecar
adds one implementation of each, not a second contract.

### Inventory

Every interface the ring defines for something the host supplies, measured
on v5 at `8bce0b0` (Oct 25, 2026) with
`grep -rn 'interface {' src/crux --include='*.go' | grep -v _test`. The
last column is the classification this stage settles: a *capability* is
something hive owns (a credential, a registry, an accounting ledger, a
forge client) that the runtime asks for; a *runtime feature* is behaviour
the runtime owns and only lets the host observe or tune.

| Seam | Defined in | Hive implementation today | What crosses the boundary | Sensitivity | Class |
|---|---|---|---|---|---|
| `pushbroker.TokenScoper` (`ScopedTokenForRepos(ctx, tier, repos)`) | `crux/pushbroker` | `*github.AppAuth` via `cmd/hive`'s `appMinter` | tier + repo names in; an installation token out | **credential**: the token is a live GitHub write credential | capability |
| `pushbroker.TokenMinter` (`MintPushToken(ctx, repo)`) | `crux/pushbroker` | `pushbroker.GitHubAppMinter{Auth: TokenScoper}` (ring code over the seam above) | same | same | capability (derived from `TokenScoper`) |
| `agent.AppTokenMinter` (`WriteAgentToken(ctx, agent, tier, uid)`) | `crux/agent` | `*github.AppAuth` (`Manager.SetAppAuth`) | agent, tier, uid in; hive **writes the agent's token cache file** as a side effect | **credential**, plus a filesystem write into the agents' tree | capability, but mis-shaped for a process boundary (see below) |
| `agent.AgentMintIssuer` (`Enabled`, `MintAgentToken(agent, tier)`) | `crux/agent` | `mint.AgentMinter` (`cmd/hive` `buildMinter`) | agent + tier in; an OIDC/JWT out | **credential** (opt-in, additional) | capability |
| `agent.LinearCredential` + `SetLinearCredentialResolver(func() LinearCredential)` | `crux/agent`, used by `crux/proxy` | closure in `cmd/hive` over hub OAuth / `hive.yaml` | a Linear access token or API key out | **credential** | capability |
| `agent.PRCreator` (`CreatePR(ctx, repo, head, base, title, body)`) | `crux/agent` | `*github.Client` (`SetSandboxPRClient`) | PR fields in; `crux.PRResult` out; hive authors as the App bot | hive's forge identity acts; no secret crosses | capability |
| `proxy.GitHubRESTAccounting` (`RecordRESTRequest`, `RecordInjectedRESTRequest`, `CoreRateLimitSnapshot`) | `crux/proxy` | `cmd/hive`'s `githubRESTAccounting{}` over `pkg/github` package state | caller, method, path, status and the rate-limit response headers in; the shared core-bucket snapshot out | telemetry; the snapshot feeds the per-agent GitHub budget's reserve floor | capability (hive owns the ledger) |
| `proxy.UsageSink` (`Record`, `RecordCopilot`, `RecordCopilotWithCache`) | `crux/proxy` | `*tokens.InferenceSink` | per-agent inference token counts in | telemetry (billing) | capability |
| `proxy.CanaryScanner` (`Scan(agent, text, source)`) | `crux/proxy` | `*ioscan.CanaryRegistry` | a GitHub write body in; "leaked" + which agent/source out | the body may hold agent work product; the registry holds the planted canaries, which **must not** be shipped to the runtime | capability |
| `toolapprove.TextScanner` (`ScanInput`, `ScanOutput`) | `crux/toolapprove` | `ioscan.ToolApproveScanner` (`SetDefaultTextScanner` in `cmd/hive`'s `init`) | tool payload text in; a verdict with masked snippets out | payload may hold secrets the scanner is there to catch | capability (hive owns the rule set) |
| `watchdog.UsageProber` (`Provider`, `ProbeError`) | `crux/watchdog` | `cmd/hive`'s `usageProber` over `pkg/rotation` | provider name in; sanitised probe error out | hive holds the provider credentials the probe uses | capability (deferred; stays with the rotation watchdog until stage 3 moves it) |
| `crux.ConditionReporter` (`Observe`, `Clear`) | `crux` root, used by `crux/agent`, `crux/watchdog` | `fleetreport.DefaultReporter{}` | class, key, message, scrubbed diagnostics out | diagnostics are already scrubbed at the call site | capability (hive owns the fleet report) |
| `agent.SpanStarter` | `crux/agent` | `tracing.StartSpan` | a live `trace.Span` | none | **runtime feature**: not remotable, see below |
| `crux.AuditSink` (`Record(actor, action, agent, fields)`) | `crux` root, used by `crux/agent` | the dashboard's agent-audit recorder, injected by `cmd/hive` | lifecycle audit entries out | none (already scrubbed) | capability (deferred with `UsageProber`; must not block, so it becomes a batched route, not a per-call one) |
| `watchdog.Alerter`, `watchdog.AuthProbe`, `watchdog.KickPeriodReporter`, `watchdog.KickSuppressionReporter`, `trajectory.Sink` | `crux/watchdog`, `crux/trajectory` | `cmd/hive` adapters over Discord/Slack, `pkg/rotation`, `pkg/fleetreport` | alerts and observations out | none | observers; they ride the `api.Event` stream in stage 3, not callbacks |
| `Kicker.SetKickObserver`, `proxy.SetAgentTokenSource`, `SetRepoPausedFunc`, `SetAgentRepoScopeFunc`, `SetWriteSurfaceEnforceFunc`, `SetWriteRefusedAuditFunc`, `SetAgentGitHubBudgetConfig`, … | `crux/agent`, `crux/proxy` | `cmd/hive` closures over `pkg/config` | policy answers (bools, caps) and observer callbacks | none | runtime features configured by the host; they become API routes or config in stage 3, not callbacks |

Two of the capability rows are not callback-shaped as they stand:

- `AppTokenMinter.WriteAgentToken` has hive write a file into the agents'
  token-cache directory as the agent's uid. Across a process boundary the
  runtime owns that directory and those uids, so the capability collapses
  onto *mint a scoped token* and the runtime writes the cache file itself.
  Stage 3 replaces `SetAppAuth` in sidecar mode with a writer built over the
  mint callback; the in-process path keeps `*github.AppAuth`.
- `SpanStarter` returns an in-process `trace.Span` whose `End` must be
  called by the runtime; there is no sensible remote form. The runtime's
  default (the OpenTelemetry global tracer) is the sidecar answer, with the
  usual W3C context propagation on the API requests, and hive's
  per-component reach bookkeeping stays a hive concern. This is why span
  start is **not** in the callback contract.

### Direction

Runtime → hive. The runtime is the caller for every capability; hive never
pushes a credential into the runtime, and the runtime never receives the
App private key, the installation JWT, the canary registry, the Linear
OAuth refresh token or the rotation providers' credentials. Tokens are
minted per request, scoped to a tier and to the named repositories
(`ScopedTokenForRepos`' existing contract), attributed to an agent, and
carry an expiry the runtime must honour (GitHub installation tokens expire
in an hour; the response says when). The runtime keeps a token only as long
as the operation that asked for it (`pushbroker` holds it for one push, and
already keeps it out of `argv` and logs) or, for the per-agent cache file,
until the expiry the response carried.

Trust boundary: the runtime process is trusted to attribute requests to
the right agent, because it is the enforcement point that identifies the
agent today (the proxy's per-agent routing, the broker's `AgentName`). The
callback endpoint authenticates *the runtime*, not individual agents, and
hive keeps the authority to refuse (an agent not in its fleet, a repo not
in the installation, a tier the agent is not entitled to). An agent cannot
reach the hive socket: it lives in hive's private runtime directory, mode
`0600`, and agents run as their own uids.

### Transport

The callback endpoint reuses `crux/api`'s conventions: HTTP+JSON, routes
under `/v1`, `api.Error` bodies with stable `code`s, a `0600` unix socket in
a uid-owned `0700` directory, optional TCP only behind a bearer token, and
a token-less server that answers only requests it saw arrive on its own
socket. The contract is the stdlib-only package `crux/api/callback`; a
hive that mounts it links nothing of the runtime, and a runtime that calls
it links nothing of hive.

- **Hive serves.** Bidirectional means two listeners: `crux serve` keeps
  its socket for hive's commands, and hive opens a second one for the
  runtime's callbacks. Default location: `hive.sock` next to the runtime's
  socket, so `$CRUX_HIVE_SOCKET` if set, else
  `$XDG_RUNTIME_DIR/crux/hive.sock`, else `<tmp>/crux-<uid>/hive.sock`.
  The two processes run as one uid in one container by default (stage 4
  supervision), so the file mode is the credential, exactly as for
  `crux.sock`. `$CRUX_HIVE_TOKEN` adds a bearer token; `$CRUX_HIVE_LISTEN`
  (TCP, for the k8s sidecar-container option) requires one.
- **Routes** (`callback.Path*`), one per capability, request and response
  bodies as the `callback.<Name>Request/Response` types:

  | Capability | Route | In | Out |
  |---|---|---|---|
  | Health | `GET /v1/health` | — | `{version}` plus which capabilities are configured |
  | Mint scoped token | `POST /v1/tokens` | agent, repo (`owner/name`) or repos (names), tier, requested TTL | token, `expires_at` |
  | GitHub REST accounting | `POST /v1/github/rest` | caller, method, path, status, injected (bool), the `X-RateLimit-*` response headers | `204` |
  | Core rate-limit snapshot | `GET /v1/github/rate-limit` | — | remaining, reset, ok |
  | Inference usage | `POST /v1/usage` | agent, model, kind (`gateway` / `copilot` / `copilot_cache`), input, output, cache read, cache creation | `204` |
  | Canary scan | `POST /v1/scan/canary` | agent, text, source | detected, agent, source (**never the canary token**) |
  | Text scan | `POST /v1/scan/text` | direction (`input` / `output`), text | blocked, critical_injection, findings (kind, masked snippet, rule, high) |
  | Condition | `POST /v1/conditions` | action (`observe` / `clear`), class, key, message, diagnostics | `204` |
  | Linear credential | `GET /v1/linear/credential` | — | access_token or api_key |
  | Create PR | `POST /v1/pulls` | repo, head, base, title, body | `crux.PRResult`'s fields |

  A capability hive does not have (no GitHub App, no canary registry, no
  Linear) answers `404 not_configured`, which the runtime treats exactly as
  the nil seam it stands in for today (no App → the push fails; no registry
  → nothing is scanned; no Linear → no injection). A hive-side failure of a
  configured capability (GitHub refused the mint) is `502 upstream_error`
  with the message; a bug is `500 hive_error`. Both are refusals to the
  caller.
- **Both ends in Go.** `callback.Capabilities` is the one interface set,
  with wire types on both sides. `*callback.Client` implements it over
  HTTP; the hive-side `pkg/cruxhost` implements it directly over hive's
  packages (`*github.AppAuth`, `*ioscan.CanaryRegistry`,
  `*tokens.InferenceSink`, `fleetreport`, `*github.Client`, the Linear
  resolver) and `callback.NewServer(caps)` serves either. `crux/hivecap`
  turns any `Capabilities` into the ring's seam interfaces
  (`pushbroker.TokenScoper`, `proxy.GitHubRESTAccounting`,
  `proxy.UsageSink`, `proxy.CanaryScanner`, `toolapprove.TextScanner`,
  `crux.ConditionReporter`, `agent.PRCreator`) so the runtime's existing
  setters take them unchanged.
- **In-process mode: no socket.** `cmd/hive` keeps installing its
  implementations into the seams directly, as it does now; that path is
  not changed by this stage and is deleted only in stage 6. Because
  `crux/hivecap` is written against `callback.Capabilities` rather than
  `*callback.Client`, a hive that wants one wiring path for both modes can
  hand `pkg/cruxhost`'s `Capabilities` value to `hivecap` in-process and a
  `*callback.Client` in sidecar mode; the parity test in `crux/hivecap`
  asserts the two produce identical seam behaviour. Stage 3 selects between
  them on `crux.mode`.

### Failure semantics: fail closed

When hive is unreachable, every capability refuses rather than degrades
into a less-safe answer. The runtime already has a refusal path for each
seam, and the callback adapters return the value that takes it:

| Seam | Hive unreachable / unauthorized | Effect |
|---|---|---|
| `TokenScoper` | error wrapping `callback.ErrUnavailable` | `pushbroker` fails the push ("minting push token"); the agent's branch stays local. The per-agent cache file is not rewritten, so the proxy's token source has no token and `rewriteGitHubAuth` forwards the write **without credentials** → upstream 401. The agent continues read-only on public repos. |
| `CanaryScanner` | reports a detection with an empty token and the source suffixed ` (hive callback unavailable)` | with `canaryFailClosed` the proxy refuses the write; without it the write is audited as a leak. Both are what an unscannable body does today. |
| `TextScanner` | a `Blocked` verdict with one finding (`kind=callback`, `rule=hive_unavailable`) | `toolapprove` denies, as `ErrNoTextScanner` does with no scanner. |
| `GitHubRESTAccounting` | records are dropped; `CoreRateLimitSnapshot` returns `ok=false` | the agent GitHub budget applies the per-agent cap alone, the documented behaviour when no snapshot is installed. |
| `UsageSink` | records are dropped | billing undercounts for the outage; counted in `Client.Status()`. |
| `ConditionReporter` | reports are dropped | the fleet report misses the window; counted in `Client.Status()`. |
| `LinearCredential` | empty credential | the proxy injects nothing; the agent's Linear call fails upstream. |
| `PRCreator` | error wrapping `callback.ErrUnavailable` | the sandbox executor reports the kick as failed, same as a forge error. |

`401` from hive (token mismatch) is `callback.ErrUnauthorized` and takes
the same rows; it is never retried with a different credential. The client
has no logger and its errors never contain the bearer token, a minted
token, or a request body; `MintTokenResponse` prints with the token
redacted so an accidental `%v` cannot leak it.

The outage is a runtime condition. `callback.Client.Status()` reports
`Reachable`, the last error, when it began and how many calls failed
closed; the class name `hive_callback_unavailable` is reserved for it.
Stage 4 surfaces it on `GET /v1/health` and as an `api.Event`, and defines
the governor's reaction (no new dispatch, holds kept) alongside the other
"socket is gone" cases. Recovery is per call: the next successful callback
clears the condition; there is no circuit breaker, because the fail-closed
answers are already cheap and the per-call timeout
(`callback.DefaultTimeout`, 5 s) bounds the cost of a dead socket.

### What stage 3 wires

- `cmd/hive` (#14248, done): `cruxhost.New(cruxhost.Deps{...})` over the
  values it already constructs, served on
  `callback.Listen(callback.DefaultSocket())` next to the runtime socket,
  in sidecar mode only. `Usage` is left `not_configured` until the proxy
  moves; the Linear resolver is read lazily because `bootSupervision`
  builds it after `bootAgents`.
- `crux serve` (#14246, done): a `--hive-callback` address (or
  `$CRUX_HIVE_SOCKET`), `callback.NewClient`, `hivecap.New(client)`, and
  the manager setters fed from it instead of nil (`toolapprove` text
  scanner, condition reporter, PR creator, Linear resolver, sandbox push
  minter over the token scoper). The proxy setters follow the proxy move.
- Stage 4: `AppTokenMinter` → a mint-callback-backed cache writer in
  `crux/agent`; `AgentMintIssuer` and `UsageProber` follow the same route
  pattern when the watchdog's rotation probe moves.

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
| `serve` | all of the above, over the API (Stage 3; [crux-serve.md](../crux-serve.md)) |
| `agy-turn` | none: the pane-side `agent.RunAgyTurn` subprocess the manager spawns from its own executable (#14247) |

With `--server ADDR` / `CRUX_SERVER` the first five go through `api.Client`
to a `crux serve` instead; `serve` itself is only ever local.

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
single-work-dir convenience and is not the Stage 3 API; what Stage 3 had to
add to `Runtime` (or beside it) is exactly the piece the socket papers over:
a way for a process other than the launcher to own or adopt an agent.
`crux serve` is that piece, by making the server the one launcher and
everything else a client; adoption of an agent started by some other
process is still not a `Runtime` capability.

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
setters or `crux.<Name>` definitions, never new exported
`agent` symbols the host has to name.

## Running the Crux set alone

The ring is the module at `src/crux`, so it is tested as one:

```sh
cd src && go test ./crux/...      # from the workspace
cd src/crux && go test ./...      # from inside the module
```

`./cmd/crux` is a host that builds against it, like `./cmd/hive`, and is
tested with the rest of the root module; `./crux/cmd/crux` is the module's
own host and builds standalone (`cd src/crux && GOWORK=off go build
./cmd/crux`; `make smoke` there exercises `crux serve` end to end). Both ratchets live in
`./internal/testutil` and run with that package. In CI, a PR whose diff is
entirely under `src/crux/` gets the `crux-only` verdict from
`crux-scope.sh` and runs only the agent slices (Stage 1 item 5). Two
`crux/agent` tests fail on macOS because the tmux socket path under
`$TMPDIR` exceeds `sun_path`; they pass on Linux and in CI.

## Not doing

- Repo split before Stage 1: the co-change tax lands before any second
  consumer exists and doubles v5/v6 top-up work.
- Rewriting engines or the proxy. This is a boundary, not a rewrite.
- Shortening the PR gate by splitting: `crux/agent` is a 429 s serial test
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
  (stage 1, closed); stage 2/3 tracker
  [hivecommons/hive#14138](https://github.com/hivecommons/hive/issues/14138)
- Ratchets: `src/internal/testutil/crux_boundary_ratchet_test.go` (inbound),
  `src/internal/testutil/crux_outbound_ratchet_test.go` (outbound)
- CI scope: `.github/scripts/crux-scope.sh` (#13541)
- Prior extractions from Hive: pluk, promptargs, rationguard, hotshot,
  spektacular, dibs
- Related: #13532 (control arm)
