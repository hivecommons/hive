# Delegation chains (observe-only)

A **delegation chain** is a cryptographically verifiable record of *which
authorizations composed* to produce an action — signed by the hive, published in
a form a third party can check, and, today, **used for nothing**.

> **Observe-only.** Nothing in hive consults a chain to decide anything. Chains
> are minted, logged, and published. No code path refuses, degrades, or alters
> behavior based on a chain's presence, absence, or validity. Moving to
> enforcement is a **separate decision**, not a follow-up chore — see
> [Observe-only today](#observe-only-today-enforcement-is-a-separate-decision).

## Why this exists

Hive actions are almost never authorized by one party. A hosted spoke agent
opening a PR is acting under a GitHub App installation, which is scoped by the
hive the hub provisioned, which exists because an operator asked for it.

Today that composition is reconstructible only by reading the
[audit log](audit-log.md) and knowing how the pieces fit. It is institutional
knowledge, not evidence. A delegation chain makes it evidence.

**Multi-tenancy is what makes this load-bearing.** A tenant's hive runs inside a
fleet *someone else operates*. "Trust the operator's dashboard" is exactly the
assurance a tenant cannot accept. So the chain is published in a form the
tenant's own services verify with **no hive credentials** and **no request-time
call to the hub**.

## Chain shape

The chain follows [RFC 8693](https://www.rfc-editor.org/rfc/rfc8693)'s `act`
(actor) claim nesting: the outermost actor is the most immediate one, and each
nested `act` is the party that delegated *to* it.

```json
{
  "v": "hive-delegation-v1",
  "sub": { "type": "agent", "id": "scanner", "hive_id": "acme" },
  "act": [
    { "type": "app", "id": "kubestellar-hive[bot]", "installation_id": 67890,
      "via": "app-installation-token" },
    { "type": "hive_authority", "id": "acme", "via": "hosted" }
  ],
  "action": "pr_opened",
  "hive_id": "acme",
  "g": 1,
  "iat": 1787934973,
  "exp": 1788021373
}
```

The **root** is the innermost actor — the party whose authority is not derived
from anything else present. It is the link that must be honest above all others.

### Principal types

Every link carries an explicit type. This is the single most important design
decision in the format: a bare login string cannot tell a reader whether
`kubestellar-hive[bot]` is a person, and the answer to "did a human authorize
this?" must never depend on string inspection.

| Type | Meaning | Human? | May be a root? |
| --- | --- | :---: | :---: |
| `user` | A natural person, from a server-resolved identity | **yes** | yes |
| `app` | A forge App acting through an installation | no | yes |
| `hive_authority` | A hive acting on its own standing delegated authority | no | yes |
| `hub` | The control plane acting on a spoke | no | yes |
| `agent` | A named agent process within a hive | no | **no** |

`agent` cannot root a chain. An agent has no authority of its own — it runs
because a cadence or a person started it — so a chain bottoming out at an agent
has dropped the link that actually authorized the work.

### Signature algorithm

**Ed25519**, not the ES256 an RFC 8693 reader might expect. Hive already
derives, provisions, rotates, and dual-accepts Ed25519 keys across the fleet
(`pkg/hub/hub_keys.go`, `hub_pubkey_generations.go`), and the generations
machinery that makes rotation survivable is written against them. A second
algorithm would mean a second rotation story and a second thing to get wrong,
for a property no consumer depends on. **The `act` nesting — the interoperable
part — is preserved verbatim.**

## The five identity situations

The chain shape was derived from these, not the other way round.

### (a) Hosted spoke agent under a GitHub App installation token

```
hive_authority:<hive>(hosted) -> app:<bot>(app-installation-token) -> agent:<name>
```

**The root is not a human, and this is the situation most likely to be got
wrong.** A `ghs_…` installation token has **no user identity at all** — `gh api
user` 403s for every staff agent, which is exactly why `bin/gh-wrapper.sh` reads
the trusted bot-login file (`github.BotLoginFilePath()`) rather than asking the
token who it is (#4044/#4049).

The tempting wrong answer is to root this at the hive owner, since a human did
once install the App. That is a **fabricated root**: the owner did not authorize
*this* action, may not know it happened, and may no longer be with the org. The
installation is what authorized it.

If the bot login or installation ID does not resolve, **no chain is emitted**.

### (b) Self-hosted / native spoke

```
hive_authority:<hive>(self-hosted) -> agent:<name>
```

Two links, not three: no App installation, and no hub provisioning authority
above it. Collapsing (a) and (b) would tell a tenant that the fleet operator
authorized something the fleet operator never touched.

### (c) Contribute-plane client with a user token

```
user:<login>(contribute-plane)
```

The **only** situation with an honest human root. A user token *does* resolve
`/user` — the exact opposite of the installation-token case — and the dashboard
resolves identity server-side (session → hub-injected `X-Hive-User` → persisted
owner token), never from a client-supplied header.

An anonymous caller, or one of the audit log's pseudo-users (`""`, `system`,
`local`, `unknown`), yields **no chain**.

### (d) Hub → spoke heartbeat-delivered directive

```
hub:<hub-id>(heartbeat-directive:<directive>)
```

The heartbeat *response* is hive's command channel (`UpgradeTo`, `SwitchToTag`,
`RestartSpoke`, `AuthorizedUsers`). Authentication runs only in the reverse
direction: the spoke proves its identity with its per-hive derived bearer, and
the response is trusted because it arrived on that connection.

**So the response carries no actor.** This chain is honestly *shallow*: it roots
at the control plane, not at whichever hub admin might have clicked something,
because that admin's identity is genuinely not present in the data the spoke
receives. Attributing it to an admin looked up elsewhere would be a fabricated
root assembled from a plausible correlation.

*If a future PR propagates a real originating actor onto the directive, this
situation gains a link.*

### (e) Scheduled / cadence-triggered work no human initiated

```
hive_authority:<hive>(cadence:<label>) -> agent:<name>
```

The governor selects agents purely from wall-clock state, and `SendKick` takes no
actor at all. This is the **largest volume of unattributed action in hive**.

The root is the hive's own standing delegated authority: an operator delegated
that authority when they configured the cadence, and the hive is exercising it.
Naming *that operator* would be the fabrication — they authorized the standing
rule, not this occurrence. This is the same distinction that led #4055 to coin
`hook:<name>` instead of attributing hook-driven pauses to a dashboard user.

## Never fabricate a root

When an honest chain cannot be constructed, hive emits **no chain** — never a
plausible-looking one.

This follows precedent already in the codebase:

- `Manager.PauseBy`: *"empty means 'no human actor' — never fabricate one."*
- `hookPauseActor` (#4055): a hook-driven pause records `hook:<name>`, an
  explicitly non-human identity *"a reader cannot confuse with a person."*
- `mint.SharedSecretIdentityName`: *"inventing a name per request would make the
  audit log claim an identity the mechanism never established."*

Emitting nothing is a truthful statement ("we cannot prove who authorized
this"). Emitting a plausible chain is a false one — and a false chain is
strictly worse than no chain, because the entire value here is that a chain is
**evidence**.

Construction returns `ErrNoHonestRoot`, and the emit path turns that into the
same empty token every other no-chain case produces.

## How a tenant verifies independently

### 1. Fetch the published material

```
GET /api/hub/delegation-keys
```

**Anonymous, stable, documented.** No credential. Cacheable.

```json
{
  "version": "hive-delegation-keys-v1",
  "enabled": true,
  "chain_version": "hive-delegation-v1",
  "keys": [
    { "generation": 2, "public_key": "<64 hex chars>",
      "algorithm": "EdDSA", "curve": "Ed25519", "current": true },
    { "generation": 1, "public_key": "<64 hex chars>",
      "algorithm": "EdDSA", "curve": "Ed25519", "current": false }
  ],
  "generated_at": "2026-08-28T16:38:48Z"
}
```

`enabled` is published so a tenant can distinguish *"no chains exist because the
feature is off"* from *"no chains exist because nothing happened"*. Without it,
an absence of chains is unreadable.

**Why not literal RFC 7517 JWKS?** A JWK for Ed25519 is `OKP`/`Ed25519` with a
base64url x-coordinate (RFC 8037) — a different encoding from the hex Ed25519
public keys hive already publishes into every spoke's env. Shipping a second
encoding of the same key type means two places to get decoding wrong, for no
gain. This document is *JWKS-shaped* and maps onto RFC 7517 mechanically:

```
{"kty":"OKP","crv":"Ed25519","alg":"EdDSA",
 "kid":"<generation>","x":base64url(hex_decode(public_key))}
```

### 2. Verify

Token format:

```
base64url(claims JSON) "." base64url(Ed25519 signature over the body STRING)
```

The signature covers the **base64 body string**, not the raw JSON — the same
construction as hive's SSO tokens and session cookies. This means a verifier
never has to re-serialize JSON byte-identically to check a signature.

**Verify the signature before parsing any payload byte.** Parsing
attacker-controlled JSON before authenticating it gives a verifier a pre-auth
attack surface.

A runnable, dependency-free implementation lives at
[`src/docs/examples/verify-delegation-chain/`](examples/verify-delegation-chain/main.go).
It imports only the Go standard library — no hive packages — so it is a genuine
independent implementation rather than a call back into the code that minted the
chain:

```console
$ curl -s https://<hub>/api/hub/delegation-keys > keys.json
$ go run ./main.go -keys ./keys.json -token "$(cat chain.txt)"
CHAIN VERIFIED
  action:      pr_opened
  hive:        acme
  generation:  1
  root:        hive_authority:acme@acme(hosted)
  authorized by a HUMAN: no — this is machine authority (hive_authority)
  delegation path (root first):
    hive_authority:acme@acme(hosted)
      app:kubestellar-hive[bot]@acme(app-installation-token)
        agent:scanner@acme
```

A tampered chain exits non-zero:

```console
$ go run ./main.go -keys ./keys.json -token "$(cat tampered.txt)"
CHAIN NOT VERIFIED: chain REJECTED: no published key verifies this signature
(tampered, forged, or minted by a different hub)
```

Because the key document can be saved and reused, **verification works offline**
— during a hub outage, or after leaving the platform. That is the property that
makes it evidence rather than the operator's opinion restated: if verification
required asking the operator, the operator could stop answering for a tenant
they were in dispute with.

## Rotation

Chains reuse hive's existing
[master-key generations](design/master-key-rotation.md) machinery. No new secret,
no separate rotation path.

- The signing seed is `HMAC-SHA256(generation secret, "hive-delegation-ed25519-v1")`,
  used verbatim as the Ed25519 seed. A **distinct domain label** means a chain
  signature can never verify as an SSO handoff or a session cookie, and vice
  versa.
- Only the **current** generation mints. Dual acceptance is a verifier-side
  property.
- Every still-acceptable generation is published, so a chain minted just before
  a rotation keeps verifying afterwards — a third party trial-verifies across
  the set and never needs to know a rotation happened.
- A generation whose `VerifyUntil` has passed **leaves the document**, and a
  *missing* `VerifyUntil` is treated as already expired so a hand-edited
  generations file fails closed. The acceptance window closes on a wall clock
  with no operator action.

Before any rotation — the state of every hub in the fleet today — the document
carries exactly one key.

## The comparison harness

Minting chains nobody reads would prove nothing. The point of the observe phase
is to answer, **with data rather than argument**, a question we cannot currently
answer: does the chain agree with the attribution hive already records?

`delegation.Compare` joins observed chains against
[audit log](audit-log.md) records on `(action, agent, timestamp-to-the-second)`
and classifies each pair:

| Verdict | Meaning |
| --- | --- |
| `agree` | The chain's human root matches the audit user. |
| `chain_only` | The audit log recorded a pseudo-user where the chain has a real machine principal. **Where the audit log is losing information today** — every cadence kick lands here. |
| `audit_only` | The audit log named a human where the chain declined to emit. Our root resolution is incomplete, or the audit log is over-attributing. |
| `conflict` | Both name a human and they **differ**, or the audit log named a person where the chain's root is a machine. |

The join is deliberately **strict**. A loose join matching on action alone would
manufacture agreement between unrelated events — and a harness reporting *false*
agreement is worse than no harness, because it would be cited as evidence that
the chain is safe to enforce.

`Compare` is pure: no I/O, no clock, no environment. `AuditRecord`'s JSON tags
match the documented on-disk format exactly, so the harness runs over a
downloaded `audit.jsonl` with no hive running.

## Observe-only today; enforcement is a separate decision

**This is not a phase-one convenience. It is the design.**

Hive's auth path is where three separate incidents landed in one week:

- **#3982** hardened the identity oracle and broke every staff agent's
  author-gated listing; a fleet owner was dead in the water until **#4049**.
- **#4045**'s token bypass existed because a scrub sat at the wrong boundary.
- **#4043**'s label injection failed closed and bricked operations.

Each was a correct-looking tightening of an auth path that turned out to have a
consumer nobody had enumerated. A delegation chain touches **every identity
situation at once**, so shipping it enforcing would be all three failure modes
simultaneously.

### What is pinned

| Guarantee | Test |
| --- | --- |
| No call site gates on chain validity | `TestObserveOnlyInvariant_NoEnforcementConsultsChain` |
| The package exposes no decision-shaped API | `TestObserveOnlyInvariant_MinterHasNoDecisionAPI` |
| Flag off is byte-identical to baseline | `TestFlagOffIsByteIdenticalToBaseline` |

The first is a **source-level scan of the whole tree**, not a behavioral test. A
behavioral test proves only that the paths it exercises do not enforce; the
property needed is universal, and it must survive a PR written by someone who
never read this page. Enforcement would be a small, locally-reasonable-looking
diff (`if !chain.HasHumanRoot() { http.Error(...) }`) in a package far from here.

**Turning enforcement on therefore requires amending that test — a visible,
reviewable, arguable act.** That is what makes "enforcement is a separate
decision" enforceable rather than aspirational.

### The feature flag

`HIVE_DELEGATION_CHAIN_ENABLED` — accepts `1`/`true`/`yes`/`on`, matching
`HIVE_METRICS_ENABLED`. **Default off.**

**Off** (the state of every hive on merge): no chain constructed — the situation
constructor never runs, so no identity resolution happens on any hot path — no
chain signed, no log line, no response body change. The key endpoint reports
`enabled: false` with an empty key list.

**On**: chains are minted, emitted, and verifiable. **Nothing else.**

The flag controls **blast radius of computation** (identity resolution and
signing across the fleet), *not* enforcement. Observe-only holds in **both**
states.

### What would need to change to move from observe to enforce

Enforcement is a decision for the operator to make later, deliberately. It would
require, at minimum:

1. **An empty `conflict` set in the comparison harness, sustained.**
   `ComparisonReport.HasBlockingFindings()` must be false across a representative
   window. A non-empty conflict set means the chain and the audit log disagree
   about who authorized something — enforcing on top of that would refuse real
   work or permit unattributed work, and we would not know which.
2. **A decision about `ErrNoHonestRoot`.** Today "no honest root" means no chain.
   Under enforcement it must mean either *deny* (which breaks every path where
   the bot-login file is transiently absent — the #4049 failure mode exactly) or
   *allow* (which makes the chain optional and therefore not a control). Both are
   defensible; neither is obvious; the choice must be explicit.
3. **A rollout that is not a flag day.** The flag is currently binary and
   fleet-wide. Enforcement needs per-hive opt-in and a dry-run mode that reports
   what *would* have been refused — the lesson of #4043, which failed closed and
   bricked operations.
4. **Coverage of every emit site.** A chain absent because no emit site exists
   there is indistinguishable, at an enforcement point, from a chain absent
   because authorization failed. Enforcing before coverage is complete converts
   a missing feature into a denial.
5. **Amending the observe-only invariant tests**, with the reasoning recorded in
   the PR that does it.

Until all five are settled, chains are **evidence, not a control**.
