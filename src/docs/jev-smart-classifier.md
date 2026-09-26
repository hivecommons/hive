# Jev smart classifier

Hive's default classifier is deterministic: issue titles and labels are matched
against configured lane and tier keywords, and the scheduler uses that result to
route work. The optional **Jev smart classifier** adds TypeSafe AI's Jev
typed-decision model as an advisory measurement backend for the same decisions:

- **Lane** — which work lane should receive the issue.
- **Tier** — the complexity tier that maps to the model family Hive should use:
  Simple → haiku, Standard → sonnet, Complex → opus.
- **Triage** — the run triage verdict used by planning and scheduler admission.

Jev asks all enabled decisions in **one batched request per issue**, caches the
answer by repository, issue number, and `updated_at`, and records whether Jev
agrees with the deterministic keyword/label answer. Jev never changes routing,
tier/model selection, or triage; keywords and labels remain authoritative. With
the default `backend: keywords`, Hive makes **zero Jev network calls**.

## When to use it

Use Jev when keyword routing is too coarse for your issue queue and you want a
typed model to measure lane, complexity, and triage disagreements from the issue
state. The dashboard groups repeated disagreement title tokens into proposed
deterministic rule edits such as adding a token to `classifier.simple_keywords`,
`classifier.complex_signals`, or an agent's `lane_keywords`. Operators must
approve each suggestion; Hive never applies a model-generated routing change on
its own.

## Prerequisites

Choose one key source. The key value is never returned by the dashboard or API.

1. **OpenRouter connected in the dashboard** — open **Settings → Model
   Gateways**, then click **⚡ Fund this hive with OpenRouter**. The PKCE flow
   stores a scoped key as the `openrouter` gateway. The Smart classifier panel
   reads only key presence (`openrouter_connected`), never the key.
2. **Environment key** — set `JEV_API_KEY` on the spoke, or set
   `classifier.jev.api_key_env` to the name of another environment variable that
   holds the key.

Provider choices:

- `provider: openrouter` (default) uses model `typesafe/jev-1.13` and falls back
  from `api_key_env` to the connected `openrouter` gateway key.
- `provider: typesafe` uses TypeSafe directly. Configure the provider endpoint
  and key env var explicitly if you run this path.

Hosted hives should prefer the dashboard flow: connect OpenRouter from **Model
Gateways**, then enable Jev from **Smart classifier**. Self-hosted hives can use
the same dashboard flow or inject `JEV_API_KEY` through their deployment
environment/Secret.

## Enable from the dashboard

Open **Settings → Smart classifier** on the spoke dashboard.

1. Confirm the prerequisite checklist shows either **OpenRouter connected with an
   API key** or **JEV_API_KEY environment key** as present.
2. If OpenRouter is missing, click **Connect OpenRouter**; this reuses the same
   PKCE/QR flow as **Settings → Model Gateways**.
3. Set **Backend** to **Jev**. The Jev option is disabled until a key source is
   present.
4. Keep `min confidence` at `0.8` initially unless you have a reason to be more
   conservative.
5. Keep all decisions (`lane`, `tier`, `triage`) checked for the normal batched
   request, or uncheck decisions you want keywords to own.
6. Click **Save**. Only roles allowed to edit config can save these controls.
7. Review **Suggested deterministic rule updates** and click **Apply** only for
   rule edits you want to add to the saved config.

The same panel shows live advisory stats, recent disagreements, suggestions, and
estimated spend from `GET /api/classifier/stats`.

## Enable from YAML

Add or edit the top-level `classifier:` block in `hive.yaml`:

```yaml
classifier:
  backend: jev           # keywords (default) | jev
  jev:
    provider: openrouter # openrouter (default) | typesafe
    model: typesafe/jev-1.13
    endpoint: ""         # default depends on provider
    api_key_env: JEV_API_KEY
    min_confidence: 0.8  # 0.0-1.0, default 0.8
    timeout: 2s          # default 2s; scheduler falls back after this
    decisions: [lane, tier, triage]
  simple_keywords: [typo, i18n, rename]
  complex_signals: ["race condition", performance, "api change"]
```

Validation rejects unknown backends, providers, decisions, confidence outside
`0..1`, and non-positive timeouts. `classifier.mode` is obsolete; legacy
`mode: shadow` is tolerated, but `enforce` is rejected because Jev is advisory
only. Empty keyword lists keep the built-in defaults, so you can enable Jev
without redefining existing keyword behavior.

There is no general `hive config set` subcommand in this release line. Use the
dashboard Settings save path or edit the winning config layer described in
[Config layering](config-layering.md). If a future CLI config editor is added,
the equivalent keys are `classifier.backend`, `classifier.jev.min_confidence`,
and `classifier.jev.decisions`.

## Cost and caching

Jev is billed on input tokens only. The expected cost is roughly
`$0.00002` per issue for the batched lane/tier/triage request, depending on
issue size and provider pricing. Hive records estimated input tokens and spend
from Jev usage data and shows them in **Settings → Smart classifier** and
`GET /api/classifier/stats`.

The cache key is repository, issue number, and `updated_at`. Repeated scheduler
sweeps of the same unchanged issue do not re-bill; editing the issue invalidates
the cache because the state may have changed.

## Observability

`GET /api/classifier/stats` returns:

- `decisions.<lane|tier|triage>.agree`
- `decisions.<lane|tier|triage>.disagree`
- `decisions.<lane|tier|triage>.fallback`
- `estimated_input_tokens`
- `estimated_spend_usd`
- `disagreements.<decision>[]` with repo, issue number, title, keyword answer,
  Jev answer, and confidence
- `rule_suggestions[]` with the deterministic config target, keyword, support,
  and examples
- `openrouter_connected`
- `key_source` (`openrouter`, `env`, or `none`)

Classification JSON keeps optional `source` and `confidence` fields for
compatibility, but Jev advisory mode returns keyword decisions as the
authoritative classifier source.

## Rollout guidance

1. Enable `backend: jev` for several representative governor sweeps.
2. Inspect agreement counters. A high agreement rate means Jev is matching your
   current routing; targeted disagreements are useful when they identify issues
   keywords could not infer.
3. Inspect `fallback`. Frequent fallback usually means missing keys, timeouts,
   low confidence, or a disabled decision.
4. Apply only the deterministic keyword suggestions operators agree with, then
   keep measuring.
5. Roll back instantly by setting `classifier.backend: keywords` in the
   dashboard or YAML. In keyword mode there are no Jev network calls.

## Troubleshooting

| Symptom | What to check |
|---|---|
| Jev option disabled in Settings | Connect OpenRouter or set `JEV_API_KEY` / `classifier.jev.api_key_env`. |
| `key_source: none` | The dashboard cannot see an env key and `openrouter` has no resolved gateway key. |
| High fallback count | Check missing key, provider errors, timeout, low confidence, and whether the decision is enabled. |
| Scheduler feels slow | Keep `classifier.jev.timeout` low; timeout fallback is bounded and defaults to `2s`. |
| No spend changes | Backend may still be `keywords`, cache may be serving unchanged issues, or no classified issues have changed. |
| Need emergency rollback | Set `classifier.backend: keywords` and save. |

Related docs: [Planning intelligence](planning-intelligence.md), [Hosted Hive Hub
onboarding](hosted-hub.md#configure-model-gateways-and-keys), [Dashboard API
reference](api-reference.md), and [Environment variable reference](env-vars.md).
