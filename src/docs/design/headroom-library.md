# Headroom library boundary

Issue [#8753](https://github.com/hivecommons/hive/issues/8753) proposes moving the provider headroom probers from `src/pkg/rotation` into a standalone Go library and CLI, `ccleft`, while keeping Hive adoption drop-in. Hive now has an opt-in adapter for `github.com/tuna-os/ccleft`; the in-tree probers remain the default while operators compare readings.

## Interface contract

`pkg/rotation` owns the Hive-facing reading types:

- `Headroom`: provider name, availability, remaining percentage, reset time, per-window limits, optional plan/paid-credit metadata, and a preserved probe error/cause.
- `LimitWindow`: the normalized window shape used by rotation, contributor readings, and `/api/providers/headroom`.
- `ProbeErrorCause`: stable diagnostic categories for failed measurements.

The prober boundary is `rotation.HeadroomSource`:

```go
type HeadroomSource interface {
    Provider() string
    Probe(context.Context) rotation.Headroom
}
```

A source must be read-only: it must not spend credits, mutate billing state, refresh tokens, or rewrite credentials. It must respect the caller's context and return a fail-open `Headroom` with `ProbeErr` and `ProbeErrCause` when a measurement is inconclusive. A source must only report `Available: false` when it positively measured exhaustion or a limit state. Unknown providers remain fail-open through `Manager.HeadroomFor`.

The dashboard boundary is `rotation.HeadroomReporter`, which exposes only `HeadroomResponse()`. `/api/providers/headroom` depends on that reporter instead of concrete prober implementations, so the API surface can stay stable while the implementation moves.

The in-tree Claude, Codex, Agy, DeepSeek, Copilot, and Kiro probers continue to implement `HeadroomSource`, and `rotation.Manager` consumes only that interface. Operators select the implementation with:

```yaml
rotation:
  headroom_source: builtin # default; set to ccleft to opt in
```

`builtin` preserves existing behavior. `ccleft` uses the external library for providers it covers and falls back to the in-tree prober for any configured Hive provider without a ccleft mapping.

## ccleft mapping

`ccleft` keeps the same conceptual shape, so the adapter translates at the boundary instead of changing rotation or dashboard consumers.

| Hive `pkg/rotation` | ccleft concept | Notes |
| --- | --- | --- |
| `HeadroomSource.Provider()` | `Source.Provider` / prober provider | Same stable provider key used by rotation config. |
| `HeadroomSource.Probe(ctx)` | `Probe(ctx, Source)` or `Client.Get(ctx, Source)` | Adapter returns Hive `Headroom`; `Client` can add single-flight, per-account cadence, backoff, and stale last-good behavior. |
| `Headroom` | `Reading` | Preserve provider, state, non-secret account key/message when Hive grows fields for them, and windows. |
| `LimitWindow` | `Window` | Map ccleft kind/unit/remaining into Hive kind, duration, percentages, reset, and scope. |
| `ProbeErrorCause` | ccleft state/message | `auth_required`, `unsupported`, `rate_limited`, and generic errors remain fail-open in Hive unless positively exhausted. |

The adapter maps Hive provider names to ccleft provider names:

| Hive provider | ccleft provider | Notes |
| --- | --- | --- |
| `anthropic` | `claude` | Reads Claude Code OAuth usage from the agent home. |
| `openai` | `codex` | Reads ChatGPT/Codex usage directly from ccleft's read-only endpoint path. |
| `google` | `agy` | Runs `agy --print /usage --output-format json` with `HOME` set to the shared CLI home. |
| `github` | `copilot` | Uses ccleft's Copilot quota reader; Hive's `monthly_allowance` is only used by the builtin prober. |
| `deepseek` | `deepseek` | Uses `DEEPSEEK_API_KEY` from the environment map. |
| `aws-kiro` | `kiro` | Uses `KIRO_API_KEY` from the environment map. |

`ccleft.StateOK` becomes a successful Hive `Headroom`; Hive still applies `rotation.threshold_pct` to the binding ccleft windows. `limited` and `exhausted` are positive no-headroom readings. `rate_limited`, `auth_required`, `unsupported`, and `error` become fail-open Hive probe errors with a mapped `ProbeErrorCause`; stale ccleft last-good readings keep their windows and carry the stale probe error so rotation does not act on a failed fresh measurement.

## License and NOTICE

Hive is Apache-2.0. The imported `ccleft` repository is also Apache-2.0 and its `NOTICE` credits Hive's in-tree probers as the origin. Before updating the dependency, maintainers should verify that:

1. the ccleft repository keeps Apache-2.0 licensing and NOTICE attribution,
2. any copied Hive prober code carries compatible headers/notice treatment, and
3. Hive's dependency metadata and release notes continue to mention the library.

## Migration steps

1. Keep the current in-tree probers behind `HeadroomSource` and keep dashboard consumers behind `HeadroomReporter`.
2. Land the in-tree prober bug fixes from #8718-#8722 and #8726 without changing the interface contract.
3. Add a ccleft adapter in `pkg/rotation` that implements `HeadroomSource` and translates ccleft readings to Hive `Headroom` (**done behind `rotation.headroom_source: ccleft`**).
4. Wire providers to the adapter behind existing rotation configuration, preserving the current response shape and fail-open semantics (**done; default remains `builtin`**).
5. Compare readings on reference hives for at least one release. Keep in-tree probers available as a fallback during the comparison window.
6. Remove the in-tree provider-specific probing code only after maintainers agree the external readings match and operational rollback is covered.

## Open repo-home decision

The RFC deliberately leaves governance open. Maintainers still need to decide whether ccleft should stay at `tuna-os/ccleft` or move to `hivecommons/ccleft`. The interface added here supports either home because Hive consumes a narrow adapter boundary rather than repository-specific APIs.
