# Formal verification in the quality lane

Hive can let the quality agent author and maintain Spin/Promela models for
protocol-shaped code. This is an optional capability, not another default lane
and not a claim that Hive proves functional correctness.

## Enablement and maturity gate

Enable it in the trusted hive configuration:

```yaml
acmm_level: 5
quality:
  formal: true
```

You can also set the same `quality.formal` toggle from the dashboard under Settings -> Features.

Both conditions are required. `quality.formal` defaults to false, and an
explicit true is inert below ACMM L5. Downgrading a hive therefore disables
formal-model authoring immediately without deleting the operator's preference;
raising it back to L5/L6 restores the capability.

The contract is injected after ordinary policy-template resolution. It reaches
scheduled and manual kicks, quality replicas, and locally customized or remote
quality prompts. Operators can customize the rest of the quality policy without
silently weakening what `quality.formal` promises.

## What is worth modeling

The agent must justify model-worthiness in every PR that introduces a model. A
candidate should have one or more concrete signals such as:

- a state-machine lifecycle;
- concurrent actors;
- retry, escalation, or durable ledger state;
- ordering or lock invariants;
- lease, claim, or ownership transfer; or
- prior incidents caused by a protocol interleaving.

If no subsystem qualifies, the quality agent continues its normal test and
coverage work. It must not add a placeholder model. Formal scope is protocol,
safety, and liveness behavior; modeling every component or proving general
functional correctness is out of scope.

## Repository artifact contract

Each modeled subsystem owns:

```text
formal/<subsystem>/
├── model.pml
├── run.sh
└── README.md
```

A repository organized below a component root may put `formal/` at that root.
The README maps every modeled state and transition to its production code,
documents abstractions and fairness assumptions, lists each property, and gives
counterexample replay commands. The model PR explains why ordinary tests cannot
reliably explore the interleavings being modeled.

`run.sh` is the executable source of truth. It performs exhaustive Spin checks
and returns nonzero whenever an observed verdict differs from the pinned
expectation. That includes both must-pass properties and intentional
expected-fail witnesses. A new violation and an undocumented fix that makes a
known witness disappear are both model drift and both make the script red.

## CI and failure reporting

The model PR adds a path-gated CI job covering the model, the corresponding
production subsystem, and the workflow itself. The job is reporting-only and
must not be configured as a required merge check. It preserves the logs or
trails needed for local replay, but a raw Spin trail is not an actionable issue.

The quality agent minimizes and replays each unexpected counterexample, then
files one issue per violated property using this stable title:

```text
[formal:<subsystem>:<property-id>] <property summary>
```

Before creating, it performs only a narrow exact-title lookup. If that property
already has an open issue, the agent updates it with the new model revision,
run, and narrative instead of creating a duplicate. Different properties never
share one issue. The narrative records user impact, an actor-by-actor
interleaving, the reproducible model revision/run, matching production code,
and what a modeled candidate fix proves when one exists.

Issue narration stays agent-owned. A pull-request workflow should not receive
broad issue-write permission solely to turn an untrusted raw trail into prose.
When the repository supplies an issue relay such as `hive-open-issue`, the agent
uses that relay and its exact-title deduplication contract.

## Modeled-code maintenance

A PR that touches a modeled subsystem and turns its formal job red must either
fix the code or update the model in the same PR with the new invariant story.
Rerunning past the evidence, weakening a property, deleting an expected witness,
or narrowing the path gate merely to make CI green is not maintenance.

Existing red quality PRs still take precedence under FIX-BEFORE-NEW. Enabling
formal verification does not authorize the quality agent to abandon repair work
and start a fresh model.

## Current repository models

- `src/formal/escalation/` models the escalation ledger and reviewer lane.
- `src/formal/contribute-lease/` models the contributor lease/reconnect
  protocol: task assignment, websocket drop, release cooldown, reconnect resume,
  lease expiry, and reassignment for one issue and two contributors. It pins two
  lease invariants: no issue is held by two contributors with valid leases at
  once, and an abnormal disconnect that reconnects within the relay's first
  backoff window is not booked as `abandoned_disconnect`. Its expected-fail rows
  preserve the pre-fix #7773 and #7838 counterexamples.
