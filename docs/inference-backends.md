# Inference backends

Hive can route agents through OpenAI-compatible model gateways instead of a subscription CLI model. The supported gateway backend IDs in v2 HEAD are `vllm`, `llm-d`, `litellm`, and `watsonx`; this guide focuses on `vllm`, `llm-d`, and `litellm`.

## How routing works

The agent still launches Claude Code in bare mode. Hive writes Claude settings that point `ANTHROPIC_BASE_URL` at Hive's local translator, then the translator converts Anthropic Messages API calls to OpenAI-compatible requests and forwards them to the selected gateway. The backend name selects the upstream route; it is not a separate agent binary.

## Configure a gateway

Use the dashboard's **Governor Config → Model Gateways** UI, or YAML. A gateway needs a name, kind, endpoint, optional key reference, and optional default model. Agents can then set `backend:` to either the built-in kind (`vllm`, `llm-d`, `litellm`) or to a configured gateway name.

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

See also [`v2/deploy/inference/README.md`](../v2/deploy/inference/README.md) for the sample in-cluster vLLM-compatible deployment.
