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

The 12 shell-known CLIs (`KNOWN_BACKENDS` in `config/backends.conf`) already
fall into three classes that are enforced in code. This document names them.

| Tier | Meaning | Backends today | Enforcement point |
| --- | --- | --- | --- |
| **T1 - core (headless-pod)** | Runs unattended in a TTY-less pod from staged or env-injected credentials | `claude`, `litellm`, `copilot`, `codex`, `goose` | `HEADLESS_BACKENDS` in the `Justfile` (`contribute-k8s`) and `K8S_HEADLESS_BACKENDS` in `src/pkg/dashboard/api_contribute.go` |
| **T2 - supported (confined)** | Has a confinement floor hive can wire on the contributor local path: an OS sandbox, or at least a host-state deny-list | `claude`/`litellm`, `codex`, `copilot`, `muse` (sandboxed); `opencode` (deny-listed) | `backend_perm_flag` and the `*_local_perm_flag_shell` helpers in `config/backends.conf` |
| **T3 - experimental (unconfined)** | No confinement hive can wire; local mode refuses to launch without a per-backend opt-in, container mode is the default | `goose`, `agy`, `bob`, `pi`, `aider`, `kilo` | `unconfined_local_perm_flag_shell` and `HIVE_<BACKEND>_DANGEROUSLY_RUN_UNCONFINED=1` (#4918) |

Tier assignment is per launch path. `goose` is T1 on the pod path (it has a
one-shot `goose run` and an env-key credential) and T3 on the local path (no
sandbox). A PR states the tier it claims for each path it wires.

`gemini` is Go-only (`cliBackendExceptions`): it is launched by the hub-side
manager and has no contributor-relay wiring at all, so the local-path tiers do
not apply to it. Adding it to the relay would go through this bar as a new
backend.

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
   `src/pkg/config/config.go`, in the same PR. The guard is
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
   `--dangerously-skip-permissions` for claude and
   `--dangerously-bypass-approvals-and-sandbox` for codex).
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
     `TestShellAndGoDenyListsAgree` (`src/pkg/agent/host_state_deny_test.go`)
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

Policy: a backend without token/cost coverage **cannot run under a
budget-gated hive unless the operator explicitly marks it unmetered**, and the
dashboard must display that backend as `unmetered` rather than folding it into
normal spend. New backend PRs should state whether the backend is metered,
unmetered but allowed outside budget gates, or blocked until a scanner/sink
exists. This ties directly to the ccusage sourcing RFC
[#6234](https://github.com/hivecommons/hive/issues/6234): if Hive adopts
ccusage for the nine covered CLI backends, tier assignment should prefer that
shared parser over adding one-off scanners.

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
`CLIBackends` in `src/pkg/config/config.go`, and to the local posture matrix as
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
