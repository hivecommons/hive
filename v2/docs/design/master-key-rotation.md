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
generation, kid, or secret-version concept anywhere in `v2/pkg/hub`.

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
contract shared with the Node proxy (`v2/proxy/server.js`) and with already-
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
| 5 | Terminal + invite key dual acceptance (spoke-side, symmetric) | M | **Yes** — rolls pods via reconcile |
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
`runHub()` (`v2/cmd/hive/main.go:6943`) is the only caller of
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
