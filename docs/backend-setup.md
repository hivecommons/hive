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

## Contributor relay image

`v2/Dockerfile.contributor` builds the ClankeR image used by `just contribute-hive`. It installs Claude Code, Copilot, Codex, Bob, Goose, Pi, `gh`, Go, tmux, and the relay scripts. `v2/compose-contributor.yaml` runs that image with your local Hive config and selected backend:

```bash
AGENT_BACKEND=claude just contribute-hive
AGENT_BACKEND=goose GOOSE_PROVIDER=anthropic GOOSE_MODEL=claude-sonnet-4-6 just contribute-hive
AGENT_BACKEND=litellm HIVE_LITELLM_ENDPOINT=https://litellm.example.com just contribute-hive
```

`just contribute-check <backend>` runs a read-only preflight before registration. It checks that the chosen CLI exists and that obvious auth prerequisites are present.

## Secrets

Store secret values outside `hive.yaml`. YAML should contain env var names or key-file paths, not keys. The dashboard and config save path rewrite YAML, so a literal secret in YAML would be persisted in plaintext.
