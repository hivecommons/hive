# legacy-redirect (#6430)

Replaces the ingress-nginx `permanent-redirect` annotation on the `hive-hub/hive-hub`
Ingress, which drops the request path and query string on every redirect from
`hive.kubestellar.io` to `hive.hivecommons.dev`. That data loss is the highest-value
item in [#6430](https://github.com/hivecommons/hive/issues/6430): every legacy
inbound link (campaign `utm_*` tags, deep links to `/learn`, `/get-started`,
`/dashboard`, `/api/saas/whoami`, ...) lands on the bare homepage in GA4 instead of
its real entry page, destroying landing-page and campaign-attribution data.

## Why not just fix the annotation?

The obvious fix is `nginx.ingress.kubernetes.io/permanent-redirect:
https://hive.hivecommons.dev$request_uri`. It does not work on this cluster:

- The ingress-nginx v1.12.2 admission webhook's URL validator rejects any
  `permanent-redirect` value containing `$` with "contains invalid value" —
  confirmed by hand against the live `hive-hub` Ingress.
- Snippet annotations (`nginx.ingress.kubernetes.io/configuration-snippet`,
  `server-snippet`), which could work around that with raw nginx config, are
  intentionally **disabled** on this controller (the `ingress-nginx-controller`
  ConfigMap has no `allow-snippet-annotations` / `enable-annotations`
  overrides for them) and we are not enabling them for one redirect.

So the redirect has to happen in something that isn't the annotation. This
stages a tiny nginx backend — the same pattern already running in this cluster
as `ingress-nginx/hive-error-pages` (an `nginx:1-alpine` Pod driven by a
ConfigMap-mounted `conf.d`, with a `/healthz` location and readiness/liveness
probes) — that does the redirect itself and preserves `$request_uri`.

## Files

- `00-legacy-redirect-configmap.yaml` — the nginx config. One `server` block per
  legacy host:
  - `hive.kubestellar.io` → `301 https://hive.hivecommons.dev$request_uri` (live).
  - `dibs.kubestellar.io` → `308 https://dibs.hivecommons.dev$request_uri`
    (staged for [#5925](https://github.com/hivecommons/hive/issues/5925); inert
    until something in the `dibs` cutover points traffic at this Service — see
    `src/deploy/dibs-domain-cutover/README.md`).
  - A `default_server` block answers `/healthz` with `200` (this is what
    kubelet's probes hit, since they send no `Host` header) and `404` on
    anything else, so an unrecognized `Host` does not silently redirect.
- `01-legacy-redirect-deployment.yaml` — a 2-replica `Deployment` (image
  `nginx:1-alpine`, the config mounted read-only at `/etc/nginx/conf.d`,
  readiness/liveness probes on `/healthz`, the same resource requests/limits as
  `hive-error-pages`) and a `Service` named `legacy-redirect` in namespace
  `hive-hub`, port 80 → container port 8080.

## Applying

```bash
kubectl --context hive-oke apply -f src/deploy/legacy-redirect/
kubectl --context hive-oke -n hive-hub rollout status deploy/legacy-redirect
```

### In-cluster verification (before repointing the live Ingress)

```bash
kubectl --context hive-oke run curl-legacy-redirect --rm -i --restart=Never \
  --image=curlimages/curl:8 -- \
  curl -sI -H 'Host: hive.kubestellar.io' \
  'http://legacy-redirect.hive-hub.svc.cluster.local/learn?utm_source=x'
# expect: HTTP/1.1 301, location: https://hive.hivecommons.dev/learn?utm_source=x
```

### Repointing the live Ingress

The `hive-hub/hive-hub` Ingress currently redirects via the (path/query-dropping)
`permanent-redirect` annotation. Switch it to route to this Service instead,
keeping every TLS/cert-manager annotation untouched (the `hive-hub-tls`
certificate must keep renewing):

```bash
kubectl --context hive-oke annotate ingress -n hive-hub hive-hub \
  nginx.ingress.kubernetes.io/permanent-redirect-
kubectl --context hive-oke patch ingress -n hive-hub hive-hub --type=json -p '[
  {"op": "replace", "path": "/spec/rules/0/http/paths/0/backend/service/name", "value": "legacy-redirect"},
  {"op": "replace", "path": "/spec/rules/0/http/paths/0/backend/service/port/number", "value": 80}
]'
```

### External verification

```bash
curl -sSI 'https://hive.kubestellar.io/learn?utm_source=newsletter' | grep -i '^location'
# expect: location: https://hive.hivecommons.dev/learn?utm_source=newsletter
curl -sSI 'https://hive.kubestellar.io/get-started' | grep -i '^location'
curl -sSI 'https://hive.kubestellar.io/' | grep -i '^location'
curl -sSI 'https://hive.kubestellar.io/api/saas/whoami' | grep -i '^location'
curl -sSI 'https://hive.hivecommons.dev/' | head -1   # unaffected, still 200
```

### Rollback

If anything breaks, restore the previous Ingress spec (strip `status` and
`resourceVersion` first) and delete this Deployment/Service/ConfigMap:

```bash
kubectl --context hive-oke apply -f /path/to/hive-hub-ingress-backup.yaml
kubectl --context hive-oke delete -f src/deploy/legacy-redirect/
```

## Not fixed here

This only addresses item 1 of #6430 (the path/query-dropping redirect). Items
2 and 4 in that issue (GA4 client-ID reset annotation, and "unwanted referrals"
/ configured domains for `G-4707R797K3` and `G-45JER7SJ4W`) are GA4-console-only
and out of scope for this repo.
