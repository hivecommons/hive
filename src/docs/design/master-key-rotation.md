# Hub master secret rotation

Status: foundational code landed; per-domain adoption pending (see "Follow-on PRs").

## The problem

Every piece of signing material on the platform is a pure function of ONE value,
the hub master secret, with no marker recording which master produced it:

```
heartbeat bearer   = HMAC(master, "hive-heartbeat-v1" || 0x00 || hiveID)
session cookie key = HMAC(master, "hive-session-v1")
session Ed25519    = HMAC(master, "hive-session-ed25519-v1")   -> seed
SSO Ed25519        = HMAC(master, "hive-sso-ed25519-v1")       -> seed
impersonate key    = HMAC(master, "hive-impersonate-v1")
terminal key       = HMAC(master, "hive-terminal-v1" || 0x00 || hiveID)
invite key         = HMAC(master, "hive-invite-v1"   || 0x00 || hiveID)
```

The `-v1` suffixes version the DOMAIN LABEL, not the key. There is no
generation, kid, or secret-version concept anywhere in `src/pkg/hub`.

The consequence is that rotation is a fleet-wide flag day. Changing the master
changes all seven derived values simultaneously, and no verifier can accept
both the outgoing and incoming forms. In one instant every browser session is
invalidated, every heartbeat 401s, every SSO handoff fails to verify, and every
terminal grant is rejected — across ~66 hosted spokes. In practice this means
the master has never been rotated and cannot be, which is exactly the property
a master secret must not have.

## Why not simply bump the info labels

The obvious alternative — rotate per-domain by moving `hive-session-v1` to
`hive-session-v2` — was considered and rejected as the primary mechanism.

It does not rotate the secret. Every `-v2` label is still `HMAC(master, ...)`
of the SAME master. If the master leaks, bumping labels changes the derived
bytes but the attacker who holds the master re-derives every `-v2` key as
easily as every `-v1` key. Label bumping is a domain-separation tool, not a
compromise-recovery tool, and compromise recovery is the reason to rotate.

It also does not compose. A label bump requires editing a constant, which is a
code change and therefore a deploy, which means the rotation cadence is bounded
by the release cadence and cannot be done urgently. And the labels are a
contract shared with the Node proxy (`src/proxy/server.js`) and with already-
provisioned spoke Deployments, so a bump is fleet-visible in exactly the way an
emergency rotation must not be.

So: rotate the MASTER, and add a generation marker so verifiers can select.
The info labels stay as they are and keep meaning what they mean.

## Design

### Generations

A generation is `(id, secret)`. The hub holds an ordered set: exactly one
**current** generation, which is the only one that MINTS, plus zero or more
**previous** generations that are accepted for VERIFY only, each with an
explicit expiry.

```
current:  gen=3  secret=<64 hex>        mint + verify
previous: gen=2  secret=<64 hex>        verify only, until 2026-08-20T00:00:00Z
```

The generation id is a small monotonically increasing integer. It is NOT
secret — it appears in cookies and log lines and that is fine; it names a key,
it is not a key.

Derivation is unchanged. `deriveDomainKey(gen.secret, info)` and
`derivePerHiveKey(gen.secret, info, hiveID)` are called exactly as today,
once per generation. This matters: it means no domain's key FORMAT changes,
so a rotation does not require the Node proxy or any spoke to understand
generations in order to keep working. A spoke that is handed
`HIVE_SESSION_KEY=<derived from gen 3>` simply has a different string in its
env than it had before, and every existing code path treats it identically.

### On-disk representation

Today the master lives at `/data/saas/hub-secret.key` — 64 bytes, raw. That
file remains the generation-1 secret and is never rewritten, so a hub that
rolls back to pre-rotation code finds exactly what it expects. Generations are
stored alongside it in `/data/saas/hub-generations.json`:

```json
{
  "current": 3,
  "generations": [
    {"id": 3, "secret": "...", "created": "..."},
    {"id": 2, "secret": "...", "created": "...", "verify_until": "..."}
  ]
}
```

When that file is absent — the state of every hub today — the loader
synthesizes a single generation `{id: 1, secret: <contents of
hub-secret.key>}`. So an un-rotated hub behaves byte-identically to today, and
"has never been rotated" and "has been rotated back to one generation" are the
same state. There is no migration step.

**"Absent" and "unreadable" are opposites, not synonyms** (audit 8, F20). The
fallback above is licensed by ENOENT and by nothing else: the file is created
only by `saveGenerations`, so its non-existence is a POSITIVE fact — no rotation
has ever been persisted, and generation 1 really is current. Every other failure
to establish the file's contents (an unretryable read error, bytes that do not
parse, a parsed set with no minting generation) means a rotation MAY have
happened and the hub cannot tell which generation is current. Falling back to
the legacy set there would re-install SUPERSEDED material as the minting
generation, drop the post-rotation generation out of the accepted set entirely,
and — because `rotated_at` goes with it — clear the anti-stranding cooldown.

So those cases fail closed to NO set at all. The hub keeps `hubSecret` and
therefore keeps authenticating existing spokes on the documented single-master
verify path, but it mints nothing it cannot vouch for and `rotateMasterSecret`
refuses outright (`force` does not override it — `force` beats the convergence
cooldown, not "I do not know what generation I am on"). The read is retried a
bounded number of times first, because the fault this guards against is
transient by construction and a crash-loop on a PVC blip is a worse failure than
the one being fixed. This is the same philosophy as `VerifyUntil.IsZero()`
meaning ALREADY EXPIRED: a generation set that cannot be trusted must never
quietly widen what is accepted or revert what is current.

### Dual acceptance, and how it ends

Verifiers try the current generation first, then each unexpired previous
generation. Minting only ever uses current.

The prior art here is a warning, not a template. The F1/F2 lanes that took
five audits to remove were "verify-both" lanes that were **unversioned and
permanent**: nothing in the code said which alternative was the legacy one,
and nothing said when it stopped being accepted, so the only way to end them
was an audit finding. This design fixes both properties:

- **Explicitly versioned.** A previous generation is a numbered entry in a
  list, not an unnamed `if` branch. "Which key accepted this request" is a
  value the code can return and the telemetry can count.
- **Explicitly finite.** Every previous generation carries `verify_until`. An
  expired generation is not accepted, full stop — the acceptance window closes
  on a wall clock whether or not anyone remembers to close it. The default
  window is 7 days, matching `cookieMaxAgeDays`, because the longest-lived
  artifact bound to a generation is a session cookie.

The end state is observable rather than inferred: `GET
/api/saas/admin/auth-rollout` reports, per generation, how many spokes carry
material derived from it. Retirement is safe when the previous generation's
count reaches zero AND its `verify_until` has passed.

### Which artifacts can carry a generation marker

This is the part that constrains the design, and it splits three ways.

**CAN carry a marker — payload room, hub controls both ends.**

| Artifact | Format | Marker placement |
|---|---|---|
| Impersonation cookie | `admin\|target\|exp.<sig>` | prepend `g<N>.` |
| Session cookie (HMAC) | `user.<sig>` | prepend `g<N>.` |
| Session cookie (Ed25519) | signed payload | field in payload |
| SSO handoff token | structured, versioned | field in payload |

For these, a verifier reads the marker and selects the one generation to check
against. No trial verification, no extra HMAC work, and — importantly — the
audit log can record which generation was used.

**CANNOT carry a marker — bare derived strings.**

| Artifact | Why |
|---|---|
| Heartbeat bearer | The bearer IS the derived key, presented raw in `Authorization`. There is no envelope to put a marker in. |
| Terminal assertion key | Symmetric key read from env by both Go and Node; adding structure changes the contract with `proxy/server.js`. |
| Invite key | Same shape as terminal. |
| SSO/session PUBLIC keys | A hex Ed25519 public key has a fixed 32-byte form. |

For these the answer is **bounded trial verification**: compare the presented
value against the derivation from each live generation, in current-then-previous
order, constant-time, and stop at the first match. The bound is the number of
live generations, which the loader caps at
`maxLiveGenerations = 2` (one current, one previous). So worst case is two
HMAC computations per heartbeat instead of one — a heartbeat already does
strictly more work than that, and the comparison stays constant-time within
each attempt.

Prefixing the bearer with `g<N>.` was considered and rejected. The bearer's
format is a contract with already-deployed spoke Deployments and with
`SpokeHeartbeatKey()`'s self-derivation lane; changing it would make the
rotation mechanism itself require a flag day, which defeats the purpose.

**Deliberately NOT rotated in the first instance: the SSO and session Ed25519
public keys.** These are injected into spoke Deployments and verified by the
Node proxy, which has no generation concept. Rotating them is a
reconcile-lane problem (below), not a verifier problem, and it is sequenced
last because it is the most fleet-visible.

### Rotating spoke-held material without re-provisioning

Fleet re-provisioning is ruled out. The mechanism is the existing reconcile
lane, `perhive_env_reconcile.go`, which already does exactly the right thing
and already handles the rotation case — its `perHiveEnvDrift` doc comment
says so explicitly:

> A var present with a DIFFERENT value counts as drift and is corrected — that
> is the case a stale hive ID or **a rotated master** produces.

So `desiredPerHiveEnv` derives from the CURRENT generation, drift detection
notices every spoke still holding gen-N-1 material, and the existing
rate limiter (3 patches per 15-minute cycle) walks the fleet over ~6 hours.
Each spoke rolls its pod once. During that walk the hub's dual acceptance is
what keeps the not-yet-patched spokes authenticating — which is precisely why
dual acceptance is the prerequisite for rotation and not an optimization.

No new machinery is required for this. The reconcile lane needs one change:
derive from the current generation rather than from `provisionMasterSecret()`.

### Observability

`PerHiveEnvStatus` gains a per-generation breakdown, sourced the same way the
existing counts are — from the hub's OWN Deployment reads, NOT from heartbeat
recency.

This distinction is load-bearing and was learned the hard way.
`AuthRolloutReadiness` drops any hive not seen in 24h and, as its own comment
records, cannot distinguish "hive absent" from "hive never existed". A paused
spoke silently leaves the denominator and the fleet reads ready while that
spoke is still on the old generation. `PerHiveEnvSnapshot` is immune: a paused
hive still has a Deployment, so it is still read, still counted, and still
blocks convergence.

Rotation readiness therefore gates on the Deployment-sourced counts:

```json
"key_generations": {
  "current": 3,
  "spokes_on_current": 61,
  "spokes_on_previous": 5,
  "previous_verify_until": "2026-08-20T00:00:00Z",
  "safe_to_retire_previous": false
}
```

`safe_to_retire_previous` is true only when `spokes_on_previous == 0` with a
non-zero denominator AND `verify_until` has passed. It fails closed on zero
observations.

## Rotation procedure

1. Operator posts to the (admin-only) rotate endpoint. The hub generates a new
   64-byte secret, makes it current, demotes the outgoing one to previous with
   `verify_until = now + 7d`, and persists.
2. Hub immediately mints only with the new generation. Existing cookies and
   bearers keep verifying against the previous generation.
3. The reconcile lane detects drift on every spoke and patches 3 per cycle.
   ~6 hours to converge 66 spokes.
4. Operator watches `safe_to_retire_previous`.
5. After `verify_until`, the previous generation stops being accepted —
   automatically, whether or not anyone is watching.

No step requires re-provisioning, and no step invalidates every session at once.

## Scope landed in this PR

Foundational only, deliberately:

- The generation type and ordered set (`hub_generations.go`)
- Load/synthesize-from-legacy-file, so today's hubs are unchanged
- Dual-generation derivation helpers
- Dual acceptance in ONE verifier: the impersonation cookie

The impersonation cookie was chosen as the pilot because it is the
lowest-risk verifier on the platform: hub-only (no spoke, no Node proxy, no
Deployment env), a 30-minute TTL that bounds any mistake to half an hour, and
a blast radius limited to the admin's own read-only "view as" feature — which
is already fail-closed and grants no privileges when it fails. It exercises
the full marker path (mint with marker, verify by selection, accept unmarked
legacy) without touching anything a tenant depends on.

The other six domains are deliberately untouched. See below.

## Follow-on PRs, in order

| # | Scope | Size | Fleet-visible |
|---|---|---|---|
| 1 | Session cookie (HMAC + Ed25519) dual acceptance, marker in payload | M | No — hub-side verify only |
| 2 | Heartbeat bearer bounded trial verification against both generations | M | No — hub-side verify only |
| 3 | `desiredPerHiveEnv` derives from current generation; per-generation counts in `PerHiveEnvStatus` | S | No — read path until #4 |
| 4 | Admin rotate endpoint + persistence of the generation file | S | No — arms the rest | ✅ landed
| 5 | ~~Terminal + invite key dual acceptance (spoke-side, symmetric)~~ → **per-hive terminal + invite keys; no dual acceptance (see as-built)** | M | **Yes** — rolls pods via reconcile |
| 6 | SSO/session Ed25519 public key rotation, incl. `proxy/server.js` accepting two public keys | L | **Yes** — Node proxy contract |
| 7 | Retire generation automatically past `verify_until`; alert if a generation is pinned open | S | No |

PRs 1–4 are hub-internal and can land without any spoke rolling. Only 5 and 6
touch the fleet, and both go through the existing rate-limited reconcile lane
rather than a re-provision.

## As-built notes for PR #4

Two decisions were made during implementation that the design above did not
pin down.

**Double rotation is REFUSED, not warned about, with `force` as the override.**
`maxLiveGenerations` is 2 and `rotate()` carries forward only the outgoing
current, so a second rotation DROPS the generation from two rotations ago —
which is the one most of the fleet is still on, because the reconcile lane
walks spokes at 3 patches per 15-minute cycle. A second rotation an hour into
the first would leave ~54 of 66 spokes holding material the hub no longer
accepts: heartbeat 401s until the sweep reaches them, hours later. That is the
flag day this design exists to prevent, reintroduced through the front door.

A warning on a response body is not a control. The operator most likely to
double-rotate is the one who did not realise the first rotation was still in
flight — precisely the operator who will not read it. So the endpoint returns
409 with a `retry_after_seconds`, and `force: true` is required to override.

`force` is honoured rather than omitted because there is a real case for it: if
the NEW generation is itself compromised, rotating again immediately is correct
even at the cost of stranding spokes for a few hours. A guard with no override
would turn the cooldown into a window of known-compromised material.

`rotationCooldown` is 8 hours — sized to CONVERGENCE (66 spokes at 3 per
15-minute cycle is ~5.5h, plus margin for cycles lost to unreachable clusters),
deliberately NOT to `defaultVerifyWindow`. Waiting for the previous generation
to expire before permitting another rotation would block emergency re-rotation
for a week, and the hazard being guarded is unconverged spokes, not the old key
still being accepted. A test asserts `rotationCooldown < defaultVerifyWindow`.

**Double-submit is guarded by the same mechanism, not a separate one.**
`rotateMasterSecret` holds the generation write lock across
evaluate-generate-persist-install, so two concurrent POSTs cannot both observe a
pre-rotation `lastKeyRotation`; the second is refused by the cooldown. The
cooldown IS the idempotency guard.

**Persistence.** `/data/saas/hub-generations.json`, a sibling of
`hub-secret.key` on the hub PVC, at mode 0600 (not the 0644 its JSON neighbours
use — this file holds master secrets in plaintext). `hub-secret.key` is never
rewritten, so a hub that rolls back to pre-rotation code finds exactly what it
expects. `rotated_at` is persisted alongside the set, because otherwise the
cooldown would reset on every hub roll — several times a day — and stop
guarding anything.

The set is persisted BEFORE it is installed in memory: a failed write leaves
the hub on the old set and returns an error, rather than minting on a key it
forgets at its next roll.

A corrupt generations file is quarantined and the loader falls back to the
legacy single-generation set — it is NOT discarded and replaced with a fresh
rotation, which would mint material the fleet has never seen while forgetting
the generation the fleet is actually on. This differs deliberately from the
alert-acks precedent, where starting fresh is the safe direction.

Expiry is NOT filtered at load time. An expired previous generation is loaded
and then excluded by `acceptableGenerations` at every verify, so the wall clock
is the only thing that closes the window — filtering at load would make expiry
depend on when the hub last restarted.

## Note: the empty-master readers

The task flagged that `hub_keys.go:314`, `:356`, and `:390` read
`os.Getenv("HIVE_HUB_SECRET")` directly, and that this env var is UNSET on the
live hub pod — raising the question of what is currently deriving keys from an
empty master.

It is not a bug. Those three sites are inside `spokeDomainKey`,
`SpokeHeartbeatKey`, and `SpokeSSOPublicKey` — all SPOKE-side resolvers. The
`hive` binary serves both roles but selects between them at startup:
`runHub()` (`src/cmd/hive/main.go:6943`) is the only caller of
`hub.NewHubServer`, and it is a distinct mode from the spoke path. On a hub
pod those three functions are never called, so the empty `HIVE_HUB_SECRET`
they would read is never consulted.

The hub's own material comes from `NewHubServer` (`server.go:1107`), which
reads the env var, falls back to `/data/saas/hub-secret.key`, and generates +
persists a fresh secret if both are absent — and `provisionMasterSecret()`
mirrors that same order. Both resolve to the 64-byte file that exists on the
live hub PVC. `s.hubSecret` is non-empty in production, which is confirmed by
the fleet authenticating at all: `verifyHeartbeatBearer` fails closed on an
empty master, so an empty `s.hubSecret` would 401 all 66 spokes.

Worth keeping in view rather than fixing: the three spoke-side sites bypass
`provisionMasterSecret()` and so do NOT have the file fallback. That is
correct today (a spoke has no hub PVC) but it means the two resolution orders
are similar enough to be mistaken for each other and different in a way that
is not stated at either site.

## As-built notes for PR #5

The design filed the terminal and invite keys under "CANNOT carry a marker →
use bounded trial verification". Implementing it established that **the first
half is right and the second half does not apply**, so PR #5 is smaller and
differently shaped than this document anticipated. The row above should be read
with this section.

**A spoke has no generation set, and cannot be given one.** Trial verification
means "derive from each live generation and compare". A spoke holds exactly ONE
master (`HIVE_HUB_SECRET`) and ONE injected value per key; nothing in
`saas_provision.go` puts generation material into a Deployment, and nothing
should — shipping the generation set to 66 spokes would hand every tenant
operator the previous master as well as the current one. So on the spoke there
is nothing to trial against, and the machinery the row implies cannot be built
there.

**Nor is it needed, because these two keys are mint-and-verify on the SAME
spoke.** The terminal assertion is minted by `dashboard/session.go` and verified
by `proxy/server.js` in the same pod; the invite token is minted and verified by
the same process. Both sides resolve the key from the same env at the same
instant, so minter and verifier can never disagree about which generation is in
force. Dual acceptance exists to bridge a gap between two parties that rotate at
different times — for these keys there is no such gap. Contrast the heartbeat
bearer (PR #2), where the spoke mints and the HUB verifies: there the two rotate
hours apart and trial verification is exactly right.

What rotation costs here is therefore bounded and self-healing: when the
reconcile lane patches a spoke's `HIVE_TERMINAL_KEY`, in-flight terminal
assertions become invalid and users re-acquire one at their next login — the
assertion's TTL is 15 minutes. In-flight invite links break once; an invalid
invite already degrades to "no attribution", never to an error.

**What PR #5 actually had to fix.** Auditing the two resolvers for rotation
readiness surfaced two live defects, both of which would have made a rotation
incorrect rather than merely inconvenient:

- `TerminalSigningKey()` still carried both FLEET-UNIFORM fallback lanes that
  audit N3 was believed to have closed. N3 closed the gap in PROVISIONING (by
  injecting a per-hive `HIVE_TERMINAL_KEY` so lane 1 wins) but left the lanes in
  the resolver: `HIVE_SESSION_KEY` (measured live: 65/65 spokes, ONE distinct
  value) and `deriveTerminalKeyFrom(master)` (the master is likewise fleet-
  uniform, and that derivation takes no hive ID). Either one resolving means an
  assertion minted on one tenant's spoke verifies on every other — the exact N3
  forgery lane, one absent env var away from being live, which is precisely the
  state a re-provision or manifest reapply produces (see
  `perhive_env_reconcile.go`'s header on the fleet's out-of-band posture). Both
  are deleted; the fallback now SELF-DERIVES the per-hive key from the master
  plus `HIVE_ID`, mirroring `SpokeHeartbeatKey`'s lane 2 (audit F2) so no
  re-provision is required for a spoke to become correct.
- `inviteSigningSecret()` used the RAW MASTER as its HMAC key, and measured live
  that was the lane in force on 65/65 spokes: `HIVE_INVITE_KEY` is emitted by
  the provisioning template but was NOT carried by the reconcile sweep, so no
  live spoke has ever received it. Every spoke signed invites with an identical
  key, and the per-hive binding `provisionInviteKey` exists to provide was in
  force nowhere. Now routed through `hub.SpokeInviteKey()`, which is per-hive in
  both lanes.

**The reconcile lane gained a sixth var.** `EnvInviteKey` was missing from
`desiredPerHiveEnv`/`perHiveEnvNames`, which is why the fleet never got it and
also means a rotation would never have converged the invite key. Adding it is
what makes the invite key rotate at all.

**Fleet effect when this merges.** One pod roll per spoke, rate-limited by the
existing lane at 3 per 15-minute cycle (~6h for the fleet), caused by the newly
reconciled `HIVE_INVITE_KEY`. `HIVE_TERMINAL_KEY` is already present and correct
on 65/65 spokes, so the terminal key is NOT re-keyed by this PR — the resolver
change only alters which key a spoke would compute if that var were ever absent.

**`proxy/server.js`'s terminal lane IS in scope, and is changed here.** PR #6
landed first and explicitly deferred it ("The terminal key path in server.js is
untouched (PR #5's scope)"), which is correct: `TERMINAL_SIGNING_KEY` mirrors
`hub.TerminalSigningKey` and the two must stay in lockstep, so deleting the
fleet-uniform lanes on the Go side while leaving them in Node would leave the
N3 forgery lane live in the verifier — the half that actually decides whether a
shell opens. Both fallbacks are removed there too and replaced with the same
per-hive self-derivation, using `HIVE_ID`, which the proxy already read.

The two derivations are asserted equal byte-for-byte (`HMAC(master, info || 0x00
|| hiveID)`; the 0x00 separator must match exactly, since hive IDs are
attacker-influenced and plain concatenation would be ambiguous).

Note this strengthens the wrong-hive tests in a way worth recording: with a
per-hive key, an assertion minted for hive X and presented on hive Y now fails
on the SIGNATURE rather than only on the payload's `h` claim. The tests that
specifically exercise the `h`-claim check therefore pin the signing hive
explicitly, so they keep testing the claim check rather than silently becoming
duplicates of the signature test.

## As-built notes for F19 (audit 8): wiring the derivation path

The eighth security audit found that the entire rotation wave was **inert**, and
this note records why, because the shape of the mistake is more instructive than
the fix.

`provisionGenerationSet()` — the single function every spoke-bound derivation
flows through — resolved to `legacyGenerationSet(provisionMasterSecret())` in
production. Its only other branch read a package-level override that **nothing
but a test helper ever wrote**. So:

- the hub's MINTING and VERIFYING side used the real persisted set
  (`s.keyGenerations`), which `rotateMasterSecret` correctly advanced;
- the hub's SPOKE-BOUND side rebuilt generation 1 from `/data/saas/hub-secret.key`,
  a file rotation never rewrites.

The two were disjoint. After a rotation the hub minted under generation N+1 while
the reconcile sweep computed "desired" per-hive env from generation 1, compared it
against what the spokes already held, found no drift, and patched nothing. Step 3
of the rotation procedure above silently did not happen.

**Why no test caught it.** Coverage existed on both halves and was split across two
files that never referenced each other: `hub_generations_store_test.go` drove
`rotateMasterSecret`, `perhive_env_generation_test.go` drove `desiredPerHiveEnv`
through the test-only seam. Each half passed. The join — "a real rotation changes
what a spoke would be handed" — was the only thing that was false, and it was the
one thing nobody asserted. `TestRotationReachesDesiredPerHiveEnv` is that test,
and it deliberately asserts `provisionGenerationsOverride == nil` so it can never
decay back into testing the seam instead of the wiring.

### What the fix is

A package-level `liveGenerations` pointer, installed by `NewHubServer` from the
same `loadGenerations` call that populates `s.keyGenerations`, and replaced by
`rotateMasterSecret` and `retireExpiredGenerations` inside their existing
critical sections. Package-level rather than threaded through a receiver because
every caller in the chain (`desiredPerHiveEnv`, the four `provision*Key`
helpers, the provisioning template map) is a free function, several invoked from
template expansion with no `HubServer` in scope.

Three states, not two:

| State | Meaning | Behaviour |
|---|---|---|
| installed | a hub is running | derive from the live set |
| nil, not untrusted | no hub in this process | fall back to legacy — byte-identical to pre-F19 |
| untrusted | F20's `generationsUntrusted` | **refuse**; return nil, derive nothing |

The third is the handoff F20 left open. When the loader cannot establish which
generation is current, the derivation path must not re-derive its own notion from
the raw master file — after a rotation that file holds SUPERSEDED generation-1
material, and writing keys from it onto live Deployments would drag the fleet
backwards onto a retired generation. That is F20's silent un-rotation reappearing
on the side F20 did not cover. "Untrusted" is a separate flag rather than a
sentinel nil because nil must keep meaning "no hub constructed".

### Staleness: the installed set is not unconditionally authoritative

`liveGenerations` is process-global but `provisionMasterSecret()` is re-read from
the environment or the file on every call, so the two can disagree — a second
`NewHubServer`, or an operator changing `HIVE_HUB_SECRET` without the generations
file following. `generationSetDescendsFrom` requires the ambient master to still
be present in the installed set before trusting it. That is sound because rotation
only ever ADDS a generation and demotes the outgoing one, so on a correctly
installed hub the ambient master is still there as current or previous.

### The stranding interlock, and why rotation is gated shut today

F21 measured that **44 of 70 hosted spokes** sit on `pull_only` clusters
(`vllm-d`, `a-ks-wec2`) that the reconcile sweep structurally cannot read or
patch. That is by design and is not changing until the Option D wrapped-master
delivery path lands.

Wiring F19 without addressing that would be actively worse than leaving it inert:
a rotation would converge the 26 reachable spokes, leave 44 on the outgoing
generation, and strand them when retirement fires **unconditionally** at
`verify_until` seven days later. The audit's phrase is "converts a silent no-op
into a 7-day-delayed outage".

So `rotateMasterSecret` now refuses unless `FleetFullyObserved` — the last sweep
saw every admitted hive with none unreachable. A pull-only cluster increments
`UnreachableHives` for every hive on it, so this is **false on the production hub
today and stays false** until every spoke is reachable. The wiring lands now; the
ability to fire it unlocks only when the thing that makes it safe exists.

Three deliberate choices:

- **In `rotateMasterSecret`, not the handler.** It is the single choke point every
  rotation passes through; a handler gate could be bypassed by a future internal
  caller.
- **`force` does not override it.** `force` exists to override the convergence
  COOLDOWN — "the current generation is compromised, accept the cost". With 44
  spokes structurally unreachable the cost is not lag, it is a guaranteed outage
  with no in-band way to avert it. An operator who must rotate a compromised
  master ahead of Option D has to make the fleet observable first.
- **Retirement is NOT gated the same way, and must never be.** Gating retirement
  on convergence would let one unreachable spoke pin a superseded master live
  forever — the failure mode explicit finiteness exists to prevent. Gating the
  START of a rotation strands nobody, because nothing has been demoted yet. The
  two gates correctly point in opposite directions.

Step 4 of the rotation procedure above ("operator watches `safe_to_retire_previous`")
is therefore now enforced at the front of the procedure rather than being advisory
at the back.
