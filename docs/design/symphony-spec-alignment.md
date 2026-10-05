# OpenAI Symphony specification alignment review

## Scope and verdict

Review for [#10626](https://github.com/hivecommons/hive/issues/10626), against
Hive v5 commit `0904d04b8` and Symphony Draft v1
[`SPEC.md` at `be10a1b79df723d6d7612b5651c8522704dafb2e`](https://github.com/openai/symphony/blob/be10a1b79df723d6d7612b5651c8522704dafb2e/SPEC.md).
The [README at that revision](https://github.com/openai/symphony/blob/be10a1b79df723d6d7612b5651c8522704dafb2e/README.md)
is product/demo context, not normative requirements. The spec is evolving;
recheck the pinned revision before using this as a compatibility statement.

**Hive shares the operating model, but is not Symphony-spec-conformant.**
In particular, Hive does not expose the required `WORKFLOW.md` loader/reload,
strict prompt-template contract, or Codex app-server runner. A vocabulary change
cannot close those gaps. This review changes documentation only, not dispatch,
workspace retention, permissions, or merge behavior.

Verdict key:

- **Satisfies**: the scoped purpose is already present; not a certification of
  every normative clause.
- **Small alignment**: a narrowly scoped documentation or naming improvement is
  feasible; any underlying behavioral gap is called out separately.
- **Diverges**: Hive intentionally uses a different contract or architecture.
  Compatibility would require design and implementation, not a small patch.

## Evidence map

Local links below refer to the reviewed v5 tree. Symbol/file references are used
rather than fragile line numbers.

- **E1 — Scheduling and policy:** [architecture](../../src/docs/architecture.md),
  especially governor and deterministic pipeline;
  [script index](../../bin/README.md) (`run-pipeline.sh`,
  `enumerate-actionable.sh`, `issue-classifier.sh`, `merge-gate.sh`).
- **E2 — Tracker model:** [provider contract](../../src/docs/integrations/work-source-providers.md),
  [`Issue` / `WorkSource`](../../src/pkg/worksource/worksource.go),
  [`FromConfig`](../../src/pkg/worksource/factory.go),
  [`LinearSource`](../../src/pkg/worksource/linear.go).
- **E3 — Agents/config:** [agent configuration](../../src/docs/agent-configuration.md),
  [`hive.yaml` example](../../src/hive.yaml.example),
  [agent launcher](../../bin/agent-launch.sh).
  Hive also uses app-server probes for
  [subscription headroom](../../src/pkg/rotation/rotation.go) and
  [model discovery](../../src/pkg/dashboard/cli_models_codex.go); those are not
  a per-issue coding runner.
- **E4 — Isolated execution:** [`SandboxExecutor.Run` / `prepareWorkspace`](../../src/pkg/agent/sandbox_executor.go),
  [path/cwd tests](../../src/pkg/agent/sandbox_executor_paths_test.go).
  This is an execution path, not a guarantee for all tmux agents.
- **E5 — Safety:** [security self-assessment](../../src/docs/security-self-assessment.md),
  [ACMM matrix](../../src/docs/acmm-policy-matrix.md),
  [MITM enforcement ADR](../../src/docs/adr/0002-mitm-proxy-network-enforcement.md).
- **E6 — Linear integration:** [work sources](../../src/docs/work-sources.md),
  [Linear agent integration](../../src/docs/linear-agent.md),
  [Linear factory tests](../../src/pkg/worksource/factory_linear_test.go),
  [Linear adapter tests](../../src/pkg/worksource/linear_test.go).

## Per-section verdicts

Rows cover every numbered top-level section and Appendix A. Subsection ranges
identify the contract being compared; mixed sections explicitly identify the
part that aligns and the part that does not.

| Symphony section | Verdict | Hive evidence and boundary / next action |
| --- | --- | --- |
| Normative language | Small alignment | Use MUST/SHOULD only for an actual contract, not marketing equivalence. No RFC 2119 conformance claim is made here. |
| 1 — Problem statement | Satisfies (daemon workflow and observability); diverges (workspace/workflow contract) | E1/E3: persistent service dispatches coding work and exposes operator status. E4 is isolated execution, but the standard fleet is long-lived; repo-owned `WORKFLOW.md` and universal per-issue cwd are not established. |
| 2 — Goals and non-goals (§§2.1–2.2) | Diverges | E1/E3/E5: multi-runtime fleet, dashboard, hub/spoke, and governance are core Hive concerns, whereas Symphony excludes a rich multi-tenant control plane/general distributed scheduler. Sharing polling and bounded execution does not imply identical recovery behavior. |
| 3 — System overview (§§3.1–3.3) | Satisfies (conceptual layering); diverges (component contracts) | E1/E2/E3: policy, config, governor, source, execution, observability have analogs. Hive's YAML/config and CLI launcher do not implement the specified Workflow Loader or app-server client. |
| 4 — Core domain model (§§4.1–4.2) | Diverges | E2: `Issue` has source, repo, external ID, priority strings, state, URL, timestamps, dependencies. It is not Symphony's all-fields-present model with opaque `id`, `native_ref`, integer priority and explicit `dispatchable`; no `thread_id-turn_id` session contract. An interoperability mapper needs identity/collision tests, not field renaming alone. |
| 5 — Workflow specification (§§5.1–5.5) | Diverges | E3: Hive uses `hive.yaml`, agent definitions and policies, not the specified cwd-default `WORKFLOW.md`, YAML front matter, lifecycle hooks and strict Liquid-compatible prompt rendering. A workflow importer would be a separate feature. |
| 6 — Configuration (§§6.1–6.4) | Diverges | E2/E3: typed config and secret references exist, but do not establish REQUIRED `WORKFLOW.md` watch/reapply, schema/defaults, per-tick validation, or invalid-reload semantics. Do not equate general config reload with §6.2 conformance. |
| 7 — Orchestration state machine (§§7.1–7.4) | Diverges | E1/E3: governor kicks and agent supervision differ from one authority owning per-issue run/claim/retry maps, same-thread continuation and a one-second retry after normal exit. Hive claim coordination is not evidence of the entire reference state machine. |
| 8 — Polling/scheduling/reconciliation (§§8.1–8.6) | Diverges | E1/E2: actionable work, holds and queue-depth cadence exist. Fixed cadence, priority/creation/identifier sort, per-state slots, exact 10-second exponential backoff, ID-refresh cancellation and terminal-workspace startup cleanup are not established as one portable contract. Symphony already gates before dispatch; calling that “none” would be incorrect. |
| 9 — Workspace management/safety (§§9.1–9.5) | Diverges | E4: sandbox execution mounts a workspace and runs there; ordinary sandbox clones use agent/time-based paths, and run-stage worktrees are removed after execution. This differs from deterministic collision-resistant issue keys, reuse, four hook phases and terminal cleanup. Do not promise all Hive agent commands stay in per-issue directories. |
| 10 — Agent runner protocol (§§10.1–10.7) | Diverges | E3/E4/E5: Codex execution is a CLI backend; headroom/model-discovery app-server probes are not a coding runner implementing same-thread turns, tool declarations, event/usage framing and read/silence/stall timeouts. Approval controls are documented through Hive's backend/ACMM posture, not the Symphony protocol. |
| 11 — Tracker integration (§§11.1–11.5) | Diverges | E2/E6: `WorkSource` exposes `ListIssues`, filtering actionable items inside adapters. Symphony requires state-list plus opaque-ID full-snapshot refresh and scheduler-owned dispatchability/label filtering. Provider-specific helpers and proxy-gated writes are not host-side app-server tools; compact adapter profiles/error mappings would need additional work. |
| 12 — Prompt construction (§§12.1–12.4) | Diverges | E3/E4: Hive assembles kick prompts and policies; no claim of strict unknown-variable/filter errors, preserved issue objects or `attempt=null` then 1-based continuation contract. Supporting those is part of a workflow adapter, not vocabulary adoption. |
| 13 — Logging/status/observability (§§13.1–13.7) | Small alignment (terminology); diverges (interoperability) | E1/E3: dashboard, logs and cost/status visibility exist. Standardizing issue/run/session labels in documentation is cheap; exact required log fields, app-server absolute token accounting and `/api/v1/state`, issue, refresh shapes are not proven. HTTP/status are optional, but shipping Hive's dashboard does not certify §13.7. Dashboard-independent operation is separately scoped on v6 in the repository roadmap. |
| 14 — Failure/recovery (§§14.1–14.4) | Diverges | E1/E3: supervision, stall handling and operator controls exist. Hive does not adopt Symphony's in-memory per-ticket recovery, terminal cleanup and workflow-edit intervention contract. Do not infer those semantics from process restart support. |
| 15 — Security/operational safety (§§15.1–15.5) | Satisfies (documented trust posture); diverges (specific invariants) | E5 explicitly documents trust, ACMM, proxy enforcement and deployment-dependent confinement. E4 offers credential-free isolated execution, but is not universal enforcement of per-issue cwd/root/key invariants or host-side tracker-tool secret isolation. Hooks would require their own timeout/trust contract if adopted. |
| 16 — Reference algorithms (§§16.1–16.6) | Diverges | E1–E4: governor/fleet paths are not ports of the startup/tick/reconcile/worker/continuation algorithms. Treat the pseudocode as input to a future compatibility mode, not a description of current Hive. |
| 17 — Test/validation matrix (§§17.1–17.8) | Diverges | E4/E6: adapter and path tests cover useful overlaps, not the Symphony core-conformance matrix. A compatibility feature needs workflow/reload, collision, protocol, retry/reconcile and telemetry tests. Real-provider tests must report skipped when credentials are unavailable. This documentation review did not run that integration profile. |
| 18 — Implementation checklist (§§18.1–18.3) | Diverges | Required workflow, runner and lifecycle contracts remain absent/different; optional dashboard and Linear support cannot substitute for §18.1. No conformance badge or auto-land guarantee should be published. |
| Appendix A — SSH workers (§§A.1–A.3, optional) | Diverges | E1: hub/spoke and contributor relays address remote capacity, but are not central app-server-over-SSH ownership with per-host slots and issue workspace locality. Neither implementation of this extension nor compliance is claimed. |

## Cheap vocabulary alignment adopted here

“Work item”, “run attempt”, “workspace”, “handoff”, and “proof of work” are useful
shared terms. Their boundaries matter:

- A **run attempt** is execution, not proof that a ticket reached `Done`.
- A **workspace** is a checkout or worktree; isolation claims must name the
  execution path and enforcement layer. Workspace separation is not a sandbox.
- A **handoff** can be a ready-for-review PR. Only authorized review/merge paths
  may land it; this review does not increase agent authority.
- **Proof of work** means linked, inspectable evidence, not the model's assertion
  that checks passed. Symphony's demo names artifacts; its spec does not require
  a walkthrough video or complexity report for every run.

For a Hive PR or an outreach example, the following headings can make existing
evidence legible without changing runtime schemas or introducing a new gate:

```markdown
## Proof of work
- CI: link to the run/checks for this head, or state not run/pending.
- Review: link to review feedback; do not imply approval when none exists.
- Complexity: describe scope/risk, or state that no analysis was produced.
- Walkthrough: link to a recording or reproducible steps, or mark not applicable.
- Run isolation: name the execution mode/workspace and its limitations.
- Handoff: ready for review; merge remains subject to repository policy.
```

These are recommended documentation headings, **not** newly emitted artifacts,
a standardized machine-readable schema, or mandatory PR fields. Runtime
protocol alignment and universal workspace guarantees are not cheap changes;
they require a separately reviewed feature proposal.

## Linear work-source decision: do, using the existing adapter

The proposed stretch implementation is already in v5. Reuse
`governor.work_source.type: linear` and `LinearSource`, rather than filing a
second adapter implementation issue. E2/E6 document team/state selection,
project-to-repo routing, current-cycle filtering, holds, assignment/delegation,
pagination, secret references and tests. The target repository remains a forge
repository; Linear supplies the work item, not a replacement for PR review.

A Symphony user can configure the same Linear team/project and active states
in Hive using the [existing configuration example](../../src/docs/integrations/work-source-providers.md#configuration-examples).
This is a manual mapping: Hive does **not** import Symphony's `tracker.provider`
or `WORKFLOW.md`, nor promise identical sorting, claims or state transitions.
Use a dedicated scope or assignment filter when trying both systems so two
orchestrators do not independently dispatch the same ticket. Validate credentials,
routing, blocked items and handoff policy before enabling unattended work.

**Defer** a Symphony workflow importer/app-server compatibility mode until there
is concrete interoperability demand and a separately reviewed scope. That is
not a blocker to publishing this positioning review, and no new Linear adapter
or generic tracker-write abstraction is needed for this issue.

## Outreach copy (draft, not posted)

> Running the Symphony model on GitHub with deterministic merge gates:
> Hive shares Symphony's work-driven operating model, adding a multi-runtime
> fleet and deterministic policy around GitHub/GitLab review and merge. Linear
> is already an available work source. Our section-by-section review explains
> the overlap and the gaps: Hive is not a `WORKFLOW.md`/Codex app-server compatible
> Symphony implementation. Proof of work remains inspectable CI/review evidence,
> and landing remains subject to repository policy.

Link the published [landscape comparison](../../src/docs/landscape.md#openai-symphony)
and this review when sharing. External posting is not part of this change:
follow [outreach anti-spam guidance](../outreach-antispam.md), and post a
Symphony discussion only if the venue accepts this comparison. Do not advertise
conformance, universal sandboxing, or approval-free auto-landing.
