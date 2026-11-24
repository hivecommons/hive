# Session taint: remembering what an agent has read (#18987)

Status: **Proposed, with the observe-only recorder built (phase 0 slice, see
[Phase status](#phase-status)).** Spec for RFC
[#18987](https://github.com/hivecommons/hive/issues/18987). The recorder is off
by default and never blocks, holds, restricts or changes any request or
action; no enforcement exists yet. Every phase below is off by default and
ships behind its own issue; the RFC stays open for discussion, and until it
decides the enforcement phases nothing here changes what an agent may do. The parent defence pages are [ioscan](../ioscan.md),
[the security model](../security-model.md) and
[the tool policy](crux-tool-policy.md).

Scope: a Hive-owned record of what each agent session has read, and a small
deterministic rule set that checks later outbound actions against it. Out of
scope: detecting prompt injection (that is `ioscan`'s job), replacing pattern
scanning, and any change to who may write what (that is ACMM mode and the
write-surface gate).

Line references are to `v5` at `122f54645` and give orientation. The identifier
next to each one is the stable reference. A statement marked **(unverified)**
was not confirmed against code and is listed again under
[Verification notes](#verification-notes).

## Summary of the decisions

1. **Coarse, session-level taint, not data-flow tracking.** Hive cannot see
   inside an agent's context, so a label means "this session has observed X",
   not "this output derives from X". Labels only grow within a scope.
2. **Labels come only from Hive observations**, never from an agent's claim.
   v1 sources: a secret-shaped string in a tool result Hive can see, a read of
   a private repository, the kick's own private target repository, and a
   canary hit.
3. **Sinks are the places Hive already mediates**: relay bodies, proxy writes,
   the push broker and the sandbox PR step. Network egress and MCP tools are
   named but mostly out of v1, because Hive cannot see inside them.
4. **The first enforcement verdict is `hold`, not `deny`**, reusing the
   tool-approval desk's `operator-approve` lane, so a false positive costs a
   click rather than a stalled task.
5. **Observe-only first, per rule, with written exit criteria.** The whole
   feature is absent from a hive that sets nothing.
6. **State is in the hive process**, small, never holds secret values, and
   fails toward `hold` only once a rule is enforced.

## Threat model

### What we protect

- **Confidentiality of things the agent could read but must not repeat in a
  wider audience.** Credentials and key material seen in tool output, content
  of private repositories, responses obtained with a brokered credential.
- **The audience boundary of each outbound action.** A comment, issue, PR or
  push has an audience: the readers of the destination repository.
- **Evidence.** An operator must be able to answer "why was this refused or
  held" from the audit log.

### From whom

| Actor | What they do | In scope |
|---|---|---|
| Prompt-injected agent | An issue, comment or file persuades the agent to read a sensitive source and relay a reworded copy through an allowed channel (RFC flow 4). | Yes. The main case. |
| Careless but honest agent | Reads a private issue while working, then summarises it in a public comment (RFC flow 1). No attacker needed. | Yes. Probably the most common case. |
| Agent that paraphrases or splits | Avoids every pattern deliberately or by accident. | Yes: this is exactly what pattern scanning cannot catch. |
| Hostile agent with arbitrary code execution | Defeats Hive's own process boundary or bypasses the proxy. | No. Covered by sandbox isolation and forced egress ([sandbox-isolation](../sandbox-isolation.md), [net-admin-requirement](../net-admin-requirement.md)). Taint inherits their failure modes. |
| Malicious operator or hub | Reads or edits state. | No. |

### Non-goals

Taint does not prove an action is safe, does not stop an agent leaking through
a channel Hive does not mediate, and does not judge whether information is
"actually" sensitive. It answers one narrow question deterministically: has
this session observed something that the destination's audience should not
see.

## What exists today

Every row below was read in code. The point of the table is to show that Hive
has strong per-message checks and no memory.

| Control | Where | What it does | What it cannot do |
|---|---|---|---|
| Per-kick canary plant | `addCanaryPreamble` (`src/pkg/scheduler/ioscan_canary.go:11`), called at `src/pkg/scheduler/kickmessage.go:341` | Adds a random `HIVE-CANARY-` token to each kick message. On by default: `CanariesEnabled` (`src/pkg/config/ioscan_config.go:129`) treats an absent key as true. | Only text Hive planted has a marker. A kick comment still says the canary is "opt-in"; the code defaults it on. |
| Canary registry and scan | `CanaryRegistry.Scan` (`src/pkg/ioscan/canary.go:137`) | Checks text, including decoded variants, against the agent's tokens and then every other agent's. | A paraphrase has no token. The file has no eviction (`delete`, `Remove` and `Prune` do not appear in it), so the registry grows per kick. Elsewhere **(unverified)**. |
| Canary at the proxy | `inspectCanaryEgress` (`src/crux/proxy/github_proxy_policy.go:266`), called at `src/crux/proxy/github_proxy_mitm.go:242` | Reads write bodies up to `maxGitHubWriteBodyScan` and denies with code `DenyReasonCanaryEgress` (`src/crux/proxy/denial_message.go:24`) in closed mode. Refuses `git-receive-pack` in closed mode because the pack cannot be scanned. | Only bodies on paths `githubWriteBodyMayLeak` accepts (`src/crux/proxy/github_proxy_policy.go:363`): issues, pulls, comments, reviews, git objects, GraphQL. |
| Canary at the relays | `scanCanaryText` call sites at `src/pkg/github/issue_request_watcher.go:1596`, `src/pkg/github/pullrequest.go:233`, `src/pkg/github/review_request_watcher.go:598`; advisory findings at `src/cmd/hive/eval_cycle_seams.go:722` | Same scan on relay bodies and on agent advisory findings; leak recorded as `ioscan_canary_leak` with a critical bead by `handleCanaryLeak` (`src/cmd/hive/github_client_config.go:482`). | Same limit: marker only. |
| Secret classifier | `FindSecret` (`src/crux/logscrub/handler.go:157`) over `secretPatterns` (`src/crux/logscrub/handler.go:37`) | Returns a category for the first exact-shape credential, never the value. Heuristic shapes are skipped on purpose "because refusing a push on one blocks legitimate work". | Exact shapes only. A token in an unknown format, or a reworded secret, is not found. |
| Push broker | The secret gate in `Broker.Run` (`src/crux/pushbroker/pushbroker.go:252`) | Rejects an outgoing diff containing a `FindSecret` hit, after fast-forward, protected-path and sign-off checks. The push itself is recorded through the effects boundary and the broker is built by the sandbox executor. | The scan covers the diff (`git diff`, or `git show` when no base resolves), not commit messages **(unverified beyond reading the two git invocations)**. It has no idea what the agent read. |
| Proxy modes and rules | `rules` (`src/crux/proxy/rules.go:135`), `AllowedByModeCaps` (`src/crux/proxy/rules.go:248`), `denyRules` (`src/crux/proxy/rules_deny.go:43`) | Method and path need a minimum ACMM mode; unknown writes are denied; direct PR create and merge are hard-denied. | Looks at the request, not at history. |
| Write-surface gate | `WriteSurfaceEnforceRefusal` (`src/crux/proxy/write_surface_enforce.go:49`) | For a lane under `write_surface.enforce`, every direct write is refused so the relays are the only write path. | Says nothing about relay content. |
| Which hosts are inspected | `NeedsMITM` (`src/crux/proxy/rules.go:89`), `IsCopilotAPIHost` (`src/crux/proxy/rules.go:124`) | TLS is terminated for the forge API host and the tracker host, and for completion hosts only to read usage. Everything else is an opaque tunnel (`tunnelDirect`, `src/crux/proxy/github_proxy_connect.go:605`) guarded by destination class (`plainForwardDenyReason`, `src/crux/proxy/plain_forward_guard.go:119`). | No payload visibility on other hosts, so no content-based egress rule is possible there. |
| Inference translator | `translateAnthropicMessage` handles `tool_result` blocks (`src/crux/proxy/anthropic_translate.go:102`, `src/crux/proxy/anthropic_translate.go:122`); caller identity by `translatorAgentName` (`src/crux/proxy/github_proxy_inference.go:74`) | For a rerouted route the conversation, tool results included, passes through Hive. | Only rerouted backends. A CLI talking straight to its provider goes through the opaque tunnel. |
| Agent identity | `identifyAgentFromReq` (`src/crux/proxy/github_proxy_identity.go:18`) | Attributes a connection to an agent by the socket owner's UID. With `HIVE_PROXY_ADVISORY_OK=true` it can accept a self-asserted name (`src/crux/proxy/github_proxy.go:303`). | Under that opt-out, attribution is forgeable, so taint keyed on it would be too. |
| Sentinel | `Evaluate` (`src/pkg/sentinel/sentinel.go:214`) | Heuristic findings on a PR's diff; applies a label and comment, "never blocks on its own". Its repo-privacy lookup `sentinelRepoPrivate` (`src/pkg/github/sentinel_sweep.go:104`) treats a failed lookup as private. | Post-hoc, per PR, no session memory. The fail-toward-private precedent is reusable. |
| Tool policy and desk | `ToolPresets` (`src/crux/config/tool_presets.go:5`), `Desk.Resolve` (`src/crux/toolapprove/desk.go:279`), [tool-approval-desk](tool-approval-desk.md) | Launch-time deny of the forge's MCP write tools; a desk with `auto-approve`, `security-scan`, `operator-approve`, `deny` verdicts and an ACMM ceiling. The page states the desk is default-off. | No taint input. Other MCP servers are not mediated by anything found. |
| Transcript review | [trajectory-review](../trajectory-review.md) | A second model reads a tail of each running agent's terminal transcript; fail-open by design. | A window, a model judgement, and not deterministic. |

What does not exist: any record of what a session has read, any label model,
any sink that consults one. The per-task canary is the existing one-bit
version of this idea ("this marker left the estate"). This design generalises
it from planted markers to observed reads.

## Proposed mechanism

### Scope and session identity

A **session** is the key `(agent, epoch)`. The epoch advances when:

- a sandboxed kick starts (one task per workspace, a natural boundary);
- the agent process restarts;
- an operator clears it.

For long-lived CLI agents the model's context survives between kicks, so
advancing the epoch at a kick boundary would claim a forgetting that did not
happen. v1 therefore advances only on process restart or operator action, with
an optional maximum age. This is the largest open trade-off; see
[Open questions](#open-questions).

### Labels

A closed set, each with an `origin` so a rule can compare audiences:

| Label | Meaning | Origin field |
|---|---|---|
| `secret` | A credential-shaped value was seen (a non-heuristic `FindSecret` category) or a canary was seen leaving. | category name, never the value |
| `private-repo` | Content from, or the working target of, a repository whose audience is narrower than some sink. | `owner/repo` |
| `credentialed-response` | A response fetched with a brokered credential from a host the operator lists as sensitive. | host |
| `unknown` | State could not be established (restart, store error). Only used by fail-closed rules. | reason |

Each label records first-seen time and the source that set it. Join is set
union; labels never weaken inside an epoch.

### Sources (Hive observations only)

| Source | Sets | Needs | Visible for |
|---|---|---|---|
| S0 Kick target | `private-repo(target)` at kick start when the target repository is private | host-side visibility lookup, which `sentinelRepoPrivate` already models | every sandboxed kick, all backends |
| S1 Translator tool results | `secret` | scan new `tool_result` blocks with `FindSecret` | rerouted inference routes only |
| S2 Proxy reads | `private-repo(R)` | request path names `R` (`ExtractRepo`), response is 2xx, visibility from a host-side cache | traffic through the inspected forge host; git over the opaque host is **(unverified)** |
| S3 Canary hit | `secret` | the existing leak handler | wherever a canary scan runs today |
| S4 Brokered credential use | `credentialed-response` | host list plus credential injection event | phase 5 |

S2 does not need to parse the response body: the request line identifies the
repository. It does need a response hook; today the proxy streams the response
verbatim (`http.ReadResponse` then `resp.Write` in
`src/crux/proxy/github_proxy_mitm.go`, around lines 396-429), so status
capture is a small addition, not a body tap.

Visibility lookups use Hive's own credential on the host, never the agent's,
and cache with a short TTL. A failed lookup for a **source** is treated as
private (taint more), matching `sentinelRepoPrivate`. A failed lookup for a
**sink destination** is treated as public (restrict more). Both directions
fail conservative, so both raise false positives; hence `hold` first.

### Propagation across an agent's tool calls

There is no per-value tracking. Propagation is the join above: once any tool
call in the session produces an observation, every later sink in that session
sees the label. This over-approximates on purpose: it survives paraphrase,
splitting and re-encoding, which is the gap in the RFC.

For text Hive sees, one cheap precision gain is optional in phase 5: remember
the matched secret value in memory only, and run the existing canary variant
matcher (`canaryScanVariants`, `src/pkg/ioscan/canary.go:242`) for it on later
sinks, so a verbatim or encoded copy is caught even with no label rule. It
must stay in memory and be dropped at epoch end. Persisting it, even hashed,
defeats the matcher (it needs plaintext to build variants) and widens the
blast radius of a state-file leak.

### Sinks

Each sink gets a descriptor `(kind, destination repo, destination audience,
agent, epoch)` and a pure function from `(labels, descriptor)` to a verdict.

| Sink | Where it is checked | Notes |
|---|---|---|
| S-relay: `hive-open-issue`, `hive-open-pr`, `hive-review` and the spool-processed equivalents | beside `scanCanaryText` in `pkg/github` | Host-side, attribution comes from the manifest, so it works for sandboxed lanes without proxy identity. Highest value, lowest risk. |
| S-proxy: direct REST and GraphQL writes by lanes not under `write_surface.enforce` | body-gate stage of the pipeline in [crux-tool-policy](crux-tool-policy.md), next to the canary gate | Requires UID attribution; observe-only when `HIVE_PROXY_ADVISORY_OK` is in effect. |
| S-push: the broker and any `git-receive-pack` | the broker, beside its secret gate and before the push token is minted (`src/crux/pushbroker/pushbroker.go:252`) | Destination visibility is the point: a push to a public fork with a `private-repo` label is the RFC flow 1 in code form. |
| S-pr: the sandbox PR step (`CreateAgentPR` after a broker push) | `src/crux/agent/sandbox_executor.go` after `broker.Run` | PR title and body come from workspace files (`sandboxPRText`), so they are agent-authored text. |
| S-advisory: agent advisory findings | the eval-cycle canary gate | Same text, same rule. |
| S-egress: connections to hosts the proxy does not inspect | `tunnelDirect` | Destination-only: allow or hold by host, never content. Disabled by default even in enforce; see risks. |
| S-mcp: non-forge MCP tools | none | Not mediated today. The desk could take labels as an input later. |

### Rule shape (v1)

A small fixed table, not a new rule language:

| Rule id | If session has | And sink is | Verdict |
|---|---|---|---|
| `taint.secret.public-write` | `secret` | any write whose destination audience is public, and any push | hold, later deny |
| `taint.private.audience` | `private-repo(R)` | a write to `R2` where `R2 != R` and `R2`'s audience is not known to be within `R`'s | hold |
| `taint.cred.public-write` | `credentialed-response` | a public write | hold |
| `taint.unknown.any-write` | `unknown` | any enforced sink | hold |

"Within" in v1 is equality, plus an operator-supplied list of repository
groups that share an audience. Real audience comparison (teams, forks) is
hard and not attempted.

Verdicts are `allow`, `hold` (create a desk item in the `operator-approve`
lane carrying a scrubbed summary of the proposed action) and `deny`. A `deny`
returns an agent-facing directive in the style of `WriteSurfaceEnforceReason`
(`src/crux/proxy/write_surface_enforce.go`) with a new reason code beside the
others in `src/crux/proxy/denial_message.go`, so the agent knows which rule
fired and can reroute.

## Where it lives

```text
  sources                          state                         sinks
  -------                          -----                         -----
  kick start (S0)  ─┐                                       ┌─▶ relays (pkg/github)
  translator (S1)  ─┤                                       ├─▶ proxy body gate
  proxy reads (S2) ─┼─▶  taint store  ──▶  evaluator  ──────┼─▶ push broker
  canary hit (S3)  ─┘   (hive process)    (pure fn)         ├─▶ sandbox PR step
                              │                              └─▶ egress (opt-in)
                              └──▶ audit entry per label set and per non-trivial decision
```

- **Package placement.** `crux/proxy` and `crux/pushbroker` are Crux ring
  packages and cannot import host packages (the shared `CanaryLeak` type exists
  for this reason: `src/crux/canary.go:9`). The store and evaluator therefore
  belong in a small Crux-ring package with an interface, wired by setters the
  way `SetCanaryScanner` is (`src/crux/proxy/github_proxy_setters.go:46`). The
  host supplies visibility lookups and the audit writer.
- **One process.** The proxy, relays and the sandbox executor run in the hive
  process, so an in-memory store with a single owner needs no distributed
  consistency. A hub only sees summaries, if any (not in v1).
- **Integrity.** The store is hive-owned memory, plus an optional hive-owned
  `0600` file following the canary registry's pattern (`canaryFilePerm` in
  `src/pkg/ioscan/canary.go`). Agents run as other UIDs
  ([security-threat-model](../security-threat-model.md)), so they cannot read
  or write it. This holds only while per-UID isolation holds.
- **Policy interaction.** The evaluator takes a read-only `TaintView`, so
  labels can later become an attribute in desk rules or in whatever runtime
  guardrail language #12815 or #17091 produce. v1 does not add a rule
  language; deciding whether labels are inputs to that language or a separate
  table is an open question below.

## Failure and fail-closed semantics

Fail-closed only applies to a rule in `enforce`. In `observe` every failure is
fail-open plus a log line and a counter; the feature must never make an
observing hive less available.

| Failure | `observe` | `enforce` |
|---|---|---|
| Store read error at a sink | allow, count `taint_store_error` | hold |
| Spoke restarted mid-epoch, state lost | nothing | session becomes `unknown` and rule `taint.unknown.any-write` holds writes until the kick ends or an operator clears it. Knob `on_state_loss: tainted\|clean`, default `tainted`. |
| Source scan exceeds its size or time budget | count `taint_unscanned` | label is not set, but a rule marked `strict` treats the session as tainted |
| Visibility lookup fails (source) | label set as private | same |
| Visibility lookup fails (destination) | would-hold logged | hold |
| Unattributed caller or advisory-ok attribution | do not record | do not enforce; refuse to enable enforcement while attribution is forgeable |
| Proxy bypassed (advisory-only egress, no forced redirect) | no observations | no observations; documented as a coverage loss, not a taint failure |

Taint never overrides a hard deny. It can only add `hold` or `deny` to an
action that every earlier gate allowed. It cannot grant anything.

## False positives and operator overrides

The expected false positive is concrete: a test fixture or documentation page
containing an access-key-shaped example string will set `secret` on the
regex alone (by construction of the pattern, not tested here). The design
answers in layers:

1. **Source precision.** Use only the non-heuristic `FindSecret` set, the same
   decision `logscrub` already made for pushes. Add a deterministic list of
   well-known documentation placeholder values that never taint.
2. **Verdict softness.** `hold` before `deny`. A hold costs one decision.
3. **Narrow labels.** `private-repo` compares audiences, so working inside a
   private repo and commenting in it taints nothing observable.
4. **Operator controls**, all audited:
   - release one action (single use, bound to the agent, epoch, sink and a
     digest of the payload, so it cannot be replayed on different text);
   - clear a session's labels;
   - allow-list a path, repository pair or fixture value;
   - set a rule to `observe`, `hold`, `deny` or `off` independently.
5. **Visibility.** Every hold names the rule, the label and its source, so an
   operator can decide without reading the transcript.

### False-positive budget and exit from observe

Proposed numbers, to be argued in the RFC. The values live as named
configuration constants, not literals.

A rule may leave observe when, over at least 14 days and at least 50
would-act events, an operator has classified every would-act event (or a
random 50 if more) and no more than 10 percent are false positives, with no
more than 2 false holds per agent per week projected at that rate. Below 50
events the rule stays in observe; a rule that never fires is not a candidate
for enforcement.

## Cost

- **Latency.** The hot path is S1: the translator scans every request body.
  The messages API shape resends full history each turn, and the translator
  converts every message in the request
  (`translateAnthropicToOpenAI`, `src/crux/proxy/anthropic_translate.go:24`),
  so naive scanning is quadratic over a long session. Scan only `tool_result`
  blocks not seen before in this epoch (dedupe by block id), cap bytes scanned
  per result with a named constant, and measure in phase 2. Proposed target:
  added p99 under 5 ms for typical bodies, with the figure to be replaced by
  measurement. `FindSecret` is a fixed list of regular expressions on Go's
  linear-time engine; the canary matcher uses byte tables for the same reason
  (`src/pkg/ioscan/canary.go`, comment above `canaryHexByte`).
- **Observe mode** scans off the request path where possible and drops
  observations on a full queue with a counter, so it can never add tail
  latency. Enforcement cannot: a source seen after the sink is useless, so S1
  scanning is synchronous once any rule is enforced.
- **Storage.** A session holds a handful of labels, under 1 KiB. Audit
  entries are written once per `(session, label, origin)` and once per
  decision that is not a plain allow on an unlabelled session, which keeps
  cardinality bounded by sessions, not requests. Counters are per rule, in the
  style of `effects.Recorder` (`src/crux/effects/effects.go:246`).
- **Persistence.** In-memory by default. An optional best-effort file copy,
  pruned at epoch end, is phase 5; unlike the canary registry it must not
  grow without bound.

## Coverage matrix

What is observable, by path. Backend names are not enumerated because which
backend uses which path depends on per-hive routing **(unverified per
backend)**; see [backend-support-tiers](../backend-support-tiers.md) for the
backend list.

| Path | S0 kick target | S1 tool results | S2 forge reads | Sinks reachable |
|---|---|---|---|---|
| Sandboxed kick, any backend | Yes | Only if rerouted | Only via inspected traffic | Relays, broker, PR step |
| Long-lived CLI agent, rerouted inference | n/a | Yes | Yes | Relays, proxy writes |
| Long-lived CLI agent, direct to its provider | n/a | No (opaque tunnel) | Yes for the forge API host | Relays, proxy writes |
| Local file reads, shell output, stdio MCP servers | n/a | No | n/a | Not observable by any path; only the sink side applies |
| Hosted agent runtimes | n/a | Not applicable | n/a | Out of v1 |

The UI and `hivectl` should show "not covered" per agent for each column
rather than implying protection. A label view that is empty must not read as
"nothing was read".

## Rollout

Every phase is independently shippable, default off, and ends with a decision
point. Config is a single `session_taint` block; absent means off, which is
byte-for-byte today's behaviour.

| Phase | Change | Behaviour change | Exit | Status |
|---|---|---|---|---|
| 0 | This record merges; RFC decides labels, scope, sink list, budget | none | RFC acceptance criteria met | Record merged; decisions proposed in the recorder PR, RFC open |
| 1 | Taint store, audit entry type, read-only API and `hivectl` view, no sources | none | Store passes its tests | Store and audit entries built (in memory, bounded); read-only API and `hivectl` view not yet |
| 2a | S0 and S3 sources, observe | audit only | Labels appear for real kicks | S3 (canary hit) built; S0 (kick target) not yet |
| 2b | S1 source, observe, off the request path | audit only | Latency and miss rate measured | Not started |
| 2c | S2 source with visibility cache and response status hook, observe | audit only | Visibility lookup rate and failure rate known | Not started |
| 3a | Rules on relays and the sandbox PR step, `would-act` logging | audit and counters | Per-rule data against the exit criteria above | Rules and would-act logging built for the issue, comment, PR and review relays; sandbox PR step not yet |
| 3b | Same on proxy writes and the push broker | audit and counters | Same | Not started (waits for the proxy changes in flight to land) |
| 4 | Enforce one rule at a time as `hold`, with release and clear | holds appear | Hold review shows the false hold budget met | Not started; nothing in the tree can hold or deny |
| 5 | Persistence across restart, long-lived scoping, in-memory dynamic canary, desk integration, optional egress rule, cross-agent | varies | Each split into its own issue | Not started |

Phases 2a to 2c are separate so the cheapest, highest-coverage source (S0)
does not wait for the harder ones. Phase 3 splits by sink class because the
relays are host-side and easy, and the proxy and broker carry the
attribution and latency risk.

## Phase status

What the observe-only recorder does today, and what it deliberately does not.

- **Off by default.** `session_taint.enabled` is absent or false: the observer
  is never built, no lookup is made, nothing is recorded or logged, and
  behaviour is byte-for-byte what it was. A change needs a restart.
- **Package.** `crux/sessiontaint`, stdlib only, so the Crux ring can take the
  same observer later. The evaluator (`Evaluate`) is a pure function of
  `(labels, sink)`; the observer wraps it with a bounded store.
- **What it records.** Labels (`secret` today; the others exist in the type and
  the rule table but have no source yet) and would-be verdicts. A label is a
  kind, an origin that is a category name or an owner/repo id, a source name and
  a timestamp. No credential, token, body, title or file content is stored,
  logged or audited, and free text given as an origin is replaced by the
  placeholder `invalid`.
- **Where labels come from.** S3 only: the existing canary leak handler sets a
  `secret` label, origin `hive-canary`, on the session of the agent whose token
  was seen. An unattributed caller is not recorded.
- **Where it looks.** The issue, comment, PR and review relays, once per relay
  write that completed: after every existing gate, retry and dedupe step, so the
  data never describes an action the hive refused or retried, and a reused issue,
  comment or PR is skipped. Observe-only has nothing to gate, so it runs after the
  write; the enforcement phases must move the check before it. A session with
  no label costs a map lookup. A session with a label triggers one destination
  visibility lookup (the hive's own credential, cached for an hour; a failed
  lookup counts as public and is not cached).
- **What it does with a finding.** One log line and one audit entry per
  distinct would-act decision per session, a counter per rule for every
  request, and a bounded ring of recent decisions. The only verdict that
  exists is `hold`, and it is only ever recorded. The observer entry points
  return nothing, so no caller can branch on them, and a panic inside them is
  absorbed.
- **Bounds.** Sessions (least recently used evicted), labels per session,
  decisions in the ring and distinct logged decisions per session are all
  capped by named constants; the `session_taint` keys `max_sessions`,
  `max_labels_per_session` and `max_decisions` override the first three.
- **Eviction.** The least recently used session is evicted at the session
  bound, which would drop a tainted session's labels. That is harmless while
  nothing is enforced; before enforcement, eviction must resolve to `unknown`
  rather than clean (see the failure table).
- **Restart.** The store is memory only. A restart starts every session clean
  and invents no `unknown` label; that failure mode belongs to enforcement
  (see [Failure and fail-closed semantics](#failure-and-fail-closed-semantics)).
- **Not built.** S0, S1 and S2 sources, the proxy and push-broker sinks, the
  sandbox PR step, the read-only API and `hivectl` view, operator release and
  clear, persistence, and any `hold` or `deny` behaviour.

## Test strategy

- **Evaluator.** Table-driven tests over `(labels, descriptor)` for every rule,
  asserting the verdict and the reason code. Properties: monotonic labels,
  evaluation is a pure function, `observe` never returns `deny`, no rule can
  turn a prior deny into allow.
- **Invariant tests, not just examples.** In the spirit of the project's
  existing rule that security tests assert the invariant: a test that every
  sink call site consults the evaluator (a coverage test over the sink list),
  and that every enforced failure mode in the table above resolves to `hold`.
- **Sources.** Feed recorded `tool_result` bodies with and without
  secret-shaped strings, including the documentation placeholder list, and
  private versus public repository responses including failed lookups.
- **Parity.** With `session_taint` absent, request and response behaviour is
  byte-identical, pinned the way the tool policy's migration is
  ([crux-tool-policy](crux-tool-policy.md)).
- **Restart.** Kill the process mid-epoch and assert the `unknown` path.
- **Adversarial.** Paraphrase and split-across-calls fixtures that the
  existing scanners miss must be held when a label is present. A fixture
  where the agent never read the source must not be held, to pin the
  false-positive side.
- **Performance.** A benchmark over large histories to confirm the dedupe
  keeps scanning linear.
- **Live.** Enforcement is not accepted on unit tests alone; it follows a
  report-only soak on a real hive.

## What this cannot catch

- **Reads Hive cannot see.** Local files, shell output, stdio MCP servers and
  anything on a path the proxy tunnels opaquely produce no label. A session
  can read a secret from disk and leave no trace.
- **Sinks Hive does not mediate.** An agent that can write to a destination
  outside relays, the proxy's inspected hosts and the broker is unconstrained
  by this design. Opaque egress can be restricted by destination only.
- **Other agents and other sessions.** Information handed over through a
  shared file, a comment another agent reads, or a human is outside the
  session. Cross-agent propagation is not in v1.
- **A restart or a reset the model survives.** If the context outlives the
  epoch, later sinks look clean though the agent remembers.
- **Information the audience check cannot rank.** Public-versus-private is
  coarse. Two private repositories with different readers look the same unless
  an operator lists them.
- **Bypass of the proxy.** Advisory-only egress and forged attribution disable
  the proxy-side halves.
- **Authorised leaks.** A human release or an operator allow-list entry is a
  decision to leak.
- **Sensitive content with no recognisable shape.** Personal data, internal
  hostnames and business text carry no label unless they came from a private
  source we can name.
- **Prompt injection.** Taint limits what a persuaded agent can repeat; it
  does not tell Hive the agent was persuaded.

## Open questions

1. **Scope for long-lived agents.** Epoch on process restart plus a maximum
   age, per kick, or per conversation? A label that never clears turns one
   read into a permanent block; one that clears too early is theatre.
2. **Visibility and coverage.** Is partial coverage acceptable if every agent
   view states "not covered" per source? Which backends route through the
   translator in practice? **(unverified)**
3. **Git over the opaque host.** Is a clone or fetch of a private repository
   visible to the proxy in a default deployment? If not, S2 is blind to the
   most common read. S0 narrows the gap for sandboxed kicks only.
4. **False-positive budget.** Are 14 days, 50 events, 10 percent and 2 false
   holds per agent per week the right numbers? Is `hold` acceptable for the
   first enforced rules on hives with no one watching the desk?
5. **Audience model.** Equality plus operator groups, or real audience
   resolution? The latter is large and unlikely to be right.
6. **Laundering across agents.** In or out? If in, what is the key: repo,
   task, issue?
7. **State.** In-memory only, a file following the canary registry's
   pattern, or a journal beside `effects.Boundary`? What is acceptable
   behaviour on a mid-task restart: `unknown`-tainted, or clean with a
   notice?
8. **Interaction with runtime guardrails and the authorization RFC.** Are
   labels inputs to that language, or is the v1 table enough until it lands?
9. **Latency budget.** What added latency is acceptable on the inference path
   at each ACMM level, and is a synchronous scan acceptable at L6 throughput?
10. **Canary registry growth.** Should the registry gain eviction as part of
    this work, since taint adds a second per-kick store?
11. **Egress.** Is destination-only control on the opaque path worth shipping,
    given builds and package fetches need many hosts?
12. **Commit messages.** The broker scans a diff; should message and branch
    text also pass the same gate in phase 3b? **(unverified, see below)**

## Risks

- **Operator fatigue.** A noisy rule trains operators to click release. The
  exit criteria exist to stop a noisy rule shipping; the release record must
  make a rubber-stamp rate visible.
- **False assurance.** An empty label view reads as safe. Coverage must be
  displayed beside labels.
- **Lookup dependence.** Source and sink decisions depend on repository
  visibility lookups that can fail and can race a visibility change.
- **Attribution fragility.** Everything proxy-side inherits UID attribution
  and its opt-out.
- **Scope creep.** Each added source or sink is a new way to be wrong. The
  phases are split so each can be dropped without losing the others.
- **Interaction cost.** The desk, the write-surface gate and the canary gate
  already compose in a documented order; a fifth decision point needs a place
  in that order and a reason code, or reviews of denials get harder.

## Exit criteria

#18987 stays open until the RFC records:

- [ ] a decision on the label set, scope and sink list for phase 1;
- [ ] the coverage matrix above confirmed per backend, or the unverified
      cells resolved;
- [ ] an agreed false-positive budget and the observe exit criteria;
- [ ] phases 1 to 3 cut into task-sized issues, each referencing this page.

## Verification notes

Verified by reading code on `v5` at `122f54645`: everything in the
[What exists today](#what-exists-today) table except the cells marked
unverified; the default-on canary; the single `FindSecret` call in the
broker; that only the forge API host and the tracker host are inspected by
`NeedsMITM`; that responses on the inspected host are streamed unmodified.

Not verified, and the reason:

- **Which concrete backends use the translator path.** Depends on per-hive
  routing configuration.
- **Git smart-HTTP to the main forge host being inspected.** `NeedsMITM` says
  it is tunnelled opaquely, yet the proxy carries git-path credential
  injection and git rules; the deployment-specific wiring was not traced.
- **Commit messages and branch names in the broker scan.** Read from the two
  `git` invocations in `outgoingDiff`, which exclude messages; not exercised.
- **Canary registry eviction elsewhere in the tree.** Only the registry file
  was searched.
- **Desk wiring in a live hive.** The desk page says default-off; this page did
  not check which agent tool calls reach it at runtime.
- **Repository visibility from the forge API.** That a repository record
  states its visibility is taken from the RFC text and from the sentinel's
  use of the `Private` field, not from a separate API check.
- **The latency figure, the false-positive numbers and the 1 KiB estimate.**
  Proposals, not measurements.

## References

- RFC [#18987](https://github.com/hivecommons/hive/issues/18987), with related
  [#12815](https://github.com/hivecommons/hive/issues/12815),
  [#17091](https://github.com/hivecommons/hive/issues/17091) and
  [#16966](https://github.com/hivecommons/hive/issues/16966).
- [ioscan](../ioscan.md), [security model](../security-model.md),
  [threat model](../security-threat-model.md),
  [sandbox isolation](../sandbox-isolation.md),
  [trajectory review](../trajectory-review.md).
- [Crux tool policy](crux-tool-policy.md),
  [tool-approval desk](tool-approval-desk.md),
  [gate-integrity invariants](gate-integrity-invariants.md).
