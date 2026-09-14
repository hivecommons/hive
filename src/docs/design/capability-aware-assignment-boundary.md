# Capability-aware contributor task assignment: the hub/relay decision boundary

Status: **Proposed — awaiting maintainer sign-off on [#6825](https://github.com/hivecommons/hive/issues/6825).**

This design records how [RFC #6825](https://github.com/hivecommons/hive/blob/v5/docs/rfc-6825-capability-aware-contributor-task-assignment.md)
lands on the contributor protocol that already exists on `v5`. The RFC is the
product-level proposal — capability tiers, a model-capability evidence policy, a
contributor-offered configuration inventory, and a contributor portal. This page
is narrower: it names the **assignment decision boundary** between the hub and
the relay, ties each half of the boundary to the code that already implements
the closest existing behaviour, and resolves the one boundary question the
capability-negotiation work in [#6954](https://github.com/hivecommons/hive/issues/6954)
deliberately left open — whether the [#6541](https://github.com/hivecommons/hive/issues/6541)
in-flight provider-quota park should become a first-class capacity decline or
stay the `task_failed`/`environment` verdict it is today.

It is a boundary document, not an implementation plan. Where the RFC still has
an open question for maintainers, this page keeps it open and says so; it does
not pre-empt a contested decision by writing code around it.

## Why this needs a written boundary before code

Two properties of the contributor path make "the hub matches a task's
requirements against a relay's capabilities" more dangerous than it sounds, and
both are already load-bearing on `v5`:

- **A relay's declaration is unverified client text.** The capability fields the
  hub reads are an honest self-report bounded on receipt and, in the words of
  the field's own doc comment, "NEVER a trust signal". `ContributorCapabilities`
  in `src/pkg/dashboard/contribute_protocol.go` says so at the struct, and
  `Sanitized`/`sanitizeCapabilityTokens` enforce the bound. Any new
  capability-matching logic inherits that premise: a relay that claims a
  capability has *asserted* it, not *proven* it.
- **Fail-open is the recurring bug class here.** A capability whose value cannot
  be determined must not silently resolve to "capable". This is not a
  hypothetical: [#6909](https://github.com/hivecommons/hive/issues/6909),
  [#6951](https://github.com/hivecommons/hive/issues/6951), and the
  [#6833](https://github.com/hivecommons/hive/issues/6833) tracker all record
  variants of the same failure — an unreadable or unrecognized signal being
  treated as permission. `evaluateContributorQuota` in `bin/contributor-relay.js`
  encodes the corrected shape: `unknown` (a source is configured but unreadable)
  is a HOLD, and only `unprovisioned` (no source configured at all) admits, and
  then warns once. Capability matching must copy that distinction, not the
  pre-#6951 fail-open it replaced.

The rest of this page states the boundary as a set of rules that preserve those
two properties.

## The existing foundation this builds on

Capability-aware assignment is not being invented from nothing. Three mechanisms
already on `v5` are the substrate, and the design reuses them rather than
introducing parallel vocabulary:

1. **Relay→hub capability negotiation (`relay_capabilities` / `DeclaresCapability`).**
   `#6954` added `ContributorCapabilities.RelayCapabilities` (wire field
   `relay_capabilities`) and `DeclaresCapability(token)`, and moved the hub off
   the `RelayProtocolVersion != ""` proxy it used before. The hub now gates on an
   **advertised token**, not on the mere presence of a version string. The relay
   advertises `quota_preflight_v1` (`RELAY_CAPABILITIES` in
   `bin/contributor-relay.js`) because it genuinely implements the preflight
   decline. This is the exact shape capability-aware assignment needs: a stable,
   sanitized, exact-match token vocabulary that an old peer safely ignores. New
   negotiated behaviour is a **new token**, never a redefinition of an existing
   one — the struct comment already warns that whichever of #6954/#6825 lands
   second "must not redefine the token vocabulary silently".
2. **Hub-derived task requirements (`ContributorTaskRequirements`).**
   `TaskRequirementsFromLabels` derives hard routing requirements (container
   runtime, OS, arch, CLI backend, credential type) from issue labels, and
   `ContributorCanRunTask` matches a self-declared client against them. Its
   compatibility rule is the one to preserve: an **undeclared field fits** (old
   relays are treated as unknown, not incapable), and only an *explicit
   contradiction* excludes the client. Capability-tier matching is a new
   requirement axis composed with this one, not a replacement for it.
3. **The admission/credential-after-acceptance flow.** `contributorSupportsQuotaPreflight`
   gates the auto-accept credential hold on the advertised `quota_preflight_v1`
   token: a declaring relay answers an offered task with `task_accepted` or a
   `local_capacity_guard` `task_declined` *before* the scoped credential is
   delivered; a non-declaring relay takes the pre-#6833 immediate-delivery path.
   The credential provably leaves the hub only after an acceptance decision is
   recorded (`deliverTaskCredential`). Capability-aware selection extends this
   handshake; it does not get its own credential path.

## Invariant 1 — the hub owns selection; the relay owns launch and veto

**Statement.** The hub decides *which* task and *which offered configuration*
to propose. The relay decides whether it can *actually launch* that configuration
now, and may veto with a decline. The hub never sends an executable command,
model argument, or environment variable for the relay to run; it sends an
opaque configuration **identifier** the relay resolves locally.

**Rationale.** This is the same split `#6899` is circling from the OMP side:
server-owned selection, admission, generation fencing, and the
credential-after-acceptance rule must stay on the hub, while agent execution and
local resolution stay on the client. A relay that could be told *how* to launch
would let a compromised or buggy hub inject arbitrary local execution; a hub that
let the relay pick the task would lose ordering, ownership, and abuse control.
Splitting on *identifier proposed by hub, resolved by relay* keeps both
authorities intact and matches how `task_assign` already carries metadata while
the relay owns the tmux/headless launch.

**Boundary detail.**

- The hub selects from the inventory the relay advertised, records the
  configuration ID, inventory revision, capability-policy revision, task ID, and
  assignment generation **together** (one fencing tuple), and proposes them.
- The relay confirms the entry still exists, is offered for this task, and can
  launch in its approved local execution mode before it accepts. It resolves the
  identifier to a real launch locally. A stale inventory or failed preflight is a
  decline with a specific reason, not a silent substitution.
- An unexplained substitution — the relay launched something other than the
  proposed configuration — is *surfaced*, never recorded as a successful match.

## Invariant 2 — capability matching fails closed, exactly like the quota guard

**Statement.** When a task carries a capability requirement and the relay's fit
for it cannot be determined, the task does not admit on that relay. "Cannot
determine capability" is a distinct state from "determined capable" and from
"determined incapable", and it is never collapsed into capable.

**Rationale.** This is the direct application of the fail-open bug class above.
`evaluateContributorQuota` already draws the line the matcher must copy:

- **No requirement declared** ⇒ fits, unchanged (the `ContributorCanRunTask`
  "undeclared fits" rule — an old relay or an unlabelled task is not gated).
- **Requirement declared, relay advertises a matching capability token** ⇒ may
  admit, subject to every existing gate.
- **Requirement declared, relay advertises a *contradictory* capability** ⇒
  excluded, the same way a container-runtime contradiction excludes today.
- **Requirement declared, relay's capability is *unknown/unreadable*** ⇒ HOLD /
  do-not-select, mirroring `unknown ⇒ HOLD`. It must not become "capable".

**The one deliberate asymmetry.** Back-compat still requires that a relay
advertising *nothing* is treated as a pre-capability relay and is not stranded
(Invariant 3). That is not fail-open: it is the "no source configured"
(`unprovisioned`) case, which is legitimately different from "a source is
configured but I cannot read it" (`unknown`). The matcher must keep those two
apart with the same care `readContributorQuotaReading` does, because conflating
them is exactly how a fail-open re-enters.

## Invariant 3 — mixed-version behaviour reuses the #6954 matrix verbatim

**Statement.** Capability-aware selection is gated on a **new advertised token**
(call it `capability_select_v1` pending maintainer naming), negotiated the same
way `quota_preflight_v1` is. The four mixed-version cells behave as the existing
matrix already establishes, so no peer is stranded and no peer silently assumes a
switch occurred.

| Relay | Hub | Behaviour |
| --- | --- | --- |
| New (declares the token) | New (understands it) | Full negotiated selection: hub proposes a configuration, relay preflights and acknowledges the binding before the credential is released. |
| New (declares the token) | Old (ignores it) | Old hub never reads `relay_capabilities` for this token; it assigns as it does today. The relay must not assume a hub-selected configuration and falls back to its legacy single-configuration launch. |
| Old (declares nothing) | New | Empty/absent capability list reads as "declares no negotiated capability" and takes the legacy path — the same deliberate compatibility choice `contributorSupportsQuotaPreflight` makes for the quota gate. The old relay is **not** withheld work it can never earn. |
| Old | Old | Unchanged v5 behaviour. |

**Rationale.** `#6931`'s failure — proxying on `RelayProtocolVersion` withheld
the auto-accept credential from *every* relay that declared any version — is the
cautionary tale the matrix exists to prevent. Gating on an advertised token, and
treating an absent token as legacy, is what keeps "new relay ↔ old hub" and "old
relay ↔ new hub" both safe. Capability selection must not reintroduce a version
proxy; it must add its own token.

## Invariant 4 — resolving the #6541 park-path boundary

This is the question `#6954` explicitly declined to answer so it would not
pre-empt `#6825`. State the current behaviour precisely, then the proposal.

**Current behaviour (two different paths for one condition).**

- **At assignment time**, when `evaluateContributorQuota(msg)` holds, the
  `task_assign` handler answers with a first-class `task_declined` whose reason is
  `local_capacity_guard` — *if and only if* the hub advertises `quota_preflight_v1`
  (`hubSupportsQuotaPreflight`). If it does not, the relay closes the connection
  rather than mislabel a capacity hold as `task_failed`. This is the clean,
  pre-emptive, capacity-shaped decline.
- **In flight**, when the provider prints a quota-exhaustion banner *while a task
  is already running*, `#6541`'s `enterQuotaHold` parks the relay and the current
  task is reported as `task_failed` with `failure_kind: 'environment'`. The relay
  log surfaces the banner loudly, but on the wire the hive issue is marked
  **failed**, not **declined for capacity**.

So today the same underlying condition — this account has no provider headroom —
produces a `local_capacity_guard` **decline** if caught at the boundary and a
`task_failed`/`environment` **failure** if caught mid-task.

**Proposal.** Introduce a first-class in-flight capacity verdict — a
`task_parked` / `local_capacity_guard` decline distinct from `task_failed` — so
that a provider-quota park caught mid-task is reported as *capacity withdrawn*,
not *the work failed*. Concretely:

1. **A capacity park is not a task failure.** `failure_kind: 'environment'`
   already means "a verdict about the runtime, not the work", but it still counts
   as a failure for retry/abuse accounting. A dedicated capacity verdict lets the
   hub release the lease and re-offer later *without* charging the task a failed
   attempt, matching how the assignment-time `local_capacity_guard` decline is
   already not a failure.
2. **It stays server-owned.** Per Invariant 1 and the closing sentence of
   RFC #6825's assignment contract, client reports must not independently raise a
   task's tier or suppress it fleet-wide. The relay reports the park; the hub
   decides lease release, re-offer timing, and any cooldown, reusing the existing
   server-owned lease/retry/abuse controls.
3. **It is negotiated, not assumed.** Like every other change here, a hub that
   does not understand the new verdict must degrade to the current
   `task_failed`/`environment` behaviour, so an old hub is never confused by a
   frame it cannot read.

**Why this is the right resolution and why it is still an open question.** The
resolution is *consistent labelling*: capacity exhaustion should read the same
whether it is caught one second before launch or one second after, because it is
the same condition and the contributor's headroom is the thing being protected in
both. But whether the hub should treat an in-flight capacity park as fully
equivalent to a pre-launch decline — in particular, what happens to partial work
already done under the lease — is a maintainer decision with lease-accounting
consequences, so it is listed as an open question below rather than settled here.

## Invariant 5 — self-declared capability is input, not authority

**Statement.** A relay declaring a capability, a configuration inventory, or a
capability tier is providing *self-asserted input*. The hub must not treat any of
it as a grant of trust, GitHub permission, execution privilege, or rate-limit
allowance. Additional offered configurations confer no additional authority.

**Rationale.** The trust model is already written into the `RelayCapabilities`
doc comment and the `Sanitized`/bounds code: every field is honest self-report,
bounded on receipt, never a trust signal. Capability-aware assignment must not
quietly promote self-report into authority. Specifically, the hub must not take
on faith:

- **That a claimed capability is real.** A relay advertising `capability_select_v1`
  has stated it will *answer* a selection proposal, exactly as advertising
  `quota_preflight_v1` states it will answer with accept/decline. It has **not**
  proven it can run any particular model at any particular tier. Evidence about a
  model's capability is the hub's adopted policy (RFC #6825's evidence section),
  kept separate from the relay's self-report — observations feed proposed
  mappings, and only operator adoption makes them active policy.
- **That an offered inventory expands the contributor's rights.** An inventory of
  three configurations is still one active task slot unless capacity was
  separately offered; the relay/session identity, ownership, and rate-limit rules
  stay authoritative, as RFC #6825's inventory section requires.
- **That a client report can change global state.** A relay must not be able to
  raise a task's tier, suppress an issue across the whole hive, or reserve an
  issue indefinitely by declaring something. Those remain server-owned.

**Things the hub must independently enforce regardless of declaration:** existing
holds, dependency gates, existing-PR claims, cooldowns, role restrictions, the
Model Filter, ownership checks, admission, and generation fencing. An `unknown`
capability tier does not exempt an unknown model identifier from the Model
Filter.

## What is intentionally *not* decided here

- **The capability-tier vocabulary and evidence source** (T1/T2/T3 vs `unknown`,
  Artificial Analysis vs operator-authored mappings, score-scale versioning) are
  RFC #6825's to settle. This page only requires that whatever is chosen keeps
  observations separate from adopted policy and treats `unknown` as fail-closed.
- **Advisory vs automatic selection.** RFC #6825 proposes advisory-first with
  automatic selection as a later opt-in. This boundary is written so both fit:
  advisory mode annotates the recommendation and preserves task selection;
  automatic mode uses the same negotiated handshake and server-owned selection.
- **The exact new token names** (`capability_select_v1`, `task_parked`) are
  placeholders pending maintainer naming, so they can be chosen in one place
  without redefining the existing `quota_preflight_v1` vocabulary.

## Open questions for maintainers

1. **Park-path lease accounting (Invariant 4).** When an in-flight provider-quota
   park is reported as a capacity verdict rather than `task_failed`, what happens
   to work already performed under the lease — is the issue re-offered clean, held
   for the same relay after the stated reset, or credited as a partial attempt?
   This is the specific decision `#6954` deferred.
2. **Does the in-flight capacity verdict need its own token, or does it ride
   `quota_preflight_v1`?** The assignment-time decline already lives under
   `quota_preflight_v1`. A mid-task capacity verdict is arguably the same
   capability observed at a different moment, or arguably a new one an old hub
   should not be assumed to understand. Maintainers should pick, because it
   determines the mixed-version cell for the park path.
3. **Where do task capability assessments and maintainer overrides live**, and
   what authorization creates an override? RFC #6825 wants an authorized
   maintainer's explicit value to win over an automated suggestion while
   preserving the suggestion; the storage and authorization surface is unspecified.
4. **Which launch paths get negotiated selection first?** RFC #6825 limits initial
   automatic routing to launch paths that can demonstrate reliable prepare/launch/
   cleanup, leaving others advisory. The concrete first set is a maintainer call.
5. **Relationship to #6899's embeddable-lease question.** If Hive exposes a
   versioned lease API (or an embeddable transport) for hosts like OMP, the
   capability-selection handshake in Invariant 1 must be expressible over it. Is
   that a requirement on the #6899 seam, or does capability selection stay
   WebSocket-only for now?

## References

- RFC: [#6825 design document](https://github.com/hivecommons/hive/blob/v5/docs/rfc-6825-capability-aware-contributor-task-assignment.md)
- Capability negotiation: [#6954](https://github.com/hivecommons/hive/issues/6954) — `RelayCapabilities` / `DeclaresCapability`, `src/pkg/dashboard/contribute_protocol.go`, `contributorSupportsQuotaPreflight` in `src/pkg/dashboard/contribute_ws.go`.
- Contributor quota guard: [#6833](https://github.com/hivecommons/hive/issues/6833) tracker, [#6951](https://github.com/hivecommons/hive/issues/6951) fail-open removal, [#6909](https://github.com/hivecommons/hive/issues/6909); `evaluateContributorQuota` in `bin/contributor-relay.js`.
- In-flight quota park: [#6541](https://github.com/hivecommons/hive/issues/6541); `enterQuotaHold` in `bin/contributor-relay.js`.
- Related boundary discussion: [#6899](https://github.com/hivecommons/hive/issues/6899) — server-authorized assignment handoff for an existing OMP session.
