# External workflow admission

Status: **Gate 0 decided ([#8302](https://github.com/hivecommons/hive/issues/8302)); Gate 1 report-only pilot shipped on v5, default off ([#8361](https://github.com/hivecommons/hive/issues/8361)). Publication (Gate 2) remains a separate, undecided gate.**

The first half of this page is the Gate 0 decision record for
[#8201](https://github.com/hivecommons/hive/issues/8201). The sections from
"Gate 1 implementation" onward describe what #8361 built against that
decision: the engine-neutral binding, the Flue adapter and fixture, the
side-effect inventory, operational status and recovery, and the comparison
against today's local integration.

This page records the maintainer selection requested by #8201 before any Flue
binding is implemented. It is a decision record, not an implementation plan for
a new store, CRD, DSL, broker, scheduler, publication path, or credential grant.
The selected release line is **v5**, because the contribution policy routes
protocol and structural changes through the v5 RFC path.

## Decision

Hive will pursue **option 2 from #8201: reuse the existing contributor protocol
and add one durable external-execution binding**. The first workflow is a
bounded Flue generation workflow. It is not the Astro triage handler: that
handler owns preview publishing and other GitHub lifecycle behavior, while this
pilot is report-only and has no publication authority.

The selected path is narrower than a general factory framework:

- **Workflow:** one externally hosted, bounded Flue generation conversation that
  accepts a pinned source/context bundle and returns a bounded report plus an
  optional patch artifact.
- **Hive owner:** the contributor protocol and task-lease owner on v5 owns
  admission, assignment generation, stage progression, current authorization,
  and acceptance of a candidate.
- **Durable owner:** the Hive task lease is the workflow record. Its stage is the
  Hive-owned `spec`, `plan`, or `implement` field from #8297, and each retry is a
  new generation of that stage. The native Flue submission identity is bound to
  that Hive logical execution record rather than replacing it.
- **Acceptance predicate:** Hive accepts a result only when the stage receipt
  added by #8295 validates, the receipt binds the current work key, assignment,
  generation, stage, contract revision, execution key, engine/workflow version,
  remote run identity, immutable input revision, immutable output digest, result
  class, timestamps, and bounded provenance, and the exact artifact bytes match
  the receipt and the current policy.
- **Release line:** v5.
- **Initial operating mode:** default off, then report-only. A shadow phase may
  observe configured state but must not dispatch a second external run or perform
  any external mutation.

## Rejected alternatives

1. **Existing local sandbox or launch-command integration only.** This remains
   valid when an operator only needs to run a different local command. It is not
   sufficient for the asynchronous case #8201 names: an independently managed
   Flue conversation may survive restarts, expose a native submission receipt,
   and need reattachment, cancellation, and conflict behavior that a synchronous
   launch result does not provide.
2. **A standards-based remote service binding first.** A2A, Nexus, Temporal, and
   Tekton-style contracts remain useful references for lifecycle vocabulary,
   operation tokens, cancellation limits, and artifact exchange. They do not
   replace Hive's admission, task scope, current authorization, or acceptance
   predicate. Gate 1 should borrow semantics where they reduce work, not require
   a new service, CRD, broker, Temporal deployment, or second scheduler.
3. **The Astro triage Action as-is.** It carries GitHub lifecycle and preview
   publishing behavior. The selected workflow is bounded generation only.

## Authority and credential constraints

The external engine never receives target-repository write credentials, a Hive
dashboard token, production deployment credentials, or an unrestricted
publication tool. It receives only the accepted, minimal context bundle for the
selected task and stage. Applying a patch, running returned tests, publishing a
finding, opening a PR, merging, or releasing remains a separate Hive-controlled
decision under current authorization.

The binding must preserve these constraints:

- no new global store, CRD, broker, Temporal deployment, DSL, or scheduler;
- no GitHub credentials in the external engine;
- no dashboard token forwarded through Task MCP;
- no autonomous widening of repository scope, contract revision, or acceptance
  criteria by the external workflow;
- no legacy downgrade for an enrolled peer missing a required capability; and
- no publication effect in shadow or report-only mode.

## Source-to-guarantee matrix

| Existing source or landed issue | Guarantee now available | Gate 1 obligation |
| --- | --- | --- |
| Contributor protocol capability declarations | Transport and routing can be reused, and declarations are bounded client input rather than authority. | Enroll the Flue path with an explicit capability/version token and fail closed when the token or version is missing for enrolled work. |
| Contributor leases, fixed by #8322 / #8287 | Lease grants now propagate persistence failures instead of handing out an assignment that can vanish on restart. | Treat durable lease persistence as the durable-before-accept precondition for external authority; do not dispatch Flue before the logical execution binding is durable. |
| Lease stage and stage advance from #8297 | A run is one lease with a Hive-owned stage; retries are new generations of that stage. | Bind the Flue idempotency key and receipt to work key, assignment id, generation, stage, and contract revision. |
| Stage receipt schema from #8295 | `outputschema` has the versioned stage receipt shape shared by Hive-launched and external stages. | Reject missing, malformed, or conflicting receipts before artifact intake or acceptance. |
| Mutation boundary and executor | Hive has an effect boundary and journal concepts for external effects. | Do not use report-only Flue completion as proof of a mutation or outcome; publication waits for a later gate with its own authority tests. |
| Mutation journal, fixed by #8321 / #8288 | The journal has the cross-process lock-and-reload discipline that the audit found missing. | If a later publication gate uses the journal, test the exact selected store and owner topology rather than assuming a component name gives exactly-once behavior. |
| Proof and outcome packages | Exact-subject proof concepts exist, while the outcome package remains staged. | Reuse immutable subject and provenance concepts without fabricating PR numbers, producers, generations, or repository convergence. |
| Task MCP P1 | Dashboard-authenticated context exists for hub-launched agents. | Do not distribute dashboard credentials to Flue. Use a bounded context bundle until a separate task-scoped remote authorization contract is accepted. |
| Sandbox executor and push broker | Hive already separates unprivileged generation from credentialed push/PR paths. | Keep external generation unprivileged. Any later publication effect must pass through the trusted broker or an equivalently accepted boundary. |
| Push broker | Credentialed publication can be centralized. | Gate 1 must not invoke it. A later publication gate must choose one mutation and prove conditional-write, idempotency, and reconciliation behavior. |

## Native Flue receipt probe

The probe used Flue commit `c5a2a725fe1d93209ed294cca90af97060f6f2e2` and an
isolated checkout created under this working tree. The exact commands were:

```sh
git clone -q https://github.com/withastro/flue .probe-flue
cd .probe-flue
git checkout -q c5a2a725fe1d93209ed294cca90af97060f6f2e2
corepack pnpm install --frozen-lockfile
cat > packages/runtime/src/hive-receipt-abort-probe.test.ts <<'PROBE'
import { fauxAssistantMessage, fauxProvider, fauxText } from '@earendil-works/pi-ai';
import { expect, it } from 'vitest';
import { init, useModel } from './index.ts';
import { sqlite, start } from './node/index.ts';

it('deduplicates same idempotency key and rejects changed payload', async () => {
	function ProbeAgent() {
		useModel('faux/model');
		return 'Reply once.';
	}

	const faux = fauxProvider({ models: [{ id: 'model' }] });
	faux.setResponses([
		fauxAssistantMessage([fauxText('accepted')], { stopReason: 'stop' }),
	]);
	const runtime = await start({
		agents: [ProbeAgent],
		db: sqlite(),
		providers: [faux.provider],
		env: {},
	});
	const agent = init(ProbeAgent, { id: 'hive-8302-probe' });

	try {
		const first = await agent.dispatch({ message: 'payload A', idempotencyKey: 'hive-8302-key' });
		const retry = await agent.dispatch({ message: 'payload A', idempotencyKey: 'hive-8302-key' });
		expect(retry.submissionId).toBe(first.submissionId);
		expect(retry.deduplicated).toBe(true);

		await expect(
			agent.dispatch({ message: 'payload B', idempotencyKey: 'hive-8302-key' }),
		).rejects.toMatchObject({ type: 'submission_conflict', status: 409 });

		await expect(agent.read(first)).resolves.toMatchObject({ text: 'accepted' });
	} finally {
		await agent.abort();
		await runtime.stop();
	}
});
PROBE
corepack pnpm --dir packages/runtime exec vitest run src/hive-receipt-abort-probe.test.ts
```

Observed result:

```text
✓ src/hive-receipt-abort-probe.test.ts (1 test) 47ms
Test Files  1 passed (1)
```

A retry with the same `idempotencyKey` and the same payload returned the same
`submissionId` with `deduplicated: true`. A different payload under the same key
was rejected at admission with `submission_conflict` status 409. The probe also
called `agent.abort()` in cleanup, confirming the abort request path is available
for the admitted instance; cancellation delivery and workload termination remain
separate facts that Gate 1 must test with the actual deterministic workload.

## Gate 1 conformance split

Gate 1 must pass every row that concerns admission, receipt, observation,
recovery, cancellation visibility, artifact validation, disabled/shadow behavior,
and removal-sensitive wiring for the report-only Flue binding. Publication-only
rows are deferred until a separate publication gate because Gate 1 deliberately
has no target write credentials.

| # | Scenario from #8201 | Gate 1 disposition |
| --- | --- | --- |
| 1 | Admitted task and valid immutable report | **Must pass.** This is the happy path for the selected workflow. |
| 2 | Held/blocked task | **Must pass.** Held work must not start Flue, and unrelated ready work must continue. |
| 3 | Capability missing, unsupported version, or attempted downgrade | **Must pass.** Enrolled behavior must fail closed while legacy clients stay compatible. |
| 4 | Same execution key and same payload delivered twice | **Must pass.** The Flue probe shows the native primitive; the adapter must bind it to Hive identities. |
| 5 | Same key with changed payload or recreated engine instance | **Must pass.** Changed payload conflicts; incarnation reuse must not silently adopt unrelated work. |
| 6 | Process dies before start, after remote acceptance before receipt persistence, or after receipt persistence | **Must pass.** These are the ambiguous-start windows the binding exists to handle. |
| 7 | Assignment owner changes while external run continues | **Must pass.** The replacement must adopt/observe the existing logical run or record uncertainty. |
| 8 | Cancel delivered but workload ignores it; success races cancellation | **Must pass for report acceptance.** Publication consequences are deferred. |
| 9 | Poll/stream outage, duplicate or out-of-order events | **Must pass.** Authoritative native state must eventually win over local timeout guesses. |
| 10 | Artifact missing, truncated, malicious path, wrong digest, or unavailable store | **Must pass.** No artifact acceptance or privileged execution is allowed. |
| 11 | Exact input, contract revision or output subject changes | **Must pass.** Old receipts cannot authorize new subjects. |
| 12 | Factory says success but verifier rejects | **Must pass.** Keep execution evidence while refusing candidate acceptance. |
| 13 | Remote task waits for a human or reaches cost/time cap | **Must pass.** Waiting, blocked, compute budget, and admission capacity must stay visible and bounded. |
| 14 | Store write fails, record is corrupt, or supported process overlap occurs | **Must pass.** The selected durable lease/receipt storage must not activate unpersisted authority. |
| 15 | Replay of settled result | **Must pass.** Typed output and provenance must be recoverable without re-running Flue. |
| 16 | New binding disabled or shadow-only | **Must pass.** Disabled and shadow modes must not start external work or write externally. |
| 17 | Guard or binding removed | **Must pass.** A negative production-path test must fail while positive controls prove it is not reject-everything. |

The later publication gate must add crash-after-effect/before-ack, delayed old
writer, ambiguous negative lookup, grant revocation, exact-target
conditional-write, target credential, and broker reconciliation tests before any
repository write credential is granted.

## Audit observation disposition

Every observation in the audit linked from #8201 is accounted for here:

| Audit observation | Disposition |
| --- | --- |
| Assignment fencing is not external-effect fencing. | Mapped to this decision's no-publication constraint and the later publication-gate tests. No new issue for Gate 1 because no external mutation is permitted. |
| Imported proof package is not a deployed proof gate. | Mapped to this document's proof/outcome row and Gate 1 verifier-reject scenario; no fabricated proof or outcome activation is allowed. |
| Wired mutation adapter uses synthetic outcome and generation 1. | Mapped to the source-to-guarantee matrix; Gate 1 does not use that adapter as a report acceptance proof. |
| Lease persistence failures were logged, not returned. | Mapped to #8287 and fixed by #8322. |
| Mutation journal lacked cross-process locking. | Mapped to #8288 and fixed by #8321. |
| Task MCP P1 is not ready-made remote delegated authority. | Mapped to the credential constraints and Task MCP matrix row; no dashboard token is given to Flue. |
| Flue already supplies keyed admission and incarnation checks. | Mapped to the native Flue probe and Gate 1 rows 4 and 5. |
| The contract gap was already raised by #7620 comments. | Mapped to #8201 and #8290; this record selects the bounded path rather than opening a competing epic. |
| Generalized convergence is not a prerequisite for a useful pilot. | Mapped to the report-only Gate 1 scope and deferred publication gate. |
| Contributor protocol is prior art, not a universal executor contract. | Mapped to option 2 and the explicit capability/enrollment requirement. |
| Lease persistence needed durable-before-accept behavior. | Mapped to #8287/#8322 and the Gate 1 pre-dispatch durability requirement. |
| Mutation wiring exists but outcome ownership is synthetic at that adapter. | Mapped to no outcome-ledger activation in Gate 1. |
| Epoch checks cannot revoke an independently credentialed remote writer. | Mapped to no external write credentials in Gate 1; publication requires a separate boundary. |
| Claim-ledger serialization did not imply journal serialization. | Mapped to #8288/#8321. |
| Observation function exists but automatic authoritative recovery was not established. | Mapped to Gate 1 recovery/observation rows; the adapter must test the actual native observation path. |
| Deduplicated acknowledgment must rehydrate typed results. | Mapped to Gate 1 replay row 15. |
| Shadow behavior requires tests. | Mapped to Gate 1 row 16. |
| Existing sandbox/broker separation is the lowest-risk leverage point. | Mapped to the authority split and rejected-alternative analysis. |
| Prior art from Flue, Nexus/Temporal, Tekton, A2A, GitHub Agentic Workflows, OpenHands, and in-toto. | Mapped to option selection: borrow concrete semantics and digest/provenance shapes without adding a mandatory new controller or service. |
| Non-obvious failure scenarios in the audit. | Mapped to Gate 1 rows for start acknowledgment, late results, negative lookup limits, wrong authority, missing artifact bytes, duplicate semantic work, retention/incarnation reuse, retry budget, and artifact intake. Publication-boundary TOCTOU is deferred to the publication gate. |
| Filing and branch mechanics. | Not reproduced as a runtime concern; #8201, #8290, and #8302 now carry the upstream tracking state. |

## Gate 1 implementation (#8361)

### Shape

| Piece | Where | What it owns |
| --- | --- | --- |
| Engine-neutral contract | `src/pkg/extwork` | `Adapter` (keyed `Start`, `Observe` with accepted/running/waiting/terminal/unknown, `Cancel` returning requested/acknowledged/stopped as separate facts, `OpenArtifact` by execution key and relative path only), `Admission` and its derived `ExecutionKey`, the accept-or-decline `Host` step, `ProgressEvent`s, `Decide`, receipt fetch with path, size, and digest rules applied before parsing, `Registry`. |
| Flue adapter | `src/pkg/extwork/flue` | Maps the contract onto Flue's native surface as probed at commit `c5a2a725fe1d93209ed294cca90af97060f6f2e2`: `idempotency_key` dispatch (`deduplicated`, `submission_conflict`), runtime uid as incarnation, abort, artifacts. Talks only to its configured endpoint, honours no proxy environment, carries no credential. Linked into `hive` only with `-tags extwork_flue`. |
| Deterministic fixture | `src/pkg/extwork/flue/fixture` + `testdata/flue-fixture/` | A second local process that executes the three-stage workflow (`analyze`, `instrument`, `report`) from a pinned source bundle, advances only when ticked, resumes from its own state file, and can be told to ignore abort. No Node, network, model, or GitHub token. `cmd/flue-fixture` wraps it for `examples/flue/`. |
| Hive seams | `src/pkg/dashboard/extwork_binding.go`, `src/cmd/hive/extworkwire.go` | The task lease is the admission and authority record: the hub exports `VerifyLeaseAuthority`/`FlushLeaseAuthority` (extwork-free signatures, since the dashboard is at its internal-import ceiling and must not link the adapter), `extwork.LeaseStore` refuses an admission the hub does not vouch for and forces the lease to disk before dispatch; progress events go to the agent audit trail through `extwork.AuditProgressSink`; `cmd/hive` constructs the binding (`newExternalFlueBinding`) only when `runs.external.flue` is on and the engine is linked, and backs the dashboard's `ExternalExecution` status seam with the registry. The `ext-exec/flue` relay capability gates offers; an item bound to an engine is refused, never downgraded. |
| Config and toggle | `runs.external.flue` in `hive.yaml`; Settings > Features | Default off. Enabled with no mode is shadow. `report-only` is the only mode that dispatches. Endpoint and workflow version are yaml-only. |

### Identity binding

`ExecutionKey` is `StableDigest(work key, assignment id, generation, stage,
contract revision, engine, workflow version, input revision)`. It deliberately
excludes the request digest, so a changed bundle under an unchanged identity
reaches the engine under the same key and is rejected there as
`submission_conflict` (#8201 row 5) instead of quietly becoming a second run.
The request digest, the contract and input revisions, the engine incarnation
pinned at dispatch, and the native run id recorded once the engine accepted
are the parts the lease cannot hold; they are written beside the verified
receipt under the agent report directory
(`extwork/<assignment>.admission.json`), and `Load` re-checks the lease before
trusting that file. No lease, no authority.

### Side-effect inventory (step 8)

The Astro triage handler that Flue ships publicly performs these effects. Each
is classified as (a) blocked by the proxy and scoped MCP, (b) expressible in
the stage receipt as an artifact, or (c) not available under Hive at all. The
pilot proceeds with class (c) effects absent. The fixture's `instrument` stage
attempts one class (a) effect of each network kind and the conformance test
`TestConformanceSideEffectsBlockedAndArtifactsListed` proves both are refused
at the egress boundary while every class (b) artifact appears in the receipt.

| Astro triage effect | Class | Under Hive | Proven by |
| --- | --- | --- | --- |
| Clone the target repository | (b) | The engine reads the pinned source bundle in the context bundle; the bundle's `input_revision` is echoed in the receipt and must match the admission. | `BindReceipt` input revision check; row 11 test. |
| Instrument / build the checkout | (b) | Local to the engine sandbox; nothing leaves it. Its result is a claim carried as `report.md`. | Receipt artifact listing. |
| Publish a preview package to pkg.pr.new | (a) | Outbound POST is refused by the egress boundary; no publication credential exists in the engine. | Fixture `outbound_post` attempt refused (CONNECT 403 recorded by the deny proxy). |
| Post a comment through the workflow's own GitHub channel | (a) | GitHub write is refused by the egress boundary and no token is present (`github_token_present: false`). | Fixture `github_write` attempt refused; stats assert no token. |
| Open a pull request | (a) for the engine; (c) for the pilot | The engine cannot reach GitHub; Hive's own publication path is deliberately not enabled at Gate 1. | Report-only scope; Gate 2. |
| Write files to a sandbox that outlive the run | (b) | Only what the receipt lists exists for Hive: `report.md` and the optional `patch.diff`. Anything else in the engine's sandbox is invisible and unused. | Receipt artifact set equals the workflow's declared artifacts. |
| Outbound calls from author-defined tools | (a) | Every route goes through the egress boundary; without a route the fixture records `no_route` and opens no connection. | Fixture effect outcomes. |
| Label, assign, close, or edit the issue | (c) | Not available; Hive keeps issue lifecycle. | Out of scope. |

Any effect that is neither blocked nor described is a Gate 1 failure; the
fixture's stats endpoint and the deny proxy make such an effect visible.

### Operational status and recovery

Status is read from two places: the engine's native state through
`Binding.Observe` (the runs API surfaces the lease stage; the audit trail
carries the `ext_work_*` events keyed by assignment id), and the Features
panel, which reports the effective mode, whether the adapter is linked into
this build, and whether an endpoint is configured.

| Situation | What Hive does | Operator action |
| --- | --- | --- |
| Binding off, or engine not linked | Nothing external happens; `newExternalFlueBinding` fails closed with a named error. | None. A build without `-tags extwork_flue` cannot be switched on by configuration. |
| Shadow mode | Admissions are persisted and `ext_work_shadow_observed` is recorded; `Observe` reads native state; no `Start`, no `Cancel` effects. | Promote to `report-only` only after the shadow soak shows the expected admissions. |
| Hub restarts before start | `Recover` finds the durable admission, asks the engine by key, gets not-found, and issues one keyed start (idempotent at the engine). | None. |
| Hub restarts after the engine accepted | `Recover` observes the existing run and adopts it (`deduplicated: true`); no second dispatch. | None. |
| Hub restarts after the receipt was persisted | `Recover` returns terminal from the receipt store; `Replay` rehydrates the typed receipt. The engine is not called. | None. |
| Engine unreachable | `Observe` returns `unknown` with `ErrTransport`; `Recover` returns uncertain and starts nothing. | Fix the endpoint. When the same incarnation returns, its state wins. |
| Engine deleted and recreated | The pinned incarnation no longer matches, so whatever the new instance holds is never adopted. If the durable record names a native run, that run is lost with the old instance: `Recover` returns uncertain and starts nothing. If the record names no run (death after remote accept, before the record was updated), nothing is outstanding on the live instance: the admission is re-pinned to it and started once. | For the uncertain case, retire the assignment (a stage retry mints a new generation and therefore a new execution key) or restore the original engine state. |
| Cancel ignored by the workload | `Cancel` records requested and acknowledged, `stopped: false`; the run stays visibly running. Late output is rejected when authority is no longer current. | Wait for the engine or retire the lease; never assume stopped. |
| Receipt missing, truncated, wrong digest, malicious path, oversized | Refused before parsing; `ext_work_receipt_refused` is recorded; nothing is stored. | Inspect the engine. |
| Engine reports no_change, blocked, failed, unknown | Own verdicts (`no_change`, `blocked`, `rejected`, `uncertain`); never success. | Per verdict. |

### Comparison: local sandbox launch versus the binding (step 7)

The same fixture workflow can be run through today's local integration (the
sandbox executor launching a local process and reading its report file) and
through the binding. Measured on the conformance suite, one assignment, one
stage:

| Measure | Local sandbox launch | Flue binding |
| --- | --- | --- |
| Dispatch count, happy path | 1 process launch | 1 native run; a repeated dispatch of the same key and payload is `deduplicated`, never a second run. |
| Recovery after hub restart before start | Re-launch (the launch itself is the record; a lost launch is invisible). | 1 keyed start after a positive not-found from the engine. |
| Recovery after hub restart mid-run | The orphaned process is not re-attachable; work is repeated on re-launch. | 0 new dispatches; the run is adopted by execution key and pinned incarnation. |
| Recovery after the receipt is on disk | Re-launch unless the caller checks the report file first. | 0 engine calls; replay from the receipt store. |
| Repeated work across the three crash windows | Up to 3 launches for one logical execution. | 1 native run. |
| Operator interventions | Manual cleanup of orphaned processes and duplicate PR candidates. | None for the covered windows; one decision (retire or restore) for a recreated engine. |
| Resource accounting | Local CPU and disk, counted per launch. | Engine compute counted per native run; Hive holds no repository mutation slot while waiting, only the lease. |
| Cancellation | Process kill: stopped is certain, acknowledgement is not a concept. | Requested, acknowledged, and stopped are separate facts; an ignored abort stays visibly running. |
| Evidence | Report file, trusted by location. | Receipt bytes verified against digest and size, schema-validated, bound to the admission identities. |

The binding buys re-attachment and non-duplication for an asynchronous,
restart-surviving engine; it does not reduce issue count, and that was never
the measure. For a synchronous local command, the local integration remains
the right tool.

## Deferred items

- Any publication authority, target-repository mutation, broker integration,
  security-finding publication, or generalized outcome aggregation (Gate 2).
- The OMP workbench as a second host (#6899): the accept-or-decline step
  (`extwork.Host`, `InteractiveHost`) and the progress events
  (`extwork.ProgressEvent` on the lease audit) are the seams it needs and are
  in place; the OMP transport itself is not.
- Wiring the binding into the assignment path so an admitted run-stage item is
  dispatched automatically; Gate 1 refuses external items to relays without the
  capability and constructs the binding, but the hub does not yet call
  `Dispatch` on assignment.
