# Token mint (`mint:` config block)

The `mint:` block configures Hive's opt-in OIDC token mint
(`pkg/mint`) — a short-lived, scoped-JWT issuer that lets agents (and
external Workload Identity Federation brokers) obtain a bounded-lifetime
credential instead of a long-lived shared secret. This page is the
operator-facing companion to
[ADR-0007: Mint short-lived scoped agent credentials](adr/0007-token-mint.md),
which records the design decision; read that first for *why* this exists.
This page covers *how to configure it* and, more importantly, **what an
operator must get right for the trust boundary to hold.**

**Disabled by default.** An absent `mint:` block, or `enabled: false`, is
byte-identical to a hive with no mint — the config comment says so
explicitly (`pkg/config/config.go:213-217`), and this document exists
because the feature had zero operator docs before this page.

## What it issues, and to whom

The mint signs short-lived RS256 JWTs (`pkg/mint/mint.go:34-186`). Each
token carries:

- **Standard claims** — `iss` (the configured `issuer`), `sub` (the
  requesting subject — for agent tokens, the agent's name), `aud` (a fixed
  audience string), `exp`/`iat`/`nbf`/`jti`.
- **`scopes`** — a custom claim listing what the bearer may do, e.g.
  `issues:read`, `contents:write`, `pulls:merge`
  (`pkg/mint/agent.go:27-38`).
- **`hive_id`** — which hive minted the token.

For agent credentials specifically (`pkg/mint/agent.go`), scopes are derived
from the agent's existing trust tier — the same tier string
`AgentMode.TokenTier()` already produces (`advisor`/`newcomer`/
`contributor`/`trusted`) — mapped through a fixed, fail-closed table
(`tierScopes`, `agent.go:43-48`):

| Tier | Scopes granted |
|---|---|
| `advisor` | `issues:read` |
| `newcomer` | `issues:read`, `issues:write` |
| `contributor` | `issues:read`, `issues:write`, `contents:write`, `pulls:write` |
| `trusted` | `issues:read`, `issues:write`, `contents:write`, `pulls:write`, `pulls:merge` |
| *unrecognized tier* | falls back to `advisor` (read-only) — **never** widens on a typo (`agent.go:53-58`) |

The audience embedded in agent tokens is the fixed constant `hive-agent`
(`agentTokenAudience`, `agent.go:22`) — a downstream WIF/verifier is
configured to accept exactly this audience for hive agent workloads.

**This supplements, not replaces, the GitHub App token path.** An agent that
gets a mint token keeps using its GitHub App token for GitHub operations;
the mint token is an *additional* credential a WIF-aware external system
(cloud provider, container registry) can accept
(`main.go:1738-1750`, ADR-0007 "Consequences").

## Config shape

```yaml
mint:
  enabled: true                      # default false — the mint is off unless you turn it on
  key_path: /data/mint-signing-key.pem   # required when enabled
  issuer: https://your-hive.example.com  # required when enabled — see "Choosing issuer" below
  max_ttl_seconds: 900                # optional — default 900 (15m), hard-capped at 3600 (1h)
```

Fields (`MintConfig`, `pkg/config/config.go:218-230`):

| YAML key | Go field | Required (when `enabled: true`) | Default | Notes |
|---|---|---|---|---|
| `enabled` | `Enabled` | — | `false` | Turns the service on. `buildAgentMinter` is only invoked when this is `true` (`main.go:1743`). |
| `key_path` | `KeyPath` | **Yes** | — | PEM path of the RSA signing key. Startup fails with `mint.key_path is required when mint is enabled` if empty while `enabled: true` (`main.go:890-891`). |
| `issuer` | `Issuer` | **Yes** | — | The `iss` claim, and the identity string WIF providers are configured to trust — typically the hive's public URL. Startup fails with `mint.issuer is required when mint is enabled` if empty (`main.go:893-894`). |
| `max_ttl_seconds` | `MaxTTLSeconds` | No | `900` (15m, `mint.DefaultMaxTTL`) | Bounds a minted token's lifetime. **Clamped, never trusted verbatim**: any configured value is silently clamped into `[MinTTL 1m, HardCapTTL 1h]` (`mint.go:96-111,172-186`) — you cannot configure a token that outlives one hour no matter what you set here. |

There is no field for scopes, entitlements, or a listener address in
`MintConfig` — those live outside `hive.yaml` today (see Trust boundary
below).

## Key lifecycle

`key_path` is loaded — or, if the file doesn't exist, generated and
persisted — by `mint.LoadOrCreateKey` (`mint.go:277-353`, called from
`main.go:896`):

- A missing key file causes a **fresh 2048-bit RSA key to be generated** and
  written as PKCS#8 PEM with **`0600` permissions**, atomically (temp file +
  rename, so a crash mid-write never leaves a partial key on disk).
- An existing file is parsed (PKCS#8, falling back to PKCS1 for keys written
  by other tooling) and reused.
- **There is no rotation mechanism in this codebase.** Nothing rotates
  `key_path` automatically. Rotating means: generate a new key out of band
  (or delete the file and let the mint regenerate one on restart), update
  any WIF provider's trusted-key configuration to the new `kid`/JWKS entry,
  and expect a window where tokens signed by the old key are still being
  verified by providers that haven't picked up the new JWKS yet. Plan key
  rotation as a coordinated operator action, not an automated one.
- `key_path` must be on **persistent storage** the hive process can write
  (e.g. under `/data`) — if it lives on ephemeral storage, every restart
  mints a brand-new key, and every WIF provider trusting the old key's JWKS
  entry starts rejecting the hive's tokens until reconfigured.

## Choosing `issuer`

`issuer` is checked on every `Verify` call (`mint.go:196-221`, `jwt.WithIssuer`) —
a token is only valid against a minter configured with the exact same
issuer string. Set it to the stable, public URL a WIF provider (or any other
verifier) will be configured to trust. Changing `issuer` after external
providers have been configured against the old value breaks verification for
every token issued with the new value until the provider config is updated
too — treat it as effectively immutable once anything trusts it.

## The JWKS endpoint — and what is NOT wired

ADR-0007 and the mint package describe a `.well-known/jwks.json` document
that downstream WIF providers use to verify tokens (`Minter.JWKS`,
`mint.go`). **As of this branch, nothing in the hive process serves that
document and no HTTP mint server is compiled in.** Today, `mint.enabled: true`
only wires the in-process `AgentMinter` that stamps agent-scoped tokens — it
does **not** stand up an HTTP `/mint` endpoint or a public JWKS endpoint. If
your use case needs an external WIF broker to independently verify
hive-minted tokens over HTTP, that transport does not exist yet in this
codebase; enabling `mint:` today only benefits in-process consumers of
`AgentMinter`.

## Trust boundary — read this before enabling

This is the security-sensitive part. State these plainly to yourself before
turning `mint.enabled: true` on:

1. **Who can obtain a token today**: only in-process callers holding an
   `*mint.AgentMinter` — currently the agent manager, which mints a token
   per agent on the same refresh cadence as its GitHub App token. There is no
   exposed HTTP endpoint for an external caller to request a mint token from
   this hive (see JWKS section above).
2. **What a token grants**: exactly the scopes for the subject's tier —
   never more (fail-closed on unknown tiers, `agent.go:53-58`) — for the
   fixed `hive-agent` audience, for at most `min(max_ttl_seconds,
   3600s)`. A minted token by itself is not a GitHub credential and grants
   nothing against GitHub's API; it only means something to a verifier that
   has been separately configured to trust this hive's `issuer` (via a JWKS
   fetch mechanism you must supply, since this repo doesn't serve one yet).
3. **What the private key protects**: everything. Anyone who can read
   `key_path` can forge a token with **any** subject, **any** scopes
   (nothing enforces the tier table outside `AgentMinter.MintAgentToken`
   itself — a caller using `Minter.Mint` directly is not restricted to
   `tierScopes`), and up to the hard-cap TTL. `key_path` must be `0600`,
   owned by the process user, and on storage an attacker who compromises one
   agent's sandbox cannot read. Never commit it, never log it, never put it
   in a ConfigMap — this repo's own dashboard code deliberately omits
   `KeyPath` from every operator-facing status payload for exactly this
   reason (`status_builder.go:438-447`, `api_governor_features.go:27-29`
   comment: "the dashboard overlay is deliberately secret-free").
4. **What "leaked" costs you**: bounded blast radius by design (ADR-0007
   Consequences) — a leaked minted token expires within the hour at the
   absolute worst, and its scopes are capped to one trust tier's ladder. It
   is not equivalent in blast radius to leaking `key_path` itself (point 3),
   which is unbounded until you rotate.
5. **What an operator must never do**: never hand-write a value into
   `key_path`'s target file, never set `max_ttl_seconds` expecting it to
   exceed one hour (it won't — the clamp is silent, not an error, so a
   config claiming a longer TTL will look accepted but simply be
   ineffective), and never treat `mint.enabled: true` as adding
   external-facing authentication surface — it does not, until a listener is
   wired.

## Dashboard visibility

An owner can toggle `mint.enabled` and `mint.issuer` from the Governor
config dialog (`POST` handled by `handleGovernorFeatures`,
`api_governor_features.go:136-139`) — but **not** `key_path`, by design; the
signing key never appears in any dashboard payload. Status reporting
(`status_builder.go:438-447`) exposes only `enabled`, `issuer`, and a
boolean `keyPresent` (whether the file at `key_path` currently exists) —
never the key material or the path itself in a form that leaks key location
semantics beyond presence.

## Open questions

- No HTTP listener for `POST /mint` or `.well-known/jwks.json` exists in
  this branch's `cmd/hive/main.go` or `pkg/dashboard`. Until one is wired,
  `mint:` only benefits in-process agent-credential issuance
  (`AgentMinter`), not external WIF exchange. Treat any plan that assumes an
  externally reachable mint endpoint as **not yet implemented** rather than
  a configuration gap on the operator's side.
- Key rotation is entirely manual/out-of-band; there is no `hivectl` or
  dashboard action that rotates `key_path` for you.
