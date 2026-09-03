# Copilot per-repo cost capture at the MITM proxy (#4836 phase 4)

Status: **implemented on v5 for #5802.** The implementation follows the
timestamps-only path recommended below: Copilot completion usage that the MITM
proxy already observes is persisted as timestamped live-capture sessions, then
`/api/repo-cost` joins those events to the existing authenticated audit-log
`repo=` timeline. The proxy does not invent a repo from adjacent request
traffic; unsupported historical Copilot shutdown totals still remain in
`backend_unsupported`.

All citations are against `origin/v4` at `8d21a8aa`.

## Why phase 4 was proposed

Epic #4836 attributes agent token cost to the repository the work was for by
joining timestamped token usage to audited `repo=` events on `(agent,
time-interval)`. Phases 2–3 do this for Claude, whose transcripts carry
per-message usage with a per-entry timestamp.

Copilot has no such grain in its session files: `ScanCopilotSessions` accrues
`ModelMetrics` usage only in the `session.shutdown` case, so the whole session's
tokens land at one instant with no intra-session distribution
(`src/pkg/tokens/copilot_scanner.go:68`, the `session.shutdown` accrual). Interval
attribution from session files is therefore structurally impossible, exactly as
the epic states.

This is not academic. The reference multi-repo hive runs seven Copilot agents,
so on the deployment that motivates the epic, phases 1–3 yield no per-repo cost
at all. Phase 4 — capturing Copilot usage at the MITM proxy, where per-request
data exists — is the only proposed path to a number there.

## Question 1 — is per-request Copilot token usage available at the proxy?

**Yes, and it is already captured today.** This is the strongest finding on the
page, and it is stronger than the epic assumes: the epic describes phase 4 as
work to be done in `pkg/proxy/`, but the per-request capture half of it has
already shipped.

The path, end to end:

- The proxy MITMs the Copilot completion hosts. `IsCopilotAPIHost`
  (`src/pkg/proxy/rules.go:112`) matches `githubcopilot.com` and any
  `.githubcopilot.com` subdomain, covering the enterprise per-account hosts. Two
  call seams dispatch into the sniff path — the explicit-CONNECT seam
  (`src/pkg/proxy/github_proxy.go:799`) and the iptables-transparent seam
  (`:461`), both guarded identically by `p.tokenSink != nil && agentName != ""`.
- `proxyCopilotHTTP` (`src/pkg/proxy/github_proxy.go:2232`) runs a keep-alive
  HTTP request/response loop over the terminated TLS connection. It identifies
  completion requests via `isCopilotCompletionsPath`
  (`src/pkg/proxy/copilot_usage.go:40`, suffix `/chat/completions`), buffers the
  request body, and reads the model id out of it (`:2260`).
- For streaming requests it rewrites the body to carry
  `stream_options.include_usage=true` (`ensureStreamUsageRequested`,
  `src/pkg/proxy/copilot_usage.go:63`; call site `github_proxy.go:2262`).
  Without that hint the OpenAI-compatible endpoint omits the terminal usage
  chunk and streamed completions are uncountable. Note this is the one place the
  proxy alters an agent's request payload.
- `forwardCopilotResponseWithUsage` (`github_proxy.go:2311`) buffers the response,
  extracts usage via `extractCopilotUsage`
  (`src/pkg/proxy/copilot_usage.go:136` — SSE terminal-chunk path at `:104`,
  non-streaming JSON path via `extractOpenAIUsage`), resolves the concrete model
  when the request pinned the `auto` sentinel
  (`extractCopilotResponseModel`, `:147`), and records it at `github_proxy.go:2331`.

So at `github_proxy.go:2331` the proxy holds, per completion response:
`(agentName, resolvedModel, inputTokens, outputTokens)`, at a known wall-clock
moment. That is the per-request grain phase 4 wants, and it already exists.

`liveCaptureSinceMs` comes from exactly one production writer:
`tokenCollector.SetCopilotLiveCapture(time.Now().UnixMilli())` at
`src/cmd/hive/main.go:3175`, called immediately after `SetTokenSink`. The
collector stores it (`src/pkg/tokens/collector.go:162`) and passes it to
`ScanCopilotSessions` (`collector.go:212`), which zeroes shutdown tokens for
sessions whose `LastActive` is at or after that moment
(`copilot_scanner.go:108`) — the double-count guard the epic's must-not #1
names.

### What is lost between the proxy and storage

The timestamp is discarded one function call later. `InferenceSink.Record`
(`src/pkg/tokens/inference_sink.go:148`) folds each call into a cumulative
per-agent total — `inferenceAgentTotals` holds `Model`, `InputTokens`,
`OutputTokens`, `FirstSeen`, `LastSeen` (`inference_sink.go:44-52`) — and
`writeAgentFile` (`:192`) rewrites a **two-line** JSONL file per agent: one
`user` pin line and one `assistant` line carrying the cumulative totals. The
session id is deliberately stable (the agent name) so the collector's
dedup-by-session-ID replaces rather than sums across scans.

That design is correct for its purpose and was hard-won: `restoreFromDisk`
(`inference_sink.go:79`) exists because an in-memory-only sink restarted every
counter at zero and overwrote the persisted file with a lower value — the
"$18k→$394 regression" the `SetCopilotLiveCapture` comment names
(`collector.go:150-160`).

**So the epic's table row "Inference sink (proxy): cumulative per-agent, no
timestamps — ❌ impossible" is accurate about the sink, but misattributes the
cause.** The timestamps are not missing from the proxy. They are present at
`github_proxy.go:2331` and thrown away by the sink's cumulative-rewrite storage
model. That relocates phase 4's real work from `pkg/proxy/` to
`pkg/tokens/inference_sink.go`.

## Question 2 — is repo context available at the proxy?

**No.** Neither in the Copilot request, nor in per-agent proxy state, nor
recoverable from adjacent traffic. This is the finding that decides the page.

### 2a. Not in the Copilot request

The structs the proxy parses out of a completion request are `model`, `stream`,
and `stream_options` (`copilotUsageRequest`, `src/pkg/proxy/copilot_usage.go:32`).
No repo, workspace, or path field is read, and no request header is inspected
for one — `proxyCopilotHTTP` forwards headers verbatim and branches solely on
`req.URL.Path` (`github_proxy.go:2245`). A grep for repo-bearing Copilot
editor headers (`Editor-Version`, `Copilot-Integration-Id`, `X-GitHub-*`) across
`src/pkg/proxy/` returns nothing, and the sniff-test fixtures send only `Host`,
`Content-Type`, `Content-Length`, and `Connection`
(`src/pkg/proxy/copilot_sniff_test.go:155`, `:253`, `:450`, `:604`, `:682`,
`:750`).

Whether the live Copilot CLI sends a repo hint in a header or in the `messages`
array (a system prompt naming the workspace) is **not answerable from this
repository** — the proxy neither parses nor logs it. See
[Open questions](#open-questions).

### 2b. Not in per-agent proxy state

`GitHubProxy` (`src/pkg/proxy/github_proxy.go:152-207`) holds exactly one
repo-shaped field: `allowedRepos map[string]bool` (`:158`), built once at
construction from the hive's configured repo list (`NewGitHubProxy`, `:247`,
loop at `:270-272`). It is a hive-wide allow-set, identical for every agent —
the same defect the epic already rejected `HIVE_REPO` for. There is no
per-agent "current repo" or "last repo seen" map anywhere in the struct.

`agentName` itself comes from a UID lookup per connection:
`LookupUIDByLocalPort` then `p.uidMap.LookupByUID(uid)`
(`github_proxy.go:461` region; `UIDMap.LookupByUID` at
`src/pkg/agent/uidmap.go:80`). `UIDMap` maps UID ↔ agent name and nothing else
(`uidmap.go:24`, methods at `:43`, `:60`, `:80`, `:113`, `:129`). It carries no
repo dimension, and the lookup is per-connection with no cross-connection state.

### 2c. Not recoverable from adjacent traffic

The proxy *can* extract a repo from a URL path — `ExtractRepo`
(`src/pkg/proxy/rules.go:290`) matches `^/repos/([^/]+/[^/]+)` (`:279`) and
`^/([^/]+/[^/]+)\.git/` (`:281`). Two facts kill this as a phase-4 source:

1. **It has exactly one non-test caller**, `RepoFilterAllowed`
   (`rules.go:300`, call at `:307`), which uses it for an allow/deny decision on
   write methods and discards it. `RepoFilterAllowed` in turn has exactly one
   non-test caller, `github_proxy.go:975`, reached only when
   `len(p.allowedRepos) > 0` — so on a hive with no configured repo list the
   extraction does not run at all. It returns early for every non-write method
   (`rules.go:301-303`), so even where it does run it observes only writes.
   Nothing records the extracted repo, per agent or otherwise; the block path
   (`recordViolation`, `github_proxy.go:1213`) increments a per-agent counter and
   logs method and path, with no repo field and no timestamp.
2. **The git branch of the regex is effectively dead for clone traffic.**
   `github.com` — where `git clone`/`push` go — is registered in `githubHosts`
   for awareness (`rules.go:32-36`) but opaquely tunneled, not inspected:
   `NeedsMITM` returns true only for `api.github.com` and the Linear host
   (`rules.go:78-80`), and `NeedsInspection` (`:91`) is the gate every call seam
   consults. The comment at `rules.go:67-71` states the intent explicitly, and
   `github_proxy.go:803-806` repeats it at the CONNECT seam. Such a connection
   falls to `tunnelDirect` (`github_proxy.go:808`) or the raw relay
   (`:466`), so the proxy sees only SNI `github.com` (`extractSNI`, `:422`) and
   never a `/owner/repo.git/info/refs` path. The `gitPathPrefix` branch of
   `ExtractRepo` therefore cannot fire on real clone traffic, and the proxy
   cannot learn the repo the agent cloned.

That leaves `api.github.com` REST calls as the only per-request repo signal the
proxy sees. Recording those into a per-agent "last repo" and stamping subsequent
Copilot completions with it would be a **new heuristic**, not a recorded fact:
it asserts that a completion issued after a REST call to repo A is *for* repo A.
The agent may read repo A's issues and then reason about repo B for many
completions. The epic's own systematic-bias warning describes precisely this
failure, and its must-not list forbids inventing an attribution that looks
authoritative.

And it is strictly worse than what phase 3 already does: phase 3 joins against
the **audit log**, which is a durable, timestamped, mediated record of the
agent's real outputs (`repo=` k=v pairs at seven emission sites, per the epic's
corrected citation table, read back via `OutputActionsSince`,
`src/pkg/dashboard/audit.go:190`, with rotated-backup coverage via
`auditLogFiles`, `:223`). A proxy-side "last REST repo seen" is an inference
from read traffic with none of that durability and no rotation-aware reader.

## Question 3 — what would have to change, and what is the blast radius?

If phase 4 were built as scoped, the change set is:

| # | Change | Package | Risk |
|---|---|---|---|
| 1 | Timestamped per-request usage storage — either a new append-only sidecar or extending `InferenceSink` to keep an event list alongside the cumulative totals | `pkg/tokens` | **Medium.** The cumulative-rewrite model and its stable session id exist to stop the collector double-summing across scans (`inference_sink.go:192-197`); a second file in the same dir is globbed by the same collector scan. Getting this wrong reproduces the $18k→$394 class of bug, in the opposite direction. |
| 2 | Plumb a repo value to `Record` | `pkg/proxy` → `pkg/tokens` | **High**, and *there is no correct value to plumb* — see question 2. This is the blocker, not a difficulty. |
| 3 | Per-repo rollup + the epic's mandatory `unattributed` / `backend_unsupported` buckets and reconciliation test | `pkg/dashboard` | Low. |

Change 1 alone touches `pkg/tokens`, not the request path, and is therefore the
low-blast-radius half. Change 2 is the one that would touch
`forwardCopilotResponseWithUsage`, and that function is on the critical path for
**every Copilot completion every agent makes**. The existing code is visibly
written under that constraint:

- The sniff seams fall through to an opaque tunnel unless a sink is active and
  the agent is identified, "so Copilot traffic is never broken by usage capture"
  (`github_proxy.go:795-799`).
- Bodies are buffered under a 32 MiB cap and oversized bodies are still
  forwarded in full, with only usage extraction skipped
  (`copilotSniffBodyLimit`, `github_proxy.go:2225`).
- Every extraction helper returns a zero value rather than failing on a
  malformed body (`copilot_usage.go:47-53`, `:104-130`, `:147-173`).
- The one payload mutation, `ensureStreamUsageRequested`, is a documented no-op
  on any body it cannot parse and preserves every other field byte-for-byte via
  a generic map (`copilot_usage.go:63-97`).

A regression here does not degrade a metric; it breaks agent execution
fleet-wide, and the failure surfaces as agents going quiet rather than as an
error. The blast radius of change 2 is *all agent inference traffic*. That is a
real cost to weigh against a metric — and in this case against a metric that,
per question 2, could not be made correct anyway.

## Recommendation

**Phase 4 as scoped — per-repo Copilot cost via the proxy — should not be
built.** The reason is single and structural:

> Per-request token usage is available at the proxy and already captured
> (`github_proxy.go:2331`). Per-request **repo context is not available there in
> any form**, and the only proxy-side signal that resembles it — a "last
> `api.github.com` repo seen" derived from `ExtractRepo` (`rules.go:290`,
> one non-test caller, allow/deny only) — is an inference from read traffic,
> weaker than the audited `repo=` events phase 3 already joins against.

Phase 4 cannot produce a better repo attribution than phase 3. At best it
reproduces the same `(agent, time-interval) → repo` join one layer down, against
a *worse* repo source, while putting the change on the critical path for all
agent traffic.

### What is worth doing instead

The genuinely valuable, separable part of phase 4 is **timestamps, not repo
context**. If per-request Copilot usage were persisted with its timestamp
(change 1 above — `pkg/tokens` only, off the request path), Copilot would move
from "❌ structurally impossible" to the same footing as Claude after phase 2:
a timestamped usage time-series that phase 3's existing audit-log join can
consume unmodified. No proxy change, no new repo heuristic, no new blast radius.

That reframing is worth filing as its own issue and is the one recommendation
this page makes. It is deliberately *not* proposed here as a decision — see the
status header — and it inherits every must-not in #4836, in particular:

- must-not #1 (double-count across the scanner/sink boundary): a per-request
  event log and the cumulative file must not both be summed by the collector.
- must-not #2 (Σ `by_repo` + `unattributed` + `backend_unsupported` == hive
  total) as a test.
- must-not #7 (never attribute the leading interval): Copilot completions before
  an agent's first audited `repo=` event go to `unattributed`.

It also inherits the retention constraint recorded on #4836: the join window is
bounded by audit-log coverage and must be reported alongside any number.

## Open questions

Things this investigation could not settle from the repository, and what would
settle them:

1. **Does the live Copilot CLI send any repo/workspace hint the proxy could
   read?** The proxy parses only `model`/`stream`/`stream_options`
   (`copilot_usage.go:32`) and inspects no headers, so the repository cannot
   answer this. Settling it requires capturing one real completion request from
   a running Copilot agent — full headers plus the `messages` array — on a spoke
   with live capture active. **This is the one finding that could change the
   recommendation.** If a trustworthy repo hint exists in the request, phase 4
   becomes a recorded fact rather than an inference and is worth revisiting.
   Note the hint would still be *agent-supplied and unauthenticated*, the
   objection that killed the session-id route in the epic's rejected-routes
   table — so it would need the same scrutiny, not automatic acceptance.
2. **How much Copilot spend actually falls outside any audited interval?** The
   epic estimates ~80–90% attribution for single-repo Claude sessions and gives
   no Copilot figure. Measuring this on the seven-Copilot-agent reference hive
   would bound the value of the timestamps-only path before it is built. It
   needs phase 1's per-repo activity data (shipped, #4854) cross-referenced with
   Copilot completion timing — obtainable today from the `copilot usage
   recorded` log lines at `github_proxy.go:2332`, which already carry agent,
   model, and token counts at a known time.
3. **Would a second file in the metrics dir disturb the collector's scan?** The
   sink writes into `cfg.Data.MetricsDir` (`main.go:3171`) and the collector
   globs `*.jsonl` there (`inference_sink.go:15-18`). Whether an append-only
   per-request log can coexist with the cumulative file without being
   double-counted depends on the collector's dedup-by-session-ID behaviour,
   which was read but not exercised here. Verifying this is a prerequisite for
   change 1 and is a test question, not a design question.
