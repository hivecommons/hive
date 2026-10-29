# Prometheus Monitoring Kustomize Component

This opt-in Kustomize component configures Prometheus Operator monitoring for
Hive.

It provides:
- A `ServiceMonitor` (`monitoring.coreos.com/v1`) targeting Hive's `/metrics`
  endpoint on the `dashboard` port (3002).
- A Deployment patch enabling metrics collection via `HIVE_METRICS_ENABLED=true`
  and wiring `HIVE_METRICS_TOKEN` from the `hive-secrets` Secret (marked
  optional so a missing token fails closed with 403 rather than crashing the pod).
- Bearer token authentication for the scraper via `authorization.credentials`
  referencing `HIVE_METRICS_TOKEN` in `hive-secrets`.

## Prerequisites

- **Prometheus Operator**: The `ServiceMonitor` CustomResourceDefinition must be
  installed on the cluster (e.g. via `kube-prometheus-stack`).
- **Metrics Token**: Add `HIVE_METRICS_TOKEN` to your `hive-secrets` Secret.

```bash
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_METRICS_TOKEN="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

## Usage

Add the component to your overlay's `kustomization.yaml`:

```yaml
components:
  - ../../components/monitoring
```

For example, when building `src/deploy/kustomize/overlays/standalone`:

```bash
kubectl kustomize src/deploy/kustomize/overlays/standalone
```

## Security

Hive's `/metrics` endpoint exposes sensitive operational and financial metrics,
including estimated AI model costs and token usage totals. Access is guarded by
a mandatory bearer token (`HIVE_METRICS_TOKEN`). If metrics are enabled without
a token, Hive fails closed and returns HTTP 403 Forbidden to any scraper.

## Alerts

The component ships a `PrometheusRule` (`prometheusrule.yaml`) with three alerts
that use only series Hive exports today. Each carries `summary`, `description`
and a `runbook_url` pointing at the matching section below.

| Alert | Fires when | Severity |
| --- | --- | --- |
| `HiveMetricsDown` | `up{job="hive"} == 0` for 10m | critical |
| `HiveCostBurnHigh` | `increase(hive_estimated_cost_usd_total[1h])` is above the threshold | warning |
| `HiveStageTransitionAnomalies` | `increase(hive_stage_transition_anomaly_total[30m]) > 0` | warning |

`HiveMetricsDown` assumes the scrape `job` label is `hive`. The Prometheus
Operator sets `job` from the Service name, which is `hive` in this repository's
manifests.

### HiveMetricsDown

Prometheus cannot scrape `/metrics`. Check that the pod is running
(`kubectl -n hive get pods`), then that `HIVE_METRICS_TOKEN` is set in
`hive-secrets` and that the `ServiceMonitor` credentials point at it. A wrong
or missing token makes Hive answer 403, which also sets `up` to 0.

### HiveCostBurnHigh

Estimated LLM spend (token counts times list price) rose by more than the
threshold in one hour. The default is 50 USD per hour, high enough to stay quiet
on a normal hive. Check the dashboard Cost card or `GET /api/cost` for the model
or agent driving it, and pause that agent if the spend is unexpected.

Set your own threshold with a Kustomize patch in your overlay:

```yaml
patches:
  - target:
      group: monitoring.coreos.com
      kind: PrometheusRule
      name: hive
    patch: |-
      - op: replace
        path: /spec/groups/0/rules/1/expr
        value: increase(hive_estimated_cost_usd_total[1h]) > 10
```

### HiveStageTransitionAnomalies

Hive's stage engine rejected one or more label transitions in the last 30
minutes. The `kind`, `from` and `to` labels name the rejected transition. Look
for the matching `stage transition` warnings in the hive logs and for items
stuck in an unexpected stage.

### Validating the rules

`promtool` reads plain rule files, not the `PrometheusRule` resource.
`src/scripts/check-prometheusrule.sh` extracts `spec.groups`, runs
`promtool check rules`, and runs the unit tests in `tests/` that force each
alert to fire. CI runs it through `.github/workflows/prometheusrule-check.yml`.

To see each alert fire on a kind cluster: scale the Deployment to 0 for
`HiveMetricsDown`, patch the cost threshold to `> 0` for `HiveCostBurnHigh`,
and trigger a rejected transition for `HiveStageTransitionAnomalies`.
