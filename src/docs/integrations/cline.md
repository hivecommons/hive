# Cline coding workers

Cline is an opt-in coding backend, implemented using the official headless CLI,
not the VS Code extension and not an embedded SDK. Install the tested CLI:

```sh
npm install -g cline@3.0.74
cline --version
cline --help
cline auth
```

Use `cline auth` interactively to configure a **dedicated API-key provider**.
Never put an API key in Hive YAML, a prompt, or launch arguments. Workers read
`HIVE_CLINE_CONFIG_DIR/data/settings/providers.json` (default `~/.cline`), copy
**only the selected provider** into owner-only per-turn state, and remove that
state after the process group stops. Global rules, MCP servers, plugins, hooks,
conversations, and other providers' credentials are not copied.

The model must be `provider/model`; for OpenRouter a model containing `/`
is preserved: `openrouter/anthropic/claude-sonnet-4-6` launches
`--provider openrouter --model anthropic/claude-sonnet-4-6`.
The account must be entitled to that model. There is no implicit provider,
model guessing, or fallback to another coding engine.

## Hive and Crux

Configure an agent using the normal definition/configuration path:

```yaml
backend: cline
model: openrouter/anthropic/claude-sonnet-4-6
reasoning_effort: high
enabled: false
```

Enable the agent explicitly when its credentials and isolation are ready.
Standalone Crux without a per-UID worker refuses an unconfined launch unless
`HIVE_CLINE_DANGEROUSLY_RUN_UNCONFINED=1` is deliberately set.
Hive's per-UID panes need read access to a dedicated provider config directory;
set `HIVE_CLINE_CONFIG_DIR` on the Hive process to that directory. Do not mount
a user's entire home or credential collection. CLI presence/capabilities and
provider configuration can be checked without inference:

```sh
crux cline-turn --model openrouter/anthropic/claude-sonnet-4-6 --check
```

The same subcommand is available as `hive cline-turn`. Hive's interactive pane
remains attachable: each kick invokes one foreground CLI turn, streams lifecycle
events and the final text, then returns to a stable ready marker. This does not
scrape Cline's changing TUI. Turns use the existing pane workspace and environment
(including the policy proxy and credential-scrubbing wrappers).

### Disposable sandbox kicks

Cline does **not** have complete parity across the optional disposable sandbox
runtimes. Kubernetes Jobs are supported through their existing
`job.env_from_secrets` credential boundary. Rootless Podman kicks are not:
`sandbox.LaunchSpec` has no credential-file mount or Secret reference, and its
only filesystem grant is the Git workspace. This is a Cline-specific refusal,
not a blanket refusal for all other coding backends.

For the Job runtime, provision a **dedicated** Secret in the spoke's namespace
with only `HIVE_CLINE_PROVIDERS_B64`, containing base64-encoded selected-provider
JSON. The existing export narrows the source without inference; this pipeline
keeps credential bytes out of arguments and the repository:

```sh
crux cline-turn --model openrouter/anthropic/claude-sonnet-4-6 \
  --export-provider-config | base64 | tr -d '\n' |
  kubectl -n your-spoke-namespace create secret generic cline-provider \
    --from-file=HIVE_CLINE_PROVIDERS_B64=/dev/stdin
```

Use an approved image containing the matching `crux cline-turn` and official
Cline CLI; a pre-integration image cannot execute this command. Configure the
existing shared-workspace PVC and reference the Secret **on the Cline agent**:

```yaml
agent_sandbox:
  enabled: true
  image: your-approved-image-with-cline-turn
  job:
    workspace_claim: hive-data
agents:
  cline-worker:
    backend: cline
    model: openrouter/anthropic/claude-sonnet-4-6
    reasoning_effort: high
    enabled: false
    sandbox:
      enabled: true
      runtime: job
      job:
        env_from_secrets: [cline-provider]
```

Enable the agent deliberately after provisioning credentials, the shared PVC
(RWX when needed), namespace RBAC, and the existing restricted egress policy.
The host neither reads the Job Secret nor puts its contents in the Job
manifest. The worker's explicit `--provider-secret` path consumes and unsets
the transport variable, revalidates/narrows the provider, and writes owner-only
turn state outside the Git workspace, including through symlinks. No file or
ambient-provider fallback is allowed. The bounded worker, Job cancellation,
transcript scrubbing, review spool, publish verification and push broker remain
in charge. Structured usage appears in the captured transcript; private usage
files in an ephemeral Job are not automatically collected from its filesystem.

Podman support requires a separate owner decision: approve a selected-provider,
read-only, out-of-workspace credential mount with lifecycle/UID/SELinux rules,
or provide a brokered inference endpoint that needs no provider credential in
the workload. Neither capability exists in the current Podman launcher.
Do not pass provider JSON through its environment allowlist, put credentials
in the Git workspace/image, or turn off the sandbox to evade this refusal.

## ClankeR

ClankeR is Hive's contributor relay, not a coding launcher in Dibs. No Dibs
dependency or generated standalone Crux mirror needs a hand-written patch.
Install a current `crux` that exposes `cline-turn` alongside the Cline CLI:

```sh
export AGENT_MODEL=openrouter/anthropic/claude-sonnet-4-6
export AGENT_REASONING_EFFORT=high # optional; none|low|medium|high|xhigh
just contribute-setup cline
just contribute-hive cline
```

The contributor container includes Cline and the canonical Crux adapter. Setup
stages only the selected provider config read-only; the task runs in the relay's
workspace. The relay uses bounded headless tasks in both TTY and unattended
deployment modes. Existing registration, leases, heartbeats, assignment fencing,
verdict/PR verification, log redaction and attribution remain authoritative.
`HIVE_CLINE_RUNNER=hive` selects an installed Hive binary with the same subcommand.

Cline provides **no OS sandbox or shell deny-list**. Container mode is the default.
Native mode refuses to run unless the operator explicitly opts into
`HIVE_CLINE_DANGEROUSLY_RUN_UNCONFINED=1` on a disposable/externally isolated host.
That setting does not grant bypasses to the hub or change other engines.

For Kubernetes, the normal `just contribute-k8s` generator exports only the
selected provider into `HIVE_CLINE_PROVIDERS_B64` in the existing Secret.
The entrypoint materializes it as owner-only `providers.json`, then unsets the
transport variable. Treat generated YAML as secret material, just like the
existing contributor GitHub credential. A manually managed deployment can
instead mount `providers.json` under `/home/dev/.cline/data/settings/`.
Do not put credential JSON in a ConfigMap or CLI arguments.

## Limits and failure semantics

- CLI capabilities are checked before execution; legacy editor-dependent CLIs
  receive an actionable install error. No SDK package is added to Hive.
- The default per-turn timeout is 30 minutes. `crux cline-turn --timeout 45m`
  changes it for a direct turn; ClankeR also retains its outer task deadline.
  SIGINT/SIGTERM cancels the CLI process group; ignored cancellation is killed
  after a bounded grace period. No background Cline hub is used.
- Tool auto-approval is scoped to the isolated, bounded invocation so coding can
  run unattended; no global Cline approval setting is changed. The outer
  container/policy proxy is the security boundary, not Cline's data-dir feature.
- Hive `tools` and `connections` policies cannot be faithfully translated to
  Cline's CLI. Such agent definitions are refused rather than silently dropping
  their restrictions. OAuth provider entries are also refused: copying
  single-use refresh tokens without a synchronization protocol is unsafe.
- Custom `launch_cmd` overrides are refused for Cline, including sandbox Jobs.
  Disposable Podman sandbox kicks remain unsupported; the Job runtime uses only
  explicit Secret references as described above. Per-UID workers and contributor
  containers are separate supported paths, not silent fallbacks.
- Structured usage is retained as `CLINE_USAGE` in captured output and sanitized
  per-turn normalized records under `~/.local/state/crux/cline/usage` (30-day
  retention). Hive collects shared/per-agent home records through its existing
  token, budget and cost-estimation pipeline, including spend reported before a
  failed turn. These are cumulative turn totals, not per-request traces.
  Cline has no
  CLI model-catalog command; the UI requires an explicit provider-qualified model,
  not Copilot's catalog. Unsupported account quota/cost discovery stays unknown.
- Only `run_result` with `finishReason=completed` is success. Missing results,
  malformed NDJSON, aborts, task errors, timeout and nonzero exits fail the task.

## Verification scope

The integration's tests exercise capability refusal, argv construction,
credential narrowing, isolated state, result/error parsing, timeout/cancellation
and relay routing with controlled fake executables. These are adapter tests,
not proof that a provider can execute paid work. The official npm Cline 3.0.74
native CLI was checked with `--version` and `--help` on macOS; no authenticated
coding task or production contributor registration was launched.
An isolated invalid-provider stdin/NDJSON smoke also returned the expected
structured error and nonzero exit without contacting an authenticated provider.

`src/crux` in Hive is canonical. After merge, the existing Crux sync publishes
the module-path-rewritten standalone mirror. Consumers of released standalone
Crux binaries need a normal maintainer release containing this change; do not
patch the generated mirror or invent a release.
