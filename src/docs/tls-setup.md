# TLS, HTTPS, and certificates

Hive does not implement native TLS configuration in `hive.yaml`. The dashboard and hub servers call Go `ListenAndServe`, and the bundled Docker gateway listens with plain HTTP. Production HTTPS is therefore an edge concern: terminate TLS at an ingress controller, OpenShift Route, load balancer, or reverse proxy, then proxy HTTP to Hive.

## What the spoke serves

- Standalone/source runs: the Go dashboard serves HTTP on `dashboard.port` (3001 by default).
- Kubernetes examples: the Go dashboard serves HTTP on 3002 behind the `hive` Service.
- Docker Compose: nginx listens on 3001 and proxies HTTP to the hive process; the compose example does not mount certificates or enable TLS.
- Internal local listeners 18443 and 18444 are not public TLS endpoints.

## Kubernetes Ingress or Route

Use a normal TLS-terminating Ingress/Route:

1. Create or reference a certificate Secret for the public host.
2. Route HTTPS traffic to Service `hive`, port `dashboard`/3002.
3. Preserve WebSocket/SSE behavior with HTTP/1.1 upgrades and long read timeouts.
4. Include `/gh-setup` on the same host if using GitHub App Setup URL callbacks.

For cert-manager, annotate the Ingress with your cluster issuer and put the desired host in `spec.tls[].hosts`. On OKE or any other Kubernetes distribution, Hive has no special cert-manager integration; use the platform's normal ingress controller/certificate flow.

OpenShift Routes may use edge or re-encrypt termination. Edge termination is enough for the stock manifests because the backend Service is HTTP. Use re-encrypt only if you add a sidecar/proxy that serves HTTPS to the Service.

## External reverse proxy

A Caddy/nginx/HAProxy/load-balancer deployment should terminate HTTPS and forward to Hive over HTTP:

- Docker Compose: proxy to the published gateway on `http://127.0.0.1:3001`.
- Kubernetes: proxy to the Ingress/Route or Service as appropriate.
- Pass `Upgrade`/`Connection` headers for `/terminal` and contributor WebSockets.
- Use long read/send timeouts for `/api/events` and terminal sessions.

## Certificate handling

Hive does not read a dashboard TLS certificate/key from config. Certificate renewal, key rotation, HSTS, and public CA/private CA policy belong to the terminating proxy or ingress controller.

Do not confuse dashboard TLS certificates with GitHub App private keys. The GitHub App key is configured with `github.key_file` (for example `/secrets/gh-app-key.pem`) and is used only to mint GitHub installation tokens.

## Framing and HTTPS origins

`dashboard.snapshot_frame_ancestors` controls which HTTPS origins may embed the read-only `/snapshot` page. It is not a TLS setting. Keep it empty unless you intentionally allow embedding; when set, use exact origins such as `https://status.example.com`.
