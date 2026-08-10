# Agent backend setup

Hive validates backend names in `v2/pkg/config` and launches CLIs in `v2/pkg/agent/manager.go`. `backend:` selects the runtime for an agent; inference backends are covered separately in [inference-backends.md](inference-backends.md).

## CLI backends

| Backend | Binary launched by the Go manager | Auth / setup | Notes |
| --- | --- | --- | --- |
| `claude` | `claude` | Install Claude Code and log in once. Hive launches with `--dangerously-skip-permissions`; inference routes add `--bare --settings`. | Advisory/issue modes add disallowed GitHub MCP tools. |
| `copilot` | `copilot` | Install GitHub Copilot CLI and authenticate with GitHub. Hive also probes Copilot model entitlements live. | Launched with `--no-auto-update --allow-all`; write tools are denied by mode when needed. |
| `gemini` | `gemini` | Install Gemini CLI and configure its normal auth/API key. | Hive passes `--model`. |
| `goose` | `goose` | Install Block Goose and configure provider/model (`GOOSE_PROVIDER`, `GOOSE_MODEL`, or `goose configure`). | Hive launches `goose run -s` and appends `--model` when set. |
| `pi` | `goose` in the Go manager; `pi` in contributor scripts | In the server-side manager, `pi` is currently a Goose alias because `backendBinary("pi")` maps to `goose`. Configure Goose for pod agents. The contributor relay image/scripts still know a separate `pi` binary. | Do not assume the pod path uses pi.dev until the manager mapping changes. |
| `bob` | `bob` | Provide `HIVE_BOB_API_KEY` or `/secrets/bob_api_key` for pods; contributor mode requires `BOBSHELL_API_KEY`. | Hive uses API-key auth headlessly and accepts the Bob license at launch. |
| `codex` | accepted by config; contributor scripts launch `codex` | Install `@openai/codex` and provide `OPENAI_API_KEY`. Contributor mode sets a per-agent `CODEX_HOME`. | v2 HEAD server-side `backendBinary` does not map `codex`, so use it in contributor mode unless the manager mapping has been updated. |
| `aider` | accepted by config; contributor scripts launch `aider` | Install Aider and configure its provider/API key normally. | v2 HEAD server-side `backendBinary` does not map `aider`, so use it in contributor mode unless the manager mapping has been updated. |

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

`v2/Dockerfile.contributor` builds the ClankeR image used by `just contribute-hive`. It installs Claude Code, Copilot, Codex, Bob, Goose, Pi, `gh`, Go, tmux, and the relay scripts. `v2/compose-contributor.yaml` runs that image with your local Hive config and selected backend. It mounts `${HOME}/.config/hive`, `${HOME}/.claude`, and `${HOME}/.config/claude-code` read-only, then reads the registered `HIVE_HUB` and `HIVE_REGISTRATION_TOKEN` from `${HOME}/.config/hive/contributor.env` inside the container.

```bash
AGENT_BACKEND=claude just contribute-hive
AGENT_BACKEND=goose GOOSE_PROVIDER=anthropic GOOSE_MODEL=claude-sonnet-4-6 just contribute-hive
AGENT_BACKEND=litellm HIVE_LITELLM_ENDPOINT=https://litellm.example.com just contribute-hive
```

`AGENT_BACKEND` selects the CLI, `AGENT_MODEL` optionally pins the model, and `CONTRIBUTOR_MODE` defaults to `interactive` (tmux with a TTY). `CONTRIBUTOR_MODE=headless` is reserved for one-shot/no-TTY task delivery.

`just contribute-check <backend>` runs a read-only preflight before registration. It checks that the chosen CLI exists and that obvious auth prerequisites are present.

## Secrets

Store secret values outside `hive.yaml`. YAML should contain env var names or key-file paths, not keys. The dashboard and config save path rewrite YAML, so a literal secret in YAML would be persisted in plaintext.
