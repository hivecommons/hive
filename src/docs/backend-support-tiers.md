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

The 11 shell-known CLIs (`KNOWN_BACKENDS` in `config/backends.conf`) already
fall into three classes that are enforced in code. This document names them.

| Tier | Meaning | Backends today | Enforcement point |
| --- | --- | --- | --- |
| **T1 - core (headless-pod)** | Runs unattended in a TTY-less pod from staged or env-injected credentials | `claude`, `litellm`, `copilot`, `codex`, `goose` | `HEADLESS_BACKENDS` in the `Justfile` (`contribute-k8s`) and `K8S_HEADLESS_BACKENDS` in `src/pkg/dashboard/api_contribute.go` |
| **T2 - supported (confined)** | Has a confinement floor hive can wire on the contributor local path: an OS sandbox, or at least a host-state deny-list | `claude`/`litellm`, `codex`, `copilot` (sandboxed); `opencode` (deny-listed) | `backend_perm_flag` and the `*_local_perm_flag_shell` helpers in `config/backends.conf` |
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
and the evidence for each criterion below. The criteria are cumulative: T3 is
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
    `bin/contributor-relay.sh` with the one-shot sub-command or flag, and a
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
    are unverified in a pod, and `agy`, `opencode`, and `kilo` are kept out
    of the allowlist for the same reason (see
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

## Worked example

PR [#6222](https://github.com/hivecommons/hive/pull/6222) (Muse Code) is the
shape this bar expects: it claims T2 on the local path (own OS sandbox, on by
default, narrowed rather than bypassed), shows the invalid-value transcript
for `--approval-mode` (criterion 3), explains why `detect_cli` probes the
credential rather than `--version` (criterion 4), pins the image install by
digest (criterion 6), and explicitly declines T1 because unattended pod
credentials were not verified (criterion 11). A future PR that supplies
criterion 11 promotes it.
