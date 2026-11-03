# ClankeR and Flue-style external execution

## Audience

This page is for authors of external workflow engines that want Hive to hand them bounded work the way the Flue pilot does. It is also for operators deciding whether ClankeR is the right integration point.

For example, Hive can send an allowed task to an external runtime, poll until it reports completion, then verify the returned report. Reporting completion does not give that runtime permission to publish code or bypass Hive's checks.

| What you have | Route | Prerequisites |
| --- | --- | --- |
| A local coding CLI/subscription to lend | Use the [contributor relay setup](../contributor-relay.md#basic-setup), not the HTTP calls below | A hive registration, supported backend, credentials, and the selected launch boundary |
| A service that accepts and runs workflow requests | Implement an HTTP-runtime adapter such as Flue | A reachable runtime endpoint, pinned workflow version, a Hive build linking the adapter, and trusted network access |
| No runtime yet; you want to understand the contract | Use the deterministic [fixture walkthrough](../../../examples/flue/README.md) | A Hive checkout, Go and curl; no model or GitHub token |

## Concepts

**Keyed admission** means “start this request only once”: retrying the same key and payload must not start a duplicate. An **incarnation** identifies one lifetime of a runtime so Hive cannot mistake a recreated server for the old one. A **receipt** is the runtime's structured evidence about its output; Hive binds that evidence to the exact execution before accepting artifacts. A **sentinel error** is a named error value the caller can classify.

**Shadow** checks the integration without dispatching work. **Report-only** dispatches work but does not grant publication credentials. A **seam** is simply a code boundary; operators do not need to implement one to run a contributor relay.

In this codebase, **ClankeR** is the contributor relay: a contributor-owned process connects to a hive over `/api/contribute/ws`, advertises backend/model/capabilities, receives one task at a time, heartbeats, and reports completion metadata. The user-facing setup is documented in [ClankeR contributor relay](../contributor-relay.md).

Flue's v5 adapter lives in this repository under `pkg/extwork/flue`. The contract and pinned upstream version are implementation reference below, not an instruction to install an arbitrary latest Flue release.

## Interface

<details>
<summary>Adapter-author contract and source references</summary>

The adapter comment names the probed upstream Flue runtime commit `c5a2a725fe1d93209ed294cca90af97060f6f2e2` and maps Hive's external-work contract to Flue's keyed admission, runtime UID, status, abort, and artifacts (`src/pkg/extwork/flue/flue.go:1`).

The engine-neutral contract is `pkg/extwork.Adapter`:

- `Engine() string` returns the engine name (`src/pkg/extwork/adapter.go:111`).
- `Start(ctx, StartRequest)` admits a keyed request idempotently; same key plus same payload should deduplicate, same key plus different payload should conflict (`src/pkg/extwork/adapter.go:47`, `src/pkg/extwork/adapter.go:114`).
- `Observe(ctx, key, incarnation)` reports one of `accepted`, `running`, `waiting`, `terminal`, or `unknown`; transport errors become unknown, not fabricated failure (`src/pkg/extwork/adapter.go:11`, `src/pkg/extwork/adapter.go:71`).
- `Cancel(ctx, key, incarnation)` reports requested, acknowledged, and stopped separately (`src/pkg/extwork/adapter.go:88`, `src/pkg/extwork/adapter.go:119`).
- `OpenArtifact(ctx, key, incarnation, path)` streams an artifact by execution key and relative path; the binding verifies size, path, and digest before parsing (`src/pkg/extwork/adapter.go:121`).

Adapters are constructed through `extwork.Factory` and registered in an `extwork.Registry` (`src/pkg/extwork/registry.go:16`, `src/pkg/extwork/registry.go:36`). The Flue build tag links the Flue adapter into the default registry (`src/cmd/hive/extwork_flue.go:17`). Dashboard code sees only the `ExternalExecution` seam, not the adapter package (`src/pkg/dashboard/extwork_binding.go:51`).

</details>

## How Flue maps the contract

Flue's `Config` contains only an endpoint, a pinned workflow version, and an optional test HTTP client (`src/pkg/extwork/flue/flue.go:52`). `New` validates that the endpoint is `http` or `https`, has a host, and has no userinfo/query/fragment, then uses an HTTP client with proxy disabled (`src/pkg/extwork/flue/flue.go:71`).

The HTTP surface used by Hive is:

| Hive call | Flue request | Real fields |
| --- | --- | --- |
| Pin incarnation | `GET /` | `engine`, `version`, `incarnation` (`src/pkg/extwork/flue/flue.go:93`) |
| Start | `POST /dispatch` | request body `idempotency_key`, `payload`; response `submission_id`, `uid`, `deduplicated` (`src/pkg/extwork/flue/flue.go:186`) |
| Observe | `GET /submissions?key=<execution-key>` | response `id`, `uid`, `state`, `stage`, `result_class`, `receipt` (`src/pkg/extwork/flue/flue.go:132`, `src/pkg/extwork/flue/flue.go:282`) |
| Cancel | `POST /submissions/{id}/abort` | response `requested`, `acknowledged`, `stopped`, `detail` (`src/pkg/extwork/flue/flue.go:303`) |
| Artifact | `GET /submissions/{id}/artifacts/{path}` | stream returned bytes after the binding verifies the declared receipt (`src/pkg/extwork/flue/flue.go:323`) |

The adapter never receives a GitHub token, dashboard token, or publication credential. `runs.external.flue` has `enabled`, `mode`, `endpoint`, and `workflow_version`; enabled with no mode is shadow, and only `report-only` dispatches (`src/pkg/config/runs.go:38`).

## Step-by-step: integrate a third-party engine

1. Decide whether your engine should be a **relay peer** or an **HTTP runtime**. Flue is an HTTP runtime; OMP is a relay peer behind the same `extwork` contract.
2. Implement `extwork.Adapter`, returning sentinel errors such as `ErrConflict`, `ErrRefused`, `ErrNotFound`, `ErrIncarnationMismatch`, and `ErrTransport` so the binding can classify outcomes (`src/pkg/extwork/adapter.go:95`).
3. Add a factory and registry name. Flue's factory reads `endpoint` and `workflow_version` from plain string settings (`src/pkg/extwork/flue/flue.go:89`).
4. Wire the engine in `cmd/hive` with a build tag, following `extwork_flue.go` and `newExternalFlueBinding` (`src/cmd/hive/extwork_flue.go:17`, `src/cmd/hive/extworkwire.go:226`).
5. Add config under `runs.external.<engine>` if it needs operator settings. Keep the default off and use shadow before report-only, as `FlueBindingConfig` does (`src/pkg/config/runs.go:38`).
6. Provide a deterministic fixture and conformance tests. Flue's fixture lives under `pkg/extwork/flue/fixture`, with adapter tests and smoke tests (`src/pkg/extwork/flue/conformance_test.go:1`, `src/test/extwork/flue_smoke_test.go:45`).

## Example: minimal Flue-style calls

**Illustrative only — not a runnable smoke test.** `flue-runtime.example` is a placeholder, and `"base64-or-json-bundle"` is not a valid demonstrated payload. For a known-good request, use the bundle construction and request sequence in [`examples/flue/run.sh`](../../../examples/flue/run.sh) and the [fixture README](../../../examples/flue/README.md).

The fixture runs as a separate process alongside the caller, not inside a contributor's coding session. From the Hive checkout, its example script starts `cmd/flue-fixture` with the workflow in `src/pkg/extwork/flue/testdata/flue-fixture/`. A real runtime must be started by its own operator and reachable from the Hive process; this guide does not install one.

```sh
curl -s http://flue-runtime.example/ | jq .

curl -s -X POST http://flue-runtime.example/dispatch \
  -H 'content-type: application/json' \
  -d '{"idempotency_key":"hive-example-key","payload":"base64-or-json-bundle"}' | jq .

curl -s 'http://flue-runtime.example/submissions?key=hive-example-key' | jq .
```

Hive makes those calls through the adapter, not shell commands; the snippet is for implementers validating their runtime surface.

**Expected fixture results:** runtime info names `engine`, `version`, and `incarnation`; first dispatch returns a submission ID and UID, replay with the same key/payload reports `deduplicated`, and a changed payload yields HTTP `409 submission_conflict`. Three explicit fixture ticks advance the deterministic workflow and expose `receipt.json`. Without an egress route its attempted effects are recorded as `no_route`, not silently published. The example then invokes conformance tests; repository agents should leave execution to CI as required by `AGENTS.md`.

## Testing

Use the engine-neutral tests in `pkg/extwork` for admission and receipt behavior, plus adapter-specific conformance. Flue's test suite verifies factory validation, duplicate/different payload behavior, status mapping, cancellation facts, artifact fetch, and fixture smoke paths (`src/pkg/extwork/flue/flue_test.go:58`, `src/pkg/extwork/flue/conformance_test.go:1`).

## Operational notes

- Auth: v5 Flue carries no auth header in the adapter. Put the runtime behind network controls or add a reviewed adapter auth field before using it outside a trusted environment.
- Rate limits and backpressure: use `waiting` for human, budget, or capacity waits instead of returning terminal failure.
- Failure modes: transport failure is `unknown`; incarnation mismatch is a refusal to adopt a possibly unrelated recreated runtime; artifact digest mismatch rejects the candidate while preserving evidence.
- To end a trial, restore the previous `runs.external.flue` settings (normally disabled) through the deployment's configuration path. Stopping future dispatch is not proof that an existing external worker stopped: cancellation reports requested, acknowledged, and stopped as separate facts.
- Terminology: describe this as an external workflow engine or contributor relay capability, not a GitHub-only integration.

## Gaps

- ClankeR is not an independent plugin marketplace or MCP server. It is Hive's contributor relay protocol plus code in this repository.
- The Flue binding is compile-time/build-tagged. A third-party engine still needs a Hive PR unless it mimics an already-wired protocol endpoint.
