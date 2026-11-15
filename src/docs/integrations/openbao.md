# OpenBao Transit as the spoke keystore

**Status: Opt-in.** The spoke stores its secrets at rest through a keystore
provider ([security-model.md](../security-model.md#5-network-and-credentials),
[#18214](https://github.com/hivecommons/hive/issues/18214)). The default
provider derives a key from material the hive already holds. This page is the
second provider: [OpenBao](https://openbao.org/), the Linux Foundation /
OpenSSF key and secret manager (MPL-2.0, Vault-API compatible), sealing and
opening every secret through its
[Transit engine](https://openbao.org/docs/secrets/transit/) so the data
encryption key never exists on the spoke
([#18216](https://github.com/hivecommons/hive/issues/18216)).

Hive talks to OpenBao over its HTTP API with the standard library (no SDK):
`POST /v1/<mount>/encrypt/<key>` and `POST /v1/<mount>/decrypt/<key>` with
the hive's additional authenticated data as `associated_data`, token auth via
`X-Vault-Token`, optional Kubernetes auth at `POST /v1/auth/<mount>/login`
with the pod's ServiceAccount JWT, `renew-self` ahead of the lease, and
`X-Vault-Namespace` when a namespace is set. The same configuration works
against a HashiCorp Vault server, since OpenBao keeps that API.

## What changes when it is on

- Every envelope the spoke writes names the provider and the transit key
  version: `hive-enc:v1:openbao:hive-<id>@v3:vault:v3:…`. The ciphertext
  inside is OpenBao's; the spoke cannot open it without the server.
- Boot requires the server. With `HIVE_KEYSTORE_PROVIDER=openbao` and an
  unreachable, sealed or refusing server the hive logs the reason and exits.
  It never falls back to plaintext.
- A transit key rotation on the server (`bao write -f transit/keys/<key>/rotate`)
  is picked up on the next write; files sealed under the previous version
  read as stale and are re-sealed on their next read, and the boot sweep
  covers the rest. Keep `min_decryption_version` at or below the oldest
  version still on disk until that has happened.
- Nothing sensitive is cached on the spoke: the client token lives in process
  memory, plaintext passes through on its way to and from the server.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `HIVE_KEYSTORE_PROVIDER` | `env` | Set to `openbao`. |
| `HIVE_OPENBAO_ADDR` | none (required) | Server URL, e.g. `https://openbao.openbao.svc:8200`. |
| `HIVE_OPENBAO_TOKEN` | none | Client token. Prefer `HIVE_OPENBAO_TOKEN_FILE` or Kubernetes auth. |
| `HIVE_OPENBAO_TOKEN_FILE` | none | File holding the client token (a mounted Secret). |
| `HIVE_OPENBAO_K8S_ROLE` | none | Kubernetes auth role; the pod's projected ServiceAccount JWT is exchanged for a token. Used when no token is set. |
| `HIVE_OPENBAO_K8S_MOUNT` | `kubernetes` | Kubernetes auth mount. |
| `HIVE_OPENBAO_K8S_JWT_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Where the ServiceAccount JWT is read from. |
| `HIVE_OPENBAO_TRANSIT_MOUNT` | `transit` | Transit engine mount. |
| `HIVE_OPENBAO_KEY` | `hive-<hive id>` | Transit key name. Created (`aes256-gcm96`) when missing and the token may create it. |
| `HIVE_OPENBAO_NAMESPACE` | none | `X-Vault-Namespace` for every request. |
| `HIVE_OPENBAO_CA_FILE` | system roots | PEM bundle that verifies the server certificate. |
| `HIVE_OPENBAO_TIMEOUT` | `10s` | Per-request timeout. |

The token (or role) needs `update` on `<mount>/encrypt/<key>` and
`<mount>/decrypt/<key>`, `read` on `<mount>/keys/<key>`, and `create` on
`<mount>/keys/<key>` only if Hive should create the key itself:

```hcl
path "transit/encrypt/hive-*" { capabilities = ["update"] }
path "transit/decrypt/hive-*" { capabilities = ["update"] }
path "transit/keys/hive-*"    { capabilities = ["read", "create"] }
```

## Dev-mode walkthrough

Run a throwaway OpenBao, enable Transit, create the hive's key and point a
Docker Compose hive at it. The dev server keeps everything in memory and
listens on plain HTTP; it is for trying the wiring, not for keeping secrets.

```sh
# 1. Dev server (root token chosen here; the server listens on :8200).
bao server -dev -dev-root-token-id="dev-only-token"

# 2. In another shell: enable Transit and create the key.
export BAO_ADDR=http://127.0.0.1:8200
export BAO_TOKEN=dev-only-token
bao secrets enable transit
bao write -f transit/keys/hive-dev

# 3. Point the hive at it. HIVE_ID names the hive; the default key is hive-<id>.
export HIVE_KEYSTORE_PROVIDER=openbao
export HIVE_OPENBAO_ADDR=http://host.docker.internal:8200   # or the host IP
export HIVE_OPENBAO_TOKEN=dev-only-token
export HIVE_ID=dev
docker compose up -d
```

The boot log carries one line naming the provider and key:

```text
keystore: secrets at rest are encrypted provider=openbao key_generation=hive-dev@v1
```

Save a key from the dashboard (Governor Config → Inference → LiteLLM, say) and
inspect the file:

```sh
docker compose exec hive cat /data/secrets/litellm_api_key
hive-enc:v1:openbao:hive-dev@v1:vault:v1:8SDd3WHDOjf7mq69CyCqYjBXAiQQAVZRkFM13ok481zoCmHnSeDX9vyf7w==
```

Rotate and watch the next read re-seal:

```sh
bao write -f transit/keys/hive-dev/rotate
docker compose restart hive     # the boot sweep re-seals under v2
```

## Kubernetes auth

On a cluster, avoid a static token: enable the
[Kubernetes auth method](https://openbao.org/docs/auth/kubernetes/) on the
OpenBao side, bind a role to the hive's ServiceAccount and namespace, attach
the policy above, and set `HIVE_OPENBAO_K8S_ROLE=<role>` on the spoke
Deployment. The spoke logs in with its projected ServiceAccount JWT at boot,
renews the token at two thirds of the lease, and logs in again when a renewal
is refused.

## Switching back

Set `HIVE_KEYSTORE_PROVIDER=env` only after every sealed file has been
re-entered or re-sealed: envelopes name their provider, and the `env` provider
refuses an `openbao` envelope with a clear error rather than guessing. The
practical path is to clear and re-enter the few dashboard-held keys while the
OpenBao server is still reachable.

## See also

- [Security model, section 5](../security-model.md#5-network-and-credentials)
- [Environment variable reference](../env-vars.md#secrets-at-rest-keystore)
- [Integrations](../integrations.md)
