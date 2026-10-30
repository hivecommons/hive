# Prometheus Monitoring Kustomize Component

This opt-in Kustomize component configures Prometheus Operator monitoring for
Hive.

It provides:
- A `ServiceMonitor` (`monitoring.coreos.com/v1`) targeting Hive's `/metrics`
  endpoint on the `dashboard` port (3002).
- A Deployment patch enabling metrics collection via `HIVE_METRICS_ENABLED=true`
  and wiring `HIVE_METRICS_TOKEN` from the dedicated `hive-metrics-token` Secret
  (marked optional so a missing token fails closed with 403 rather than crashing the pod).
- Bearer token authentication for the scraper via `authorization.credentials`
  referencing `HIVE_METRICS_TOKEN` in `hive-metrics-token`, so the Prometheus
  Operator can be granted read access to that Secret alone and never needs
  `hive-secrets` (GitHub token, App key).

## Prerequisites

- **Prometheus Operator**: The `ServiceMonitor` CustomResourceDefinition must be
  installed on the cluster (e.g. via `kube-prometheus-stack`). With
  `kube-prometheus-stack` default values Prometheus only selects ServiceMonitors
  labelled `release: <your-helm-release>`; add that label (e.g. via a `labels:`
  patch in your overlay) or set
  `prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false`.
- **Metrics Token**: Create a dedicated `hive-metrics-token` Secret in the
  `hive` namespace with a `HIVE_METRICS_TOKEN` key (commented template at the
  end of `src/deploy/k8s/secret.yaml`). The ServiceMonitor and the base Hive
  Deployment (`src/deploy/k8s/deployment.yaml`) both read the token from it,
  so scraper and server always agree, and `hive-secrets` (GitHub token, App
  key) never has to be readable by the scraper.

```bash
kubectl -n hive create secret generic hive-metrics-token \
  --from-literal=HIVE_METRICS_TOKEN="$(openssl rand -hex 32)"
```

**Migrating from `hive-secrets`**: earlier versions of this component and of
the base Deployment read `HIVE_METRICS_TOKEN` from `hive-secrets`. Copy the
existing value into the new Secret before upgrading, otherwise `/metrics`
answers 403 until you do:

```bash
kubectl -n hive create secret generic hive-metrics-token \
  --from-literal=HIVE_METRICS_TOKEN="$(kubectl -n hive get secret hive-secrets \
    -o jsonpath='{.data.HIVE_METRICS_TOKEN}' | base64 -d)"
```

Afterwards the `HIVE_METRICS_TOKEN` key can be removed from `hive-secrets`
(see [monitoring.md](../../../../docs/monitoring.md) for the exact command).

## Usage

The component patches the Hive Deployment, so it only applies to overlays that
include the `k8s/` base (`standalone`, `openshift`, `kind`). The policy-only
overlays (`openshift-netadmin`, `backup-exec-restriction`) fail with
`no matches for Id Deployment.v1.apps/hive.hive`.

Add the component to the overlay's `kustomization.yaml`:

```yaml
components:
  - ../../components/monitoring
```

Then build the overlay, for example `overlays/standalone`:

```bash
kubectl kustomize src/deploy/kustomize/overlays/standalone
```

## Security

Hive's `/metrics` endpoint exposes sensitive operational and financial metrics,
including estimated AI model costs and token usage totals. Access is guarded by
a mandatory bearer token (`HIVE_METRICS_TOKEN`, stored in the `hive-metrics-token` Secret). If metrics are enabled without
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
`hive-metrics-token` and that the `ServiceMonitor` credentials point at it. A wrong
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
`promtool check rules` (the script needs `promtool` and `yq` on PATH), and runs the unit tests in `tests/` that force each
alert to fire. CI runs it through `.github/workflows/prometheusrule-check.yml`.

To see each alert fire on a kind cluster: scale the Deployment to 0 for
`HiveMetricsDown`, patch the cost threshold to `> 0` for `HiveCostBurnHigh`,
and trigger a rejected transition for `HiveStageTransitionAnomalies`.

## Grafana dashboard

The component also generates a `hive-grafana-dashboard` ConfigMap from
`hive-metrics.json`, labelled `grafana_dashboard: "1"` so the
kube-prometheus-stack Grafana sidecar imports it. It targets Grafana 11 or
later and takes the Prometheus data source from a `datasource` variable, so no
UID is hardcoded. A `hive_id` variable filters by hive.

Rows and the series they read (all exported by `/metrics` today):

| Row | Series |
| --- | --- |
| Cost (estimated) | `hive_estimated_cost_usd_total`, `hive_estimated_cost_usd_by_agent`, `hive_estimated_cost_usd` |
| Tokens | `hive_model_input_tokens_total`, `hive_model_output_tokens_total` |
| PRs, reviews and rework | `hive_prs_by_model_total`, `hive_reviews_by_model_pair_total`, `hive_pr_rework_by_model` |
| PR follow-ups | `hive_pr_followup_resumed_total`, `_deferred_total`, `_handoffs_total`, `_fallback_total`, `_skipped_total` |
| Stage engine | `hive_stage_transition_anomaly_total`, `hive_stage_duplicate_pills_healed_total` |

The cost panels are estimates (token counts times list price), not billed
spend. The `hive_pr_followup_*` series appear only once the PR follow-up feature
has written its counter file, so those panels stay empty until then.

If your Grafana sidecar only watches its own namespace, set the sidecar's
`searchNamespace` to include `hive` (or `ALL`).

`src/scripts/check-prometheusrule.sh` also checks that the dashboard is valid
JSON and that every metric it queries is one `/metrics` exports.
