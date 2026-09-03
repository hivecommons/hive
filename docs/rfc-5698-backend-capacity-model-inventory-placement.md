# RFC #5698: Backend capacity, model inventory, pacing, and placement

Status: accepted design for v5 phase-1 delivery  
Refs: #5698, #5690, #5691

## Summary

Hive should treat backend selection as a placement problem, not as a static
agent setting. A healthy fleet needs to know which providers can answer now,
which models each provider can run now, which model rungs satisfy each agent's
competence floor, and which operator policy should be used when more than one
choice is possible. The design below keeps the existing v5 behaviour unless an
operator enables placement, but it makes the shared contracts explicit so the
current dashboard discovery, rotation probes, heartbeat status, and future
scheduler all consume one normalized source of truth.

The core invariant is that model and quota facts must not be hardcoded as the
primary source of truth. Hardcoded fallbacks are allowed only as labelled,
non-authoritative safety nets when a provider cannot enumerate models or when a
probe is temporarily unknown.

## Goals

- Publish one normalized, shared reading for backend capacity. Consumers must
  not probe providers independently because provider usage endpoints can be
  rate-limited and provider-through-provider probes can deadlock when the same
  backend is exhausted.
- Represent provider headroom as a list of limit windows, not as one scalar.
  Availability, pacing, and scoped model caps require different projections of
  the same measurement.
- Discover model inventory through backend-specific implementations and use
  authoritative inventories to validate new backend/model edits.
- Give operators an editable policy for tier floors, provider preference,
  exhaustion thresholds, pacing behaviour, and model-adoption review.
- Rotate only on confirmed exhaustion, invalid model, or policy violation. Do
  not churn a working agent merely because a preferred or cheaper provider is
  available.

## Non-goals for v5 phase 1

- Automatically adopting benchmark rankings into production policy.
- Replacing every CLI-specific fallback with a provider API on day one.
- Making placement mandatory. The v5 implementation remains opt-in under
  `governor.rotation` / future placement policy.
- Using placement as the only pacing actuator. Pacing also needs model-rung and
  cadence controls because a fleet may have exactly one usable provider.

## Current v5 baseline

v5 already contains useful pieces that this RFC standardizes around:

- `src/pkg/rotation` has provider probers, fail-open behaviour, thresholds, and
  `GET /api/providers/headroom` shaping.
- `config.RotationConfig` carries opt-in rotation settings, provider classes,
  backend lists, thresholds, high-volume subscription guards, and agent tiers.
- Dashboard model discovery exists for CLI and inference backends.
- `PUT /api/config/agent/{name}/models` is the atomic backend+model mutation
  route and is documented as the safe path for changing both fields together.
- Heartbeats already expose related signals such as `QuotaExhausted`,
  `ProviderLimitReason`, and `ProviderLimitRebuffs`.

These are implementation starts, not the final contract. In particular, Claude
usage must distinguish unscoped provider limits from scoped model caps, and
model validation must be shared rather than dashboard-only.

## Data model

### Capacity reading

A backend capacity reading contains metadata and zero or more limit windows:

```yaml
backend: claude
provider: anthropic
status: ok | exhausted | degraded | unknown | unauthenticated
observed_at: 2026-09-02T16:55:00Z
source: oauth-usage
limits:
  - kind: session
    percent_used: 37
    resets_at: 2026-09-02T19:30:00Z
    scope: {}
  - kind: weekly
    percent_used: 67
    resets_at: 2026-09-02T23:00:00Z
    scope: {}
  - kind: weekly_scoped
    percent_used: 100
    resets_at: 2026-09-02T23:00:00Z
    scope:
      model_class: fable
```

Rules:

1. Unscoped limits constrain provider availability.
2. Scoped limits constrain only the matching scope, such as a model class. They
   must be reported separately so placement can avoid that model without marking
   the provider exhausted.
3. A consumer asking "can this provider serve?" projects the reading over
   unscoped limits and the configured exhaustion thresholds.
4. A consumer asking "how fast may this provider be used?" evaluates each
   unscoped limit against its own reset time; it must not use a collapsed max.
5. Probe errors are fail-open for scheduling unless policy says otherwise, but
   the status is `unknown`/`degraded` and must be visible.

### Pacing history

Pacing requires persisted readings, not just the latest sample. The probe layer
stores enough recent readings to fit a rate over multiple samples and reports
`learning` until it has a minimum configured span. Window rollovers are explicit
reset events; a fit must never cross a reset boundary. If observed burn exceeds
allowed burn and no model/cadence actuator remains, the pacer reports
`saturated` rather than pretending no action is needed.

### Model inventory

Each backend implements a model inventory interface:

```go
type Inventory interface {
    ListModels(ctx context.Context, backend string) (InventoryReading, error)
}

type InventoryReading struct {
    Backend       string
    Authoritative bool
    ObservedAt    time.Time
    Models        []ModelInfo
    Source        string
}
```

Authoritative inventories may reject a newly submitted model ID when the ID is
absent. Non-authoritative fallbacks populate UI choices and diagnostics but do
not prove that an arbitrary ID is invalid. Current expected authority:

| Backend | Inventory source | Authoritative for rejection |
| --- | --- | --- |
| Claude | `GET /v1/models` with OAuth credentials | yes when the call succeeds |
| DeepSeek/OpenAI-compatible gateway | `GET /models` or `/v1/models` | yes when the call succeeds |
| agy | parsed `agy --print /models` prose | no until a structured source exists |
| Codex | no enumerable source; accepts strings | no |

`PUT /api/config/agent/{name}/models` remains the only safe mutation path. It
validates backend names today; the next phase validates model IDs whenever the
selected backend has a fresh authoritative inventory. Discovery failures should
not brick existing configs or prevent an operator from keeping a previously
working model when the inventory is unavailable.

## Tier and policy model

Policy is operator-editable and auditable. It contains:

- Per-agent, role, or ACMM-pack tier floors with provenance.
- Provider preference order. Preference is used only when a placement decision
  must be made; it is not a reason to churn a working agent.
- Per-provider thresholds for exhaustion and pacing deadbands.
- Model-class avoidance rules derived from scoped caps.
- Whether new benchmark rankings are auto-proposed or held for operator review.

Benchmark imports are pluggable and write proposed rankings separately from the
adopted policy. A failed refresh, missing API key, or surprising benchmark must
not silently rewrite live placements. Rankings should be based on agentic
benchmarks for agent+model+harness combinations; size or price alone is not an
acceptable proxy for competence.

## Placement algorithm

For each agent tick, placement evaluates in this order:

1. Load the agent's configured backend and model.
2. Check whether the configured backend is known, authenticated, and not
   exhausted by unscoped provider limits.
3. Check whether the configured model violates a scoped cap or authoritative
   inventory reading.
4. Check whether the configured model satisfies the agent's tier floor.
5. If all checks pass, leave the agent in place even if policy has a more
   preferred backend.
6. If a check fails, choose the first candidate backend/model pair that is
   usable, inventory-valid when authoritative, at or above the tier floor, not
   blocked by scoped caps, and permitted by high-volume/subscription policy.
7. Apply the pair with the atomic backend+model endpoint and record the reason.

The placement engine is opt-in and fail-open by default. It may decline to move
an agent when every candidate is unknown, unauthenticated, exhausted, below the
floor, or policy-blocked; that state is surfaced as a placement condition.

## Shared publication and API surfaces

There is exactly one producer per provider credential set. It publishes capacity
readings for the dashboard, governor, hub heartbeat, placement engine, and
pacer. The publication must include `observed_at`, freshness, source, and any
probe error category so consumers can distinguish exhausted, unauthenticated,
rate-limited, and unknown.

Recommended v5 surfaces:

- Extend `GET /api/providers/headroom` to return the normalized limit list while
  preserving existing scalar fields during a compatibility window.
- Add a dashboard/admin inventory endpoint backed by the shared inventory
  service rather than dashboard-local discovery helpers.
- Add agent conditions for provider exhaustion, scoped model cap, invalid model,
  inventory unknown, and placement saturated.
- Include normalized fleet headroom in hub/spoke heartbeat summaries so a hub can
  reason over multiple spokes without probing through them.

## Rollout plan

1. **Contract/documentation:** land this RFC and keep the already-documented
   atomic backend+model route as the supported mutation path.
2. **Inventory extraction:** move dashboard discovery into a reusable service and
   validate authoritative model IDs on new atomic edits.
3. **Capacity shape:** evolve `rotation.Headroom` to carry limit windows and
   scoped caps; fix Claude handling so `weekly_scoped` avoids only that model
   class.
4. **Conditions/surfaces:** map normalized readings to API, heartbeat, and
   dashboard status without changing scheduling defaults.
5. **Opt-in placement:** extend rotation from provider failover to backend+model
   placement under policy.
6. **Pacing:** persist readings, add rollover-aware rate fitting, and expose
   model-rung/cadence actuator status.
7. **Tier refresh:** ingest pluggable benchmark proposals and require operator
   adoption unless policy explicitly enables auto-adoption.

## Acceptance criteria for closing #5698

This RFC is the accepted design artifact for the issue. It captures the five
capabilities identified in the issue and comments, reconciles them with the v5
baseline, and defines bounded phase-1 scaffolding plus follow-on implementation
phases. Subsequent coding work should reference this RFC rather than reopening
#5698.
