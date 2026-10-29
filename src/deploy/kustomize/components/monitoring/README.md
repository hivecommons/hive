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
