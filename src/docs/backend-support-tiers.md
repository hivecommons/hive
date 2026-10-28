# Agent-backend support tiers and acceptance bar

This is the acceptance bar for adding a CLI backend to hive, and the policy
for which support tier it lands in. It exists so a backend PR can point at
the criterion it satisfies instead of restating the argument every time.
Every criterion below names the file or test a submitter has to touch; the
existing backends are the worked examples. Nothing here changes runtime
behavior. Tracking issue:
[#6231](https://github.com/hivecommons/hive/issues/6231).

Companion docs: [docs/backend-setup.md](../../docs/backend-setup.md) describes
each backend after the fact; [sandbox-isolation.md](sandbox-isolation.md)
holds the per-backend confinement matrix; `config/backends.conf` is the
implementation.

## The three tiers

The 15 shell-known CLIs (`KNOWN_BACKENDS` in `config/backends.conf`) already
fall into three classes that are enforced in code. This document names them.

| Tier | Meaning | Backends today | Enforcement point |
| --- | --- | --- | --- |
| **T1 - core (headless-pod)** | Runs unattended in a TTY-less pod from staged or env-injected credentials | `claude`, `litellm`, `copilot`, `codex`, `goose` | `HEADLESS_BACKENDS` in the `Justfile` (`contribute-k8s`) and `K8S_HEADLESS_BACKENDS` in `src/pkg/dashboard/api_contribute.go` |
| **T2 - supported (confined)** | Has a confinement floor hive can wire on the contributor local path: an OS sandbox, or at least a host-state deny-list | `claude`/`litellm`, `codex`, `copilot`, `muse` (sandboxed); `opencode` (deny-listed) | `backend_perm_flag` and the `*_local_perm_flag_shell` helpers in `config/backends.conf` |
| **T3 - experimental (unconfined)** | No confinement hive can wire; local mode refuses to launch without a per-backend opt-in, container mode is the default | `goose`, `agy`, `bob`, `pi`, `aider`, `kilo`, `omp`, `openhands`, `omnigent` | `unconfined_local_perm_flag_shell` and `HIVE_<BACKEND>_DANGEROUSLY_RUN_UNCONFINED=1` (#4918) |

Tier assignment is per launch path. `goose` is T1 on the pod path (it has a
one-shot `goose run` and an env-key credential) and T3 on the local path (no
sandbox). `omp` is now a hub-agent method, but remains T3 on contributor local
mode because its unattended approval flag is not an OS sandbox. A PR states the
tier it claims for each path it wires.

`openhands` is T3 experimental, headless-only, on the legacy local-path CLI.
Upstream considers that CLI/headless path legacy and no longer actively
maintained (OpenHands/OpenHands#18046), and Hive will not promote OpenHands
beyond T3 on that surface.

`omnigent` is T3 experimental (open-source meta-harness over Claude Code, Codex, Cursor, Pi, and custom agents). On the contributor local launch path, the bare CLI runs with no OS sandbox wired by Hive, so local launches refuse by default without `HIVE_OMNIGENT_DANGEROUSLY_RUN_UNCONFINED=1`. While Omnigent has its own internal policy engine and optional agent sandboxing (`enforce_sandbox`), Hive treats these as additive: Hive's API/egress proxy strictly decides and controls all GitHub writes (`POST /repos/{owner}/{repo}/pulls`, `PUT /repos/{owner}/{repo}/pulls/{n}/merge`, Git pushes), and Omnigent policies can never override or bypass Hive proxy denials. See [backend-omnigent-cli-contract.md](backend-omnigent-cli-contract.md).

The T3 tier also bounds what an already-running OMP workbench may take as an
external host (#8361 step 9, #6899): it is admissible only in report-only and
shadow modes, where it holds no repository credential and no dashboard token
and returns a receipt Hive verifies rather than trusts, and the adapter
(`pkg/extwork/omp`, `CheckReportOnly`) refuses any write-capable stage in
code. The escape-hatch env var above concerns launching `omp` locally under
Hive; it does not widen what an external OMP host may do.

Hosted agent runtimes (Claude Managed Agents, the OpenAI Agents API) are not
CLI backends either. They have their own
[hosted execution tier](#hosted-execution-tier) below.

`gemini` is Go-only (`cliBackendExceptions`): it is launched by the hub-side
manager and has no contributor-relay wiring at all, so the local-path tiers do
not apply to it. Adding it to the relay would go through this bar as a new
backend.

## GitHub Agentic Workflows: on-ramp, not a CLI backend

[GitHub Agentic Workflows (gh-aw)](https://github.com/github/gh-aw) compiles
Markdown jobs into Actions. It has **no T1/T2/T3 assignment**: it is not a new
`KNOWN_BACKENDS` CLI. Engine overlap with Hive does not transfer confinement,
authentication, or metering evidence. The
[report-only sample](../deploy/gh-aw/README.md) lets an existing gh-aw repo use
Hive's deterministic classifier before engine judgment, keeping its existing
workflows while adopting Hive for continuous fleet work.

Hive dispatch into gh-aw through `pkg/extwork` is a proposed external-host path,
**not a supported adapter today**. As with external OMP, initial admission must
be limited to report-only/shadow stages, with no repository-write credential or
dashboard token held by the external worker. A future adapter must reject
write-capable capabilities in code, implement the extwork lease/receipt and
cancellation contracts, bind receipts to the dispatched run, and supply
conformance tests proving refusal and verification. An Actions run's success is
not a verified receipt or permission to merge. Do not promote the underlying
CLI's tier or claim budget metering based on this sample.

## The acceptance bar

A PR adding backend `<name>` states, in its body, the tier it claims per path
and the evidence for each criterion below. The PR template's backend-addition
checklist is the standing enforcement mechanism for that body-level claim: if a
backend PR deletes the optional section as inapplicable, reviewers should still
ask for it when the diff wires a CLI backend. The criteria are cumulative: T3 is
the floor, T2 adds confinement, T1 adds the unattended-credential story.

### All tiers

1. **List parity.** Add `<name>` to `KNOWN_BACKENDS`, `backend_binary`, and
   `backend_perm_flag` in `config/backends.conf`, and to `CLIBackends` in
   `src/pkg/config/backends.go`, in the same PR. The guard is
   `TestShellAndGoCLIBackendListsAgree` in
   `src/pkg/config/backend_list_parity_test.go`; a name that legitimately
   lives on one side only goes in `cliBackendExceptions` with a reason, and
   that list is deliberately short (`litellm`, `gemini`).
2. **Declared local posture.** Add `<name>` to `localBackendPostures` in
   `src/pkg/dashboard/contribute_local_mode_backend_matrix_test.go` as one of
   `postureSandboxed`, `postureDenylisted`, or `postureRefusalGated`.
   `TestEveryKnownBackendDeclaresLocalConfinementPosture` fails for any
   `KNOWN_BACKENDS` entry without one. The posture must match what
   `backends.conf` actually wires, not what the vendor advertises.
3. **Flag-honoring proof.** The PR body shows the exact `backend_perm_flag`
   string and a transcript proving the CLI rejects an invalid value for each
   safety-relevant flag with a non-zero exit, rather than ignoring it. This is
   the `agy --sandbox` lesson recorded in `config/backends.conf`: a flag the
   binary accepts and ignores is not a control. The unattended flag is the
   vendor's approval-off flag, not its disable-everything flag; the latter
   belongs behind the tier's escape-hatch env var (compare
   `--dangerously-skip-permissions` for claude and the hub-path
   `--dangerously-bypass-approvals-and-sandbox` for codex; contributor-local
   codex keeps its explicit approval/sandbox posture from
   `config/backends.conf`).
4. **Honest credential detection.** Add a `<name>` case to `detect_cli` in
   `bin/contributor-agent.sh`. `--version` is acceptable only when it proves
   the CLI can authenticate; if the binary exits 0 with no credential at all,
   probe the credential instead (the `codex_is_authenticated` pattern).
5. **Documentation.** A row in the CLI backends table of
   [docs/backend-setup.md](../../docs/backend-setup.md), a row in the
   [per-backend confinement matrix](sandbox-isolation.md#per-backend-confinement-on-the-contributor-local-path),
   and the `AGENT_BACKEND` row in [contributor-relay.md](contributor-relay.md),
   at the same level of detail as the neighbouring rows: auth mechanism,
   credential storage path, headless status, and the escape hatch.
6. **Pinned install.** If the CLI is added to `src/Dockerfile.contributor` or
   `src/Dockerfile`, it is fetched from a versioned release with a per-arch
   `SHA256` `ARG` and a `sha256sum -c` step. Download failures for optional
   backends may be tolerated; checksum mismatches never are (precedent: the
   `agy`, `goose`, and `bobshell` layers).
7. **Changelog fragment.** One `changelog.d/added-<pr>-<slug>.md` bullet.

### T3 - experimental, additionally

8. **Refusal gate.** Add `<name>` to `unconfined_local_backend_env_var` in
   `config/backends.conf` with its own `HIVE_<NAME>_DANGEROUSLY_RUN_UNCONFINED`
   variable, and extend the sentence in that file's "no confinement
   mechanism" comment that lists the unconfined backends.
   `TestUnconfinedBackendsRefuseToLaunchByDefault`,
   `TestUnconfinedBackendsLaunchWithExplicitOptIn`,
   `TestEveryUnconfinedBackendHasItsOwnEscapeHatch`, and
   `TestBackendsConfDocumentsWhyUnconfinedBackendsHaveNoWiring` pin all of
   this; the last one asserts the comment's exact wording, so the comment and
   the test change together.

The bar for T3 is honesty about posture, not confinement. A backend with no
sandbox, no deny-list, and no unattended-credential story still lands here.

### T2 - supported, additionally

9. **Confinement floor wired and tested.** One of:
   - *Sandboxed.* The CLI has an OS-enforced sandbox (Seatbelt, bubblewrap,
     seccomp, or equivalent) that is on by default or that hive turns on, and
     the unattended flags from criterion 3 leave it intact. Local mode narrows
     it to the agent cwd plus `HIVE_WORKSPACE_DIR` (codex `--add-dir`, claude
     `--settings` sandbox JSON with `failIfUnavailable`, copilot `--sandbox`),
     refuses to emit a grant it cannot express (the whitespace-path guard in
     the codex block), and has its own
     `HIVE_<NAME>_DANGEROUSLY_BYPASS_*` escape hatch. Matrix tests assert the
     narrowing flags are emitted, the bypass drops them, and the
     disable-everything flag never appears (compare the copilot and codex
     cases in `contribute_local_mode_backend_matrix_test.go`).
   - *Deny-listed.* The CLI has no sandbox but honors a command deny rule
     that survives its unattended flag. Wire the same host-state family as
     `CLAUDE_HOST_DENY_TOOLS` (privilege escalation plus the boot/deployment
     tools named in #4918), with a `HIVE_<NAME>_DANGEROUSLY_ALLOW_HOST_STATE`
     opt-out. `TestOpencodeLocalModeDeniesHostStateCommands` and
     `TestShellAndGoDenyListsAgree` (`src/crux/agent/host_state_deny_test.go`)
     are the templates. The local-mode banner must not call a deny-listed
     backend "confined" (`TestLocalModeBannerNeverCallsOpencodeConfined`).

### T1 - core, additionally

10. **Headless entry point.** Add `<name>` to `HEADLESS_BACKENDS` in
    `bin/contributor-relay.js` with the one-shot sub-command or flag, and a
    test in `bin/contributor-relay.test.js` pinning the argv it builds. The
    CLI must run the prompt to completion and exit with a meaningful status;
    an invocation that prints help and exits 0 would be reported to the hub
    as a completed task.
11. **Unattended-credential verification.** Add a `<name>` case to the
    credential preflight (#5103) in the `Justfile`'s `contribute-k8s` recipe
    that names the env var or staged file a fresh, TTY-less pod
    authenticates from, and show in the PR that such a pod completed a real
    tool call. "Works on a host that already signed in" is T2 evidence, not
    T1: `copilot` and `codex` are in `HEADLESS_BACKENDS` for capability but
    the preflight still refuses them because their OAuth state directories
    are unverified in a pod, and `agy`, `opencode`, `kilo`, and `muse` are kept
    out of the allowlist until their credentials are verified in a fresh pod (see
    [Backends excluded from the headless K8s allowlist](../../docs/backend-setup.md#backends-excluded-from-the-headless-k8s-allowlist)).
12. **Allowlist touchpoints.** Update all three together: `HEADLESS_BACKENDS`
    in the `Justfile`, `K8S_HEADLESS_BACKENDS` in
    `src/pkg/dashboard/api_contribute.go` (the `/contribute` page), and the
    excluded-backends section of `docs/backend-setup.md`. Consider adding a
    lane to the [backend smoke](backend-smoke.md) when a credential can exist
    in CI.

## Tier movement

Promotion (T3 to T2, T2 to T1) is a normal PR that supplies the missing
criteria for the higher tier, the same bar as initial acceptance. Demotion
happens when a criterion is found unmet (a flag discovered to be ignored, a
credential that does not survive a pod restart); the discovering issue cites
the criterion number above, and the fix moves the backend's posture in
`localBackendPostures` and the allowlists in the same PR.


## Gateway-path tier

The three tiers above are the CLI-agent acceptance bar (`KNOWN_BACKENDS`) and do
not apply to the OTHER on-ramp: OpenAI-compatible model gateways (`vllm`,
`llm-d`, `litellm`, `watsonx`, and named Model Gateways such as `openrouter` or
a self-hosted `kind: custom` endpoint — see
[docs/inference-backends.md](../../docs/inference-backends.md)). A gateway is
not an agent binary; it is a routing target that the built-in Anthropic-to-
OpenAI translator (`pkg/proxy/anthropic_translate.go`, "inference translation
server") calls on behalf of the Claude CLI running in bare mode. That
architectural difference is also why it was untiered: the T1/T2/T3 criteria
above are all about a CLI's own confinement and credential story, which does
not exist on this path. This section states, per feature, what a gateway
backend is expected to do — determined from what the code actually does today,
not from the vendor's advertised capability. Tracking: this section closes the
gap named in [#6515](https://github.com/hivecommons/hive/issues/6515); adopter
symptom: [#6489](https://github.com/hivecommons/hive/issues/6489).

| Dashboard/agent feature | Gateway-path status | Why |
| --- | --- | --- |
| Model inference (chat) | **Guaranteed** | The whole point of the path: `forwardToInference` translates the Anthropic Messages request to OpenAI Chat Completions, forwards it to `route.Endpoint` + `/v1/chat/completions`, and translates the response (or SSE stream) back. This is what the hermetic canary in `pkg/proxy/gateway_path_canary_test.go` asserts. |
| Model picker / model discovery | **Guaranteed, with unverified fallback** | Hive probes `GET /v1/models` on the gateway (`pkg/dashboard/gateways.go`), with bearer auth when a key is configured. If discovery fails, the dashboard falls back to a static or configured model list and marks entries unverified (`docs/inference-backends.md#model-discovery`) — the picker never simply goes empty, but an unverified entry is not proof the model is reachable. |
| Login flow | **Not applicable — no CLI-style login exists** | Gateway auth is `api_key_env` / `api_key_file` (or, for `watsonx`, an IBM Cloud API key exchanged for a short-lived IAM bearer, `pkg/watsonx`) resolved from `config.GatewayConfig.ResolveAPIKey`. There is no device-flow or interactive `/login` button for a gateway backend the way there is for `copilot`/`claude` — the operator configures the key once in **Governor Config → Model Gateways** or YAML, and a stale/missing key surfaces as a `401` from the gateway itself, not as a login prompt. |
| Token metering | **Guaranteed for the translator path itself; per-kind claim otherwise** | `InferenceSink` (`pkg/tokens/inference_sink.go`) records usage from the OpenAI usage block on every translator response regardless of which gateway kind served it — `vllm`, `llm-d`, `litellm`, `watsonx`, and any named `custom` gateway all flow through the same `forwardToInference` call and the same sink, so the sniff genuinely covers the translator path, not just the two backends historically named in [docs/token-tracking.md](token-tracking.md). The gap is upstream of the sink: a gateway that never emits an OpenAI `usage` block on its response (some third-party proxies omit it, especially on non-streaming errors) yields zero recorded tokens for that call — see `docs/token-tracking.md`'s "zero consumed" diagnostics table for how that failure is surfaced instead of silently invoicing nothing. Budget-gated hives should treat a newly-added named gateway as unmetered until a real response with usage is observed. |
| Terminal access | **Guaranteed** | Terminal handoff (`pkg/dashboard/terminal_handoff.go`) requires a non-empty `terminalassert.SigningKey()` plus a configured hive ID (`canMintTerminalAssertion`). `SigningKey()` resolves, in order: `HIVE_TERMINAL_KEY` (hub-injected), a self-derived key from `HIVE_HUB_SECRET` + `HIVE_ID`, or — since [#6489](https://github.com/hivecommons/hive/issues/6489) — a persisted per-instance key a standalone/compose spoke auto-provisions with `crypto/rand` under `/data/.hive/terminal-key` (override the directory with `HIVE_TERMINAL_KEY_DIR`); a hub-provisioned hive always resolves through one of the first two lanes and never touches the fallback file. This is independent of the gateway path itself (terminal access never touches the translator). One caveat: on a read-only `/data` mount the fallback key cannot persist, so it degrades to the pre-#6489 static allowlist rather than the signed path (safety-preserving, not a hard failure). |

### What a gateway smoke check must prove

A gateway canary — CI or manual — is only meaningful if it proves the parts of
the path that can silently regress independently of each other:

1. **Request shape survives translation.** The model id set on the route
   reaches the gateway verbatim (no silent rewrite), and Anthropic
   system/user/assistant turns map to the correct OpenAI roles in order.
2. **Auth reaches the wire.** Whatever key `api_key_env`/`api_key_file`
   resolves is the exact value sent as `Authorization: Bearer <key>` — not a
   stale or empty one, and not leaked into `ExtraHeaders` improperly for
   backends (like `watsonx`) that use both a bearer and a plain header.
3. **Both response shapes decode.** A non-streaming Chat Completions response
   and a streaming SSE response (with `stream_options.include_usage`) both
   translate back to a valid Anthropic message/SSE event sequence, including a
   non-nil `usage` block.
4. **Failure is not swallowed.** An upstream error status (e.g. `401` for a
   bad key) is surfaced to the caller as an error, not silently treated as a
   successful empty completion.

`pkg/proxy/gateway_path_canary_test.go` is the current implementation of this
list: it drives the real translator against an `httptest` fake
OpenAI-compatible endpoint for a named custom gateway authenticated via
`api_key_file`, covering non-streaming, streaming, and the wrong-key failure
case, entirely hermetically (no network, no sleeps) as an ordinary PR-time Go
test rather than a new CI workflow arm.


## Hosted execution tier

Vendors now sell hosted agent runtimes: a platform that runs the agent loop,
keeps its state, and executes its tools in a sandbox the vendor operates.
[#14093](https://github.com/hivecommons/hive/issues/14093) asked whether Hive
can drive two of them, Claude Managed Agents and the OpenAI Agents API, as
backends. This section records the decision
([#14111](https://github.com/hivecommons/hive/issues/14111)), made from two
gap analyses:

- Claude Managed Agents:
  [backend-gap-claude-managed-agents.md](backend-gap-claude-managed-agents.md)
  ([#14106](https://github.com/hivecommons/hive/issues/14106)).
- OpenAI Agents API: the note proposed in
  [#14130](https://github.com/hivecommons/hive/pull/14130)
  ([#14108](https://github.com/hivecommons/hive/issues/14108)).

Neither existing on-ramp fits a hosted runtime:

- **Not CLI-style.** Criteria 1, 2 and 8 assume a local binary whose flags
  Hive controls. A hosted runtime has no binary. A name in `KNOWN_BACKENDS`
  would make the shell launch path try to exec it.
- **Not gateway-path.** That tier routes the Claude CLI's model calls through
  `pkg/proxy/anthropic_translate.go`. A hosted runtime runs the whole loop,
  tools included, so the gateway rows (model picker, login, terminal) do not
  describe it.

What does fit is the `pkg/extwork` external-host contract that Flue, OMP and
vibe-kanban already use (see
[design/external-workflow-admission.md](design/external-workflow-admission.md)):
start keyed by execution key, observe by status, cancel, and a receipt fetched
by artifact path and checked against a declared digest. So a hosted runtime
gets its own tier, **hosted execution**. It has no T1/T2/T3 assignment and is
never added to `KNOWN_BACKENDS` or `CLIBackends`. It is an engine adapter
under `pkg/extwork`, switched on per engine under `runs.external.<engine>` in
`hive.yaml`, like the existing engines.

### Decision per runtime

| Runtime and shape | Placement | Ceiling |
| --- | --- | --- |
| Claude Managed Agents, `self_hosted` environment whose worker runs in a Hive pod with forced egress | Hosted execution | `report-only` at entry; can be promoted |
| Claude Managed Agents, `cloud` environment, credential-less: `networking: limited` with no GitHub host, no vaulted GitHub secret, no GitHub MCP server, web tools off; repo delivered as a file resource or under a read-only one-repo token | Hosted execution | `report-only` at entry; can be promoted |
| Claude Managed Agents, any setup with a direct GitHub write path: a write token on the `github_repository` resource, a vaulted GitHub secret, or the GitHub MCP server | Hosted execution | **Advisory for good** (`shadow` or `report-only`) |
| OpenAI hosted loop (Responses API with a hosted container), credential-less: no network or an allowlist without GitHub, no GitHub token, no GitHub MCP server, writes only as Hive-answered function tools | Hosted execution | `report-only` at entry; can be promoted |
| OpenAI hosted loop where the container can reach GitHub with a write credential, or a remote MCP server holds one | Hosted execution | **Advisory for good** |
| OpenAI Agents SDK loop in a Hive pod with local tools | CLI-style | Not a new backend. `codex` already covers it. Any other SDK-based CLI goes through the T1-T3 bar above. |

### Advisory-mode ceiling

The GitHub proxy hard-denies `POST /repos/{o}/{r}/pulls` and
`PUT /repos/{o}/{r}/pulls/{n}/merge` for every agent mode (`denyRules` in
`src/crux/proxy/rules.go`). Those denials only bind traffic that the iptables
redirect forces through the proxy
([net-admin-requirement.md](net-admin-requirement.md)). Traffic from a hosted
sandbox does not pass through it.

A hosted sandbox **egresses GitHub directly** when it can reach GitHub with a
write-capable credential by a path Hive's proxy does not see: from the
sandbox itself, through a vendor-side git or MCP proxy, or through a secret
the vendor substitutes at its own egress. **Such a backend cannot exceed
advisory mode**, meaning `shadow` or `report-only` (`ModeShadow` and
`ModeReportOnly` in `src/pkg/extwork/admission.go`). No promotion lifts this
ceiling. The only way past it is a setup with no direct path. The adapter
enforces the ceiling in code. Before `Start`, it checks the session or
request configuration and refuses a write-capable stage if any direct path is
present. It refuses; it does not downgrade, matching how the existing
engines treat an item bound to an engine they cannot honour.

Two consequences:

- Today `pkg/extwork` has no write-capable mode at all, so every hosted
  backend enters at advisory regardless of shape. The ceiling above is the
  part that still holds after such a mode exists.
- A read-only, one-repo token is not a write path, but it is still a GitHub
  credential held by a third party. It must be a short-lived installation
  token scoped to one repository with `contents: read`, minted per run. It is
  never the App's own key and never a broad PAT.

### Acceptance criteria

These replace criteria 1-12 for a hosted backend. They are numbered H1-H6 so
a PR can cite them next to the CLI criteria without confusion.

- **H1. Engine adapter.** The runtime is an `extwork.Adapter` registered in
  `src/pkg/extwork/registry.go`. It is linked into `hive` only behind its own
  `extwork_<engine>` build tag, and offered only to relays that declare its
  `ext-exec/<engine>` capability. It passes the #8201 conformance rows that
  the Flue and OMP adapters pin in their `conformance_test.go` (admitted task
  and replay, held task starts nothing, capability and version refused,
  conflict and recreated engine, crash windows, cancel ignored and late
  success, outage and waiting, artifact and verifier refusals, store write
  fails). `Start` is idempotent on the execution key: the adapter stores the
  vendor's session or response ID against the key (vendor `metadata` can hold
  it) and reconciles before any retry.
- **H2. Egress statement and refusal.** The PR names the environment type or
  container network policy it uses. For each row of that runtime's egress
  table in its gap note, it says why the `POST /pulls` and
  `PUT /pulls/{n}/merge` denials still hold. Tests pin that the adapter
  refuses a write-capable stage for each direct-path row.
- **H3. Default off.** `runs.external.<engine>` defaults to off and to
  `shadow` when enabled with no mode, the same as `FlueBindingConfig`. It
  dispatches only in an explicitly chosen mode. This is the hosted
  counterpart of criterion 8's refusal gate.
- **H4. Credential handling.** Before dispatch, an authenticated read proves
  the vendor key works (for example `GET /v1/environments/{id}` or
  `GET /v1/models`), the counterpart of criterion 4. The vendor key never
  enters the sandbox. A Managed Agents `self_hosted` worker holds only its
  environment key and the per-session work secret, never `ANTHROPIC_API_KEY`.
- **H5. Metering and spend cap.** Each model call becomes a token record in
  `pkg/tokens`, deduplicated on the vendor's ID (the `span.model_request_end`
  event ID, or `response.id`), with the model recorded at dispatch when the
  usage record omits it. Non-token charges (session run time, web searches,
  hosted-tool calls) are recorded, or the dashboard shows the backend as
  partly unmetered, under the policy in
  [Metering coverage and budget-gated hives](#metering-coverage-and-budget-gated-hives).
  On a budget-gated hive the dispatch caps spend from the governor's
  remaining budget. For Managed Agents that is the session `budget`; for
  OpenAI it is per-call `max_output_tokens` and `max_tool_calls`, and the
  adapter stops chaining calls when the governor says so.
- **H6. Pins, docs and changelog.** The request pins what the runtime runs: the
  agent version and toolset version for Managed Agents, and the model
  snapshot and tool version for OpenAI. The PR adds the documentation rows
  listed below and a changelog fragment (criterion 7).

### Promotion beyond advisory

A hosted backend in a setup with no direct path may later run a stage beyond
`report-only`. Promotion is a PR that supplies all of:

1. A write-capable `pkg/extwork` mode, designed in its own issue, in which the
   change still reaches GitHub only through Hive's relays (`hive-open-pr`).
   The sandbox never writes to GitHub itself.
2. Live evidence from a real run: the vendor refusing an invalid network
   policy or tool configuration with an error instead of ignoring it (the
   counterpart of criterion 3), a test showing both proxy denials still hold,
   and metering for that run reconciled against the vendor's own total
   (`session.usage.list_cost` or the organisation usage API).

A setup with a direct path is never promoted.

### Which backend is prototyped first

**Claude Managed Agents, with a `self_hosted` environment**, has the smaller
gap:

- Tool traffic goes through Hive's own proxy, so the two denials hold through
  the existing `denyRules`. No new argument about who holds GitHub tokens is
  needed.
- A session `budget` is a hard spend cap that the platform checks before every
  model request.
- Agent, environment and session objects map directly onto `Start`, `Observe`
  and `Cancel`. The durable, paginated event list is the transcript.

The OpenAI hosted loop has the smaller gap on telemetry: model and usage are
on every response. But it has no environment type that keeps tool traffic on
Hive's proxy, no server-side spend cap, and hosted-tool charges are missing
from `usage`. Its SDK-in-pod shape is already the `codex` backend.

The prototype is [#14124](https://github.com/hivecommons/hive/issues/14124).
It is implemented as the `claude-managed-agents` `pkg/extwork` adapter:
`runs.external.claude_managed_agents` is default off, `shadow` when enabled
without a mode, and dispatches only in explicit `report-only` mode. The adapter
is linked only with `-tags extwork_claude_managed_agents`, requires a
`self_hosted` environment and `forced-egress` or `proxy-only` policy, and
stores vendor session bindings under the extwork record directory. It does not
touch `KNOWN_BACKENDS`, `CLIBackends`, or CLI parity/posture tests.

### Follow-ups

These are filed as #14124 completes, as its checklist requires:

- **Second backend.** The OpenAI hosted loop as a hosted-execution adapter in
  the credential-less shape, under H1-H6, after #14124 lands.
- **Tier promotion.** The write-capable `pkg/extwork` mode and the live
  evidence that the promotion criteria above call for.

### Rows shipped for the hosted prototype

The first hosted row is now present in
[`agent-configuration.md`](agent-configuration.md#methods-subscription-clis-vs-self-hosted-inference):

- **Methods table:** `hosted / claude-managed-agents`, authenticated by an API
  key reference (`api_key_env` or `api_key_file`) and pinned agent/model
  metadata. There is no CLI login or model discovery.
- **Engine block:** a hosted engine is not a `backend:` value. It is selected
  under `runs.external.claude_managed_agents`.
- **Per-engine block:** the endpoint, workflow/agent/toolset versions, pinned
  `self_hosted` environment, forced-egress policy, report-only mode, state
  directory, and USD 25 daily ledger cap are documented. Token events are
  metered in `pkg/tokens`; cumulative session list cost, runtime, and web
  search usage are persisted in the managed-agents ledger. The adapter stops
  at the ledger cap and the vendor session budget is set to the remaining
  cents, so the hosted path is not treated as unmetered.

## Metering coverage and budget-gated hives

Backend support is also a metering contract. Budget gates in `pkg/governor`
close when token spend reaches the configured weekly budget: the governor reads
collector totals through `UpdateBudgetFromTotals`, stores the current window in
`BudgetInfo`, and suppresses scheduled, resume, and CEL kicks while the budget is
exhausted unless an agent is explicitly exempt. A backend that produces no token
or cost data can therefore make a budget-gated hive look cheaper than it is.

Metered sources today are the scanner and sink implementations in `pkg/tokens`:

| Source | Coverage today | Notes |
| --- | --- | --- |
| `claude_scanner.go` | Claude Code session JSONL | Native usage blocks from recent Claude session files. |
| `copilot_scanner.go` | Copilot CLI `events.jsonl` | Reads `session.shutdown.modelMetrics` and avoids double-counting live proxy captures. |
| `bob_scanner.go` | Bob CLI chat recordings | Uses Bob's explicit token fields, falling back to content-size estimates only when a recording lacks token data. |
| `inference_sink.go` | vLLM, llm-d, LiteLLM, live Copilot proxy usage | Hive-written JSONL under the metrics directory. |
| `managed_agents_ledger.go` | Claude Managed Agents self_hosted sessions | Deduplicates `span.model_request_end` IDs into collector JSONL and persists cumulative USD list cost, active seconds, and web searches with a hard USD 25 daily cap. |

### Tagging Bob sessions for Bobalytics

Some adopters (for example Enrico) run Hive through Bob and want those Bob
sessions to stand out in Bobalytics without renaming Hive agents. Use the Bob
session-label controls instead:

```yaml
governor:
  bob:
    session_prefix: hive-

agents:
  scanner:
    backend: bob
    # Optional override; otherwise this reports as hive-scanner.
    bob:
      session_label: hive-triage
```

When a label is set, Hive passes it to Bob as `--instance-id` and exports
`HIVE_BOB_SESSION_LABEL` for prompts/scripts. The Hive identity remains
`scanner`: tmux, workdir, HOME, beads, claims, token buckets, and local token
attribution do not move. If both fields are unset, Bob's launch command and env
stay on the legacy path.

Policy: a backend without token/cost coverage **cannot run under a
budget-gated hive unless the operator explicitly marks it unmetered**, and the
dashboard must display that backend as `unmetered` rather than folding it into
normal spend. New backend PRs should state whether the backend is metered,
unmetered but allowed outside budget gates, or blocked until a scanner/sink
exists. This ties directly to the ccusage sourcing RFC
[#6234](https://github.com/hivecommons/hive/issues/6234): if Hive adopts
ccusage for the nine covered CLI backends, tier assignment should prefer that
shared parser over adding one-off scanners.

## Advisor lane

The advisor lane ([design](design/advisor-lane.md), epic
[#9638](https://github.com/hivecommons/hive/issues/9638)) has a second model
review each agent's finished turn and speak up; it never acts. Hive registers
one turn-end hook per backend at launch, so advisor support is per backend.
Enabling the advisor for an agent on a backend without support is not an error:
the agent launches normally and the dashboard Settings view, the agent page and
`GET /api/advisor/records?agent=` report the advisor as *not active on this
backend* for it, with no records produced.

"Synchronous" means the hook holds the turn until the verdict (or the advisor
timeout) arrives, so an interjection reaches the model before it continues.
Every supported backend records the same fields: one record per review,
including a skipped review, carrying agent, turn reference, advisor model,
severity, text, whether it interjected, delivery mode, tokens, cost and skip
reason. Those records feed the agent page, the aggregated advisor spend beside
agent spend, and the `advisor_records` admin MCP tool.

| Backend | Advisor supported? | Synchronous? | What is recorded |
| --- | --- | --- | --- |
| `claude` | Yes: `Stop` hook in the launch settings JSON (phase 1, [#9722](https://github.com/hivecommons/hive/issues/9722)) | Yes | Full record; the turn reference is the Claude Code session id |
| `omp` | Yes: `session_stop` extension passed with `--extension`, plus a roles overlay passed with `--config` (phase 2, [#9723](https://github.com/hivecommons/hive/issues/9723)) | Yes | Full record |
| `copilot` | Yes: `agentStop` hook under `COPILOT_HOME` (phase 3, [#9724](https://github.com/hivecommons/hive/issues/9724)) | Yes; the CLI force-ends a turn after 8 consecutive blocks, so the block limit must stay below 8 | Full record |
| `codex` | Yes: `Stop` hook enabled with `-c features.hooks=true` (phase 3, [#9724](https://github.com/hivecommons/hive/issues/9724)) | Yes | Full record |
| `goose` | No | n/a | Nothing; reported not active |
| `pi` | No | n/a | Nothing; reported not active |
| `bob` | No | n/a | Nothing; reported not active |
| `aider` | No | n/a | Nothing; reported not active |
| `gemini` | No | n/a | Nothing; reported not active |
| `agy` | No | n/a | Nothing; reported not active |
| `opencode` | No | n/a | Nothing; reported not active |
| `kilo` | No | n/a | Nothing; reported not active |
| `muse` | No | n/a | Nothing; reported not active |
| `openhands` | No | n/a | Nothing; reported not active |
| `omnigent` | No | n/a | Nothing; reported not active |
| `vllm` | No: inference backend, no turn-end hook | n/a | Nothing; reported not active |
| `llm-d` | No: inference backend, no turn-end hook | n/a | Nothing; reported not active |
| `litellm` | No: inference backend, no turn-end hook | n/a | Nothing; reported not active |
| `watsonx` | No: inference backend, no turn-end hook | n/a | Nothing; reported not active |

A backend moves into a "Yes" row only with an adapter that answers the hook in
the backend's own format and a test that the launch carries it.

## Repeated-failure check

The repeated-failure check ([#14550](https://github.com/hivecommons/hive/issues/14550))
records each shell command an agent runs and notes on the agent's dashboard row
when the same command keeps failing the same way. It never pauses the agent and
is off by default (`repeat_failure.enabled`, see
[Agent-runtime blocks](operator-reference.md#agent-runtime-blocks-configagentruntimeconfig)).
Hive needs a way to see each backend's commands, so support is per backend.

A backend is **covered** only when a committed, re-runnable harness made its
real CLI produce a correct command record, and the evidence includes the CLI's
own `--version` output. Hand-written test data does not count. Every other
backend is **not covered**: with the check on, its agent rows say
"repeat-failure check not available for this backend". The shell-activity
signal is what the "not receiving data" safeguard watches for a covered
backend, or "safeguard unavailable" where the backend has none.

This table is generated from the code table in
`src/crux/agent/repeat_failure_coverage.go`;
`TestRepeatFailureCoverageDocsMatchCode` fails when a backend is missing or a
row here differs from the code.

| Backend | Route | Status | Evidence | Shell-activity signal | Follow-up issue |
| --- | --- | --- | --- | --- | --- |
| `claude` | BASH_ENV (neutral), unwraps `eval '…'` | Not covered: BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `copilot` | Hive reads `events.jsonl`: shell `tool.execution_complete` (`shellExecution.exitCode`) | Covered | real-run `events.jsonl` (`record.sh`), `copilot --version`: GitHub Copilot CLI 1.0.91 | shell `tool.execution_start` / `tool.execution_complete` | — |
| `goose` | BASH_ENV (neutral) | Not covered: BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `codex` | BASH_ENV (neutral), one record across `exec /bin/bash -c` | Not covered: BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `pi` | BASH_ENV (neutral) | Not covered: BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `bob` | BASH_ENV (neutral) with `SHELL=/bin/bash` (documented shell change: dash → bash) | Not covered: BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `aider` | guarded `~/.bashrc` line (`aider_bashrc`) sourcing the BASH_ENV script, Hive-managed homes only, with `SHELL=/bin/bash`; the no-tty `/bin/sh -c` path reads no rc file and is not covered | Not covered: interactive `$SHELL -i -c` ignores BASH_ENV; `aider_bashrc` route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `gemini` | BASH_ENV (neutral), unwraps the `{ …; }; __code=$?` wrapper | Not covered: BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `agy` | `agy-turn` `run_command` steps (headless launch only; the TUI launch has none) | Not covered: route built, but no committed real-run harness evidence yet (C4): agy 1.4.0 needs a Google login no harness can provide (tried 2026-10-27: `authentication required`) and takes no stub model provider | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `opencode` | BASH_ENV (neutral) with `SHELL=/bin/bash` | Not covered: BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `kilo` | BASH_ENV (neutral) with `SHELL=/bin/bash` | Not covered: BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4) | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `muse` | none yet; planned: `PostToolUse` / `PostToolUseFailure` hooks once Hive launches muse with a hook file or flag it owns | Not covered: sandboxed shell tools in a closed binary, so commands never reach the BASH_ENV route; Hive has no muse launch or hook wiring and writes no muse settings file, so the hook route is not built | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `omp` | Hive-owned `tool_result` extension (`--extension`), `details.exitCode` | Covered | stub-provider harness `run.sh`, `omp --version`: omp/18.13.1 | safeguard unavailable | — |
| `openhands` | Hive reads the saved conversation events (`~/.openhands/conversations/*/events`): `terminal` `ObservationEvent` (`observation.exit_code`) | Covered | real-run saved events (`run.sh`, stub provider), `openhands --version`: OpenHands CLI 1.16.0 | `terminal` `ActionEvent` / `ObservationEvent` | — |
| `omnigent` | none yet; planned: tool-call policy checkpoint (`ALLOW`/`DENY` verdicts) or saved session events | Not covered: meta-harness driving Claude Code, Codex or Pi through its own policy engine; commands never reach the BASH_ENV route and the backend-specific route is not built yet | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `vllm` | follows `claude` (drives Claude Code) | Not covered: follows `claude`, which is not covered | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `llm-d` | follows `claude` (drives Claude Code) | Not covered: follows `claude`, which is not covered | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `litellm` | follows `claude` (drives Claude Code) | Not covered: follows `claude`, which is not covered | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |
| `watsonx` | follows `claude` (drives Claude Code) | Not covered: follows `claude`, which is not covered | None | n/a | [#14559](https://github.com/hivecommons/hive/issues/14559) |

The BASH_ENV route is described in
[the shell route](agent-configuration.md#repeat-failure-check-the-shell-route-repeat_failure).
A backend moves to "Covered" only together with its committed harness evidence.

## Deprecation path

A backend can be demoted or removed when its upstream CLI breaks Hive's contract
and no maintainer or community owner repairs it within the grace period.

1. **Detection.** The scheduled [backend smoke](backend-smoke.md) workflow is the
   primary canary. Its `latest` lane detects incoming vendor drift before Hive's
   pinned image moves; its `pinned` lane detects breakage in the contributor
   image operators already use. Matrix/list-parity tests catch repository-local
   drift such as a backend present in `KNOWN_BACKENDS` but missing from Go config
   or docs.
2. **Triage and grace period.** A red `latest` smoke opens or updates a
   `backend-smoke` issue and starts a 14-day repair window for supported and
   experimental backends. A red `pinned` lane is production breakage: it may
   trigger immediate demotion from T1/T2 while keeping a 7-day window to restore
   the old tier. Core/headless-pod backends get maintainer escalation before
   removal because hives may depend on them for unattended operation.
3. **Demotion.** If the backend still authenticates but no longer satisfies a
   higher-tier criterion, move only the tier-specific allowlists or posture:
   `HEADLESS_BACKENDS` in the `Justfile`, `K8S_HEADLESS_BACKENDS` in
   `src/pkg/dashboard/api_contribute.go`, the confinement posture tests, and the
   docs rows that claimed the higher tier.
4. **Removal.** If the backend cannot be launched reliably or has no owner after
   the grace period, remove it from `KNOWN_BACKENDS`, `CLIBackends`, backend
   matrix tests, `docs/backend-setup.md`, this tier document, and any
   sandbox/contributor docs. The removal PR cites the smoke issue and states the
   replacement path for operators.

Deprecation is not punishment for vendor drift; it keeps the public backend list
honest. A removed backend can return through the same acceptance bar as a new
backend, with fresh smoke evidence and metering posture.

## Retroactive tier record: Muse Code

PR [#6379](https://github.com/hivecommons/hive/pull/6379) (Muse Code, carrying
forward [#6222](https://github.com/hivecommons/hive/pull/6222)) is now recorded
retroactively as **local path T2** and **pod path not T1**. The merged diff added
`muse` to `KNOWN_BACKENDS` and `backend_perm_flag` in `config/backends.conf`, to
`CLIBackends` in `src/pkg/config/backends.go`, and to the local posture matrix as
`postureSandboxed` in
`src/pkg/dashboard/contribute_local_mode_backend_matrix_test.go`. Its local
path is T2 because `muse_local_perm_flag_shell` keeps Muse Code's own OS
sandbox on, narrows it with `--workspace`, `--sandbox-network proxy-only`, and
`--no-foreign-personal-context`, and keeps the disable-sandbox `--yolo` flag
behind `HIVE_MUSE_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1`; the same posture
is documented in `src/docs/sandbox-isolation.md` and `docs/backend-setup.md`.
The pod path is not T1 yet because `muse` is absent from the `Justfile`
`contribute-k8s` `HEADLESS_BACKENDS` allowlist and from
`K8S_HEADLESS_BACKENDS` in `src/pkg/dashboard/api_contribute.go`, and
`docs/backend-setup.md` says unattended fresh-pod credentials have not been
verified. The images pin Muse Code by `MUSE_VERSION` and per-architecture
`MUSE_SHA256_*` values in both `src/Dockerfile` and
`src/Dockerfile.contributor`. Metering remains unclaimed: `pkg/tokens` has
scanners/sinks for Claude, Copilot, Bob, and inference proxy usage, but no Muse
scanner, so budget-gated hives must treat Muse as unmetered or blocked until a
scanner/sink is added.
