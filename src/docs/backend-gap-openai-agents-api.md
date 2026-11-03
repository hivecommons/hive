# Gap analysis: the OpenAI Agents API as a Hive backend

This note checks the OpenAI hosted agent stack (the
[Responses API](https://platform.openai.com/docs/api-reference/responses), its
hosted tools, and the [Agents SDK](https://openai.github.io/openai-agents-python/))
against the acceptance bar in [backend-support-tiers.md](backend-support-tiers.md):
the "All tiers" criteria (1-7) and the T3 criterion (8). It is a child of
[#14093](https://github.com/hivecommons/hive/issues/14093) and closes
[#14108](https://github.com/hivecommons/hive/issues/14108). It is analysis
only. It changes no code and does not assign a tier; that decision belongs to
[#14111](https://github.com/hivecommons/hive/issues/14111), which reads this
note next to the Claude Managed Agents note from
[#14106](https://github.com/hivecommons/hive/issues/14106).

Vendor behaviour below is taken from OpenAI's public documentation
([shell tool](https://developers.openai.com/api/docs/guides/tools-shell),
[new tools in the Responses API](https://openai.com/index/new-tools-and-features-in-the-responses-api/),
[computer environment](https://openai.com/index/equip-responses-api-computer-environment/),
[Agents SDK tools](https://openai.github.io/openai-agents-python/tools/)).
Nothing here has been run against a live account. Exact field names and
network-policy options should be confirmed by the prototype in
[#14124](https://github.com/hivecommons/hive/issues/14124).

## What the product is

"OpenAI Agents API" is not one endpoint. Three pieces matter:

| Piece | What it does | Hive analogue |
| --- | --- | --- |
| **Responses API** (`/v1/responses`) | One model turn, or a server-side tool loop. Supports `background: true` (poll or stream later), `previous_response_id` and Conversations for state, and `store` for retention. | The model call plus the transcript |
| **Hosted tools** | Run on OpenAI's side: hosted shell/container (`environment: container_auto`), code interpreter, file search, web search, remote MCP. | The agent's tool surface |
| **Agents SDK** | A client library (Python, JS) that runs the agent loop on the caller's machine, calling the Responses API for model turns. Shell and file tools can be local or hosted. | The CLI binary |

Unlike Claude Managed Agents there is no persisted, versioned "agent" or
"session" object that owns the run. State is the chain of responses (or a
Conversation) and, for hosted shell, a container. Two shapes are therefore
possible, and they have different answers below:

1. **Hosted loop.** Hive calls the Responses API with hosted tools. The loop
   and the container are on OpenAI's side.
2. **SDK loop in a Hive pod.** An Agents SDK program (or `codex`, which Hive
   already has as a T1/T2 backend) runs in a Hive pod with local tools. Only
   model calls leave the pod.

Shape 2 is not a new backend: it is the existing CLI path with a different
client. The rest of this note is about shape 1 unless it says otherwise.

## How the four things the issue asks about map

### 1. Task prompt

The task prompt is the `input` of a Responses call; Hive's policy prompt is
`instructions`. Multi-turn follow-ups chain with `previous_response_id`.
`background: true` returns immediately and the run is polled with
`GET /v1/responses/{id}` or streamed, which suits an unattended lane. Cancel
is `POST /v1/responses/{id}/cancel`.

No gap. This is a straight API call.

### 2. Repo workspace

A hosted container starts empty. The options:

| Option | How | GitHub credential held by OpenAI | Can the agent push? |
| --- | --- | --- | --- |
| **A. Upload** | Hive uploads a tarball or `git bundle` as a container file; the agent works in the container and Hive downloads the result files. | None | No |
| **B. Clone in the container** | The agent runs `git clone` inside the hosted shell. | Whatever token it is given (env, header, MCP secret) | Yes, if the token allows it, and only if the container can reach `github.com` |
| **C. Remote MCP server (GitHub or Hive-run)** | The model calls an MCP server that OpenAI's side contacts directly. | The MCP authorization value passed in the tool definition | Depends on the server |
| **D. Local shell via the SDK** | Hive clones the repo into its own pod, as today; the hosted API sees only tool inputs and outputs. | None | Only through Hive's egress |

Getting the change back out under A and D uses the same path as every other
lane: Hive takes the patch and opens the PR through `hive-open-pr`.

### 3. GitHub credential boundary and egress path

Hive's rule is that agents never hold the App installation token. Writes go
through the relays in [github-write-surface.md](github-write-surface.md), and
the proxy hard-denies, for every agent mode (`denyRules` in
`src/crux/proxy/rules.go`):

- `POST /repos/{o}/{r}/pulls` (direct PR creation), and
- `PUT /repos/{o}/{r}/pulls/{n}/merge` (direct merge).

Those denials hold only because agent traffic is forced through the proxy by
the iptables redirect in [net-admin-requirement.md](net-admin-requirement.md).
**A hosted container's traffic never touches that proxy.** Whether the hosted
sandbox reaches GitHub directly therefore depends on the container's network
policy, not on anything Hive controls:

| Path to GitHub | Seen by Hive's proxy? | Write-capable? | Verdict |
| --- | --- | --- | --- |
| Hosted container with open outbound access and any GitHub token | No | Yes | **Direct egress. Advisory ceiling.** |
| Hosted container with a domain allowlist that includes `github.com` / `api.github.com` and a write token | No | Yes | **Direct egress. Advisory ceiling.** |
| Hosted container with a domain allowlist that excludes GitHub, or no network | n/a | No | Allowed |
| Remote MCP server with a GitHub credential in the tool definition | No. OpenAI's side calls the server. | Yes | **Direct egress. Advisory ceiling.** |
| Function tool (client-executed) that Hive answers | Yes. Hive runs the call and returns the output in the next request. | Only what Hive's relay permits | Allowed. This is how a hosted run asks Hive to write. |
| Read-only, single-repo token, short-lived | No | No: GitHub refuses the write | Allowed. Nothing for the proxy to deny. |
| SDK loop in a Hive pod with forced egress | Yes | Only through the relays | Allowed. Same posture as a CLI backend. |

Hosted web search and any hosted tool with its own outbound access also run on
OpenAI's side and are not bound by a container allowlist; disable them unless
a task needs them.

**Stated plainly.** Credential boundary: the full App token is never sent to
OpenAI, in any shape. At most OpenAI holds a short-lived, one-repository,
`contents: read` installation token minted per run by Hive. GitHub egress: a
hosted container that can reach GitHub with a write credential egresses
directly, bypasses the proxy, and is capped at advisory mode (`shadow` or
`report-only` in `pkg/extwork`, `ModeShadow` and `ModeReportOnly` in
`admission.go`). The proxy denials for `POST /pulls` and
`PUT /pulls/{n}/merge` are only guaranteed in the credential-less hosted setup
(no GitHub host, no GitHub token, no GitHub MCP), where Hive turns writes into
function-tool calls, and in the SDK-in-pod shape.

### 4. Transcript and cost telemetry

The Responses object carries usage and a full output list:

| Source | Fields | Use for metering |
| --- | --- | --- |
| `response.usage` | `input_tokens`, `output_tokens`, `total_tokens`, `input_tokens_details.cached_tokens`, `output_tokens_details.reasoning_tokens` | The per-response token record. Deduplicate on `response.id`. |
| `response.id`, `created_at`, `completed_at`, `status`, `model`, `previous_response_id` | Identity, timing, final state, and the model that served the call (unlike Managed Agents, the model is on the response) | Join key, timestamps, per-model attribution |
| `response.output[]` | Messages, reasoning items, tool calls and outputs | The transcript |
| `GET /v1/responses/{id}/input_items` | The inputs of a stored response | Transcript completeness when the chain is rebuilt |
| Streaming events (`response.completed`, tool-call events) | Same usage, live | Progress while a background run is in flight |

Gaps in metering: the usage object counts tokens only. Hosted tool charges
(container sessions, code interpreter, file-search calls, web-search calls) are
billed separately and are not documented as fields on `usage`; they would have
to be derived from `output[]` items or read from the organisation usage API.
There is no cost field and no server-enforced per-run spend cap comparable to a
session budget, so a budget gate has to be Hive's: cap `max_output_tokens` and
`max_tool_calls` per call and stop chaining when the governor says so. A stored
response is retained by OpenAI (`store`), which is a data-custody point to
record for any contributor-private spoke.

Mapping to Hive: the issue calls the target the "lineage ledger". There is no
package by that name. The nearest existing sink is the token pipeline in
`pkg/tokens` (see [token-tracking.md](token-tracking.md)), which records
tokens per agent and model with a cache split.

| Ledger field | OpenAI source |
| --- | --- |
| agent / lane | Hive-side, set at dispatch |
| model | `response.model` |
| input tokens | `usage.input_tokens` minus `cached_tokens` |
| cache-read tokens | `usage.input_tokens_details.cached_tokens` |
| cache-write tokens | not reported (OpenAI caching is automatic and has no write charge) |
| output tokens | `usage.output_tokens` (includes reasoning) |
| reasoning tokens | `usage.output_tokens_details.reasoning_tokens` (informational) |
| timestamp | `completed_at` |
| dedupe key | `response.id` |
| non-token charges | not in `usage`; unmetered until derived |

## Acceptance criteria, row by row

Status key: **met**, **gap** (Hive work or vendor evidence missing), **N/A**
(the criterion is about a local CLI binary with no counterpart).

| # | Criterion | Status | Reason |
| --- | --- | --- | --- |
| 1 | List parity (`KNOWN_BACKENDS`, `backend_binary`, `backend_perm_flag`, `CLIBackends`) | **N/A** for the hosted loop; **met** for the SDK-in-pod shape if it runs as `codex` | The hosted loop has no binary; a name in `KNOWN_BACKENDS` would make the shell launch path try to exec it. The counterpart is an engine adapter registered with `pkg/extwork` (`registry.go`), not wired for this engine. |
| 2 | Declared local posture (`localBackendPostures`) | **N/A** hosted, **gap** SDK-in-pod | A hosted run has no contributor-local path. An SDK worker's posture is that of its pod and no test pins it. |
| 3 | Flag-honoring proof | **gap** | The controls are request fields (container network policy, tool list, `max_tool_calls`, `max_output_tokens`), not flags. No transcript yet shows the API refusing an invalid network policy or tool config rather than ignoring it. |
| 4 | Honest credential detection (`detect_cli`) | **gap** | The credential is `OPENAI_API_KEY`. A cheap authenticated read (for example `GET /v1/models`) proves it works, the `codex_is_authenticated` pattern. Not written. |
| 5 | Documentation rows | **gap** | The tables named all describe CLIs. A hosted backend needs rows for auth, credential storage, container network policy, and the GitHub egress verdict above. |
| 6 | Pinned install | **N/A** hosted, **met by construction** SDK-in-pod | Nothing is installed for the hosted loop; the pin that matters is the model snapshot and tool version in the request. An SDK worker is pinned by lockfile. |
| 7 | Changelog fragment | **met by construction** | Any PR that wires the adapter adds one. This note is docs-only and needs none. |
| 8 | T3 refusal gate (`HIVE_<NAME>_DANGEROUSLY_RUN_UNCONFINED`) | **gap** | The env-var gate concerns launching an unconfined local CLI and does not transfer. The counterpart is a per-repo opt-in plus code that refuses a run config with a direct GitHub write path (every "advisory ceiling" row above) when the mode would allow writes. `pkg/extwork` already defaults to off and admits only shadow and report-only. |

The T1 criteria are outside this note's scope. An API is headless (criterion
10) and an API key in a fresh pod is an unattended credential (criterion 11).
The hard parts are 1, 2 and 8: the bar assumes a local binary Hive confines,
and in the hosted shape the loop and sandbox are not Hive's to confine.

## Other facts the tier decision needs

- **Lifecycle mapping to `pkg/extwork`.** Response `status` (`queued`,
  `in_progress`, `completed`, `failed`, `cancelled`, `incomplete`) maps onto
  `Observe`'s running and terminal states; `Cancel` is the cancel endpoint.
  `Start` needs idempotency on the execution key; the adapter should persist
  the response ID against the key (response `metadata` can carry it) and
  reconcile before any retry.
- **Approvals.** Remote MCP tools support `require_approval`, which gives a
  second approval point, but it is the client's choice per call, not Hive
  policy, and it does not cover hosted shell.
- **Spend.** The API key owner pays tokens plus hosted-tool charges. On a
  contributor-private spoke that is the contributor's key, which is why
  #14124 is parked for a maintainer decision (risk: spend).
- **Codex overlap.** Hive already drives OpenAI models through the `codex`
  CLI. The incremental value of a hosted OpenAI backend is the hosted
  container, which is also the part Hive cannot confine.

## Recommendation

**CLI-style for anything that must write; a new "hosted execution" tier for the
hosted loop, capped at advisory mode.**

- The **SDK-in-pod** shape is CLI-style and already covered by `codex`. It
  keeps both proxy denials and needs no new tier.
- The **hosted loop** is not CLI-style (criteria 1, 2 and 8 assume a local
  binary) and not gateway-path (there is no CLI whose model calls Hive can
  reroute; the loop and tools run remotely). It fits the `pkg/extwork`
  external-host contract: start by execution key, observe by status, cancel,
  receipt by artifact. That contract caps admission at shadow and report-only,
  which is the advisory ceiling the issue requires.

Suggested acceptance criteria for the hosted tier, for #14111 to adopt or
change:

1. Registered as a `pkg/extwork` adapter and passes its conformance tests.
2. The PR states which container network policy it uses and, per row of the
   egress table, why the `POST /pulls` and `PUT /pulls/{n}/merge` denials
   still hold. Any direct GitHub write path caps the engine at `report-only`,
   enforced in code.
3. Metering: each response's `usage` becomes a token record keyed on
   `response.id`; hosted-tool charges are recorded or the backend is shown as
   partly unmetered.
4. Per-call `max_output_tokens` and `max_tool_calls` are set from the governor's
   remaining budget, since there is no server-side spend cap.

Against Claude Managed Agents, the OpenAI hosted loop has the larger gap on
confinement (no self-hosted environment type that keeps tool traffic on Hive's
proxy, and no platform budget), and the smaller gap on telemetry (model and
usage on every response). Which to prototype first is for #14111.
