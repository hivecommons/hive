# Monitoring a hive with Prometheus and Grafana

This page is for operators who want to scrape a hive's Prometheus `/metrics` endpoint, chart it in Grafana and alert on it. The endpoint is implemented in `pkg/dashboard/metrics_prometheus.go`.

## Turning metrics on

`/metrics` is off by default: the route is not registered, so scrapes return 404. Two settings turn it on; the base Deployment reads both from optional keys of the `hive-secrets` Secret (both listed in [env-vars.md](env-vars.md) and the [operator reference](operator-reference.md)):

1. `HIVE_METRICS_ENABLED=1` (`1`, `true`, `yes` or `on`) registers `GET /metrics` on the dashboard port (3002).
2. `HIVE_METRICS_TOKEN` holds the bearer token scrapers must present.

Put both in the `hive-secrets` Secret next to the hive's other secrets, for example:

   ```sh
   kubectl -n <namespace> patch secret hive-secrets --type merge \
     -p "{\"stringData\":{\"HIVE_METRICS_ENABLED\":\"1\",\"HIVE_METRICS_TOKEN\":\"$(openssl rand -hex 32)\"}}"
   ```

`src/deploy/k8s/deployment.yaml` maps those keys to environment variables (`secretKeyRef`, `optional: true`). Roll the hive pod afterwards (`kubectl -n <namespace> rollout restart deployment/hive`) so the environment is re-read. If you deploy from a custom manifest, add the same two `secretKeyRef` env entries yourself; files mounted at `/secrets` are not exported as environment variables.

### Why `/metrics` is fail-closed

The series include estimated cost, token and per-agent data. `/metrics` bypasses dashboard session auth, so the token is its only guard. Therefore:

| Situation | Response |
|---|---|
| Enabled, token configured, correct `Authorization: Bearer <token>` | 200 with the series |
| Enabled, token configured, missing or wrong bearer | 401 with `WWW-Authenticate` |
| Enabled, **no** `HIVE_METRICS_TOKEN` | 403 for every request, plus a startup warning in the hive log |
| Not enabled | 404 |

Enabling metrics without a token never serves the data unauthenticated.

## Adding the monitoring component to an overlay

<!-- Update this section when the monitoring component (ServiceMonitor, alerts, dashboard) lands in `src/deploy/kustomize`. -->

No monitoring component ships under `src/deploy/kustomize` yet; the ServiceMonitor, alert rules and Grafana dashboard are tracked in companion issues (alerts: [#15438](https://github.com/hivecommons/hive/issues/15438)). Until they land, use the plain `scrape_config` below, or write a `ServiceMonitor` targeting the base `hive` Service, port `dashboard` (3002), path `/metrics`, with the bearer token taken from the `hive-secrets` Secret key `HIVE_METRICS_TOKEN`.

Once the component exists you will add it to your overlay's `kustomization.yaml` `components:` list.

## Metric families

All series carry a `hive_id` label. Cost series are **estimates** (token counts × list price), not a bill.

| Family | Meaning |
|---|---|
| `hive_estimated_cost_usd{model,source}`, `hive_estimated_cost_usd_by_agent{agent}`, `hive_estimated_cost_usd_total` | Cumulative estimated spend per model, per agent and overall. |
| `hive_backend_coins_total{backend,label}` | Cumulative backend coin usage, present only when an operator coin conversion is configured. |
| `hive_model_input_tokens_total{model}`, `hive_model_output_tokens_total{model}` | Cumulative tokens per model. |
| `hive_prs_by_model_total{model,outcome}` | Attributed PR outcomes per model. |
| `hive_pr_rework_by_model{model,metric}` | All-time rework evidence for merged attributed PRs. |
| `hive_reviews_by_model_pair_total{author_model,review_model,verdict}` | Review verdicts by author/reviewer model pair. |
| `hive_review_outcome_prs{reviewed,outcome}`, `hive_review_outcome_median_hours_to_merge{reviewed}` | 30-day PR outcomes and median hours to merge, by reviewed cohort. |
| `hive_pr_followup_*` | PR follow-up routing (resumed, fallback, skipped, deferred, handoffs, pruned pointers). Appear only once follow-ups have run. |
| `hive_trajectory_*` | Trajectory review lane decisions: reviewed/skipped and the reasons. |
| `hive_stage_*` | Central stage engine: derivations, rejected transitions (`hive_stage_transition_anomaly_total`), writer writes and healed duplicate pills. |

The authoritative list is the comment block at the top of `pkg/dashboard/metrics_prometheus.go`; `curl` the endpoint to see what your hive exports.

## Alerts and runbooks

No alert rules ship in the repository yet; alert rules ship with the monitoring component (see [#15438](https://github.com/hivecommons/hive/issues/15438)). Each alert's `runbook_url` will point at a heading below, so these anchors are stable:

- `#alert-reference` — one subsection per alert, titled with the alert name, giving what it means and what to do when it fires.

### Alert reference

Conditions are specified in [#15438](https://github.com/hivecommons/hive/issues/15438); the rule files land with the monitoring component.

#### HiveMetricsDown

Prometheus cannot scrape the hive's `/metrics`. Check the pod is running, then `curl` the endpoint with the bearer token: 404 means `HIVE_METRICS_ENABLED` is not set, 401 a wrong token, 403 a missing `HIVE_METRICS_TOKEN` (see the table above).

#### HiveCostBurnHigh

Estimated spend (`hive_estimated_cost_usd_total`) is rising faster than the configured threshold. Compare `hive_estimated_cost_usd_by_agent` and `hive_estimated_cost_usd` by model to find the driver, and pause or reduce that agent if the burn is unexpected.

#### HiveStageTransitionAnomalies

The stage engine is rejecting transitions (`hive_stage_transition_anomaly_total` is increasing). Check the hive log for the rejected transitions and the work items involved; a sustained increase suggests a writer is emitting invalid stage changes.

## Scraping without Prometheus Operator

Use a plain `scrape_config` with `authorization`:

```yaml
scrape_configs:
  - job_name: hive
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/secrets/hive-metrics-token
    static_configs:
      - targets: ["hive.<namespace>.svc:3002"]
```

Mount the token as a file from the same value you stored in `hive-secrets`. Verify with:

```sh
curl -H "Authorization: Bearer $HIVE_METRICS_TOKEN" http://hive.<namespace>.svc:3002/metrics | head
```
