# RFC #6825: Capability-aware contributor task assignment

Status: proposed design for v5, advisory-first and not yet accepted  
Refs: #6825, #3958, #5698

## Summary

Hive should recommend an appropriate contributor-offered model configuration for
an assignable task by keeping three questions separate:

1. **Task capability requirement:** what level of engineering capability does the
   work appear to need?
2. **Model capability estimate:** what evidence supports treating a specific
   model, harness, and reasoning configuration as suitable for that work?
3. **Availability and preference:** can the contributor run that configuration
   now, and under what limits have they offered it?

Hive owns task requirements and the adopted capability policy. Contributors own
the inventory of configurations they are willing to run. The relay owns local
resolution and launch of the selected configuration. The first release is
advisory: it compares the current/default configuration with the recommendation
without changing scheduling. Per-task automatic selection is a later opt-in
extension that requires protocol negotiation and stronger launch binding.

The expected benefits are fewer avoidable task/model mismatches and better use
of scarce contributor resources. These are hypotheses to validate. This RFC does
not establish that model capability caused any particular failed contribution,
and it does not claim that a higher tier guarantees a successful PR.

## Goals

- Define a contributor-task matching contract that separates task capability,
  model capability evidence, and contributor availability/preference.
- Compose the new design with v5's existing classifier, environment capability
  routing, relay launch metadata, and accepted backend-capacity RFC instead of
  replacing them.
- Let contributors declare multiple offered configurations under one relay while
  preserving the existing single active task slot and contributor identity.
- Keep benchmark observations and benchmark-derived proposals separate from the
  operator-adopted capability policy used for recommendations or selection.
- Ship advisory comparison before opt-in automatic selection.
- Preserve compatibility with existing single-configuration relays.

## Non-goals for v5 phase 1

- Implementing capability routing in this RFC PR.
- Autonomous provider purchasing, installing new CLIs, logging into new
  providers, or buying additional quota.
- Benchmark-based blocking or mandatory known-tier gating.
- Changing contributor trust, GitHub permissions, role grants, or abuse controls.
- Automatic performance-based tier adjustments.
- Treating relay-reported launch metadata as provider attestation.

## Current v5 baseline

v5 already has related mechanisms. This RFC standardizes how capability-aware
assignment composes with them.

- `src/pkg/classify/classifier.go` defines existing complexity tiers
  `Simple`, `Medium`, and `Complex` plus recommendations `haiku`, `sonnet`, and
  `opus` ([lines 10-24](https://github.com/hivecommons/hive/blob/v5/src/pkg/classify/classifier.go#L10-L24)).
  `Classify` defaults to Medium/Sonnet/`scanner`, then computes lane, tier,
  model recommendation, and cluster key ([lines 144-168](https://github.com/hivecommons/hive/blob/v5/src/pkg/classify/classifier.go#L144-L168)).
  The tier rules are keyword/label rules: simple title keywords and `auto-qa`
  labels return Simple, `kind/security`, `kind/regression`, or complex title
  signals return Complex, and unmatched work returns Medium
  ([lines 279-313](https://github.com/hivecommons/hive/blob/v5/src/pkg/classify/classifier.go#L279-L313)).
  Capability-aware assignment **wraps and feeds from** this classifier. It does
  not replace the existing Simple/Medium/Complex fields or silently equate them
  with the new T1/T2/T3 model-capability policy. The current result is one
  explainable signal for task assessment; the new assessment may record
  `unknown`, maintainer overrides, and evidence beyond title/label keywords.

- `src/pkg/dashboard/contribute_protocol.go` already advertises
  `capability_routing` as a server capability for label-derived environment
  routing ([lines 81-85](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_protocol.go#L81-L85))
  and includes it in `auth_ok` server capabilities
  ([lines 98-112](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_protocol.go#L98-L112)).
  Contributor declarations are optional, self-reported, advisory, and not trust
  signals ([lines 127-136](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_protocol.go#L127-L136)).
  The task-side vocabulary covers container runtime, OS, architecture, CLI
  backend, and credential type ([lines 165-175](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_protocol.go#L165-L175)); labels such as
  `needs-docker`, `os/*`, `arch/*`, `backend/*`, and `credential/*` populate it
  ([lines 183-209](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_protocol.go#L183-L209)).
  Unknown clients still fit and only explicit contradictions exclude a client
  ([lines 212-225](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_protocol.go#L212-L225)).
  Model capability selection is distinct from this environment routing: first
  filter work/configurations through existing admission and environment fit,
  then annotate or select among offered model configurations. The existing
  `capability_routing` string should keep its current meaning; per-task
  configuration selection needs a new negotiated protocol capability rather than
  overloading it.

- `src/pkg/dashboard/contribute_ws.go` stores client capabilities from
  `auth_response`, sanitizes them, and records the relay's single
  `cliBackend`, `model`, and `reasoningEffort` on the connection
  ([lines 3820-3874](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_ws.go#L3820-L3874)).
  It advertises protocol version and server capabilities on `auth_ok`
  ([lines 3905-3917](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_ws.go#L3905-L3917)).
  Current task assignment sends a selected `task_assign` and records activity
  using that connection's current backend/model/effort
  ([lines 4018-4055](https://github.com/hivecommons/hive/blob/v5/src/pkg/dashboard/contribute_ws.go#L4018-L4055)).
  The existing contract therefore resolves one active configuration per relay;
  the gap is selecting among several offered configurations inside the same
  relay assignment flow.

- `bin/contributor-relay.js` resolves exactly one startup backend/model/effort
  from environment variables (`AGENT_BACKEND`, `AGENT_MODEL`,
  `AGENT_REASONING_EFFORT`) ([lines 14-24](https://github.com/hivecommons/hive/blob/v5/bin/contributor-relay.js#L14-L24),
  [lines 81-92](https://github.com/hivecommons/hive/blob/v5/bin/contributor-relay.js#L81-L92)).
  It reports best-effort runtime capabilities in `auth_response` and never makes
  failed probes fatal ([lines 629-674](https://github.com/hivecommons/hive/blob/v5/bin/contributor-relay.js#L629-L674)).
  Its launch metadata derives the effective provider/model and reasoning effort
  locally ([lines 865-917](https://github.com/hivecommons/hive/blob/v5/bin/contributor-relay.js#L865-L917)); model detection is limited to configured
  `AGENT_MODEL` or supported local transcript formats, and unknown formats report
  no model rather than guessing ([lines 919-936](https://github.com/hivecommons/hive/blob/v5/bin/contributor-relay.js#L919-L936),
  [lines 1067-1097](https://github.com/hivecommons/hive/blob/v5/bin/contributor-relay.js#L1067-L1097)).
  Interactive and headless launch builders construct argv/flags for the single
  selected process configuration ([lines 1111-1132](https://github.com/hivecommons/hive/blob/v5/bin/contributor-relay.js#L1111-L1132),
  [lines 1220-1255](https://github.com/hivecommons/hive/blob/v5/bin/contributor-relay.js#L1220-L1255)).
  Automatic selection would extend the relay contract with an offered inventory,
  inventory revisions, per-task configuration binding, local preflight, and an
  acknowledgement before task credentials are released. The hub must not send
  arbitrary commands or environment variables.

- RFC #5698 is already accepted for backend capacity, model inventory, pacing,
  and placement. It says Hive should not hardcode model and quota facts as the
  primary source of truth and may use hardcoded fallbacks only as labelled,
  non-authoritative safety nets ([lines 17-20](rfc-5698-backend-capacity-model-inventory-placement.md#summary)).
  In #5698's words:

  > The core invariant is that model and quota facts must not be hardcoded as the
  > primary source of truth. Hardcoded fallbacks are allowed only as labelled,
  > non-authoritative safety nets when a provider cannot enumerate models or when
  > a probe is temporarily unknown.

  Its goals include one shared capacity reading, authoritative model inventory,
  editable operator policy, and avoiding churn of a working agent merely because
  another provider is preferred ([lines 24-37](rfc-5698-backend-capacity-model-inventory-placement.md#goals)).
  Its non-goals include automatic adoption of benchmark rankings and mandatory
  placement ([lines 39-46](rfc-5698-backend-capacity-model-inventory-placement.md#non-goals-for-v5-phase-1)).
  It defines scoped and unscoped capacity readings ([lines 70-105](rfc-5698-backend-capacity-model-inventory-placement.md#capacity-reading)), model inventory
  authority ([lines 116-149](rfc-5698-backend-capacity-model-inventory-placement.md#model-inventory)), editable policy and separate benchmark proposals
  ([lines 151-166](rfc-5698-backend-capacity-model-inventory-placement.md#tier-and-policy-model)), and one producer per provider credential set
  ([lines 189-195](rfc-5698-backend-capacity-model-inventory-placement.md#shared-publication-and-api-surfaces)).
  The policy and publication rules are reused here:

  > Benchmark imports are pluggable and write proposed rankings separately from the
  > adopted policy. A failed refresh, missing API key, or surprising benchmark must
  > not silently rewrite live placements.

  > There is exactly one producer per provider credential set. It publishes capacity
  > readings for the dashboard, governor, hub heartbeat, placement engine, and
  > pacer.

  Contributor-owned configurations should reuse these contracts: shared readings
  per local credential pool, model inventory where authoritative, editable policy
  with adoption history, and benchmark proposals held separate from live policy.

## Data model

### Task capability requirement

A task capability requirement classifies the work Hive intends to assign, not the
importance of the issue or the contributor's trustworthiness.

| Requirement | Meaning | Examples |
| --- | --- | --- |
| **T1 — advanced engineering** | Significant ambiguity, interacting constraints, or difficult correctness reasoning | Concurrency, cross-component refactors, compatibility migrations, security-sensitive logic, difficult debugging |
| **T2 — standard engineering** | Bounded requirements and recognizable implementation patterns | Contained features, ordinary bug fixes, localized refactors, substantive tests |
| **T3 — bounded mechanical work** | Clear transformation, small decision space, and straightforward verification | Typo fixes, established-format documentation edits, repetitive changes with explicit rules |
| **Unknown** | Insufficient or incompatible evidence | Unclassified tasks or tasks whose evidence changed materially |

T1 is the strongest band:

```text
T1 configuration satisfies T1, T2, and T3 recommendations.
T2 configuration satisfies T2 and T3 recommendations.
T3 configuration satisfies T3 recommendations.
Unknown is not an ordered tier.
```

Use an explicit ordering function; do not compare tier strings. Because Hive also
uses T1/T2/T3 for backend support tiers, fields and UI must say **task
capability** or **model capability**, never just **tier**.

The task assessment may feed from `pkg/classify`, issue labels, target branch,
repository context, blast radius, security/concurrency implications, API/schema
changes, migration risk, and test scope. Automated assessments must explain their
signals, may return `unknown`, and should be cached against the work reference and
relevant revision. Network calls or model inference must not run in the critical
section that offers a task. A maintainer override wins over an automated
suggestion and records author, time, and reason.

### Model capability estimate

A model capability estimate describes evidence for a specific evaluated
configuration:

```text
provider + model/revision + harness/version + reasoning setting
```

The launch mode and execution constraints should be retained as context. A
benchmark result for a different tool harness, model revision, reasoning setting,
or permission envelope may not transfer directly to Hive. Model-only estimates
may be allowed only as explicit proxies under adopted policy and must be shown as
approximate.

Relay launch identifiers are separate from benchmark identifiers. A maintained,
auditable mapping connects a local CLI model argument to an evaluated model
identity. Moving aliases, provider-routed defaults, and ambiguous names remain
`unknown` until resolved.

### Availability and preference

The offered inventory is the subset of configurations the contributor authorizes
Hive to consider. Discovering a credential or model locally is not permission to
use it.

Each inventory revision contains entries like:

```yaml
revision: 2026-09-14T15:52:00Z-3
configurations:
  - id: everyday-agy
    backend: agy
    provider: google
    model: example-routine-model
    model_identity: aa:example-routine-model:2026-09
    reasoning_effort: low
    launch_mode: headless
    capability_estimate: T2
    capability_policy_revision: cap-pol-7
    availability: available | unavailable | unknown
    preference:
      order: 10
      use_for: routine
      automatic: true
  - id: deep-codex
    backend: codex
    provider: openai
    model: example-deep-model
    reasoning_effort: high
    launch_mode: headless
    capability_estimate: T1
    availability: available
    preference:
      order: 90
      reserve_for: T1
      automatic: false
      fallback: ask_or_advisory_only
```

Required fields are stable local ID, backend, launch model identifier when
applicable, reasoning setting where applicable, launch mode, capability estimate
or `unknown`, availability state, preference/restriction policy, and inventory
revision. Provider, evaluated model identity, inventory source, and quota reading
are included when known.

The contributor owns declaration and withdrawal of these entries. Preferences can
say “first choice for routine work,” “reserved for T1 tasks,” “do not use a
metered configuration automatically,” or “use the default when capability is
unknown.” Automatic selection requires explicit opt-in for the selected entry and
fallback behavior. Offering three configurations still represents one active task
slot unless the contributor separately offers additional capacity through
existing multi-session mechanisms.

## Capability and policy model

### Keep observations separate from decisions

The policy follows RFC #5698's separation of benchmark observations, proposed
rankings, and adopted operator policy:

```text
Benchmark observations -> proposed capability mappings
                                  |
                           operator adoption
                                  |
                         active capability policy
```

Hanthor's `roles/hive_ops` implementation is useful inspiration, not normative
policy. Its `hive-tiers.sh` mapping uses Artificial Analysis Agentic Index
thresholds of `>=60` for T1, `>=40` for T2, and `<40` for T3. Hive must not
hard-code that third-party benchmark or threshold scale as live policy in this
RFC. Operators select the benchmark source, access arrangement, methodology
version, mapping thresholds, proxy rules, validity windows, and overrides. A
failed refresh, missing API key, license constraint, or surprising methodology
change must not silently rewrite live assignment behavior.

Record source, metric, methodology version, observation time, evaluated model
identity, evaluation settings, proposed mapping version, policy revision, adopter,
and override reason. Preserve the policy revision used for each assignment or
recommendation.

### Editing and adoption

The active capability policy is operator-editable and auditable. It should live
with the same class of governed Hive configuration as #5698 placement policy, not
inside a contributor's local relay file. Contributors decide what they offer;
operators decide what evidence Hive treats as satisfying task requirements.

A refresh service may import benchmark observations into a proposal table. It may
also import operator-authored mappings where external benchmark access or
coverage is unavailable. Applying a proposal to the active policy is an explicit
operator action unless a future policy separately enables auto-adoption. Advisory
mode can display both the active mapping and pending proposals.

### Benchmark source constraints

Artificial Analysis is a candidate source because the issue cites an Agentic
Index implementation and its API documents agentic and coding indices. Selecting
it requires confirming data-access rights for Hive's intended internal use,
portal display, cached derived data, and attribution. Contributor credentials and
task content are not inputs to the benchmark service. The importer must be
replaceable.

## Matching algorithm

Capability matching operates only inside work and configurations already allowed
by Hive and the contributor:

```text
Existing work admission, trust, role, holds, cooldowns, and model filters
                          |
Existing environment capability routing for task/configuration fit
                          |
Task capability requirement
                          |
Offered configurations that satisfy contributor restrictions
                          |
Advisory recommendation or negotiated opt-in selection
                          |
Existing assignment, lease, credential, progress, and completion flow
```

Holds, dependency gates, existing-PR claims, cooldowns, role restrictions, model
filters, ownership checks, and trust decisions remain authoritative. Capability
metadata must not turn held work into actionable work, change global queue totals
to mean “fits this contributor,” or reserve work indefinitely while waiting for a
model.

In advisory mode, Hive preserves existing task selection and annotates the result:
current/default configuration, task capability, current configuration's estimated
capability, recommended offered configuration if any, and fallback. The user can
continue with the current configuration, choose another offered configuration
manually, or choose another task if the portal offers that UX.

In a later opt-in automatic mode, evaluate tasks in existing queue order and pick
a permitted configuration for that task. Preserve operator priority and existing
ranking semantics; do not quietly move important work behind easier work.

Among configurations that meet the recommendation:

1. Apply contributor restrictions and confirmed availability constraints.
2. Honor contributor preference order.
3. Where preference is equal, favor the least capable band that satisfies the
   recommendation.
4. Break remaining ties by stable configuration ID.

When no known configuration matches, show the mismatch and apply the contributor's
declared fallback. The default advisory fallback is to continue with otherwise
eligible existing/default behavior. Unknown capability, provider quota exhausted,
authentication unavailable, and no actionable work must remain separate states.

## Assignment and launch contract

Automatic selection requires a protocol extension beyond adding informational
fields. Existing `capability_declare` and `capability_routing` do not prove that a
relay can launch a hub-selected configuration.

The proposed negotiated lifecycle is:

1. The relay advertises its offered inventory, inventory revision, and supported
   selection behavior while retaining a legacy default configuration.
2. Hive selects a task/configuration pair and records configuration ID, inventory
   revision, capability-policy revision, task ID, and assignment generation.
3. Before accepting, the relay verifies that the entry still exists, is offered
   for this task, and can launch in its approved local execution mode. The hub
   sends identifiers, not executable commands or arbitrary environment variables.
4. In negotiated mode, the relay acknowledges the exact configuration binding
   before Hive releases the task credential. Legacy acceptance timing remains
   unchanged for older peers.
5. The relay prepares a fresh task environment, launches the selected
   configuration, and reports requested and effective configuration where
   observable. An unexplained substitution is surfaced.
6. Progress, completion, retry, reconnect, and credential renewal retain the same
   binding. Reconnect resumes the in-flight assignment instead of picking a newly
   preferred model.

Switch configurations only between tasks. Do not inject a new task into an old
interactive session whose model, instructions, credentials, or working directory
no longer match. Initial automatic selection should be limited to launch paths
that can demonstrate reliable preparation, launch, cleanup, and effective
metadata reporting; other paths stay advisory.

Relay reports are not provider attestation. Missing settings, local aliases,
transcript spoofing, unsupported transcript formats, and runtime `/model` changes
limit what Hive can know. The relay should report requested and effective values
when observable, but policy must tolerate `unknown`.

## Shared publication and API surfaces

Recommended v5 surfaces:

- Extend contributor status/fleet payloads with advisory task capability,
  current configuration capability, inventory revision, and selected/recommended
  configuration ID when negotiated. Existing clients ignore unknown fields.
- Add a server-owned capability-policy view and edit path, reusing the #5698
  policy/adoption pattern.
- Add a contributor-owned offered-inventory declaration path. It must sanitize
  bounded client text like current capabilities do and must not expose local
  credentials.
- Publish availability readings per local credential pool when available. Two
  offered configurations that share one subscription must share one capacity
  reading; they are not independent quota pools.
- Record assignment outcomes with task assessment revision, capability-policy
  revision, selected configuration, effective model/effort report, fallback or
  override, launch outcome, review outcome, and PR disposition.

The initial advisory release does not need detailed quota probing or pacing.
Those should reuse RFC #5698 readings when implemented rather than introducing a
parallel contributor-only quota model.

## Failure, fallback, and compatibility

| Condition | Proposed initial behavior |
| --- | --- |
| Capability feature disabled | Existing v5 assignment behavior |
| Peer lacks configuration-selection support | Use the legacy single-configuration contract; do not assume a switch occurred |
| Benchmark refresh fails or returns invalid/partial data | Retain previous valid observations and adopted policy; show refresh status |
| Methodology changes | Treat new mappings as proposals requiring adoption; do not apply old thresholds silently |
| No usable model evidence or task assessment | Show `unknown`; use otherwise permitted existing/default behavior |
| Evidence becomes stale under policy | Label stale; after its validity window, treat as unknown unless an operator override remains active |
| Selected configuration is withdrawn, unavailable, or cannot launch | Decline/release with a specific reason; try an allowed alternative or report why the contributor is waiting |
| No known capability match | Warn and apply the contributor's declared fallback; do not introduce benchmark gating by default |

Existing single-configuration relays and downstream protocol implementations
remain supported. Optional fields keep safe defaults, and switching semantics
require explicit negotiation. Additional configurations do not grant additional
trust, GitHub permissions, execution privileges, or account rate-limit allowance.

Strict capability matching is future work. If proposed, it must specify unknown
data behavior, stale-data limits, exceptions, and visible waiting states. It
cannot require a known match while silently falling back to unrestricted
assignment during benchmark or inventory outages.

## Rollout plan

These are design stages for discussion, not authorization to create
implementation tasks.

1. **Model metadata:** introduce evidence/adoption separation and display
   capability for the connected configuration. Support operator mappings,
   caching, freshness, and unknown states. Scheduling remains unchanged.
2. **Task assessment and advisory comparison:** add explainable task
   recommendations and maintainer overrides. Compare against the current
   configuration and gather baseline usefulness/overhead data.
3. **Offered inventory and recommendations:** let contributors declare multiple
   configurations and preferences. Recommend only configurations actually
   offered; keep launch selection in the existing contributor flow.
4. **Opt-in automatic selection:** enable negotiated per-task selection only
   after assignment binding, supported launch paths, fallback behavior, and peer
   compatibility have been demonstrated.

Benchmark-driven enforcement, adaptive performance scoring, provider purchasing,
and quota pacing are not prerequisites for these stages.

## Evaluation and validation

Record assignment and attempt outcomes, not only completed PRs. Useful metadata
includes task assessment and revision, selected/recommended configuration,
effective model/reasoning report, capability evidence and adopted policy
revision, selection reason, fallback or override, launch outcome, review outcome,
and eventual PR disposition. This does not require collecting additional task
transcripts or model-provider credentials.

Evaluate maintainer agreement with recommendations, override reasons, major
rework rate, first-attempt completion, time to accepted result, use of
contributor-designated scarce configurations, actual cost where measurable,
unknown/stale/fallback rates, launch mismatches, queue wait time, and
participation by existing contributors. Compare against a baseline within similar
task categories; stronger models receiving harder tasks makes raw merge-rate
comparisons misleading.

Before automatic selection is considered ready, validation must show that:

- Existing admission, environment-fit, model-filter, role, and trust decisions
  still hold for the selected configuration.
- Older peers never silently execute a hub-selected task with a different
  configuration.
- Benchmark failures, partial refreshes, unknown identities, stale evidence, and
  methodology changes preserve documented fallback behavior.
- Inventory withdrawal, reconnects, duplicate acceptance, and assignment
  generations cannot create unintended launches or duplicate ownership.
- Model switches refresh task context and credential state on every supported
  launch path.
- Task or model evidence can change without rewriting the recorded basis of an
  in-flight assignment.

This is a validation bar for later implementation. No implementation or test
execution is claimed by this RFC.

## Open questions

1. Which benchmark source, access arrangement, and versioned mapping should seed
   a pilot? How should coding-focused evidence be compared with Agentic Index
   evidence?
2. Which model-only proxies are acceptable when exact harness or reasoning
   evidence is absent, and how should uncertainty appear?
3. Where should task assessments and maintainer overrides live, and which
   existing repository metadata convention should expose them?
4. Which launch paths should first support automatic selection, and what evidence
   is sufficient to move beyond advisory matching?
5. How should contributor-offered inventory share #5698 capacity readings without
   exposing local credentials or implying control over unoffered resources?

## Acceptance criteria for closing #6825

This RFC is the proposed design artifact for #6825. It captures the separated
questions, composes them with the current v5 baseline, preserves the #5698
benchmark-policy separation, defines contributor-owned inventory and relay
contract changes, states advisory-first phasing, records honest limitations, and
keeps the explicit non-goals out of scope. Subsequent implementation work should
reference this RFC after maintainers accept or amend the design.

Thanks to Hanthor for the `roles/hive_ops` tiering reference and operational
findings, and to the discussions in #3958 and #5698 that shaped the design.
