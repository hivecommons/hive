# Evaluation: fronting the model backend with a generic AI gateway

**Status: evaluation, not a commitment.** This page documents how an operator can place a generic OpenAI-compatible AI gateway in front of Hive's model backend using configuration that exists today, and defines a spike that produces numbers. **LiteLLM remains the supported, bundled default.** Nothing here changes what Hive ships, any default, any manifest or any running behavior. Whether anything changes is decided after the measurements in [Spike plan](#spike-plan), not before. Tracking issue: [#18989](https://github.com/hivecommons/hive/issues/18989).

No measurements are reported on this page. The spike has not been run. Everything in [How model traffic flows today](#how-model-traffic-flows-today) was read from the code at the cited lines; everything in [Recipe](#recipe-a-generic-gateway-in-front-of-the-backend) is a procedure that has not yet been followed on a clean setup by a second person.

## Who this is for

Operators who already run an AI gateway for metering, quotas and provider translation and want Hive's agents to use it, and maintainers deciding whether the bundled LiteLLM proxy is the right default for spend control. For the existing gateway basics read [Inference backends](../../docs/inference-backends.md) first.

## How model traffic flows today

### Agents talk to a local translator, never to the gateway

For an agent on an inference backend (`vllm`, `llm-d`, `litellm`, `watsonx` or a named gateway), the manager launches the Claude CLI with `ANTHROPIC_BASE_URL=http://127.0.0.1:18444` and `ANTHROPIC_API_KEY=sk-hive-<agent>` (`src/crux/agent/manager_env.go:340-343`). The key is a placeholder: the suffix is the agent's public name, not a secret (`src/crux/proxy/github_proxy_inference.go:37`). The real upstream credential never enters the agent's environment.

```
agent (Claude CLI)
   | Anthropic Messages, x-api-key: sk-hive-<agent>
   v
inference translator 127.0.0.1:18444 (in the hive process)
   | identifies the caller by socket UID, looks up the agent's route,
   | translates Anthropic Messages to OpenAI chat completions,
   | attaches the real bearer key
   v
gateway or model backend  POST <endpoint>/v1/chat/completions
```

### The translator decides who the caller is, then picks the route

- The caller's identity is the socket owner's UID, not the `sk-hive-<agent>` header, because every agent knows every other agent's name (`src/crux/proxy/github_proxy_inference.go:41-73`, `translatorAgentName` at `src/crux/proxy/github_proxy_inference.go:74`).
- The handler looks up the agent's `InferenceRoute` (backend, endpoint, model, key, CA bundle, extra headers, key resolver) and answers `502 no inference route for agent` when none is installed (`src/crux/proxy/inference_route.go:20-46`, `src/crux/proxy/github_proxy_inference.go:354-360`).
- Only `POST /v1/messages` is translated and forwarded. Other CLI housekeeping is answered locally and never reaches the gateway (`src/crux/proxy/github_proxy_inference.go:372-380`).

### How a route is built from configuration

Routes are installed in `src/cmd/hive/main.go`:

1. A backend name that matches a configured `governor.gateways` entry (and is not one of the built-in names) routes to that gateway's `endpoint`, `default_model`, resolved key and `ca_bundle` (`src/cmd/hive/main.go:4390-4415`). The key resolver is attached there (`src/cmd/hive/inference_key_resolver.go:25-35`).
2. The built-in `litellm` backend resolves in this order: `governor.litellm.local_proxy` first, then an explicit `gateways:` entry named `litellm`, then the legacy `governor.litellm` block or `HIVE_LITELLM_ENDPOINT` (`src/crux/inference/inference.go:37-57`, `src/crux/config/litellm_route.go:67-115`).
3. `vllm` and `llm-d` use `HIVE_VLLM_ENDPOINT` and `HIVE_LLMD_ENDPOINT` and send no key (`src/cmd/hive/main.go:4350-4366`, `src/cmd/hive/main.go:4514-4520`).

The `GatewayConfig` fields available today are `name`, `kind`, `endpoint`, `api_key_env`, `api_key_file`, `default_model`, `ca_bundle`, `project_id`, `region`, `key_name` and `prices` (`src/crux/config/agent_config.go:606-660`). `kind` is descriptive at runtime: the endpoint is what routes, with `watsonx` the one exception (`src/crux/config/agent_config.go:610-616`). The recognised kinds are `openrouter`, `litellm`, `watsonx` and `custom` (`src/crux/config/agent_runtime.go:762-767`).

### The bundled LiteLLM, and what "local proxy" means

`governor.litellm.local_proxy: true` makes the hive run a pinned `litellm` binary on `127.0.0.1:18445` with `/data/litellm/config.yaml`, restarting it five seconds after any exit (`src/crux/inference/inference.go:135-186`, `src/crux/config/litellm_route.go:12-17`). It is only installed in the image for that purpose; the default flow forwards straight to a remote LiteLLM endpoint (`src/Dockerfile:934-944`, version pin at `src/Dockerfile:939`). Agents still talk to the translator, never to the bundled proxy, so mode enforcement and per-agent attribution stay in front of it (`src/crux/inference/inference.go:156-161`). With `local_proxy: true` the translator's upstream is the loopback LiteLLM, which in turn calls the real provider: that is the "bundled LiteLLM" arm of the comparison.

### What the translator sends upstream, and what it does not

The request to the gateway carries a JSON content type, then a bearer authorization header when the route has a key, then the route's extra headers (`src/crux/proxy/github_proxy_inference.go:409-410`, `src/crux/proxy/inference_route.go:251-263`). The extra-headers map is populated only for watsonx, where it carries the project id (`src/crux/inference/inference.go:114-133`). No header names the agent, the hive or the task. The request body asks for a terminal usage chunk on streams (`stream_options.include_usage`, `src/crux/proxy/anthropic_translate.go:523-534`).

Consequences that matter for a gateway:

- **One upstream key per route.** Every agent that names the same gateway presents the same bearer key, so the gateway cannot tell agents apart from the request.
- **Streaming usage depends on the gateway honouring `include_usage`.** If it does not emit a usage chunk, Hive records nothing for that call (`src/crux/proxy/host_seams.go:93-95`).
- **A named gateway gets the agentic preamble.** The preamble prepended to the system prompt is skipped only when the route's backend is exactly `litellm`; any other backend name, including a named gateway, gets `DefaultInferencePreamble` (`src/crux/proxy/inference_route.go:452-468`). No configuration key sets a custom preamble. The added text changes prompt tokens, so an A/B run must give both arms the same backend name, or the difference in token counts is partly the preamble and not the gateway.
- **LiteLLM-specific entitlement handling is keyed on the backend name `litellm`.** The learning of entitled models from a `403` is skipped for any other backend name (`src/crux/proxy/github_proxy_inference.go:238-247`).

### Errors, retries and recovery in the translator

- **Transport failure** (gateway down, refused, timeout from the OS) becomes `502` with `inference backend unreachable: ...` (`src/crux/proxy/github_proxy_inference.go:456-466`, `src/crux/proxy/inference_error_message.go:22-25`).
- **Non-2xx** becomes an Anthropic-shaped error with the upstream status and the text `inference backend returned <status>: <body, 200 bytes>` plus the redacted upstream URL and `Server` header (`src/crux/proxy/inference_error_message.go:9-20`, `src/crux/proxy/github_proxy_inference.go:477-491`). That literal prefix is parsed by the agent pane classifier and the dashboard alerts.
- **Timeouts and retry for 429 or 5xx are off by default, configurable.** The client has no overall timeout because streams can run for minutes (`inferenceHTTPClient` in `src/crux/proxy/inference_route.go`), and an upstream `429` or `5xx` is passed to the agent as it always was. Under `governor.inference_resilience` (or the `HIVE_INFERENCE_*` environment variables) an operator can opt in to a connect timeout, a response-header timeout, an idle-stream timeout (no bytes for N seconds, so a slow but active stream is never cut) and a bounded retry with exponential backoff, jitter and `Retry-After` for `429` and `5xx`, applied only while no response byte has reached the agent (`src/crux/proxy/inference_resilience.go`; see [Translator timeouts and bounded retry](agent-configuration.md#translator-timeouts-and-bounded-retry)). The only other retry is one resend after a `401` when the key resolver yields a different key (`retryInferenceOnAuthFailure` in `src/crux/proxy/inference_key_resolver.go`; see also [Key rotation without a restart](agent-configuration.md#key-rotation-without-a-restart)); it is unchanged. Whatever retrying an agent does on a `429` or `5xx` beyond that is the Claude CLI's own behaviour and has to be observed in the spike.
- **Spend refusals are told apart from rate limits by body text.** A `429` or `400` whose body contains one of a fixed list of phrases (`budget has been exceeded`, `exceeded max budget`, `insufficient_quota`, `exceeded your current quota`, `billing_hard_limit_reached`, `credit balance is too low` and a few more) latches the spend-limit signal and suppresses agent kicks; any other `429` stays an ordinary transient error (`src/crux/proxy/inference_budget.go:66-119`). A generic gateway whose quota error uses different wording will be treated as an ordinary rate limit, which is a spike item.
- **Recovery.** The next 2xx clears both the auth and spend latches (`src/crux/proxy/github_proxy_inference.go:493-496`). The latches are set from the upstream status before the translator picks between an HTTP error and an SSE `error` event, so a non-2xx on a streamed request latches them the same way (`src/crux/proxy/github_proxy_inference.go:477-484`).
- **Streams that die mid-response.** Headers and a 200 have already been sent, so the translator logs the translation error and ends the stream with an SSE `error` event instead of `message_stop` (`src/crux/proxy/github_proxy_inference.go:508` and `:741`). An idle-stream timeout that cuts such a stream ends it the same way, after one `inference upstream timeout` line. What the CLI then does is a spike item.
- **Streamed requests answer before the gateway does.** For a request the CLI asks to stream, the translator sends the 200 and `message_start` before it calls the gateway, writes an Anthropic `ping` event every 10 seconds of silence and forwards `reasoning_content` as a `thinking` block, so the CLI's 5-minute idle watchdogs do not cut a queued or reasoning request ([#19793](https://github.com/hivecommons/hive/issues/19793), `src/crux/proxy/anthropic_stream.go`). A transport failure or non-2xx on such a request therefore reaches the CLI as an SSE `error` event carrying the same message, with an Anthropic error type that follows the status, instead of the HTTP status above.

### Keys, files and rotation

A gateway key is resolved from `api_key_env` or `api_key_file`; the file must be under `/secrets` or `/data/secrets` or it is never read, and the value never goes in `hive.yaml` (see [Where a gateway key file may live](agent-configuration.md#where-a-gateway-key-file-may-live)). For `litellm` and named gateways the proxy rechecks the file every few seconds, so a rotated Secret reaches the next request without a restart (`src/crux/proxy/inference_key_resolver.go:189`). The watsonx kind has no resolver because it exchanges its key for a minted token (`src/cmd/hive/inference_key_resolver.go:25-28`).

### Seven-day token budget and GitHub writes

Hive records tokens per agent and model from each response's usage block (`src/crux/proxy/host_seams.go:93-103`). That record feeds Hive's own seven-day token budget and the cost card. The gateway has no say in either, and it has no role in what agents may write to GitHub, which stays with the policy proxy. A gateway adds a second, independent limit in front of the model; it does not replace either of Hive's.

## Recipe: a generic gateway in front of the backend

The goal is `agent -> translator -> gateway -> model backend` (or `-> gateway -> LiteLLM -> backend`), with the gateway holding the real provider credentials and enforcing budgets and rate limits. A "generic gateway" here means any service that meets these requirements, which this page does not tie to a product:

| Requirement | Why Hive needs it |
| --- | --- |
| `POST /v1/chat/completions`, OpenAI-compatible, streaming over SSE | The translator forwards to `<endpoint>/v1/chat/completions` (`src/crux/proxy/github_proxy_inference.go:403`). |
| Honours `stream_options.include_usage` and returns `usage` (including `prompt_tokens_details.cached_tokens` if it caches) | Streaming usage is otherwise lost; cached tokens are priced at the cache-read rate (`src/crux/proxy/host_seams.go:93-103`). |
| `GET /v1/models` with bearer auth, ideally entitlement-filtered per key | Model discovery probes it (`src/crux/proxy/inference_route.go:317-327`). |
| Per-key budget and rate limit, returned as `429` or `400` with the quota wording in the body | Hive tells spend limits from throttles by body text (`src/crux/proxy/inference_budget.go:77-92`). |
| Bearer key auth | The only auth Hive sends is `Authorization: Bearer` (`src/crux/proxy/inference_route.go:251-254`). |
| TLS with a CA you can hand to Hive as a PEM | `ca_bundle` is the only TLS knob, and verification is never disabled (`src/crux/proxy/inference_route.go:265-301`). |

### Step 1: run the gateway where the hive's pod can reach it

Run it beside the hive (same namespace or host) or as a shared service. Hive dials it from the hive process, so the network path and any NetworkPolicy must allow the hive pod to reach the gateway endpoint. From the hive pod, confirm `GET <endpoint>/v1/models` with a bearer key returns `200`.

### Step 2: create keys at the gateway

Create one upstream key per Hive spoke, each with a budget and a rate limit set at the gateway. If you want per-agent limits, see [Per-agent quotas](#per-agent-quotas-without-a-code-change) below: with today's configuration this means one key per agent group, not per request.

### Step 3: store each key as a file

Put each key in a Kubernetes Secret mounted under `/secrets`, or in `/data/secrets`. Never inline the value in YAML, a ConfigMap or an environment manifest checked into git. The file name is up to you; the path is what `api_key_file` references.

### Step 4: add the gateway to Hive

Use **Governor Config, Model Gateways**, or YAML:

```yaml
governor:
  gateways:
    - name: ai-gw                       # the name agents put in backend:
      kind: custom                      # descriptive only; the endpoint routes
      endpoint: https://gateway.example.com/v1
      api_key_file: /secrets/ai_gw_key  # allowed dirs: /secrets, /data/secrets
      default_model: <a model id returned by the gateway's /v1/models>
      ca_bundle: /secrets/ai-gw-ca.pem  # only if the gateway uses a private CA
      key_name: ai-gw-spoke-key         # optional label, not a secret
      prices:                           # optional, USD per 1M tokens
        input: 0.20
        cached_input: 0.02
        output: 0.60

agents:
  guide:
    backend: ai-gw
```

All keys above exist today (`src/crux/config/agent_config.go:606-660`). Use the model id exactly as the gateway's `/v1/models` returns it; Hive passes it through verbatim.

To front LiteLLM instead of the model backend directly, point the gateway at the LiteLLM endpoint as its upstream and keep Hive pointed at the gateway. To put the gateway in front of the bundled local proxy, note that `local_proxy: true` makes the translator forward to `127.0.0.1:18445` and wins over any gateway entry for the `litellm` backend (`src/crux/inference/inference.go:50-52`), so a gateway cannot sit in front of the bundled proxy with today's configuration: the loopback hop is fixed. A gateway can sit behind it only by being named as an upstream in `/data/litellm/config.yaml`, which is LiteLLM configuration, not Hive configuration.

### Step 5: confirm the four behaviours

1. **Model discovery.** The Model Gateways Test button should list the gateway's models. If discovery fails the UI falls back to static or configured lists and marks entries unverified, which is not proof the endpoint is healthy ([Model discovery](../../docs/inference-backends.md)).
2. **Price lookup.** The cost card prices a gateway model from, in order, the `prices:` block, the public catalog for the gateway kind that has one, `/model/info` for `litellm`, then the static table (see [Gateway spend and prices](../../docs/inference-backends.md#gateway-spend-and-prices)). For a `custom` gateway only the `prices:` block applies. A model with none shows as price unknown and is excluded from the total.
3. **Budget-refusal detection.** Exhaust a test key's budget at the gateway and send one prompt. Record whether the gateway's error body matches the phrases in `src/crux/proxy/inference_budget.go:77-92`, and whether the hive reports the spend-limit signal. If it does not match, the hive treats it as an ordinary rate limit and keeps launching runs against a gateway that refuses them.
4. **Key rotation.** Rotate the key file, send a prompt, and confirm no restart is needed ([Key rotation without a restart](agent-configuration.md#key-rotation-without-a-restart)).

### What the gateway does not change

- Hive's seven-day token budget and its cost card still come from Hive's own usage records.
- The policy proxy remains the only authority for GitHub writes. A gateway sees model traffic only.
- Agents still hold only the placeholder `sk-hive-<agent>` key. The gateway's key stays in the hive process.
- The default backend, the bundled LiteLLM and `local_proxy` are unchanged and remain the supported path.

### Per-agent quotas without a code change

Per-agent identity is not forwarded upstream today (see [What the translator sends upstream](#what-the-translator-sends-upstream-and-what-it-does-not)), so a gateway cannot meter or limit one agent among several sharing a key. With existing configuration, the closest approximation is one `governor.gateways` entry per agent or agent group, each pointing at the same endpoint with its own key file, and each agent's `backend:` naming its own entry. The gateway then limits per key, which is per group. Costs of this approach: N gateway entries and N Secrets to maintain, and every such backend name other than `litellm` gets the agentic preamble. This is a configuration workaround, not per-agent attribution, and the spike must say whether it is acceptable.

To forward an agent identity there is no existing setting. `ExtraHeaders` is set only for watsonx, and `GatewayConfig` has no field for static or per-agent headers (`src/crux/proxy/inference_route.go:35-39`, `src/crux/inference/inference.go:114-133`). A seam would need to add, at minimum:

1. a header (or the OpenAI `user` field) carrying the agent name set by the translator on each upstream request, taken from the UID-verified identity and never from the client-supplied `sk-hive-<agent>` header;
2. a per-gateway opt-in so the default request bytes do not change; and
3. a decision on whether the value is the agent name, a stable pseudonym, or a spoke id, since the name is visible to the gateway operator.

If the spike shows this is needed, it becomes its own issue; none of it is done here.

## Spike plan

Run every arm on the same hardware, models and traffic. Arms: **direct** (translator to the backend with no gateway), **bundled LiteLLM** (`local_proxy: true`), and **generic gateway** (translator to the gateway to the backend). Give all arms the same backend name where possible, or account for the preamble difference described above. Thresholds below are proposals expressed as named, adjustable values; per the issue they must be agreed and committed to this page before any measurement is taken, so they are not fitted to results. Changing a value after measuring requires a note in the results explaining why.

### Named thresholds

| Name | Proposed value | Meaning |
| --- | --- | --- |
| `LATENCY_P95_OVERHEAD_MS` | 50 | Added per-request overhead at p95 versus direct, non-streaming, at each concurrency level. |
| `LATENCY_TTFT_P95_OVERHEAD_MS` | 75 | Added time to first token at p95 versus direct, streaming. |
| `LATENCY_P99_OVERHEAD_MS` | 150 | Added overhead at p99, reported and gated. |
| `LATENCY_VS_BUNDLED_RATIO_MAX` | 1.0 | Gateway overhead may not exceed the bundled LiteLLM's overhead at p95. |
| `CONCURRENCY_LEVELS` | 1, 10, 50 | Concurrent agent sessions per run. |
| `RUN_REPEATS` | 5 | Repeats per arm and level; report the median of per-run percentiles and the spread. |
| `GATEWAY_DOWN_DETECT_SECONDS` | 30 | Time from gateway outage to a clear refusal in the agent pane and the dashboard. |
| `GATEWAY_RECOVERY_SECONDS` | 60 | Time from the gateway returning to the next successful agent request, with no restart. |
| `SLOW_GATEWAY_STALL_SECONDS` | 120 | A stalled upstream must surface a clear error within this time or be recorded as a miss (the translator's timeouts are off by default, so this is expected to fail until `response_header_timeout_s` or `idle_stream_timeout_s` is set). |
| `METERING_TOLERANCE_PCT` | 2.0 | Maximum difference of gateway-reported tokens versus Hive's recorded usage, per model, over the replayed workload. |
| `METERING_COST_TOLERANCE_PCT` | 3.0 | Maximum difference in cost per model versus the gateway's own cost totals. |
| `PER_AGENT_QUOTA_REQUIRED` | decided before measuring | Whether per-agent attribution is required or per-spoke metering is enough (open question in the issue). |
| `OPS_STEPS_MAX` | the bundled proxy's count | The recipe may take no more operator steps than enabling `local_proxy` and writing its config. |
| `OPS_IDLE_MEMORY_MB_MAX` | decided before measuring | Gateway idle memory, compared with the bundled proxy's. |
| `OPS_PIN_BUMPS_PER_YEAR_MAX` | decided before measuring | Security-driven version bumps, compared with the LiteLLM pin history. |

### 1. Latency overhead

Pass: all of `LATENCY_P95_OVERHEAD_MS`, `LATENCY_TTFT_P95_OVERHEAD_MS`, `LATENCY_P99_OVERHEAD_MS` and `LATENCY_VS_BUNDLED_RATIO_MAX` hold at every level in `CONCURRENCY_LEVELS`, for both streaming and non-streaming.

Procedure:

1. Use a deterministic backend for the overhead runs: a local stub that returns a fixed completion with a fixed delay, so backend variance does not hide gateway overhead. Repeat once against the real backend for realism.
2. Drive load with an open-loop HTTP load generator that records per-request latency histograms and, for streams, time to first byte and time to last byte (for example `k6`, `vegeta` or `hey` for non-streaming, and a small script using `curl -w '%{time_starttransfer}'` or a streaming-aware k6 script for TTFT). Pick one and commit the script to the results.
3. Send the same OpenAI chat-completions payload to each arm's endpoint. Measure at the translator's upstream edge, not through the agent, so agent think time is excluded. Record the translator's own log lines for the same window.
4. Warm up for a fixed period, then run for a fixed duration at each concurrency level, `RUN_REPEATS` times, arms interleaved to cancel drift.
5. Report p50, p95 and p99 per arm and level, and the overhead as arm minus direct.

### 2. Failure behavior

Pass: for each case the agent sees a clear refusal that carries the `inference backend` prefix (or a documented reason it cannot), the hive recovers without a restart, and the numbers meet `GATEWAY_DOWN_DETECT_SECONDS`, `GATEWAY_RECOVERY_SECONDS` and `SLOW_GATEWAY_STALL_SECONDS`.

Cases, each against the bundled LiteLLM and the generic gateway:

| Case | What to record |
| --- | --- |
| Gateway down (process stopped, then port closed) | Message in the pane and dashboard; time to detect; time to recover after start. |
| Gateway slow (added delay before first byte, and stalled mid-stream) | Whether any timeout fires, who fires it (translator, CLI), and what the agent shows. |
| `429` throttle with `Retry-After` | Whether the translator or the CLI retries, how often, and with what spacing. |
| Out of budget (the gateway's own quota error) | Whether the body matches `inference_budget.go` and the spend-limit signal latches, and that it clears when budget returns. |
| `500`, `502`, `503` | Message surfaced, retry behavior, latch behavior. |
| Malformed stream (truncated SSE, missing `[DONE]`, bad JSON chunk) | Whether the translator emits a terminal event, whether the CLI hangs or errors, and whether partial usage is recorded. |
| Key rotation mid-run | One `401` then success, with no restart, and no auth latch left set. |

### 3. Metering accuracy

Pass: for each model over the replayed workload, the gateway's token totals are within `METERING_TOLERANCE_PCT` of Hive's recorded usage, and its cost totals are within `METERING_COST_TOLERANCE_PCT` of Hive's cost card (when prices are configured).

Procedure: replay one fixed workload (a scrubbed capture from a real hive, or a synthetic one that includes long streams, tool calls and cached prompts; the choice is an open question in the issue and must be recorded) through each arm. Collect three totals per model and per agent: the gateway's own, the bundled LiteLLM's own, and Hive's recorded usage. Report the percentage difference per source, separately for streaming and non-streaming, and separately for cached tokens.

### 4. Per-agent quota feasibility

Pass criterion is categorical: record one of **achievable with no code change** (the one-entry-per-agent workaround, with its operating cost), **achievable with a code change** (the identity seam above, with its blast radius), or **not achievable**. State which result satisfies `PER_AGENT_QUOTA_REQUIRED`. Verify, rather than assume, the finding that no per-agent identity reaches the gateway: capture one upstream request per arm and list every header and body field that differs between two agents.

### 5. Credential handling and blast radius

Pass: no key value appears in `hive.yaml`, a ConfigMap, logs, the dashboard API or an agent's environment; the key is readable only from an allowed secrets dir; and a compromised agent cannot obtain the gateway key.

Check: search the hive pod's environment, config and logs for the key's SHA-256 or a distinctive substring (compare hashes, never print the value). Record what one leaked gateway key can do (its budget, models and rate limit), how many spokes share it, how fast it can be rotated, and whether rotation needs a restart. Compare with the bundled proxy, whose provider keys sit in `/data/litellm/config.yaml` on the PVC (`src/crux/inference/inference.go:143-144`).

### 6. Operations cost

Pass: `OPS_STEPS_MAX`, `OPS_IDLE_MEMORY_MB_MAX` and `OPS_PIN_BUMPS_PER_YEAR_MAX` hold. Record: components to run and their failure domains (the bundled proxy shares the hive pod, a separate gateway does not), idle and loaded memory and CPU, the upgrade and pinning burden (the pinned LiteLLM version at `src/Dockerfile:939` and its security-driven bumps such as [#6473](https://github.com/hivecommons/hive/issues/6473)), and the number of steps in the recipe as actually followed.

### Results and outcomes

Results go on the tracking issue as a table with the harness and raw numbers, so the run can be repeated. Each miss against a named threshold needs a written reason. The outcome is exactly one of: document only, document plus a small seam issue (for example forwarding an agent identity), or a follow-up RFC. No change to bundled components is made under this evaluation.

### Results template

Copy this onto the tracking issue and fill it in. Fix the threshold values (the table above) and the open-question choices before the first measurement, and record who agreed them.

Run details: date, runner (not the recipe's author for the clean-setup run), Hive version, gateway name and version, backend and models, load generator and script location, workload used for metering.

| Spike | Threshold (named) | Direct or bundled | Generic gateway | Pass or miss | Reason for a miss |
| --- | --- | --- | --- | --- | --- |
| 1. Latency overhead (p95, TTFT p95, p99, ratio) | `LATENCY_*` | | | | |
| 2. Failure behavior (per case) | `GATEWAY_*`, `SLOW_GATEWAY_STALL_SECONDS` | | | | |
| 3. Metering accuracy (tokens, cost) | `METERING_*` | | | | |
| 4. Per-agent quota feasibility | `PER_AGENT_QUOTA_REQUIRED` | | | | |
| 5. Credential handling | none (categorical) | | | | |
| 6. Operations cost | `OPS_*` | | | | |

Before closing the tracking issue, check that: the recipe was run once on a clean setup by someone other than the author, with notes; raw numbers and the harness are linked; each miss has a reason; and one outcome is chosen.

## What the current configuration cannot express

- **Agent identity upstream.** No setting forwards the agent, hive or task to the gateway. Needs the seam described in [Per-agent quotas](#per-agent-quotas-without-a-code-change).
- **Static or custom headers on a gateway.** `GatewayConfig` has no headers field; extra headers exist only for watsonx. A gateway that needs a routing or tenant header cannot be used.
- **A gateway in front of the bundled proxy.** `local_proxy: true` fixes the loopback hop (see Step 4).
- **A custom preamble or opting a named gateway out of it.** Only the backend name `litellm` skips the agentic preamble.
- **Timeouts and 429/5xx retries in the translator.** There is no setting for either.
- **Custom spend-refusal phrases.** The recognised wording is a fixed list in code.

## Related

- [Inference backends](../../docs/inference-backends.md): gateway setup, discovery, prices and troubleshooting.
- [Agent configuration](agent-configuration.md#where-a-gateway-key-file-may-live): key file rules and rotation.
- [Backend support tiers](backend-support-tiers.md#gateway-path-tier): what a gateway-path backend must prove.
- [Architecture](architecture.md): the inference translator in the process model.
