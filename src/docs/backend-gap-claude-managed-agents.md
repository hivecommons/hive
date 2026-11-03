# Gap analysis: Claude Managed Agents as a Hive backend

This note checks [Claude Managed Agents](https://claude.com/resources/articles/claude-managed-agents)
against the acceptance bar in [backend-support-tiers.md](backend-support-tiers.md):
the "All tiers" criteria (1-7) and the T3 criterion (8). It is the first child
of [#14093](https://github.com/hivecommons/hive/issues/14093) and closes
[#14106](https://github.com/hivecommons/hive/issues/14106). It is analysis
only. It changes no code and does not assign a tier. The tier decision belongs
to [#14111](https://github.com/hivecommons/hive/issues/14111), which reads this
note next to the OpenAI Agents API note from
[#14108](https://github.com/hivecommons/hive/issues/14108).

Verified 2026-10-25 against the Managed Agents API docs (public beta, header
`managed-agents-2026-04-01`). Vendor behaviour below is what those docs state;
nothing here has been run against a live session yet. That evidence is the job
of the prototype in [#14124](https://github.com/hivecommons/hive/issues/14124).

## What the product is

Managed Agents splits one agent run across four objects
([overview](https://platform.claude.com/docs/en/managed-agents/overview)):

| Object | What it holds | Hive analogue |
| --- | --- | --- |
| **Agent** (`/v1/agents`) | Model, system prompt, tools, MCP servers, skills. Persisted and versioned; every update is a new immutable version. | The agent definition plus the policy prompt |
| **Environment** (`/v1/environments`) | A container template: `cloud` (Anthropic runs the container) or `self_hosted` (a worker you run executes the tools). Networking policy lives here. | The contributor pod image and its egress rules |
| **Session** (`/v1/sessions`) | One run of an agent in an environment. Carries resources (files, repos), vault IDs, an optional spend budget, and an event stream. | One leased task |
| **Container** | Where tools (bash, file ops, code) execute. The agent loop itself runs on Anthropic's orchestration layer, not in the container. | The agent's tmux pane and workdir |

The important difference from every current Hive backend: **the agent loop does
not run on Hive's hardware at all.** Even with a `self_hosted` environment only
the tool calls run locally; the model sees tool inputs and outputs on
Anthropic's side. There is no binary to launch, so Hive would drive it as an
API client, not as a CLI.

## How the four things the issue asks about map

### 1. Task prompt

The task prompt goes in as a `user.message` event, either in `initial_events`
on `POST /v1/sessions` or posted to the session afterwards. Hive's policy
prompt maps to the agent's `system` field. A session can override agent fields
for one run, so a per-lane prompt does not need a new agent version. For a task
with one deliverable (a PR-sized change), `user.define_outcome` adds a rubric
and a grader loop; that is optional and costs extra model calls.

No gap. This is a straight API call.

### 2. Repo workspace

There are three ways to put a repository in front of the agent:

| Option | How | Credential held by Anthropic | Can the agent push? |
| --- | --- | --- | --- |
| **A. `github_repository` resource** ([docs](https://platform.claude.com/docs/en/managed-agents/github)) | Anthropic clones the repo into `/workspace/<repo>` before the run. | A GitHub token (`authorization_token`). It never enters the container; an Anthropic-side git proxy injects it into `git` and GitHub REST calls for that repo. It can be rotated on a running session. | Yes, if the token has `Contents: write`. |
| **B. File resource** | Hive uploads a tarball or `git bundle` with the Files API; it mounts read-only under `/mnt/session/uploads/`. | None. | No. The agent writes its result to `/mnt/session/outputs/`, which Hive downloads. |
| **C. `self_hosted` environment** ([docs](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)) | Hive's own worker claims the work item and runs the tools in a container Hive controls. `file` and `github_repository` resources are refused; Hive clones the repo itself, as it does today. | None for GitHub. The worker holds an environment key and a per-session work secret. | Only through whatever egress Hive's container allows. |

Getting the change back out is the open part. Under options B and C, and under
A with a read-only token, the agent cannot write to GitHub itself. Hive takes
the patch from `/mnt/session/outputs/` (B), or from its own workdir (C), and
opens the PR through `hive-open-pr` like any other lane. That fits the
`pkg/extwork` receipt model: `OpenArtifact` opens an artifact by execution key
and relative path, and the receipt is checked against a declared digest before
parsing.

### 3. GitHub credential boundary

Hive's rule is that agents never hold the App installation token. Writes go
through the relays in [github-write-surface.md](github-write-surface.md), and
the GitHub proxy hard-denies two calls for every agent mode (`denyRules` in
`src/crux/proxy/rules.go`):

- `POST /repos/{o}/{r}/pulls` (direct PR creation), and
- `PUT /repos/{o}/{r}/pulls/{n}/merge` (direct merge).

Those denials only work because the agent's traffic is forced through Hive's
proxy by the iptables redirect described in
[net-admin-requirement.md](net-admin-requirement.md). A `cloud` session's
traffic never touches that proxy. So the question per option is: **can the
sandbox reach GitHub with a write credential by a path Hive does not see?**

| Path to GitHub from a session | Seen by Hive's proxy? | Write-capable? | Verdict |
| --- | --- | --- | --- |
| `github_repository` resource with a write token | No. Anthropic's git proxy injects the token. The docs say GitHub REST calls for the attached repo go the same way (they also say opening a PR needs the GitHub MCP server); treat `POST /pulls` as reachable until a test shows otherwise. | Yes | **Direct egress. Advisory ceiling.** |
| `github_repository` resource with a read-only, single-repo token | No | No: GitHub itself refuses the write. | Allowed. The proxy denials hold because there is nothing to deny. |
| Vault `environment_variable` credential (e.g. `GH_TOKEN`) | No. Substituted at Anthropic's egress for the hosts listed on the credential. | Yes | **Direct egress. Advisory ceiling.** |
| GitHub MCP server through a vault credential | No. Anthropic's MCP proxy adds the credential. | Yes | **Direct egress. Advisory ceiling.** |
| `cloud` environment, `networking: unrestricted`, no GitHub credential | No | Only with a credential the agent finds or is given. Anonymous reads work. | Not acceptable as the default; use `limited`. |
| `cloud` environment, `networking: limited`, no GitHub host in `allowed_hosts` | n/a | No | Allowed. |
| Custom tool (`agent.custom_tool_use`) executed by Hive | Yes. Hive runs the call on its own side and answers with `user.custom_tool_result` over the same authenticated event stream. | Only what Hive's relay permits. | Allowed. This is how a hosted session asks Hive to write. |
| `self_hosted` worker inside a Hive pod with forced egress | Yes. Same proxy, same `denyRules`. | Only through the relays. | Allowed. Same posture as a local CLI backend. |

Two ways of running Managed Agents keep both proxy denials in force:

1. **Cloud, credential-less.** `cloud` environment, `networking: limited`,
   no GitHub host listed, `web_search` and `web_fetch` disabled, no GitHub MCP
   server, no `environment_variable` GitHub secret. The repo arrives as a file
   resource or under a read-only token that Hive mints for that one repo. Any
   write is a custom tool that Hive turns into a relay request.
2. **Self-hosted.** A `self_hosted` environment whose worker runs in a Hive
   pod. Tool traffic goes through Hive's proxy exactly as it does for a CLI
   backend. The worker must not hold `ANTHROPIC_API_KEY` (the docs warn it
   exposes an organisation-scoped key to tool calls); it needs only the
   environment key and the per-session work secret.

Every other setup (a write token on the repo resource, a vaulted GitHub
secret, the GitHub MCP server) gives the hosted sandbox a GitHub write path
Hive cannot see. Per the #14093 constraint, such a setup **cannot exceed
advisory mode**. In `pkg/extwork` terms that is `shadow` or `report-only`
(`ModeShadow`, `ModeReportOnly` in `admission.go`): report-only "starts work
but carries no publication credential of any kind".

One more custody point. Even a read-only token in option A is a GitHub
credential stored by a third party. It should be a short-lived installation
token scoped to one repository and `contents: read`, minted per session and
rotated or left to expire. It must never be the App's own key or a broad PAT.

### 4. Transcript and cost telemetry

Managed Agents reports usage at three levels
([events](https://platform.claude.com/docs/en/managed-agents/events-and-streaming),
[sessions](https://platform.claude.com/docs/en/managed-agents/sessions)):

| Source | Fields | Use for metering |
| --- | --- | --- |
| `span.model_request_end` event | `id`, `processed_at`, `is_error`, `model_request_start_id`, `model_usage.{input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens}` | One record per model request, with a timestamp and a stable event ID to deduplicate on. This is the per-call row. |
| `session.usage` event, and `usage` on the session object | Cumulative token totals; `list_cost` as `{amount, currency}` (integer string in cents, USD only, rounded to the cent); `active_seconds`; `server_tool_use.web_search_requests` and `web_fetch_requests` (the latter always 0 today) | The authoritative session total, including charges that are not tokens. |
| Per-thread `usage` (multi-agent sessions) | Per-thread `list_cost`, `active_seconds` | Breakdown only. The docs say per-thread figures do not sum to the session figure. |
| `stats.active_seconds` | Per-thread time, summed | Not the billed figure; `usage.active_seconds` is. |

The model name is not on the usage event. It comes from the agent version the
session pinned (or a session override), so the adapter has to record it at
dispatch.

`list_cost` is priced at public list rates, not the operator's contracted
price. Besides tokens it counts web searches ($10 per 1,000) and session run
time ($0.08 per hour). Hive's token pipeline (`pkg/tokens`) has no place for
those last two today. `InferenceSink` records tokens per agent and model, and
the closest existing shape is the four-field record behind
`RecordCopilotWithCache`. The issue calls the target the "lineage ledger";
there is no package by that name. The nearest thing is the collector's token
totals that feed the dashboard's spend headline and its "live token summary"
(see [dashboard-sections.md](dashboard-sections.md)).

The transcript is the event list: `GET /v1/sessions/{id}/events` is durable
and paginated, and `GET /v1/sessions/{id}/events/stream` is live SSE with no
replay. A client that reconnects must open the stream, then list, then
deduplicate by event ID. `agent.thinking` events mark progress but carry no
thinking text. Each session also has a Console trace URL. Hive's
`claude_scanner.go` reads Claude Code session JSONL and does not apply here;
a hosted backend needs its own sink.

Managed Agents also offers what Hive's budget gate would otherwise have to
build: a session `budget` (`{type: "limit", max_list_cost: {amount, currency}}`)
is a hard cap the platform checks before every model request. A session at its
cap pauses with `stop_reason: budget_reached` instead of overspending. The
adapter can set it from the governor's remaining weekly budget at dispatch. It
is create-only and can only be changed or removed afterwards.

## Acceptance criteria, row by row

Status key: **met** (the product does what the criterion needs, or the
criterion is satisfied by construction), **gap** (Hive work, vendor evidence,
or both are missing), **N/A** (the criterion is about a local CLI binary and
has no counterpart).

| # | Criterion | Status | Reason |
| --- | --- | --- | --- |
| 1 | List parity (`KNOWN_BACKENDS`, `backend_binary`, `backend_perm_flag`, `CLIBackends`) | **N/A** | There is no binary to launch or flag to pass. The `claude-managed-agents` engine is registered through `pkg/extwork` only under `extwork_claude_managed_agents`; it deliberately never enters the CLI registry. |
| 2 | Declared local posture (`localBackendPostures`) | **met** for the selected `self_hosted` shape | The prototype pins a `self_hosted` environment and requires `forced-egress` or `proxy-only`. The worker runs in the Hive pod and the direct-path refusal test covers the write boundary. |
| 3 | Flag-honoring proof | **partial** | The safety controls are API/config fields, not flags. Adapter validation rejects cloud environments, unsupported network policies, over-cap budgets, and unrecognised write paths; tests exercise the direct-path refusal. No live vendor invalid-policy transcript is claimed, so the promotion evidence remains open. |
| 4 | Honest credential detection (`detect_cli`) | **met** for the hosted contract | The adapter resolves only an API-key environment-variable or `/data/secrets`/`/secrets` file reference, authenticates with `GET /v1/environments/{id}` before dispatch, and never puts `ANTHROPIC_API_KEY` in the worker bundle. |
| 5 | Documentation rows | **met** for the hosted tier | Hosted rows now live in `agent-configuration.md`, `sandbox-isolation.md`, `contributor-relay.md`, `backend-support-tiers.md`, and the external-admission design. `backend-setup.md` and the `AGENT_BACKEND` CLI row remain unchanged because this is not a CLI backend. |
| 6 | Pinned install | **N/A** for `cloud`, **met by construction** for `self_hosted` | Nothing is installed for `cloud`. The pinned runtime identity is the explicit agent version and toolset version in the session request; the adapter itself is selected by a build tag. |
| 7 | Changelog fragment | **met** | `changelog.d/added-hosted-managed-agents.md` records the shipped prototype. |
| 8 | T3 refusal gate (`HIVE_<NAME>_DANGEROUSLY_RUN_UNCONFINED`) | **met for the hosted counterpart** | The binding defaults off, resolves enabled-without-mode to `shadow`, dispatches only in explicit `report-only`, and refuses write-capable stages for direct GitHub resource, vaulted-secret, and GitHub-MCP paths. |

The T1 criteria are outside this note's scope, but they are not where the gap
is. An API is headless by nature (criterion 10), and an API key or workload
identity in a fresh pod is an unattended credential (criterion 11). The hard
parts are 1, 2 and 8: the bar assumes a local binary that Hive confines, and
here the agent loop is not Hive's to confine.

## Other facts the tier decision needs

- **Availability.** Managed Agents is beta on the Claude API and Claude Platform
  on AWS. It is not offered on Bedrock, Vertex AI or Foundry.
- **Lifecycle mapping to `pkg/extwork`.** Session status (`running`, `idle`,
  `rescheduling`, `terminated`) maps onto `Observe`'s running / waiting /
  terminal states, with a transport failure reported as unknown. `terminated`
  covers both success and unrecoverable error, so the adapter must fetch the
  session to tell them apart. `Cancel` maps to a `user.interrupt` event.
  `Start` needs idempotency on the execution key; the docs reviewed say nothing
  about idempotent session creation, so the adapter must persist the session ID
  against the key (session `metadata` holds up to 8 keys) and reconcile before
  any retry.
- **Permission policies.** Server-run tools can be `always_allow`,
  `always_ask` (pauses for a `user.tool_confirmation`), or `auto` (the server
  decides; calls it denies cannot be overridden by the client). This gives Hive
  a second approval point, but it is the vendor's classifier, not Hive's
  policy, and it does not cover custom tools.
- **Web tools.** `web_search` and `web_fetch` run on Anthropic's servers in
  both environment types and are not limited by a self-hosted worker's egress.
  Disable them unless a task needs them.
- **Spend.** The API key owner pays: tokens at list price, plus run time and
  searches. On a contributor-private spoke that is the contributor's key. The
  prototype persists those fields in its ledger, reserves the remaining
  per-day budget before dispatch, and hard-caps the spoke at USD 25 per UTC day.

## Recommendation

**A new "hosted execution" tier, not CLI-style and not gateway-path.**

- It is not **CLI-style**: criteria 1, 2 and 8 assume a local binary whose
  flags Hive controls, and there is none.
- It is not **gateway-path**: that tier is about routing model calls from the
  Claude CLI through `pkg/proxy/anthropic_translate.go`. Managed Agents runs
  the whole loop, tools included, so the gateway rows (model picker, login,
  terminal) do not describe it.
- It fits the existing **`pkg/extwork`** external-host contract almost one to
  one: start keyed by execution key, observe by status, cancel by interrupt,
  receipt by artifact path. That contract already caps admission at shadow and
  report-only, which is exactly the advisory ceiling the issue asks for.

Suggested acceptance criteria for that tier, for #14111 to adopt or change:

1. The engine is registered as a `pkg/extwork` adapter and passes its
   conformance tests (refusal, receipt verification, cancellation).
2. The PR states which environment type it uses and, for each row of the
   GitHub egress table that applies, why the `POST /pulls` and
   `PUT /pulls/{n}/merge` denials still hold. Any direct write path caps the
   engine at `report-only`, enforced in code.
3. Metering: every `span.model_request_end` becomes a token record and
   `session.usage.list_cost` is reconciled against it; non-token charges
   (run time, searches) are recorded or the backend is shown as partly
   unmetered.
4. A session `budget` is set at dispatch whenever the hive is budget-gated.

Of the two shapes above, **self-hosted** is closer to how Hive already confines
agents, since tool egress goes through Hive's own proxy and needs no new
custody argument for GitHub tokens. The accepted hosted-tier decision in #14111
selected that shape for #14124; the OpenAI Agents API remains a separate
credential-less adapter proposal under #14108.
