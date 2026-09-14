# RFC #6825: Capability-aware contributor task assignment

Status: proposal — open for maintainer discussion, not accepted  
Target: Hive v5  
Scope: Contributor relay, task selection, model capability metadata, contributor portal  
Initially out of scope: Autonomous provider purchasing, benchmark-based blocking, changes to contributor trust, and automatic performance-based tier adjustments  
Refs: [#6825](https://github.com/hivecommons/hive/issues/6825), [#5698](https://github.com/hivecommons/hive/issues/5698), [#5691](https://github.com/hivecommons/hive/issues/5691), [#3958](https://github.com/hivecommons/hive/issues/3958)  
Related design: [RFC #5698: Backend capacity, model inventory, pacing, and placement](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5698-backend-capacity-model-inventory-placement.md)

> **This page is the design under discussion, not an adopted one.** The text is
> the proposal from [#6825](https://github.com/hivecommons/hive/issues/6825),
> committed verbatim so it can be reviewed as a diff and amended in place
> rather than edited in an issue body. Its own rollout section is explicit that
> the stages listed "are design stages for discussion, not authorization to
> create implementation tasks", and the five decisions in
> [Alternatives and decisions for maintainers](#alternatives-and-decisions-for-maintainers)
> are open. Nothing here has shipped.
>
> The maintainer hold that originally sat at the top of the issue was lifted on
> 2026-09-14; that lifted the bar on working the issue, and did not adopt the
> design. Compare [RFC #5698](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5698-backend-capacity-model-inventory-placement.md),
> whose status line reads "accepted design" — this one deliberately does not.
>
> **Branch note.** The design targets v5, and its sibling RFCs live on `v5`.
> This copy lands on `v4` because that is the branch day-to-day work targets;
> `v5` is topped up from it by the forward-port sync described in
> [v5-sync-policy.md](../src/docs/v5-sync-policy.md). The links below point at
> `v5` blobs and a pinned v5 commit on purpose: they are the code the proposal
> was written against.

## Summary

Extend v5's contributor system so Hive can recommend—and, with contributor opt-in, select—an appropriate model configuration for each task from the configurations that contributor has offered.

The design separates three questions:

1. **Task capability requirement:** What level of engineering capability does this task appear to need?
2. **Model capability estimate:** What evidence supports using this model, harness, and reasoning configuration for that work?
3. **Availability and preference:** Can the contributor run that configuration now, and under what limits or preferences have they offered it?

Hive supplies task requirements and adopts the capability policy. The contributor supplies the configurations they are willing to run. The relay remains responsible for resolving and launching the selected local configuration.

For example, a contributor might offer:

| Local configuration | Backend | Capability estimate | Contributor preference |
| --- | --- | --- | --- |
| `everyday-agy` | Agy | T2 | First choice for routine work |
| `everyday-codex` | Codex | T2 | Alternative for routine work |
| `deep-codex` | Codex | T1 | Reserved for T1 tasks |

These are illustrative assignments, not benchmark claims about particular products. Each configuration represents a specific model and reasoning setting. Hive could recommend either routine configuration for a contained bug fix and the reserved configuration for a difficult compatibility migration.

The expected benefits are fewer avoidable task/model mismatches and better use of contributor resources. These are hypotheses to validate: this RFC does not establish that model capability is the cause of any particular failed contribution, or that a higher tier guarantees a successful PR.

The recommended first release is advisory. Automatic selection is a later opt-in extension. Benchmark-based blocking is outside the initial scope.

## Existing v5 foundation

This proposal extends existing mechanisms; capability routing is not entirely new to v5.

- **A relay has one active backend/model configuration.** The launcher resolves backend, model, and reasoning settings for the relay process. Contributors can already run separate relays under one account using session labels. What is missing is selecting among several offered configurations within one relay's assignment flow. See the [v5 relay](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/bin/contributor-relay.js#L81) and [multiple-session documentation](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/docs/contributor-relay.md#running-multiple-backends-under-one-account).
- **Task complexity already exists.** `pkg/classify` assigns `Simple`, `Medium`, or `Complex` and maps those to Haiku/Sonnet/Opus recommendations. That is useful existing infrastructure, but it is not an evidence-based comparison of the configurations a contributor offers. See the [classifier](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/pkg/classify/classifier.go).
- **Environment requirements already influence assignment.** v5 derives requirements from labels covering container runtime, OS, architecture, backend, and credential type. It skips explicit contradictions while preserving assignment for undeclared clients. The protocol advertises `capability_routing`; this is separate from estimating model competence. See the [protocol](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/pkg/dashboard/contribute_protocol.go#L165) and [routing tests](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/pkg/dashboard/contribute_protocol_test.go#L540).
- **Provider placement has an accepted v5 design.** RFC #5698 defines shared capacity readings, model inventory, editable policy, and benchmark proposals separate from adopted policy. Acceptance of that document does not mean every implementation phase has shipped. This RFC should reuse those contracts for contributor-owned resources. See the [accepted design](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5698-backend-capacity-model-inventory-placement.md).
- **Launch metadata already provides a starting point.** The relay derives effective reasoning settings for supported launch paths and can detect model names for some backends. These remain relay reports, not provider attestation; missing settings, aliases, and runtime changes can limit what is known. See the [launch metadata logic](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/bin/contributor-relay.js#L887).

The new capability is therefore task-specific matching between an offered configuration inventory and a model capability policy, composed with v5's existing admission and environment checks.

## Inspiration and credit

This proposal was inspired by Hanthor's [`roles/hive_ops`](https://github.com/hanthor/dotfiles/tree/master/roles/hive_ops) and the operating experience captured in [#3958](https://github.com/hivecommons/hive/issues/3958) and [#5698](https://github.com/hivecommons/hive/issues/5698).

His [`hive-tiers.sh`](https://github.com/hanthor/dotfiles/blob/67f13e153b590362b3b49bc57ab8c4b249293f5f/roles/hive_ops/files/bin/hive-tiers.sh) implements an Artificial Analysis Agentic Index mapping:

```text
Agentic Index >= 60  -> T1
Agentic Index >= 40  -> T2
Agentic Index < 40   -> T3
```

The script retains its previous tier cache on refresh failure. The [rotation script](https://github.com/hanthor/dotfiles/blob/master/roles/hive_ops/files/bin/hive-rotate.sh) uses a built-in table when the cache is missing or too old, and the [timer](https://github.com/hanthor/dotfiles/blob/master/roles/hive_ops/files/systemd/hive-tiers.timer) schedules weekly refreshes.

An important qualification: in [#5698](https://github.com/hivecommons/hive/issues/5698), Hanthor reported that the refresher had not successfully run in his fleet because its API key was unset. The reference demonstrates the refresh and fallback design; it should not be cited as evidence that automatic benchmark tiering has already improved contributor outcomes.

This RFC carries forward the ideas of vendor-neutral evidence, explicit capability floors, and visible fallbacks. It applies them to contributor task assignment rather than resident-agent rotation.

## Capability tiers and their meaning

Start with three broad capability bands and an explicit `unknown` state:

| Capability tier | Task characteristics | Examples |
| --- | --- | --- |
| **T1 — advanced engineering** | Significant ambiguity, interacting constraints, or difficult correctness reasoning | Concurrency, cross-component refactors, compatibility migrations, security-sensitive logic, difficult debugging |
| **T2 — standard engineering** | Bounded requirements and recognizable implementation patterns | Contained features, ordinary bug fixes, localized refactors, substantive tests |
| **T3 — bounded mechanical work** | Clear transformation, small decision space, and straightforward verification | Typo fixes, established-format documentation edits, repetitive changes with explicit rules |
| **Unknown** | Insufficient or incompatible evidence | An unclassified task or a configuration without a defensible capability mapping |

These classify the assigned work, not its importance. Documentation can require T1 investigation; a configuration change can alter security or compatibility. File count, labels, and product names are insufficient on their own.

T1 is the strongest band. Define compatibility explicitly:

```text
T1 configuration satisfies T1, T2, and T3 recommendations.
T2 configuration satisfies T2 and T3 recommendations.
T3 configuration satisfies T3 recommendations.
Unknown is not an ordered tier.
```

Use an explicit ordering function rather than comparing tier strings or their numeric suffixes.

Hive also uses T1/T2/T3 for [backend support tiers](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/docs/backend-support-tiers.md#the-three-tiers). Those describe launch-path support and confinement. Fields and UI must therefore say **model capability** or **task capability**, and never overload an existing support-tier, trust-tier, or complexity field.

## Model capability evidence and adopted policy

### Keep observations separate from decisions

Follow RFC #5698's separation between imported evidence and adopted operator policy:

```text
Benchmark observations -> proposed capability mappings
                                  |
                           operator adoption
                                  |
                         active capability policy
```

A refresh may propose changes. It must not silently alter the policy used for automatic assignment. Operators can adopt a batch of mappings and override an individual mapping with a recorded reason. A future automatic-adoption option would be a separate, explicit policy choice.

Record enough provenance to explain a mapping: source, benchmark metric and methodology version, observation time, evaluated model identity, evaluation settings, mapping version, and whether an operator overrode it. Preserve the policy revision used for each assignment.

### Identify the configuration accurately

The capability identity should accommodate:

```text
provider + model/revision + harness/version + reasoning setting
```

Also retain the relevant launch mode and execution constraints as context. A benchmark result obtained with different tools, runtime limits, or permissions may not transfer directly to Hive's environment.

Keep the contributor's launch identifier separate from the benchmark identifier. Matching requires a maintained, auditable mapping; never turn a benchmark slug into a CLI model argument by guessing. Moving aliases, provider-routed defaults, and ambiguous names remain unresolved until their identity is known.

Reasoning settings belong in the identity from the start, but do not automatically add a tier for `high` or `max`. Labels differ across backends. Prefer evidence for the matching evaluated configuration. A model-only estimate may be offered as an explicitly marked proxy under operator policy; a known mismatch in model revision or reasoning configuration must not be presented as an exact match.

### Choose a source without freezing a score scale

Artificial Analysis is a candidate source. Its current API documents Agentic and Coding indices, pagination, and null values that mean unmeasured rather than zero. The Coding and Agentic indices are not independently versioned; their interpretation depends on the surrounding methodology. See the [API documentation](https://artificialanalysis.ai/data-api/docs).

The `60/40` thresholds above describe Hanthor's implementation, not proposed universal defaults. Absolute thresholds avoid promotion merely because stronger models disappear from a comparison set, but they are meaningful only against a compatible scoring methodology. Artificial Analysis's [version history](https://artificialanalysis.ai/methodology/intelligence-benchmarking#version-history) illustrates why that qualification matters.

For an initial pilot, use one documented, versioned mapping and evaluate whether its suggestions agree with maintainer judgments. Compare Agentic and coding-focused evidence during calibration. Do not average unrelated benchmark scores or claim they estimate a probability of successful completion.

The importer should be replaceable. Operator-authored mappings can support the same advisory workflow where external coverage or access is unavailable.

### Data access and publication

Artificial Analysis's [API access page](https://artificialanalysis.ai/data-api) distinguishes internal use from redistribution, and its [Data Platform Terms](https://artificialanalysiscdn.com/legal/ProDataPlatformTerms.pdf) address derived data, attribution, and model-selection products. Selecting it requires confirming the rights for Hive's intended portal display and any redistributed cache or derived tier table. A free API key should not be assumed to authorize those uses.

Fetch through an operator-configured service, keep the API key server-side, and publish only what the selected data license permits. Contributor credentials and task content are not inputs to the benchmark service.

## Contributor-offered configuration inventory

The offered inventory is a subset of what the contributor could technically run. Discovering a model or finding a credential is not permission to consume it.

Each offered configuration needs a stable local ID, backend, provider where known, launch model identifier, reasoning setting, applicable launch mode, availability status, and contributor preferences or restrictions. The inventory has a revision so an assignment cannot accidentally select an entry whose meaning has changed.

Optional policies can express preferences such as:

- Prefer one configuration for routine work.
- Reserve another configuration for T1 tasks.
- Do not use a metered configuration automatically.
- Use a designated default when capability information is missing.

The contributor controls these limits. Automatic selection requires opt-in covering the configurations and fallback behavior offered. It does not install another CLI, log into another provider, purchase credits, or enable a more permissive execution mode.

Model-provider credentials stay local. Hive continues to manage task-scoped GitHub credentials through the existing contributor flow; these are a different credential class.

An inventory of three configurations still represents one active task slot unless the contributor separately offers more execution capacity. The relay/session identity stays stable across configuration switches. Existing multi-session and multi-hub ownership and rate-limit rules remain authoritative.

If availability metadata is added, share a reading per local credential pool and preserve scoped model limits, as RFC #5698 describes. Two configurations using the same subscription are not independent quota pools. Detailed quota probing and pacing are not prerequisites for advisory capability matching.

## Task classification

Classify the work Hive intends to assign, using the issue's requirements and available repository context. Assess ambiguity, interacting components, existing implementation patterns, API/schema changes, concurrency, security implications, migration needs, test scope, and blast radius. Assignments without enough task-specific context remain unknown; a delegated role name alone does not establish the task's difficulty.

Reuse [`pkg/classify`](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/pkg/classify/classifier.go#L279) as a source of signals, but do not silently equate its current result with a validated capability requirement. Its title-keyword rules can classify a task as simple before considering more complex signals, and unmatched titles default to medium. The new assessment should explain what evidence supports its recommendation and allow `unknown` when that evidence is insufficient.

For example:

```text
Suggested task capability: T2
Reasons:
- Existing implementation pattern in the affected component
- No identified API, schema, or concurrency change
- Focused regression coverage required

Source: automated assessment
Maintainer override: none
```

Start with an explainable rubric. Deterministic signals can suggest a band; an optional model-assisted assessment may investigate cases that need more context. Classification should be cached and bounded in cost, with timeouts producing `unknown`. Network calls and model inference must not run inside the task-selection critical section.

Key assessments to the canonical work reference and relevant revision: source content, labels, target branch, and repository evidence consulted. Reassess when that basis materially changes. An old classification must not silently describe a substantially revised task.

An authorized maintainer's explicit value wins over an automated suggestion. Store the value, author, time, and reason; preserve the underlying suggestion for comparison. Ordinary issue text requesting a tier is input to assessment, not an authoritative override. A label or metadata syntax should follow existing repository conventions and authorization rules.

A failed attempt can trigger reassessment, but must not automatically promote a task. Authentication, quota, tooling, ambiguous requirements, and implementation failures need different responses. Repeated failures should not become a loop that repeatedly launches a stronger model without new evidence.

## Matching and queue behavior

Capability matching operates within the work and configurations already permitted by Hive and the contributor.

```text
Existing work admission and contributor authorization
                          |
              Task capability assessment
                          |
     Offered configurations passing applicable filters
                          |
          Recommendation or opt-in model selection
                          |
              Existing assignment and lease flow
```

Holds, dependency gates, existing-PR claims, cooldowns, role restrictions, model filters, and ownership checks remain authoritative. An unknown capability tier does not mean an unknown model identifier is exempt from the existing Model Filter. See the [admission implementation](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/pkg/dashboard/contribute_admission.go) and [model filter](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/pkg/dashboard/contribute_ws.go#L5041).

For multiple configurations, evaluate backend/model and environment compatibility against the configuration being considered. An accepted default configuration must not authorize another entry that violates the same filters.

In advisory mode, preserve task selection and annotate the recommendation. In opt-in automatic mode, examine tasks in the existing queue order and select a permitted configuration for the task. Preserve operator priority and existing ranking semantics; resource optimization should not quietly send important work behind easier tasks.

Among configurations meeting the recommendation:

1. Apply contributor limits and confirmed availability constraints.
2. Honor contributor preference order.
3. Where preference is equal, favor the least capable band that meets the recommendation.
4. Break remaining ties by stable configuration ID.

This is a predictable initial policy, not a claim to minimize monetary cost. A T1 configuration may be cheaper than a T2 configuration, and API token prices may not describe a subscription's marginal cost. Contributors should be able to express those preferences directly.

When no known configuration matches, show the mismatch and apply the contributor's chosen fallback. The default advisory behavior permits continuing with the otherwise eligible current/default configuration. Any preference to choose another task or wait is explicit and visible.

Capability metadata must not turn held work into actionable work, change global queue totals to mean “fits this contributor,” or reserve an issue indefinitely while waiting for an unavailable model.

## Assignment and launch contract

Automatic selection requires a protocol extension beyond adding informational fields.

Older peers ignore unfamiliar messages and fields. Neither `capability_declare` nor v5's existing `capability_routing` token proves that a relay can launch a hub-selected configuration. Both sides must explicitly negotiate support for inventory revisions and per-task configuration selection.

The proposed lifecycle is:

1. The relay advertises its offered inventory and supported selection behavior while retaining its legacy default configuration.
2. Hive selects a task/configuration pair and records the configuration ID, inventory revision, capability-policy revision, task ID, and assignment generation together.
3. Before accepting, the relay checks that the entry still exists, is offered for this task, and can launch in its approved local execution mode. It resolves the configuration locally; the hub does not send executable commands or arbitrary environment variables.
4. For this new negotiated mode, the relay acknowledges the exact configuration binding before Hive releases the task credential. This is an automated protocol acknowledgement, not a requirement for a person to approve every task. Legacy acceptance timing remains unchanged.
5. The relay prepares the task environment, launches the selected configuration, and reports the requested and effective configuration where observable. An unexplained substitution is surfaced rather than recorded as a successful match.
6. Progress, completion, retry, reconnect, and credential renewal retain the same binding. A reconnect resumes the existing assignment; it does not choose a different model from newly refreshed policy.

The existing protocol already separates task metadata from credential delivery and supports acceptance decisions, but defaults to automatic acceptance on the hub. The stronger configuration acknowledgement described here would be new behavior for negotiated selection. See the [current acceptance flow](https://github.com/hivecommons/hive/blob/2b95e1a88f6b40e9b692aac6c1f914e51f949e88/src/pkg/dashboard/contribute_ws.go#L4019).

Switch configurations only between tasks. Do not inject a new task into an old interactive session whose model, instructions, credentials, or working directory no longer match. Initial automatic-routing support should be limited to launch paths that can demonstrate reliable preparation, launch, and cleanup; other paths can remain advisory.

A stale inventory or failed preflight should release the offer with a specific reason and bounded retries. Distinguish a task that never started from an attempted implementation failure. Client reports must not independently raise a task's tier or suppress it across the whole Hive; reuse the existing server-owned lease, retry, and abuse controls.

## Contributor portal

Show a concise explanation at the point of selection:

```text
Issue #1234
Task capability: T1 — advanced engineering
Reason: compatibility change spanning two components

Current configuration: everyday-agy — estimated T2
Recommended offered configuration: deep-codex — estimated T1

Use recommendation | Choose another task | Continue with current
```

The actions above are proposed UX, not existing controls. Automatic-selection users can configure their standing preference during setup instead of answering for every task.

Inventory rows should distinguish offered, unavailable, and unclassified configurations. Details should explain the task assessment, model evidence, any override, and stale or approximate mappings. Raw benchmark methodology belongs in the explanation, not in the onboarding requirements.

Show **model capability**, **backend support**, and **contributor trust** separately. Never present a capability estimate as certification of the contributor or the execution environment.

## Failure, fallback, and compatibility

| Condition | Proposed initial behavior |
| --- | --- |
| Capability feature disabled | Existing v5 assignment behavior |
| Peer lacks configuration-selection support | Use the legacy single-configuration contract; do not assume a switch occurred |
| Benchmark refresh fails or returns invalid/partial data | Retain the previous valid observation set and adopted policy; show refresh status |
| Methodology changes | Treat new mappings as proposals requiring adoption; do not apply old thresholds silently |
| No usable model evidence or task assessment | Show `unknown`; use otherwise permitted existing/default behavior |
| Evidence becomes too old under policy | Label it stale; after its validity window, treat the mapping as unknown unless an explicit operator override remains active |
| Selected configuration is withdrawn, unavailable, or cannot launch | Decline/release with a specific reason; try an allowed alternative or report why the contributor is waiting |
| No known capability match | Warn and apply the contributor's declared fallback; do not introduce benchmark gating by default |

Refreshes should validate a complete snapshot, including pagination and numeric values, before atomically replacing cached observations. Keep freshness, last successful refresh, active policy, and import errors distinguishable. Task assignment should never wait for the benchmark service.

“Unknown capability,” “provider quota exhausted,” “authentication unavailable,” and “no actionable work” must remain separate states. Unknown evidence is not proof that a model is weak; an unavailable configuration cannot become usable through capability fallback.

Existing single-configuration relays and downstream protocol implementations remain supported. Optional fields retain safe defaults, and new switching semantics require explicit negotiation. Additional configurations do not confer additional trust, GitHub permissions, execution privileges, or account rate-limit allowances.

Strict capability matching is future work requiring its own policy decision. If introduced, it must specify unknown-data behavior, stale-data limits, exceptions, and visible waiting states. It cannot simultaneously promise to require a known match and silently fall back to unrestricted assignment during an outage.

## Evaluation and validation

Record assignment and attempt outcomes, not only completed PRs. Useful metadata includes the task assessment and revision, selected configuration, effective model/reasoning report, capability evidence and adopted policy revision, selection reason, fallback or override, launch outcome, review outcome, and eventual PR disposition. Use existing access and retention controls; this does not require collecting additional task transcripts or credentials.

Keep environment/quota failures distinct from attempted implementation failures. A merged PR is useful evidence, but it does not by itself establish that the original model solved the task without substantial help.

Evaluate:

- Maintainer agreement with task recommendations and the reasons for overrides.
- Major-rework rate and review effort for comparable tasks.
- First-attempt completion and time to an accepted result.
- Use of contributor-designated scarce configurations, and actual cost where measurable.
- Unknown coverage, stale mappings, fallbacks, and launch mismatches.
- Queue wait time, stranded work, and participation by existing contributors.

Compare against a recorded baseline within similar task categories. Stronger models receiving harder tasks makes raw merge-rate comparisons misleading. Do not automatically adjust contributor trust or publish contributor quality rankings from this telemetry.

Before automatic routing is considered ready, validation must demonstrate:

- Existing admission, environment-fit, model-filter, and trust decisions still hold for the selected configuration.
- Older peers never silently execute a hub-selected task with a different configuration.
- Benchmark failure, partial refreshes, unknown identities, and methodology changes preserve documented fallback behavior.
- Local restrictions, inventory withdrawal, reconnects, duplicate acceptance, and assignment generations cannot create unintended launches or duplicate ownership.
- A model switch refreshes task context and credential state on every supported launch path.
- Task or model evidence can change without rewriting the recorded basis of an in-flight assignment.

This is a proposed validation bar. No implementation or test execution is claimed by this RFC.

## Proposed rollout

These are design stages for discussion, not authorization to create implementation tasks.

1. **Model metadata:** Introduce the evidence/adoption distinction and display capability for the connected configuration. Support operator mappings, caching, freshness, and unknown states. Scheduling remains unchanged.
2. **Task assessment and advisory comparison:** Add explainable task recommendations and maintainer overrides. Compare against the current configuration; establish a baseline for usefulness and overhead.
3. **Offered inventory and recommendations:** Let contributors declare multiple configurations and preferences. Recommend only configurations actually offered. Keep launch selection under the existing contributor flow.
4. **Opt-in automatic selection:** Enable negotiated per-task selection only after assignment binding, supported launch paths, fallbacks, and compatibility have been demonstrated.

Benchmark-driven enforcement, adaptive performance scoring, and quota pacing are not prerequisites for these stages.

## Alternatives and decisions for maintainers

Keeping separate single-model relays remains a valid option, but does not provide per-task selection within a contributor's offered set. Manual selection for every task adds contributor effort. Always choosing the strongest configuration ignores resource preferences. Ranking by product name, price, or relative position in the current model list gives capability labels no stable meaning.

The proposal recommends three broad bands, explicit unknown states, operator-adopted mappings, and advisory behavior first. The main unresolved decisions are:

1. Which benchmark source, access arrangement, and versioned mapping should seed the pilot? How should coding-focused evidence be compared with Agentic Index evidence?
2. Which model-only proxies are acceptable when exact harness or reasoning evidence is absent, and how should their uncertainty appear?
3. Where should task assessments and maintainer overrides live, and which existing repository metadata convention should expose them?
4. How should contributor-offered inventory extend the shared v5 inventory/policy contracts without exposing local credentials or implying control over unoffered resources?
5. Which launch paths should first support automatic selection, and what evidence is sufficient to move beyond advisory matching?

Success means useful recommendations that reduce avoidable rework or scarce-resource consumption without materially worsening contribution access, queue delays, or maintainer effort. The decision sought here is agreement on the v5 design and its boundaries before implementation begins.

Thanks to Hanthor for the tiering reference and the operational findings that informed this proposal. Research baseline: v5 commit [`2b95e1a88f6b`](https://github.com/hivecommons/hive/commit/2b95e1a88f6b40e9b692aac6c1f914e51f949e88), reviewed September 12, 2026.

RFC created with Astra Max.

