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
  installed on the cluster (e.g. via `kube-prometheus-stack`). With
  `kube-prometheus-stack` default values Prometheus only selects ServiceMonitors
  labelled `release: <your-helm-release>`; add that label (e.g. via a `labels:`
  patch in your overlay) or set
  `prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false`.
- **Metrics Token**: Add `HIVE_METRICS_TOKEN` to your existing `hive-secrets`
  Secret (template: `src/deploy/k8s/secret.yaml`). Do **not** recreate the
  Secret: `kubectl apply` of a single-key Secret removes the other keys, which
  also hold your GitHub and dashboard credentials.

```bash
kubectl -n hive patch secret hive-secrets --type merge \
  -p "{\"stringData\":{\"HIVE_METRICS_TOKEN\":\"$(openssl rand -hex 32)\"}}"
```

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
a mandatory bearer token (`HIVE_METRICS_TOKEN`). If metrics are enabled without
a token, Hive fails closed and returns HTTP 403 Forbidden to any scraper.
