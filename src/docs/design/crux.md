# Crux: the agent runtime boundary (#13536)

Status: **Stage 1 shipped (v5); Stage 2 step 4 shipped: the ring is the
nested Go module `github.com/hivecommons/hive/crux` at `src/crux/`
(#13950), joined to the root module by `src/go.work`.** The boundary is named, the inbound and outbound
architecture ratchets and the tmux funnel are live, `crux.Runtime` exists,
the dashboard is on it, Crux owns its value types and the hub, spoke,
dashboard and `cmd/hive` reference them as `crux.<Name>`, CI path filters
select Crux-only shards for Crux-only PRs, and the `crux` CLI ships. The
ring's outbound closure, about 55 Hive packages when this record began, is
now zero, in test code too: the outbound baseline is empty (the ring's tests
use their own `crux/internal/testutil` copy of `Eventually*` and
`SkipUnlessRequired`); see [Outbound edges](#outbound-edges). What the
move did and how to work in the two-module tree is under
[Step 4: the module cut](#step-4-the-module-cut); what is left of Stage 2
is under [After the cut](#after-the-cut). Stage 3 is design only.

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
- **Fold `crux/types` into the module root.** It is a sub-package so that
  the alias packages in the root could be written against a stdlib-only
  leaf before the cut; with the ring in its own module the leaf no longer
  needs to be separate from `crux`. The root no longer imports
  `crux/agent`, so the `crux → crux/agent → crux/types` cycle is gone. One
  edge remains: the root imports `crux/watchdog` for
  `Process.WatchdogConditions`, and `crux/watchdog` imports `crux/types`
  (`ConditionReporter`, `ClassAgentCrashLoop`), so `watchdog.Condition`
  has to move into the root (watchdog aliasing it) in the same change that
  folds `types` in.
- **`crux serve`** is Stage 3, below; nothing in the module layout
  blocks it.

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

The ring is the module at `src/crux`, so it is tested as one:

```sh
cd src && go test ./crux/...      # from the workspace
cd src/crux && go test ./...      # from inside the module
```

`./cmd/crux` is a host that builds against it, like `./cmd/hive`, and is
tested with the rest of the root module. Both ratchets live in
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
- Ratchets: `src/internal/testutil/crux_boundary_ratchet_test.go` (inbound),
  `src/internal/testutil/crux_outbound_ratchet_test.go` (outbound)
- CI scope: `.github/scripts/crux-scope.sh` (#13541)
- Prior extractions from Hive: pluk, promptargs, rationguard, hotshot,
  spektacular, dibs
- Related: #13532 (control arm)
