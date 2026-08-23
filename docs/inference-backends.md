# Inference backends

Hive can route agents through OpenAI-compatible model gateways instead of a subscription CLI model. The supported gateway backend IDs are `vllm`, `llm-d`, `litellm`, `watsonx`, and named Model Gateways such as `openrouter`.

## How routing works

The agent still launches Claude Code in bare mode. Hive writes Claude settings that point `ANTHROPIC_BASE_URL` at Hive's local translator, then the translator converts Anthropic Messages API calls to OpenAI-compatible requests and forwards them to the selected gateway. The backend name selects the upstream route; it is not a separate agent binary.

## Configure a gateway

Use the dashboard's **Governor Config → Model Gateways** UI, or YAML. A gateway needs a name, kind, endpoint, optional key reference, and optional default model. Agents can then set `backend:` to either the built-in kind (`vllm`, `llm-d`, `litellm`, `watsonx`) or to a configured gateway name such as `openrouter`.

Each gateway also accepts an optional `key_name` — a human-chosen LABEL for the configured key ("Team inference key", "andy personal", …). It is safe-to-show metadata, not a secret: it records WHICH key a gateway is set to use so operators can tell keys apart without ever seeing the value. The dashboard's gateway row displays it as "Using key: `<name>`", or "(unnamed)" when no label is set. `key_name` mirrors the bob backend's `KeyName` field and is optional on every gateway kind (litellm / openrouter / watsonx / vllm); omitting it keeps existing gateways byte-identical in `hive.yaml`.

LiteLLM also has a dedicated config block:

```yaml
governor:
  litellm:
    endpoint: https://litellm.example.com
    api_key_env: HIVE_LITELLM_API_KEY
    api_key_file: /secrets/litellm_api_key
    default_model: gpt-4o
    ca_bundle: /secrets/litellm-ca.pem
    local_proxy: false
```

Endpoint environment overrides used by v2 include:

- `HIVE_VLLM_ENDPOINT` for `vllm`
- `HIVE_LLMD_ENDPOINT` for `llm-d`
- `HIVE_LITELLM_ENDPOINT` for LiteLLM
- `HIVE_LITELLM_API_KEY` or `api_key_file` for LiteLLM bearer auth
- `HIVE_LITELLM_MODELS` as a comma-separated fallback model list when discovery fails

Never put the key value itself in YAML.


## OpenRouter scan-to-fund gateway

OpenRouter is both a Model Gateway kind (`kind: openrouter`) and a guided funding flow. It routes through OpenRouter's OpenAI-compatible API at `https://openrouter.ai/api/v1`, so agents use it like any other gateway after a key is stored.

### Operator setup with Model Gateways

1. Open **Governor Config → Model Gateways**.
2. Add a gateway named `openrouter` with kind `openrouter`. The UI preset fills the endpoint as `https://openrouter.ai/api/v1`.
3. Store the key as a secret value in the UI, or point `api_key_env` / `api_key_file` at a key that already exists. Hive stores only the file path or env-var name in `hive.yaml`.
4. Pick a `default_model` from discovery or enter an OpenRouter model id manually. The curated fallback default is `deepseek/deepseek-chat`.
5. Assign an agent with `backend: openrouter` (the gateway name) and either leave `model:` empty to use the default or set an explicit OpenRouter model id.

Equivalent YAML looks like:

```yaml
governor:
  gateways:
    - name: openrouter
      kind: openrouter
      endpoint: https://openrouter.ai/api/v1
      api_key_file: /data/secrets/gateway_openrouter_api_key
      default_model: deepseek/deepseek-chat
      key_name: openrouter-prod-key  # optional: audit label shown in the dashboard, not a secret

agents:
  guide:
    backend: openrouter
    model: deepseek/deepseek-chat
```

### Scan-to-fund flow

The dashboard can start an OpenRouter OAuth PKCE flow from the OpenRouter funding card. A sponsor picks a default model, scans the QR code or opens the returned `openrouter.ai/auth` link, authorizes on OpenRouter, and returns to `/openrouter/callback`. Hive exchanges the code for a user-controlled OpenRouter API key and upserts the `openrouter` gateway.

For a spoke dashboard, the key is written to the per-gateway secret-file store on that hive's PVC and `hive.yaml` records only `api_key_file`. For a hub-funded hive, the hub queues a pending gateway named `openrouter` and delivers it over the existing TLS heartbeat response; heartbeat-only/firewalled spokes therefore receive funding without the hub POSTing into the cluster. The spoke confirms arrival by reporting configured gateway names, after which the hub stops offering the pending secret. The hub does not persist or display the key.

### Credit display and quotas

A spoke with an OpenRouter key proxies OpenRouter's `/api/v1/key` credit endpoint and displays limit, usage, and remaining credit without returning the key. The hub can only show whether a funded gateway is still pending delivery; after delivery, credit is read from the spoke because the hub no longer has the key.

## Model discovery

Hive probes `/v1/models` on OpenAI-compatible gateways. LiteLLM discovery includes bearer auth when a key is configured. If discovery fails, the UI falls back to static or configured model lists and marks entries as unverified; fallback data should not be treated as proof that the endpoint is healthy.

## Guided example: Watsonx via gateway

The watsonx path uses the same gateway machinery: configure the gateway endpoint and credentials, verify `/v1/models`, then assign an agent to that backend/gateway. Use it as the guided flow for any enterprise gateway:

1. Create the gateway in the Model Gateways UI.
2. Store the key in a secret file or env var reference.
3. Click model discovery and choose an entitlement-visible model.
4. Assign a low-risk agent and run one manual kick.
5. Check agent logs for route/model passthrough and upstream HTTP errors.

## Common failures

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `401` or repeated gateway auth errors | Missing/stale API key or wrong LiteLLM virtual key | Rotate the key, update the env/file reference, restart or reload the hive. |
| Model dropdown empty or unverified | `/v1/models` unreachable or blocked | Check endpoint URL, network policy, TLS CA bundle, and gateway logs. |
| Agent starts but every prompt fails | Endpoint does not implement OpenAI chat completions for the selected model | Select a chat-capable model or fix gateway routing. |
| Connection refused / timeout | Service name or port is wrong, or NetworkPolicy blocks it | From the Hive pod, curl the gateway health and `/v1/models` endpoints. |
| Model rejected despite discovery | The agent model differs from the gateway entitlement name | Use the exact model ID returned by `/v1/models`; Hive passes it through verbatim. |

See also [`src/deploy/inference/README.md`](../src/deploy/inference/README.md) for the sample in-cluster vLLM-compatible deployment.
