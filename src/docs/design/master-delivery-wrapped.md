# Wrapped master delivery to pull-only spokes

Status: DESIGN ONLY. No implementation. This document extends
`master-key-rotation.md` and should be read after it.

Decision of record: **Option D** from `MASTER-DELIVERY-OPTIONS-2026-08-14.md`
(2026-08-14). Options A and B are rejected; see §2.

> **§11 status (2026-08-17):** OQ-1 through OQ-5 were all decided by the
> operator on 2026-08-14 — OQ-1
> ([comment](https://github.com/hivecommons/hive/pull/3833#issuecomment-5294455083))
> and OQ-2→OQ-5
> ([comment](https://github.com/hivecommons/hive/pull/3833#issuecomment-5295761881)),
> which closed this document's merge gate ("Implementation is unblocked").
> Stage 1 (X25519 sealed-box primitive + spoke wrap-key lifecycle) has since
> landed as #3873. This design remains the reference record; it is not
> updated post hoc as later stages ship.

## The problem

`master-key-rotation.md` describes a rotation mechanism that is complete on the
hub and delivers nothing to two thirds of the fleet.

Every reconciled per-hive value is a pure function of the master plus the hive
ID. A spoke holding `HIVE_HUB_SECRET` and `HIVE_ID` self-derives all of them:
the heartbeat bearer (`SpokeHeartbeatKey`, `src/pkg/hub/hub_keys.go:485-496`), the invite
key (`SpokeInviteKey`, `:535-544`), the session key (`spokeDomainKey`,
`:443-448`), and even the SSO public key — because it holds the master, it
regenerates the hub's *private* seed via `SSOSigningSeedFromMaster`
(`:576-578`) and takes the public half (`SpokeSSOPublicKey`, `:563-568`).

The reconcile lane is therefore an optimisation, not a dependency. It
pre-computes what the spoke would compute anyway. That is why the fleet ran
normally while the lane was dead, and why the 44 pull-only spokes work today.

**The master is the one value a spoke cannot self-derive.** Delivering a *new*
master after rotation is the lane's only irreplaceable job, and on 44 of 66
spokes there is no delivery path.

`KubectlReachable()` (`src/pkg/hub/saas_provision.go:368-376`) tests `PullOnly` first and
returns false unconditionally. Per the operator correction of 2026-08-14,
`pull_only` is an **intentional architectural boundary on both clusters** — the
hub is not meant to hold write credentials into the heartbeat-only clusters. The
field's own doc comment already says this plainly: a pull-only cluster's spokes
"connect outbound over the heartbeat and nothing here can kubectl into them"
(`src/pkg/hub/saas_provision.go:207-208`).

| Cluster | Spokes | `pull_only` | Reconcile reach |
|---|---:|---|---|
| the hub-reachable cluster (in-cluster) | 22 | no | reachable |
| a heartbeat-only cluster | 39 | **yes, by design** | unreachable |
| another heartbeat-only cluster | 5 | **yes, by design** | unreachable |
| **Total** | **66** | | **22 / 44** |

All 44 are one homogeneous category. There is no "cheap RBAC win" subset.

### The constraint that shapes everything below

> **The hub can never initiate a connection to a spoke cluster.**

Every mechanism in this document — bootstrap, migration, key publication,
master delivery, revocation — must work over the spoke's existing **outbound**
channel. Any step requiring the hub to reach in, even once, even only to seed a
public key, is infeasible. §6 audits the migration path against this
specifically, because an inbound assumption is easy to hide in a migration
step.

This also removes the escape hatch the TOFU problem would normally use. There
is no out-of-band channel *from the hub*. §4 does not get to say "the operator
pins it manually" and move on.

## 1. What Option D is

The spoke generates an asymmetric keypair. The private half never leaves the
pod. It publishes the public half in its heartbeat. The hub encrypts each new
master to that public key and returns the ciphertext on a subsequent heartbeat
response.

The delivery channel need not be confidential, because the payload is already
sealed to a key only the recipient holds. That is the whole of the idea.

## 2. Why not the alternatives

**Option A (grant the hub RBAC / network into the spoke clusters) is
withdrawn.** It proposes reversing a deliberate isolation property on clusters
we do not own. It is not a fallback, not a partial mitigation, and not a
migration step, and it does not appear anywhere below.

**Option B (spoke pulls the plaintext master over its authenticated heartbeat)
is rejected** for the reason recorded in the options paper: it makes the
heartbeat bearer a master-exfiltration primitive. The bearer is
`derivePerHiveKey(master, infoHeartbeatKey, hiveID)`
(`src/pkg/hub/hub_keys.go:350`) — master-derived. An attacker holding a leaked master
derives any hive's bearer, heartbeats as that hive, and is handed each new
master as the operator rotates. Rotation stops evicting them, which is the
property rotation exists to provide. B closes F19 by making RESIDUAL-1
permanent.

Option D closes objections 1 and 3 *provided the wrapping key is not itself
gated on the master*. §4 is about whether that proviso holds. It is the crux of
this document and the answer is qualified.

## 3. Keypair lifecycle

### Algorithm choice: X25519, not Ed25519

The repo's existing asymmetric material is Ed25519 (`src/pkg/hub/sso.go`, `src/pkg/hub/hub_cookie.go`,
`src/pkg/hub/hub_pubkey_generations.go`). **Ed25519 is a signing key and cannot encrypt.**
Reusing it here would require either the Ed25519→X25519 birational map — which
is sharp-edged, easy to get wrong, and creates a key used in two algorithms —
or an ad-hoc scheme. Neither is acceptable for the one payload whose compromise
is fleet-wide.

State it plainly: **this design needs a new key type. Ed25519 is not it.**

The proposal is **X25519 ECDH + HKDF-SHA256 + AES-256-GCM**, an
age-style sealed box:

| Component | Choice | Justification |
|---|---|---|
| KEM | X25519 (`crypto/ecdh`, `X25519()`) | Stdlib since Go 1.20; module is on Go 1.25.6 (`src/go.mod`). No new dependency. 32-byte keys, hex-encodable like every existing env value. |
| Ephemeral | Fresh sender keypair per wrap | Gives forward secrecy against later compromise of the hub's state and makes each ciphertext independently sealed. |
| KDF | HKDF-SHA256 over the shared secret, both public keys, and a context label | Binds the ciphertext to both parties. |
| AEAD | AES-256-GCM, 96-bit nonce | Stdlib, constant-time on the relevant hardware, and the repo already uses AES-256-GCM for `hubbackup` (`src/pkg/hubbackup/backup.go`). Precedent, not novelty. |

`x/crypto/hkdf` is deliberately **not** a direct module dependency today — the
comment at `src/pkg/hub/hub_keys.go:31-33` records that as a considered choice, with
HMAC-SHA256 single-block expansion used instead. This design should follow that
precedent rather than reverse it: a single-block HMAC-SHA256 expansion is
sufficient here for the same reason it is sufficient there (fixed, unique
context labels, one 32-byte output). If the implementer prefers real HKDF, that
is a defensible deviation but it is a **module dependency decision the operator
should make explicitly**, not a side effect of this design. → **Open question
OQ-3.**

The AAD must bind the ciphertext to the recipient and the generation:
`hiveID || 0x00 || generationID || 0x00 || wrappingKeyFingerprint`. The `0x00`
separator matches the existing `derivePerHiveKey` convention, and for the same
stated reason: hive IDs are attacker-influenced and plain concatenation is
ambiguous (`master-key-rotation.md:461-463`).

### Storage

`/data/hive-wrap-key` on the spoke PVC, mode `0600`.

This mirrors an established pattern rather than inventing one. The spoke
already persists private key material on the same PVC at the same mode:
`spokeAppKeyPath = "/data/gh-app-key.pem"` (`src/cmd/hive/main.go:99`) and
`spokeAppKeyDir = "/data"` (`:104`), with `spokeAppKeyFileMode = 0o600`
(`:116`) and the comment "signing material must never be readable by anything
else sharing the PVC or the pod" (`:114-115`). `/data` is the PVC mount in the spoke template
(`src/pkg/hub/saas_provision.go:2585`), and `/data/hive-id` (`src/cmd/hive/main.go:5297`)
already establishes that identity-critical state persists there across
restarts.

Following that precedent, the path should be a `var` not a `const`, so tests
can redirect it and exercise the real resolution order — the reason given at
`src/cmd/hive/main.go:95-97`.

### First boot, pod roll, PVC loss

These are one mechanism, because they must be:

1. On start, read `/data/hive-wrap-key`. If present and well-formed, use it.
2. If absent or malformed, generate a fresh X25519 keypair and persist it
   **before** using it. (Persist-before-use mirrors
   `rotateMasterSecret`'s persist-before-install ordering and its stated
   rationale: never act on key material you might forget at the next roll —
   `master-key-rotation.md:339-341`.)
3. Publish the public half on every heartbeat, unconditionally, not only when
   it changes. It is a public value; re-sending it is cheap, idempotent, and
   removes an entire class of "hub missed the one beat that carried it" bug.

A pod roll with the PVC intact is therefore a no-op — same key, same
publication. **PVC loss is indistinguishable from first boot**, by construction:
the spoke generates a new keypair and publishes it. Whether the hub should
*accept* that new key is §4 and §7, and it is the sharpest question in this
design.

### Rotating the wrapping key itself

The wrapping key must be rotatable independently of the master, or it becomes a
permanent credential — the F1/F2 failure mode this project has spent five
audits removing.

Proposed: the spoke rotates its wrapping key when any of these hold — the key
is older than `wrapKeyMaxAge` (suggested 90 days, operator-settable), the key
file is malformed, or the operator triggers it. Rotation is: generate, persist,
publish the new public key, and **retain the previous private key** for a
bounded overlap (suggested 24 hours, comfortably more than the heartbeat
interval) so a master wrapped to the old key and still in flight can be
opened.

That overlap is a second dual-acceptance window, and it inherits the same
discipline the master generations carry: it must be **explicitly versioned**
(the previous key is a numbered entry with a fingerprint, not an unnamed `if`
branch) and **explicitly finite** (it carries an expiry, and an expired
wrapping key is excluded, not warned about). `master-key-rotation.md:128-141`
sets that standard and it applies here without modification.

Note the asymmetry that makes this safe: the wrapping key protects *delivery*,
not *authentication*. A spoke that loses its wrapping key entirely can still
heartbeat, still self-derive, and still function. It simply stops being able to
receive a new master until it republishes. Fail-closed here degrades to
"cannot receive", never to "authenticates as someone else".

## 4. Trust on first use — the crux

The hub must be sure the public key it wraps a master to belongs to that hive
and not to an attacker who heartbeated first.

### What the heartbeat bearer already proves

The bearer is verified by `verifyHeartbeatBearerAcrossGenerations`
(`src/pkg/hub/hub_keys.go:339-366`). Every candidate is
`derivePerHiveKey(g.Secret, infoHeartbeatKey, hiveID)` (`:350`), and the
function's banner states the invariant: "identity-bound under EVERY generation
... There is deliberately no code path here that derives without hiveID; if one
ever appears, F2 is re-opened" (`:322-327`). `handleHeartbeat` verifies the
bearer against the *claimed* `hive_id` after parsing the body
(`src/pkg/hub/server.go:1406`; the N1 comment at `:1395-1405` explains why the check must
follow the parse — the per-hive bearer is derived from the claimed ID, so the
ID must be parsed and validated first).

So a caller presenting a valid bearer for hive X **has proven possession of
`derivePerHiveKey(master_g, infoHeartbeatKey, X)` for some live generation g**.

That is a real and non-trivial proof. It is per-hive, it is constant-time, and
the fleet-wide lane that would have made it meaningless was deleted in F2.

### What it does NOT prove

It does not prove the caller is the legitimate spoke. It proves the caller
holds a value derivable from the master plus the hive ID. Specifically it does
not exclude:

- **Anyone who holds the master.** RESIDUAL-1 measures the master as plaintext
  in the PodSpec of all spokes, one distinct value fleet-wide. Any party who
  reads any one PodSpec derives *every* hive's bearer.
- **Anyone who reads one spoke's `HIVE_HEARTBEAT_KEY`** — bounded to that hive,
  which is the F2 property working as intended.
- **A replayed or re-provisioned instance** of the same hive.

### Does this reintroduce the §6 circularity? — Yes, partially. Say it plainly.

**Yes.** If the hub accepts a wrapping key solely because it arrived on a
bearer-authenticated heartbeat, then the wrapping key's trustworthiness is
bounded by the master's secrecy, because the bearer is master-derived. The
delivery is confidential against a *network* adversary and against anyone
holding only the old ciphertext — but **not** against an adversary holding the
leaked master.

Trace the attack concretely. An attacker leaks the master at generation N.
Before the spoke publishes (or at any point where they can win the race, or
after any event that legitimately causes republication — see §7):

1. They derive hive X's bearer from master N.
2. They heartbeat as X, publishing **their own** X25519 public key.
3. The hub records it as X's wrapping key.
4. The operator rotates to N+1. The hub wraps master N+1 to the attacker's key.
5. The attacker decrypts and now holds master N+1.

Rotation has not evicted them. **This is objection 3 — the objection Option B
was rejected for — reappearing inside Option D**, via the trust bootstrap
rather than via the delivery.

The honest statement is therefore:

> Option D removes the master from the *delivery* path but not automatically
> from the *trust* path. Wrapping alone does not restore rotation-as-remedy.
> Whether it does depends entirely on the pinning rule, and a naive
> accept-latest-on-valid-bearer rule does not.

Anything less than this is the "resolved-looking answer" the brief warns
against.

### What actually fixes it, and what it costs

The fix is **first-publication pinning with the master excluded from the
re-pinning decision**:

- **TOFU-pin.** The first wrapping key the hub sees for a hive is recorded with
  its fingerprint and the time. Thereafter it is the hive's key.
- **A pinned key is never silently replaced.** A heartbeat presenting a
  *different* public key for a hive that already has one is **not** accepted on
  bearer authority alone, no matter how valid the bearer is. It is recorded as
  a pending change and surfaced as an alert. The hub keeps wrapping to the
  pinned key.
- **Re-pinning requires an operator decision** — an explicit admin action
  naming the hive and the new fingerprint.

This breaks the circularity *after* the pin, because master compromise no
longer suffices to change where masters get wrapped. It does not break it
*before* the pin: the very first publication is accepted on bearer authority,
so a pre-pin attacker still wins.

### The pin is not a race for a spoke that cannot publish

The trace above says "before the spoke publishes (or at any point where they
can win the race)", and the rest of this document was written as though the
pinning event is a contest the legitimate spoke normally wins — it is already
running, it publishes every beat, the attacker has a narrow window. **For a
meaningful part of the fleet that framing is wrong, and it changes how OQ-1
should be read.**

A hive only publishes a wrapping key if it is running the new image and able to
heartbeat. For any hive that is not, there is no race at all — **the attacker
pins by default, unopposed, with no timing requirement whatsoever.** The
categories are not hypothetical:

- **Not yet rolled to the new image.** Migration step 1 reaches pull-only
  spokes via self-upgrade, which is not instantaneous or simultaneous. Between
  the hub gaining the pin store and the last spoke rolling, every unrolled hive
  is unopposed.
- **Offline, paused, or crash-looping.** A paused spoke publishes nothing. So
  does one in a crash loop.
- **Stuck in `provisioning`.** The same status that §6's corrected clause exists
  to stop leaving the denominator.
- **Already denied by a hostile pin.** Self-sustaining: once the attacker pins,
  §8 row 2 refuses the legitimate spoke's publication forever, so the hive can
  never take the pin back without operator action.

For every hive in those categories the attacker does not need to be fast, only
first — and against a spoke that never publishes, *any* time is first. The
window is not milliseconds; it is "until that spoke successfully publishes",
which for a paused or unrolled hive is unbounded.

Two consequences:

1. **The exposed population is not "spokes an attacker outraces" but "spokes
   that cannot publish at the moment the pin store goes live".** That is a
   number the operator can measure before committing, and should — see the
   re-stated OQ-1 in §11.
2. **Sequencing is security-relevant, not merely operational.** The interval
   between the hub accepting pins and the fleet finishing its roll is the
   exposure window, and it should be minimised deliberately rather than allowed
   to run for however long a self-upgrade takes. §6 step 1 should complete for
   as much of the fleet as possible *before* step 2 begins accepting pins.

This does not change the pinning rule, which is correct as written. It changes
the estimate of how much of the fleet the rule protects on day one.

**This is unavoidable given the constraints, and it should be stated as a
residual rather than engineered around.** The hub cannot reach in to seed a
key. There is no out-of-band channel from the hub. The only pre-existing shared
secret with a pull-only spoke *is* the master. Bootstrapping trust from
something that is not the master requires something the spoke or the
provisioning process supplies — which is §5.

The cost of the pinning rule is operational: a genuine PVC loss produces a new
key, which is refused, which requires an operator to re-pin. That is a real
cost and it is the correct direction — the alternative is a design where losing
a PVC and being attacked are indistinguishable and both silently succeed.

## 5. Can the trust anchor come from provisioning?

This is the only candidate for a non-master anchor, since the hub cannot reach
in and there is no out-of-band hub channel.

Provisioning is the moment a spoke is created, and the hub controls the
manifest even for pull-only clusters — the manifest is applied by an operator
context, not by the hub. The template already injects per-hive secret material
(`src/pkg/hub/saas_provision.go:1966-1986`: `HeartbeatKey`, `SessionKey`, `SSOPublicKey`,
`TerminalKey`, `InviteKey`), and the `/secrets` read-only projected mount
(`src/pkg/hub/saas_provision.go:2763`) already carries private key material at provision
time — `spokeProvisionedAppKeyPath = "/secrets/gh-app-key.pem"`
(`src/cmd/hive/main.go:98`), which the spoke holds "from its very first boot —
before any heartbeat has run" (`:108`).

So there is an existing, precedented channel for giving a spoke a secret at
birth that the hub does not have to reach in to deliver.

**For newly provisioned spokes, this closes the TOFU gap properly.** Provision
a per-hive *enrolment* value — not master-derived — and require the first
wrapping-key publication to be authenticated by it. Master compromise then does
not let an attacker pin a key on a hive they have not also provisioned.

**For the 66 spokes that already exist, it does not**, because delivering an
enrolment value to them requires either reaching in (infeasible) or sending it
over the master-authenticated heartbeat (circular again). Their first pin is
irreducibly TOFU-on-bearer.

That is the honest bottom line, and it defines the shape of the migration:

> **The 66 existing spokes bootstrap under TOFU-on-bearer and carry that
> residual permanently. Newly provisioned spokes bootstrap from a provisioned
> enrolment value and do not.** The population carrying the weaker anchor is
> fixed at 66 and shrinks as hives are reprovisioned; it never grows.

→ **Open question OQ-1**: whether the operator accepts a one-time
TOFU-on-bearer pin for the existing 66, given that the current state is
strictly worse (the master is plaintext on all of them *and* rotation reaches
none of them). My assessment is that they should, because the pin is taken once
under today's threat conditions and every subsequent rotation is protected,
whereas the status quo protects none. But it is a security-posture decision and
it is genuinely the operator's.

An honest framing for OQ-1: **the pin is a snapshot of trust taken at a moment
you must simply choose to believe in.** If the master is already leaked today,
the pins taken today are already compromised, and no mechanism available over
an outbound-only channel can detect that. What pinning buys is that the *next*
leak is remediable. That is a real improvement and it is not the same as
"secure".

## 6. Migration path

Reviewed against the no-inbound constraint. Every step below is either
spoke-local, spoke-outbound, or hub-local. **No step requires the hub to
initiate a connection to a spoke.**

| # | Step | Initiator | Fleet-visible | Inbound needed |
|---|---|---|---|---|
| 1 | Spoke generates + persists keypair on boot, publishes public half every beat | spoke (outbound) | image roll only | **no** |
| 2 | Hub records publications, pins first-seen, counts coverage | hub-local | no | **no** |
| 3 | Readiness surface reports wrap-key coverage; rotation gated on it | hub-local | no | **no** |
| 4 | Rotation wraps to pinned keys; ciphertext returned on heartbeat response | spoke pulls | no | **no** |
| 5 | Spoke decrypts, persists new master, re-derives | spoke-local | no | **no** |
| 6 | Enrolment value added to provisioning template for new hives (§5) | operator applies manifest | new hives only | **no** |

Step 1 is the only one needing a spoke image roll, and it needs **no hub
action at all** — which is precisely why it works for pull-only clusters. The
spoke's image is updated by its existing self-upgrade path
(`HeartbeatResponse.SwitchToTag`, `src/pkg/hub/heartbeat.go:1694-1698` (field at `:1698`)), whose doc comment
records that this exists for exactly this reason: "Used for branch switches on
clusters the hub can't reach over kubectl — the spoke has in-cluster RBAC
(hive-self-upgrade role) to patch its own deployment." The delivery mechanism
for the fix is one the pull-only boundary already accommodates.

Step 4's carrier has direct precedent. `HeartbeatResponse.PendingGateway`
(`src/pkg/hub/heartbeat.go:1728-1733`) is a **secret** delivered on the heartbeat response,
queued hub-side (`src/pkg/hub/openrouter.go:217`), drained on delivery, and its doc
comment states its purpose: "the delivery channel for firewalled/heartbeat-only
spokes (on a heartbeat-only cluster) the hub cannot POST to directly ... The hub sends it once
(drained on delivery) rather than every beat, since it carries a secret key
value." Wrapped-master delivery is the same shape with a strictly stronger
payload — sealed rather than plaintext — so it should reuse the queue/drain
structure rather than invent one. The "send once, drained" property should
**not** be copied verbatim, though: see §7 on replay and on the ack.

### The interlock: reuse, do not reinvent

`42dd2cce` already built the interlock and it is exactly the right shape.
`PerHiveEnvStatus` carries `UnreachableHives`, `UnreachableClusters` and
`FleetFullyObserved` (`src/pkg/hub/perhive_env_reconcile.go:552-580`), with
`FleetFullyObserved = out.ConsideredHives > 0 && out.UnreachableHives == 0`
(`:673`) — failing closed on a zero-considered sweep, because "I looked at
nothing" is not "I looked at everything". `SafeToRetirePrevious` is gated on it
(`:752-755`).

The wrap-key coverage counter should be **a new clause in the same computation,
not a parallel one**:

```
SafeToRetirePrevious = hasPrevious
                    && FleetFullyObserved
                    && SpokesOnPrevious == 0
                    && VerifyUntil has passed
                    && HivesWithPinnedWrapKey == ConsideredHives   // new
```

One critical adjustment. `FleetFullyObserved` is defined in terms of
*Deployment-read* observation, and pull-only spokes are unreachable by
definition — so under Option D the 44 must become observable by a different
route: **their heartbeat publication is the observation.** A hive that is
publishing a pinned wrapping key and receiving wrapped masters is converged,
even though the hub cannot read its Deployment.

This is a genuine semantic change to a security-critical predicate and it must
be made deliberately and visibly. `master-key-rotation.md:216-227` warns
specifically against heartbeat-sourced readiness, because
`AuthRolloutReadiness` drops hives unseen for 24h and cannot distinguish "hive
absent" from "hive never existed" — a paused spoke silently leaves the
denominator and the fleet reads ready. That warning is correct and it applies
here.

The resolution is that the **denominator must stay registry-sourced** while
only the **numerator** becomes heartbeat-sourced. A paused or vanished
pull-only spoke then keeps its place in the denominator, never publishes, and
correctly blocks retirement. What must never be built is a denominator of
"hives that heartbeated recently". → this is the single most likely place for
a test to pass for the wrong reason; see §10.

#### Correction (adversarial review B-2): the clause above was wrong

The clause first proposed here was
`HivesWithPinnedWrapKey == ConsideredHives`. **It does not implement the
property this section claims for it, and it fails open in two independent
ways.** It is recorded rather than deleted because the reasoning error is
instructive and because a future reader may otherwise re-derive it.

**(a) `ConsideredHives` is not the registry denominator.** It is the count of
hives that survived a status filter:

```go
// src/pkg/hub/perhive_env_reconcile.go:427-429
func perHiveEnvSweepEligible(status string) bool {
	return strings.TrimSpace(status) != "provisioning"
}
```

It is incremented only after that filter passes
(`src/pkg/hub/perhive_env_reconcile.go:811-816`) and published as
`out.ConsideredHives = s.perHiveEnvConsidered`
(`src/pkg/hub/perhive_env_reconcile.go:665`). A hive in `provisioning` is
therefore absent from **both** sides of the equality, and `0 == 0` is true.
That is exactly the paused-spoke-leaves-the-denominator failure that
`master-key-rotation.md:216-227` warns about and that this section claimed to
have avoided.

And a `provisioning` hive *can* already hold master-derived material: the
template injects it at provision time
(`src/pkg/hub/saas_provision.go:1966-1986`), i.e. before the status leaves
`provisioning`. So the exclusion is not harmless here.

Note *why* the exclusion is safe for the existing counters and not for this
one. The doc comment at `src/pkg/hub/perhive_env_reconcile.go:381-426` justifies
it as "a cheap-cycle optimisation, not a safety gate", and that is correct **for
a Deployment-read numerator**: a hive the sweep never reads records no
observation, so it can never count as converged. A pin-store numerator does not
inherit that property, because the pin is read from hub-local state rather than
from the spoke, and can be present for a hive the sweep never admitted. The
filter's safety argument is numerator-specific, and this design changes the
numerator. **Reusing a filter without re-checking the argument that made it
safe is the actual root of this defect.**

**(b) Equality between two independently-sourced counts is not a coverage
check.** `ConsideredHives` is recomputed each sweep from `listSaaSHives()`;
`HivesWithPinnedWrapKey` would be a count over a hub-local pin store. Nothing
constrains the two to describe the same hive set. Deprovision a hive and leave
a stale pin entry, and the numerator holds while the denominator drops — the
equality goes **true** while a *different* hive is unpinned. The existing code
is deliberate about this hazard: `perHiveEnvGeneration` takes its attribution
"from the SAME read that produced the drift list, so the two can never describe
different moments"
(`src/pkg/hub/perhive_env_reconcile.go:877-879`).

**The corrected clause.** Do not compare counts. Carry unpinned hives the way
`UnreachableHives` is carried — a counter incremented **inside the same sweep
loop**, over the same iteration of `listSaaSHives()`, with the pin looked up per
hive:

```
SafeToRetirePrevious = hasPrevious
                    && ObservedHives > 0
                    && FleetFullyObserved
                    && SpokesOnPrevious == 0
                    && SpokesUnattributed == 0
                    && windowClosed
                    && HivesWithoutPinnedWrapKey == 0   // new
```

This preserves the pattern every existing clause already uses — a `== 0` floor
or a `> 0` liveness check, never an equality between two separately-sourced
totals (`src/pkg/hub/perhive_env_reconcile.go:752-757`, and `FleetFullyObserved
= out.ConsideredHives > 0 && out.UnreachableHives == 0` at `:673`). It cannot go
true on a zero sweep, and a hive present in the registry with no pin is
*counted* rather than silently absent.

Increment it at the same point `noteUnreachable` is
(`src/pkg/hub/perhive_env_reconcile.go:803-809`).

**And count it before the status filter, not after.** `HivesWithoutPinnedWrapKey`
must be incremented for every hive returned by `listSaaSHives()`, including
those skipped by `perHiveEnvSweepEligible`, because a `provisioning` hive can
hold key material and must not be able to leave the denominator by sitting in a
status. This makes the clause's denominator deliberately *wider* than
`ConsideredHives` — that divergence is the point of the fix, not an
inconsistency, and it should carry a comment saying so, or a future tidy-up will
"align" the two and silently reinstate the hole.

### What still needs owner buy-in

Steps 1–5 need **nothing** from the owners of the heartbeat-only clusters — no
RBAC, no network, no manifest change. The spoke rolls itself via a mechanism
already in use on those clusters.

Step 6 (the provisioning enrolment value) changes the manifest applied when a
*new* hive is created on those clusters. That is a provisioning-time change on
someone else's cluster and **does need their agreement**, though it grants the
hub no new access — it adds a projected secret to a Deployment the operator
already applies. → flag for the heartbeat-only clusters owners.

Nothing in this design asks either cluster to accept inbound connections from
the hub, grant the hub RBAC, or weaken `pull_only`. That is the point.

## 7. Can RESIDUAL-1 close?

**Not fully. This is the honest answer, and the bootstrap ordering problem is
real.**

Trace what reads `HIVE_HUB_SECRET` on a spoke at boot:

| Reader | Location | Still needed once a wrapped master arrives? |
|---|---|---|
| `spokeDomainKey` (session key) | `src/pkg/hub/hub_keys.go:443-448` | No — dedicated var, or the delivered master |
| `SpokeHeartbeatKey` lane 2 | `src/pkg/hub/hub_keys.go:489-495` | **Yes at first boot** — see below |
| `SpokeInviteKey` lane 2 | `src/pkg/hub/hub_keys.go:539-543` | No |
| `SpokeSSOPublicKey` lane 2 | `src/pkg/hub/hub_keys.go:567` | No |
| `provisionMasterSecret` | `src/pkg/hub/hub_keys.go:584-592` | Hub-side only; not a spoke reader |

The blocker is the second row and it is circular in exactly the way the brief
anticipates:

> **The spoke needs a master to authenticate the heartbeat that delivers the
> master.**

If `HIVE_HUB_SECRET` is stripped and `HIVE_HEARTBEAT_KEY` is present, the spoke
authenticates on lane 1 (`src/pkg/hub/hub_keys.go:486-488`) and never needs the master —
fine. But after a rotation, `HIVE_HEARTBEAT_KEY` is stale. On a reachable spoke
the reconcile lane patches it. **On a pull-only spoke nothing patches it**, and
the spoke cannot self-derive a fresh one without the master. So it must
authenticate with the *old* bearer, which works only while the previous
generation is still acceptable (`acceptableGenerations`,
`src/pkg/hub/hub_generations.go:222-242`, default 7 days).

This yields a workable but strictly bounded property:

- **A pull-only spoke can have `HIVE_HUB_SECRET` stripped** once it holds a
  pinned wrapping key and a valid `HIVE_HEARTBEAT_KEY`.
- **It must then receive and apply each wrapped master within
  `defaultVerifyWindow`** (7 days, `src/pkg/hub/hub_generations.go:118`), because after that
  its old bearer stops verifying and it has no way to derive a new one.
- **A spoke that is paused, offline, or fails to decrypt for longer than the
  verify window is permanently stranded** and requires operator intervention on
  its own cluster.

That last bullet is the cost of closing RESIDUAL-1 on pull-only spokes, and it
is not small. Today a stranded pull-only spoke self-heals the moment it comes
back, because it holds the master and re-derives everything. After stripping,
it does not.

Options, with trade-offs, for the operator:

| Approach | Strands? | RESIDUAL-1 |
|---|---|---|
| (a) Strip on the 22 reachable only | no | closes 22/66 |
| ~~(b) Strip on all 66 after wrap-key coverage is complete~~ | **struck — see below** | — |
| (c) Strip on all 66, and deliver a fresh **heartbeat key** alongside the wrapped master | no | closes fully; more machinery |

### Why (b) is struck rather than offered as a trade

(b) was originally listed as a legitimate availability-versus-exposure choice.
**It is not a trade, because the stranding is attacker-triggerable from the same
one-PodSpec-read position that starts the whole attack.** Attacks 1 and 6 of the
adversarial review compose, and the composition is what makes (b) unacceptable:

1. An attacker reads any one spoke PodSpec and obtains the fleet-wide master.
2. They pin their own wrapping key on a hive that has not yet published (§4).
3. The legitimate spoke's later publication is now **refused** by §8 row 2 —
   which is the correct behaviour, and is precisely what prevents that spoke from
   ever receiving a wrapped master.
4. Retirement is unconditional and runs on the hub clock
   (`src/pkg/hub/hub_generations.go:222-242`,
   `src/pkg/hub/hub_generations_retire.go:238-244`), so the attacker simply waits
   out `defaultVerifyWindow` (7 days,
   `src/pkg/hub/hub_generations.go:118`).
5. Under (b) the spoke's master is already stripped, so when its old bearer stops
   verifying it has no way to derive a new one. It is **permanently stranded**.

**The hostile pin causes the stranding it then hides behind.** The refusal in §8
row 2 is load-bearing security behaviour and must not be weakened — so the
availability cost cannot be engineered out of (b); it can only be avoided by not
choosing (b).

The gain to the attacker is disproportionate: one PodSpec read converts a
confidentiality incident into a durable, fleet-scale availability incident
requiring operator intervention **on clusters the hub cannot reach into by
design** — up to 44 spokes. That is a worse outcome than the exposure (b) was
meant to reduce.

(b) is struck rather than left in the table with a caveat because it is the
option a reader skimming the table would plausibly pick as the "simpler" one.
**Do not reinstate it.** If a future reader is tempted, the test is whether
§8 row 2 still refuses a mismatched key: while it does — and it must — (b)
carries an attacker-controlled stranding trigger.

**(c) is the recommendation.** It is a small addition — the wrapped payload
carries the new master *and* the spoke's freshly derived `HIVE_HEARTBEAT_KEY`
for generation N+1 — and it removes the stranding mode by making the delivery
self-sufficient. The spoke applies both atomically or neither.

Note that (c) does **not** reintroduce Option B: the payload is sealed to the
spoke's pinned wrapping key, so possession of the master does not let an
attacker read it, and possession of a bearer does not either.

→ **Open question OQ-2**: (a) or (c). (b) is struck, not offered. The
remaining choice is scope — close RESIDUAL-1 on 22 spokes with no new
machinery, or on all 66 with the heartbeat key delivered alongside. My
assessment is (c). **Operator's call.**

## 8. Failure modes, all fail-closed

The governing principle is the one already stated for `VerifyUntil.IsZero()`
meaning ALREADY EXPIRED (`src/pkg/hub/hub_generations.go:232-238`) and for the
absent-versus-unreadable distinction (`src/pkg/hub/hub_generations_store.go:175-198`): a
state that cannot be trusted must never quietly widen what is accepted or
revert what is current.

| Failure | Behaviour | Rationale |
|---|---|---|
| Hub cannot parse/validate a published public key | Reject the publication. Keep any existing pin. Count the hive as **not** having a usable key, so it blocks `SafeToRetirePrevious`. | A malformed key must not silently unpin a good one. Blocking retirement is the safe direction. |
| Published key differs from the pinned key | **Do not accept.** Keep wrapping to the pinned key. Raise an alert naming the hive and both fingerprints. Requires an operator re-pin. | §4. This is the clause that breaks the circularity; if it ever becomes "accept latest", Option D degrades to Option B. |
| Spoke cannot decrypt a wrapped master | Spoke keeps its current master, does **not** apply, logs loudly, does **not** ack. Hub re-sends. | Applying a partially-decoded master would brick the spoke. Not-acking is what makes the hub retry. |
| Replay of an old wrapped master | Rejected. The AAD binds generation ID; the spoke refuses a generation ≤ its current. | Without this, an attacker who captured a generation-N ciphertext could roll a spoke back to a retired master after N+1 has shipped. **The ciphertext is not confidential in transit by assumption, so replay resistance cannot come from the channel.** |
| Hub queue drained but spoke never applied | Hub re-queues until the spoke's heartbeat **acks the applied generation ID**. | This is where `PendingGateway`'s "send once, drained on delivery" must **not** be copied. Delivery is not application. Ack on generation ID, not on receipt. |
| PVC loss on the spoke | New keypair generated + published; differs from pin → refused per row 2 → operator re-pins. Spoke continues on its existing master meanwhile. | Indistinguishable from attack, so treated as attack, with an operator escape hatch. Availability cost accepted deliberately. |
| Clock skew | Wrapping-key overlap and `VerifyUntil` are both hub-evaluated on the hub clock. The spoke never makes an expiry decision. | Single clock. A spoke with a skewed clock cannot extend its own acceptance window. |
| Hub has no pinned key for a hive at rotation time | Rotation proceeds (retirement is unconditional by design, `src/pkg/hub/hub_generations_retire.go:238-244`) but `SafeToRetirePrevious` stays false and the alert names the hive. | Matches the existing anti-pin property: an unconverged spoke must not be able to hold a superseded master open indefinitely. |

Two notes on the last row. Retirement remaining unconditional is deliberate and
should not be weakened by this design — `src/pkg/hub/hub_generations_retire.go:238-244`
argues it correctly. Option D's job is to make convergence *achievable* for the
44, not to make retirement wait for them.

And the `maxLiveGenerations = 2` bound (`src/pkg/hub/hub_generations.go:107`) is unaffected:
wrapping does not add generations, it adds a delivery mechanism for the one
being introduced. The two-HMAC trial-verification bound argued at `:92-99`
stands unchanged.

## 9. Threat model

**Defends against:**

- A network adversary on the heartbeat path. The payload is sealed; the channel
  need not be confidential.
- An adversary who reads a spoke's *PodSpec* after RESIDUAL-1 closes. The
  wrapping private key is on the PVC at 0600, not in the PodSpec.
- An adversary holding a leaked master, **for every rotation after the pin is
  taken**. This is the property Option B forfeits and the reason for choosing D.
- Cross-hive delivery confusion. The AAD binds `hiveID`, so a ciphertext for X
  cannot be opened as Y even if misrouted.

**Does NOT defend against:**

- An adversary holding the leaked master **before the pin is taken**. §4. For
  the existing 66 this is a one-time, permanent residual — and per §4's "the pin
  is not a race for a spoke that cannot publish", for any hive not publishing at
  that moment the adversary pins **by default rather than by racing**.
- An adversary with read access to the spoke's PVC or process memory. They hold
  the wrapping private key and the master. Out of scope, and unchanged from
  today.
- A hostile spoke *operator*. They legitimately control the pod. Unchanged.
- Cluster admins on the heartbeat-only clusters. They can read the PVC. Unchanged,
  and inherent to hosting on someone else's cluster.

**Does rotation remain a remedy for a leaked master?**

**Yes, after the pin — and this is the whole justification for the option.**
Once a hive's wrapping key is pinned, an attacker holding master N cannot
change where master N+1 is wrapped (§8 row 2), cannot decrypt a ciphertext
sealed to a key they do not hold, and cannot pass the AAD binding for another
hive.

**But "rotating evicts them" is two different claims with two different
timelines, and only one is immediate.** An earlier draft of this section said
"Rotating to N+1 evicts them", unqualified. That is the kind of
resolved-looking sentence this document elsewhere refuses to write. Precisely:

| | Timeline |
|---|---|
| **Evicted immediately at rotation** | All *future* masters. The bearer for N+1 is derived from master N+1 (`src/pkg/hub/hub_keys.go:485-496`), which is sealed to a pinned key the attacker does not hold. They cannot obtain N+1 or anything after it. |
| **Retained for `defaultVerifyWindow` (7 days)** | Everything master N itself derives, **fleet-wide**, until generation N leaves `acceptableGenerations`. |

Generation N remains live for verification for 7 days after rotation
(`defaultVerifyWindow`, `src/pkg/hub/hub_generations.go:118`), excluded only on
the hub clock (`src/pkg/hub/hub_generations.go:222-242`). During that window an
attacker holding master N retains, for **every hive in the fleet**:

- **Heartbeat impersonation of any hive.**
  `verifyHeartbeatBearerAcrossGenerations` accepts any live generation
  (`src/pkg/hub/hub_keys.go:339-366`, loop at `:348`).
- **Invite signing.** `SpokeInviteKey` (`src/pkg/hub/hub_keys.go:535-544`) is
  symmetric and both minted and verified spoke-side.
- **Session and terminal cookies.** `spokeDomainKey`
  (`src/pkg/hub/hub_keys.go:443-448`) is the verify key. Cookie lifetime is
  `cookieMaxAgeDays` = 7 days, which is exactly why `defaultVerifyWindow` is 7
  days (`src/pkg/hub/hub_generations.go:109-118`).
- **Fleet-wide SSO minting.** The sharpest one.
  `SSOSigningSeedFromMaster` (`src/pkg/hub/hub_keys.go:576-578`) yields the
  Ed25519 **private** seed; its own doc comment at `:570-575` states "This is
  PRIVATE-key material: it can mint SSO tokens".

So the honest statement is:

> **Rotation evicts the attacker from all future masters immediately, and from
> heartbeat impersonation, invite signing, session cookies and fleet-wide SSO
> minting only after `defaultVerifyWindow` — 7 days. Rotation starts a clock;
> it does not close the door.**

This remains a decisive improvement over Option B, where the attacker is
*never* evicted from anything, so the option choice is unaffected. But an
operator responding to a known master leak needs to know they have a 7-day tail,
not a clean cut, and may want to force `VerifyUntil` forward to shorten it —
accepting that doing so strands every unconverged spoke, which is the same trade
§7 frames.

**No, before the pin.** An attacker who pins their own key first receives every
subsequent master, and — because re-pinning requires operator action — a
legitimate spoke's later publication would itself be refused, making the
compromise *persistent and visible* rather than persistent and silent. Visible
is a meaningful improvement over Option B, where it would be silent. But it is
not prevention.

The pinning event is therefore the single highest-value moment in this design,
and §5 is the only way to make it not depend on the master. For the existing 66
it cannot be made so.

## 10. Test strategy

Fourteen tests in this repo have been found **encoding** vulnerabilities rather
than catching them — `heartbeatBearerOK`'s test literally asserting the hub
"must accept raw master secret" (F24) is the canonical example, and F19's
`TestDesiredPerHiveEnvUsesCurrentNotPrevious` passes while validating a code
path production never takes. The house standard is therefore: positive controls
in both directions, source-asserting invariants where a behavioural test would
pass for the wrong reason, and regression replays.

### Properties most at risk of a test that passes for the wrong reason

Ranked, because these are where I would expect the failure:

1. **The re-pin refusal (§8 row 2).** A behavioural test that publishes a new
   key and asserts "the hub still wraps to the pinned key" passes trivially if
   the wrap-to-pinned code path is never exercised — e.g. if no rotation occurs
   in the test. **Required shape:** publish key A, pin, rotate, assert the
   ciphertext opens with A's private key; then publish key B on a valid bearer,
   rotate again, and assert the ciphertext **still** opens with A and **not**
   with B. The positive control is that after an explicit operator re-pin, it
   opens with B and not A. Without both directions this test asserts nothing.

2. **Wrap-key coverage in `SafeToRetirePrevious` (§6).** This is the F19 shape
   exactly, and **the test originally specified here was itself an instance of
   the failure it was meant to prevent.** It asked for a source-asserting test
   that the denominator does not read from a heartbeat-recency set. That test
   **passes against the broken clause**, because `ConsideredHives` genuinely is
   not heartbeat-sourced — it is just the wrong set. The test asserted a true
   property of the wrong variable. It is retained here as a worked example: a
   source-assertion is only as good as its choice of subject, and "not sourced
   from X" is much weaker than "sourced from the set I actually need".

   **Required shape — the test must FAIL against `HivesWithPinnedWrapKey ==
   ConsideredHives` and pass only against the `== 0` floor:**

   - **`provisioning` hole.** Register a hive whose status is `provisioning`,
     give it **no** pin, leave every other hive pinned and observed, and assert
     `SafeToRetirePrevious` is **false**. Against the equality clause this hive
     is absent from both sides and the gate reads true — the test fails, which
     is the point. Assert on `HivesWithoutPinnedWrapKey > 0` directly as well,
     so the diagnosis does not depend on reading the composite bool.
   - **Stale-pin / deprovision skew.** Register hives A and B. Pin A, then
     deprovision A leaving its pin entry in the store, and leave B unpinned.
     Assert `SafeToRetirePrevious` is **false**. Against the equality clause the
     counts coincide and the gate reads true while B — a *different* hive — is
     unpinned.
   - **Zero-sweep floor.** With no hives at all, assert the gate is false, so
     the new clause cannot resurrect the "I looked at nothing" reading that
     `FleetFullyObserved`'s `ConsideredHives > 0` exists to block.
   - **Positive control.** Every hive in the registry pinned and observed,
     window closed → assert `SafeToRetirePrevious` is **true**. Without this the
     first three assertions pass against a gate that is unconditionally false.
   - **Source-assertion, corrected subject.** Assert that the unpinned counter
     is incremented over the **full** `listSaaSHives()` iteration and not behind
     `perHiveEnvSweepEligible` — i.e. that the count of hives examined for a pin
     is `>= ConsideredHives`, in the style of the existing
     `f16_owner_gate_test.go` count-floor invariants. This is what pins the
     denominator to the registry rather than to the status filter, and it is the
     assertion the original spec should have made.

   Per §10's own framing, write these **before** the clause.

3. **AAD binding.** A test that decrypts successfully proves the happy path and
   nothing about binding. **Required shape:** wrap for hive X, attempt to open
   as hive Y, assert failure; wrap for generation N, attempt to apply as N-1,
   assert failure. Each with a positive control that the correctly-bound case
   succeeds — otherwise a test asserting "decryption fails" passes when
   decryption is broken outright.

4. **The Ed25519-is-not-an-encryption-key invariant.** Worth a source-asserting
   test that no Ed25519 key material reaches the wrapping path, because a future
   refactor "unifying the key types" is exactly the plausible mistake and a
   behavioural test would not notice until the crypto silently weakened.

5. **The stripped-master bootstrap (§7).** If the operator chooses (c),
   the atomicity of "new master and new heartbeat key applied together or
   neither" must be tested by injecting a failure between them and asserting the
   spoke is left on the *old* pair, still able to authenticate. A test that only
   exercises the success path would pass against an implementation that bricks
   spokes on a partial write.

### Regression replays

- **F2 replay.** Assert no code path derives a wrapping-key trust decision
  without `hiveID`, mirroring the banner at `src/pkg/hub/hub_keys.go:314-327`. If a
  publication can be accepted without an identity-bound bearer, F2 is re-opened
  through a new door.
- **F19 replay.** Assert the wrap path reads the hub's *live* generation set,
  not a test-only override. The F19 lesson is precisely that a seam-injected
  test proves nothing about production; this design adds a second consumer of
  `provisionGenerationSet()` and must not repeat it.
- **F20 replay.** A pinned-key store that cannot be read must fail closed
  (block retirement), never fall back to "no pin, accept latest" — which would
  silently convert D into B on an I/O fault. This is the same
  absent-versus-unreadable distinction, and it deserves the same treatment.
- **Option B replay.** A test asserting that a valid heartbeat bearer alone
  **cannot** cause a plaintext master to be emitted on any response path. This
  is the invariant that separates D from the rejected option, and nothing
  currently enforces it.

## 11. Open questions for the operator

**OQ-1 — Is a one-time TOFU-on-bearer pin acceptable for the existing 66?**

*Re-stated after adversarial review. The original framing was materially
misleading and the decision should be made against this one.*

The original text presented the residual as a race the legitimate spoke
normally wins, and asked the operator to accept a narrow timing risk. §4's
"the pin is not a race for a spoke that cannot publish" shows that is wrong. The
accurate proposition is:

> **For any hive that is not running the new image and heartbeating at the
> moment the hub begins accepting pins, an attacker holding the master pins
> first by default — with no timing requirement and no race to win. The
> exposure window for such a hive is not milliseconds; it lasts until that hive
> successfully publishes, which for a paused, crash-looping, unrolled or
> `provisioning` hive is unbounded.**

That is a different question from the one originally posed, and it should be
answered on these terms:

- **What is actually being accepted.** Not "a small chance an attacker outraces
  a spoke", but "every hive that cannot publish on day one is pinned by whoever
  holds the master, and stays that way until an operator re-pins it".
- **The quantity is measurable, not hypothetical.** Before committing, count the
  hives that are paused, crash-looping, in `provisioning`, or not yet rolled.
  That count — not 66, and not zero — is the population taking the weaker
  anchor under adversarial conditions. **This should be measured, not
  estimated.**
- **The exposure is reducible by sequencing.** Rolling step 1 to as much of the
  fleet as possible before step 2 begins accepting pins shrinks the population
  directly. This is the main lever available and it costs nothing but ordering.
- **What is unchanged.** The status quo remains strictly worse in the aggregate:
  today the master is plaintext on all 66 *and* rotation reaches none of them.
  Pinning makes the *next* leak remediable for every hive that pins honestly.
- **What detection does and does not give you.** A hostile pin makes the
  legitimate spoke's later publication fail loudly (§8 row 2) rather than
  silently — real, and better than Option B. But per §8 row 7 that alert shares
  a code path and shape with routine PVC loss, so it is only as useful as the
  operator's ability to tell the two apart.

**My assessment — mine, not a recommendation the operator should adopt
unexamined:** still accept, but the reasoning is narrower than before. It holds
because the alternative is not "a safer pin" — there is no safer pin available
over an outbound-only channel — it is "no pinning at all, and rotation continues
to reach none of the 44". Accepting a bounded, measurable, sequencing-reducible
exposure to gain remediability for every future rotation is the better trade.
**But this now depends on a fleet-state measurement that has not been taken, and
if that count is large the sequencing work in §6 should be treated as a
prerequisite rather than an optimisation.**

**Operator's call**, and it is a genuinely different call than the one the
original OQ-1 described.

**OQ-2 — Strip the master on (a) 22 reachable, (b) all 66 with a stranding
mode, or (c) all 66 with the heartbeat key delivered alongside?** (§7). My
assessment: (c). **Operator's call**, since it trades availability against
exposure.

**OQ-3 — HKDF as a module dependency, or single-block HMAC-SHA256 expansion?**
(§3). `src/pkg/hub/hub_keys.go:31-33` deliberately avoided the dependency. Following
precedent is defensible and so is deviating; it should be decided explicitly.

**OQ-4 — Wrapping-key max age and overlap window.** Suggested 90 days / 24
hours (§3). These are policy numbers with no derivation behind them and should
be set by whoever operates the rotation cadence. They must be named constants
with comments recording the reasoning, per the house standard.

**OQ-5 — Does step 6 have the heartbeat-only clusters owner agreement?** It changes
the provisioning manifest on their clusters, though it grants the hub no new
access (§6). Steps 1–5 need nothing from them.

## 12. What this design deliberately does not do

- It does not weaken `pull_only`, request RBAC, or assume any inbound path.
- It does not make retirement conditional on convergence
  (`src/pkg/hub/hub_generations_retire.go:238-244`).
- It does not change `maxLiveGenerations`, `VerifyUntil` semantics, or the
  heartbeat bearer format — the last of which is a contract with deployed
  Deployments (`src/pkg/hub/hub_keys.go:300-306`).
- It does not fix F19 or F20. Those remain prerequisites; this design describes
  what F19's convergence should *deliver to* once it works.

## Sequencing

1. F20 (fail closed on unreadable generations file) — independent, small.
2. Spoke keypair generation + publication (§3, migration step 1). Ships in the
   spoke image, needs no hub action, reaches pull-only clusters via
   self-upgrade.
3. Hub-side pin store + coverage counter folded into `FleetFullyObserved` (§6).
4. F19 (wire `provisionGenerationSet()` to the live set).
5. Wrapped delivery on the heartbeat response (§6 step 4-5).
6. Only then: rotate. Only then: consider stripping the master (§7).

Steps 2 and 3 can proceed in parallel and neither is fleet-visible beyond an
image roll. Step 6 is gated on the coverage interlock reaching the full 66.
