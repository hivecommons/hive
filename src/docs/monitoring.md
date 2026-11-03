# Monitoring a hive with Prometheus and Grafana

This page is for operators who want to scrape a hive's Prometheus `/metrics` endpoint, chart it in Grafana and alert on it. The endpoint is implemented in `pkg/dashboard/metrics_prometheus.go`.

## Turning metrics on

`/metrics` is off by default: the route is not registered, so scrapes return 404. Two settings turn it on (both listed in [env-vars.md](env-vars.md) and the [operator reference](operator-reference.md)); the base Deployment reads each from an optional Secret key:

1. `HIVE_METRICS_ENABLED=1` (`1`, `true`, `yes` or `on`) registers `GET /metrics` on the dashboard port (3002). Read from the `hive-secrets` Secret.
2. `HIVE_METRICS_TOKEN` holds the bearer token scrapers must present. Read from the **dedicated `hive-metrics-token` Secret**, not from `hive-secrets`, so the scraper (for example the Prometheus Operator) can be granted read access to that one token and never to the hive's GitHub credentials or App key.

Set the flag in `hive-secrets` and create the token Secret, for example:

   ```sh
   kubectl -n <namespace> patch secret hive-secrets --type merge \
     -p '{"stringData":{"HIVE_METRICS_ENABLED":"1"}}'
   kubectl -n <namespace> create secret generic hive-metrics-token \
     --from-literal=HIVE_METRICS_TOKEN="$(openssl rand -hex 32)"
   ```

`src/deploy/k8s/deployment.yaml` maps those keys to environment variables (`secretKeyRef`, `optional: true`, so a missing `hive-metrics-token` Secret makes `/metrics` fail closed with 403 rather than crash-looping the pod). A commented template of the Secret sits at the end of `src/deploy/k8s/secret.yaml`. Roll the hive pod afterwards (`kubectl -n <namespace> rollout restart deployment/hive`) so the environment is re-read. If you deploy from a custom manifest, add the same two `secretKeyRef` env entries yourself; files mounted at `/secrets` are not exported as environment variables.

**Migrating from `hive-secrets`**: before this change the Deployment read `HIVE_METRICS_TOKEN` from `hive-secrets`. Copy the existing value into the new Secret before rolling out the new manifest, otherwise `/metrics` answers 403 until you do; afterwards the key can be dropped from `hive-secrets`:

   ```sh
   kubectl -n <namespace> create secret generic hive-metrics-token \
     --from-literal=HIVE_METRICS_TOKEN="$(kubectl -n <namespace> get secret hive-secrets \
       -o jsonpath='{.data.HIVE_METRICS_TOKEN}' | base64 -d)"
   # optional, once the new Secret is in place:
   kubectl -n <namespace> patch secret hive-secrets --type json \
     -p '[{"op":"remove","path":"/data/HIVE_METRICS_TOKEN"}]'
   ```

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

The opt-in `monitoring` component under `src/deploy/kustomize/components/monitoring/` ships a `ServiceMonitor` for `/metrics`, a Deployment patch that sets `HIVE_METRICS_ENABLED=true`, and a `PrometheusRule` with the alerts below. Its `ServiceMonitor` takes the bearer token from the `hive-metrics-token` Secret key `HIVE_METRICS_TOKEN`, the same Secret the Deployment reads, so scraper and server always agree. Add it to the `components:` list of any overlay that includes the `k8s/` base:

```yaml
components:
  - ../../components/monitoring
```

Full prerequisites (Prometheus Operator CRDs, release label selectors) and the Secret setup are in the [component README](../deploy/kustomize/components/monitoring/README.md). Without the Prometheus Operator, use the plain `scrape_config` below.

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

The alert rules ship with the monitoring component (`src/deploy/kustomize/components/monitoring/prometheusrule.yaml`, [#15438](https://github.com/hivecommons/hive/issues/15438)). Each alert's `runbook_url` points at the matching section of the component README; the same alerts are summarised below, and these anchors are kept stable:

- `#alert-reference` — one subsection per alert, titled with the alert name, giving what it means and what to do when it fires.

### Alert reference

Conditions are defined in the component's `prometheusrule.yaml` ([#15438](https://github.com/hivecommons/hive/issues/15438)).

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

Mount the token as a file from the same value you stored in the `hive-metrics-token` Secret. Verify with:

```sh
curl -H "Authorization: Bearer $HIVE_METRICS_TOKEN" http://hive.<namespace>.svc:3002/metrics | head
```
