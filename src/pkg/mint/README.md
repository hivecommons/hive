# `pkg/mint` — caller authentication on `POST /mint`

Who is allowed to ask this mint for a token, and what may they ask for.

For what the mint *issues* (claims, TTL clamping, JWKS, WIF exchange) see the
package doc comments in `mint.go` and `agent.go`. This file is only about the
front door.

## The finding, and where it stands

[#3915](https://github.com/hivecommons/hive/issues/3915): `/mint` was gated by a
shared bearer secret alone. A shared secret proves **trusted network position**,
not **who is calling** — and nothing bounded what a holder could ask for, so any
process that obtained it could mint a token for any subject, any audience and
any scope, limited only by the TTL ceiling. Mints were logged by subject and
audience, with no record of who requested them.

| Gap | Status |
|---|---|
| No caller identity | `CallerAuthenticator` seam (`caller.go`) + `TokenReviewAuthenticator` (`tokenreview.go`) |
| Any holder may mint anything | `Entitlements`, deny-by-default per verified identity |
| No per-caller audit | the verified identity is logged on every mint and every refusal |
| mTLS backend for non-Kubernetes deployments | not implemented — same interface when it is |

## Authenticating a caller

Three implementations of one interface. `Server` does not know which is in use.

```go
Authenticate(r *http.Request) (Identity, error)
```

### `SharedSecretAuthenticator` — the original gate

Constant-time comparison against a bearer secret in `Authorization`. Still the
default, so behaviour is unchanged for anything already deployed. Every holder
gets the same identity, `shared-secret:any-holder`, which is deliberate: a
shared secret **cannot** distinguish its holders, and inventing a per-request
name would make the audit log claim an identity the mechanism never established.

### `TokenReviewAuthenticator` — a real Kubernetes identity

The caller presents its own projected ServiceAccount token; the mint asks the
API server to vouch for it and takes the answer,
`system:serviceaccount:<ns>:<name>`, as the identity.

```go
auth, err := mint.NewInClusterTokenReviewAuthenticator("hive-mint")
srv, err := mint.NewServer(minter, secret, logger,
    mint.WithAuthenticator(auth),
    mint.WithEntitlements(entitlements),
)
```

Four properties are load-bearing. Removing any of them leaves the mint no better
off than the shared secret, while looking like it is:

- **Audience scoping is checked on the response, not the request.** The review
  asks for the mint's audience, but an API server whose authenticators do not
  implement audience validation answers `authenticated: true` with the
  `audiences` field *absent*. A backend that read only `authenticated` would
  accept the API-server token every pod already has mounted at a well-known
  path. The mint's audience must appear in the returned list; empty or absent is
  a refusal.
- **A dedicated header**, `X-Hive-Mint-SA-Token`, never `Authorization`. In a
  dual-accept deployment `Authorization` carries the shared secret, and
  reviewing it would POST the mint's own secret to the API server as "a token to
  please review" — writing it into someone else's audit log.
- **Real TLS** against the in-cluster CA bundle. There is no
  insecure-skip-verify option, not even a documented one.
- **Fail closed.** A timeout, a response-size cap, and a refusal on every
  infrastructure failure — unreachable API server, missing RBAC, unparseable
  response. A TokenReview that failed open would be *worse* than the shared
  secret, because operators would believe identity was being checked.

Only `system:serviceaccount:` usernames are accepted. An entitlement map keyed
on ServiceAccount names must not be satisfiable by a human with a kubeconfig.

### `MultiAuthenticator` — migrating without a flag day

```go
auth, _ := mint.NewMultiAuthenticator(tokenReview, sharedSecret)
```

Tries backends in order and takes the first identity established. Put
TokenReview **first**: it reads its own header, so a caller presenting both is
recorded under its real identity rather than as `any-holder` — the difference
between an audit log that shows the migration finishing and one that shows
nothing changing.

Rollout: enable both → watch the audit log until no `shared-secret:any-holder`
lines remain → drop the secret from the list.

## Bounding what an identity may mint

`Entitlements` maps a verified `Identity.Name` to what it may request.

```go
mint.Entitlements{
    "system:serviceaccount:hive:hive-spoke": {
        Subjects:  []string{"spoke-alpha"},
        Audiences: []string{"//iam.googleapis.com/projects/123/locations/global/wif/providers/hive"},
        Scopes:    []string{"registry:pull"},
    },
}
```

- **An empty dimension allows nothing**, not everything. An entitlement that
  grants an audience but forgets the scopes grants a token with no scopes.
- **`"*"` is the explicit wildcard.** A caller that genuinely mints arbitrary
  subjects has to have that written down, not acquire it by omission.
- **An identity absent from a non-empty map may mint nothing.**
- **An empty map means entitlements are not configured** and the mint keeps its
  historical unbounded behaviour. `NewServer` logs a warning saying so, because
  an unbounded mint should be a visible choice rather than something discovered
  from a token that should never have been issued.

A caller that authenticates but is not entitled gets **403**, not 401 — it is
known, just not permitted. The reason is logged server-side and never returned.

## Deploying the TokenReview backend

**The mint needs permission to review tokens.** Without it every review returns
403 and every caller is refused (fail closed, but a confusing outage):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: hive-mint-tokenreview
rules:
  - apiGroups: ["authentication.k8s.io"]
    resources: ["tokenreviews"]
    verbs: ["create"]
```

…bound to the ServiceAccount the mint pod runs as.

**Each caller needs a token projected for the mint's audience.** Not the default
mounted token — that one is for the API server, and the audience check above
will (correctly) refuse it:

```yaml
volumes:
  - name: mint-token
    projected:
      sources:
        - serviceAccountToken:
            path: mint-token
            audience: hive-mint        # must match the mint's configured audience
            expirationSeconds: 3600
```

Read `/var/run/secrets/mint/mint-token` and send it as `X-Hive-Mint-SA-Token`.
Re-read it per request rather than caching: projected tokens are rotated on
disk, which is also why the mint re-reads its own reviewer token each time
instead of loading it once at startup.

## Network exposure

`Server.Handler()` returns a handler; it does not listen. Nothing in this repo
currently serves it — the mint is used in-process through `AgentMinter`. Whoever
wires a listener owns the short-term hardening the finding asks for: bind it to
localhost or a pod-internal interface, and do not expose `/mint` beyond the pod
network. Caller authentication is a second line, not a substitute for it.

`/.well-known/jwks.json` is public by design — it serves public keys, and WIF
providers must be able to reach it.
