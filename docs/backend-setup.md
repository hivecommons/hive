# Agent backend setup

Hive validates backend names in `src/pkg/config` and launches CLIs in `src/pkg/agent/manager.go`. `backend:` selects the runtime for an agent; inference backends are covered separately in [inference-backends.md](inference-backends.md).

## CLI backends

| Backend | Binary launched by the Go manager | Auth / setup | Notes |
| --- | --- | --- | --- |
| `claude` | `claude` | Install Claude Code and log in once. Hive launches with `--dangerously-skip-permissions`; inference routes add `--bare --settings`. | Advisory/issue modes add disallowed GitHub MCP tools. Every mode also denies host-state commands — privilege escalation (`sudo`/`pkexec`/`doas`/`su`) and boot/deployment tools (`rpm-ostree`/`bootc`/`ostree`/`grubby`/`bootctl`/`efibootmgr`) — because the tmux path runs unconfined on the operator's host (#4918). Set `HIVE_CLAUDE_DANGEROUSLY_ALLOW_HOST_STATE=1` only when you intentionally want an agent to manage host state. |
| `copilot` | `copilot` | Install GitHub Copilot CLI and authenticate with GitHub. Hive also probes Copilot model entitlements live. | Launched with `--no-auto-update --allow-all`; write tools are denied by mode when needed. |
| `gemini` | `gemini` | Install Gemini CLI and configure its normal auth/API key. | Supported by the server-side manager; Hive launches `gemini` and passes `--model` when a model is configured. |
| `goose` | `goose` | Install Block Goose and configure provider/model (`GOOSE_PROVIDER`, `GOOSE_MODEL`, or `goose configure`). | Hive launches `goose run -s` and appends `--model` when set. |
| `pi` | `goose` in the Go manager; `pi` in contributor scripts | Contributor mode requires `AGENT_MODEL=provider/model` plus that provider's official credential variable or `~/.pi/agent/auth.json`. In the server-side manager, `backendBinary("pi")` maps to Goose. | Contributor Pi supports interactive and headless (`--print --mode json`) delivery. No generic Pi key/provider variable exists. |
| `bob` | `bob` | Provide `HIVE_BOB_API_KEY` or `/secrets/bob_api_key` for pods; contributor mode requires `BOBSHELL_API_KEY`. | Hive uses API-key auth headlessly and accepts the Bob license at launch. |
| `codex` | `codex` | Install `@openai/codex` and run `codex login --device-auth` for subscription/OAuth auth. The CLI stores credentials in `CODEX_HOME/auth.json` (default `${HOME}/.codex/auth.json`); API-key mode can use `CODEX_API_KEY`/`OPENAI_API_KEY` or a populated auth file, but it is not required for subscription users. | Hive gives each agent its own `CODEX_HOME` and probes `auth.json` for OAuth tokens/API-key state (or API-key env presence). Contributor mode keeps `--ask-for-approval on-request --sandbox workspace-write`, grants the exact `HIVE_WORKSPACE_DIR` tree with `--add-dir`, and defaults `approvals_reviewer` to `auto_review` so an unattended task never waits on the contributor. Override with `HIVE_CODEX_APPROVAL_POLICY`/`HIVE_CODEX_SANDBOX_MODE`/`HIVE_CODEX_APPROVALS_REVIEWER`, or set `HIVE_CODEX_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1` only when you intentionally want the old bypass posture. `AGENT_REASONING_EFFORT` is passed to Codex as `-c model_reasoning_effort="..."`. |
| `aider` | contributor scripts launch `aider`; the server-side Go manager does not launch it | Install Aider and configure its provider/API key normally for contributor mode. | Not supported as a server-side agent backend in this branch: config accepts the name, but `backendBinary("aider")` returns `unknown backend: aider`, so a pod agent will not start. Use contributor mode for Aider. |
| `agy` | `agy` | Install the Antigravity CLI (`brew install --cask antigravity-cli`) and run `agy` once to sign in interactively with a Google account. **There is no API-key mode.** agy persists OAuth state under `~/.gemini` (`oauth_creds.json` with a refresh token, `google_accounts.json`, alongside the `antigravity-cli/` state dir) — `just contribute-hive agy` stages that whole directory into the container, though whether a staged credential actually re-authenticates an unattended agy has not been confirmed end-to-end (agy's binary also links an OS-keyring client, so some auth paths may need a running Secret Service the container does not provide). | **No OS-level sandbox of its own** (same posture as goose/bob/pi/aider — see [sandbox-isolation.md](../src/docs/sandbox-isolation.md)). **Container mode is the only mode with any host boundary** and is supported: `src/Dockerfile.contributor` installs the `agy` binary from Google's published, checksummed release tarball (`#5048`; it did not before). Local mode **refuses to launch** agy without `HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED=1`. Launched with `--dangerously-skip-permissions` (same contract as `claude`, or agy blocks on a per-tool approval prompt nobody is attached to answer). When a model is configured the manager appends `--model <m> --effort low`: agy *requires* `--effort` alongside `--model` and otherwise ignores the model entirely. An unrecognised model is not fatal — agy warns and falls back to its own default. Note the effort is the fixed `agyDefaultEffort` constant server-side; hive has no per-agent effort setting yet, so `AGENT_REASONING_EFFORT` applies to the **contributor relay only**, not to pod agents. Headless (`agy -p`) is verified on a host that has already signed in; whether it works unattended in a fresh container is unverified, which is also why agy stays out of `just contribute-k8s`'s headless-pod allowlist — a pod cannot complete the interactive sign-in even once. agy also exits `2` if the working directory does not resolve, where some other backends tolerate it. agy 1.1.22's own `--sandbox` flag is **not** a local OS boundary — see `config/backends.conf`'s "no confinement mechanism" section for why. |
| `opencode` | `opencode` | Install the opencode CLI ([opencode.ai/docs](https://opencode.ai/docs/)) and run `opencode auth login`; the credential is written to `~/.local/share/opencode/auth.json`. Provider-agnostic (75+ providers) — the model provider is configured in opencode's own config/auth, not in Hive, so `AGENT_MODEL` is passed through as `provider/model` on the relay path (e.g. `export AGENT_MODEL=anthropic/claude-sonnet-4-6`). | **Contributor relay only; headless mode only.** Dispatches through `opencode run "<prompt>" --auto` under `CONTRIBUTOR_MODE=headless`; there is no interactive-tmux wiring for opencode, so `CONTRIBUTOR_MODE=interactive` does not apply to it. `backend_perm_flag` maps opencode to `--auto`, opencode's unattended auto-approve flag. **Confinement note:** opencode has no OS-enforced filesystem sandbox of its own. `just contribute-hive opencode local` narrows it with a host-state command deny-list only (via opencode's own `permission.bash` config, the same command family the claude deny-list covers) — a floor, not a sandbox boundary. Container mode is the default and the stronger boundary. See [sandbox-isolation.md](../src/docs/sandbox-isolation.md#per-backend-confinement-on-the-contributor-local-path) for the full per-backend matrix. Set `HIVE_OPENCODE_DANGEROUSLY_ALLOW_HOST_STATE=1` to drop the deny-list. Not yet in `just contribute-k8s`'s headless-pod allowlist: whether the auth credential supports unattended use in a fresh pod is unverified, so it currently runs headless only on a host that has already signed in (same posture as `agy`). |

## IBM Bob headless setup

`backend: bob` launches IBM bobshell (`bob`), the IBM watsonx Code Assistant CLI. In a Hive pod or contributor container it must use API-key auth: the default IBMid/W3ID browser SSO flow opens a browser and waits on a localhost callback, which a headless pod cannot satisfy, then times out after about three minutes. Hive checks for a key before launch and parks the agent with an actionable error instead of burning that timeout.

Configure the key in one of these ways:

```yaml
governor:
  bob:
    api_key_env: HIVE_BOB_API_KEY       # hive-side env var name
    api_key_file: /secrets/bob_api_key  # mounted Secret path
```

Defaults are already wired: Hive consults `/secrets/bob_api_key`, then `/data/secrets/bob_api_key` (where the dashboard's Governor → Bob tab stores a key), then the `HIVE_BOB_API_KEY` environment variable. Use the dashboard tab when you do not have cluster Secret access; it writes the key to the PVC-backed `/data/secrets/bob_api_key` and relaunches parked bob agents. The value is injected into bob as `BOBSHELL_API_KEY`, and Hive launches bob with the hidden-but-supported `--auth-method api-key` flag plus full approval/trust flags for unattended operation. Store only the location in YAML, never the key value.

Contributor relay containers use the same bobshell package, but contributor-mode scripts expect `BOBSHELL_API_KEY` in the container environment when `AGENT_BACKEND=bob`.

The dashboard **Test key** probe intentionally sends `User-Agent: bobshell`. IBM's edge has been observed to block generic Go/curl user agents with an HTML 403 before the request reaches bob auth, while the bobshell UA returns the real backend verdict. If you reproduce a key test manually, use that UA or treat a generic-UA 403 as an inconclusive edge block, not proof that the key is invalid.

## Contributor relay image

`src/Dockerfile.contributor` builds the ClankeR image used by `just contribute-hive`. It installs Claude Code, Copilot, Codex, Bob, Goose, Pi, `gh`, Go, tmux, and the relay scripts. `src/compose-contributor.yaml` runs that image with your local Hive config and selected backend. It mounts `${HOME}/.config/hive`, `${HOME}/.claude`, and `${HOME}/.config/claude-code` read-only, then reads the registered `HIVE_HUB` and `HIVE_REGISTRATION_TOKEN` from `${HOME}/.config/hive/contributor.env` inside the container.

```bash
AGENT_BACKEND=claude just contribute-hive
AGENT_BACKEND=goose GOOSE_PROVIDER=anthropic GOOSE_MODEL=claude-sonnet-4-6 just contribute-hive
AGENT_BACKEND=pi AGENT_MODEL=openai/gpt-5 OPENAI_API_KEY=... CONTRIBUTOR_MODE=headless just contribute-hive
AGENT_BACKEND=litellm HIVE_LITELLM_ENDPOINT=https://litellm.example.com just contribute-hive
```

`AGENT_BACKEND` selects the CLI, `AGENT_MODEL` optionally pins the model, and `CONTRIBUTOR_MODE` defaults to `interactive` (tmux with a TTY). For Pi, `AGENT_MODEL` is required and must be the canonical `provider/model` token; this is a contributor preference, not task routing or assignment state. The same token is used for initial launch, restart, reconnect evidence, and headless execution. For Codex, `AGENT_REASONING_EFFORT` optionally pins the reasoning effort. `CONTRIBUTOR_MODE=headless` is reserved for one-shot/no-TTY task delivery.

Pi credentials remain in the selected provider's official environment variable (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, and so on) or Pi's `~/.pi/agent/auth.json`. Hive never maps a generic `PI_API_KEY`, never puts a credential value in argv, passes only the selected provider's variables into its contributor container, and removes unrelated providers from the ephemeral auth/models profile mounted there. Readiness is deliberately staged: `pi_binary`, `pi_configuration`, `pi_authentication`, and `pi_invocation` appear in relay capability/status JSON. A present key or auth-file entry reports `configured_unverified`; only a successful real invocation advances authentication to `verified` and invocation to `succeeded`, because `pi --version` plus a non-empty key is not authentication proof.

Headless Pi cancellation is bounded: revocation terminates the active child and fences its late exit from completing a newer task generation. Interactive Pi still uses tmux delivery and is not cancellation-conformance-proven.

Both contributor modes are unattended from Codex's perspective: Hive may
deliver work when nobody is watching the tmux pane. The default automatic
reviewer evaluates only actions that already cross the `workspace-write`
boundary. It does not widen that boundary, and the dangerous no-sandbox mode
remains opt-in. A denied or timed-out automatic review returns to Codex; in
headless mode a non-zero terminal result is reported to Hive with a bounded,
token-redacted diagnostic rather than waiting for input.

`just contribute-check <backend>` runs a read-only preflight before registration. It checks that the chosen CLI exists and that obvious auth prerequisites are present.

## Secrets

Store secret values outside `hive.yaml`. YAML should contain env var names or key-file paths, not keys. The dashboard and config save path rewrite YAML, so a literal secret in YAML would be persisted in plaintext.
