# One authorization point: decision seam, decision events and scoped credentials (#18988)

Status: **Partly shipped (shadow mode only).** Spec for RFC
[#18988](https://github.com/hivecommons/hive/issues/18988). The first slice of
the migration is in the tree, off by default and enforcing nothing: see
[Phase status](#phase-status). Everything else on this page is still a
proposal. The RFC stays open until the enforcement phases are decided; this
page is the concrete proposal to review against its acceptance criteria. Related records:
[crux-tool-policy.md](crux-tool-policy.md) (the proxy's own operation
vocabulary, a narrower scope than this page), [tool-approval-desk.md](tool-approval-desk.md)
(the one declarative rule layer that exists today), [crux.md](crux.md) (the
engine/hive split), and the operator pages
[github-write-surface.md](../github-write-surface.md) and
[audit-log.md](../audit-log.md).

Line references are to `v5` at `122f546` and are orientation only; the
identifiers are the stable reference. Every statement about current behaviour
below was read from the cited code. Where it was not, or was only inferred
from structure, it is marked **(unverified)**. Nothing here was run against a
live hive.

## Summary of the proposal

1. Add one **evaluation seam**, `authz.Decide(ctx, Request) Decision`, in the
   Crux module so the proxy, the push broker and the relays can all call it
   in-process. Input: actor, action, resource, context. Output: `allow`,
   `deny` or `hold`, a stable rule id, a reason code and the policy version.
2. The existing Go gates become the **built-in floor**, registered under
   stable rule ids. Behaviour is unchanged until an operator adds a rule.
3. Operators add rules in **CEL**, reusing the engine and error posture already
   used for tool approval and triggers. Operator rules can only **narrow**
   (deny or hold something the floor allows). They can never widen.
4. Every decision emits **one event** with a fixed schema to a dedicated
   decision stream, not into `/data/audit.jsonl`.
5. Credentials stay what the forge allows (repository plus permission set),
   but the broker mints them **after** an `allow`, **from the decision**, and
   the decision event records what was granted. The `pkg/mint` JWT is the only
   credential Hive controls end to end, and it is where per-decision scope is
   fully real.
6. **Fail closed** is per effect: an error on an enforcement point is a deny.
7. Migration is five independently shippable phases. Every phase before the
   last starts in **shadow mode**: evaluate and log, enforce nothing, prove
   parity against the existing decisions.

## Scope, and what this is not

In scope: write-side decisions at the three places that already mediate
writes to a forge or tracker: the MITM proxy (`src/crux/proxy`), the hive-side
request relays (`src/pkg/github/*_request_watcher.go`) and the push broker
(`src/crux/pushbroker`). Plus the audit record and credential issuance for
those decisions.

Not in scope:

- **Agent tool calls on the host** (shell, file, web fetch, MCP). That is the
  runtime-guardrails RFC [#12815](https://github.com/hivecommons/hive/issues/12815)
  (closed). It uses its own action vocabulary and decides inside the agent
  container, with Hive off the per-call path. This design shares only the
  **decision record** with it (see
  [Relationship to #12815 and #17091](#relationship-to-12815-and-17091)).
- **Org and team policy distribution**, signed bundles, principals and
  roles: [#17091](https://github.com/hivecommons/hive/issues/17091) (open).
  This design makes the policy source pluggable and does not pick the
  delivery mechanism.
- **Quotas.** The per-agent GitHub budget returns a 429 and is rate limiting,
  not authorization. It stays outside the seam in phase 1 (an open question
  below).
- **Containment.** Like the guardrails RFC, this is policy and evidence at the
  mediation points. The container and the forced-egress redirect are the
  boundary.

## Inventory of today's authorization decisions

### A. The GitHub MITM proxy

The order of the proxy gates is the order of statements in one function. The
table is that order at `122f546`. Reason codes are in
`src/crux/proxy/denial_message.go:13-25`.

| Step | Gate | Where | Reason code |
|---|---|---|---|
| 0 | hive's own traffic (`internalCallerName`) skips every agent gate | `src/crux/proxy/github_proxy_mitm.go:89-129`, `src/crux/proxy/github_proxy_violations.go:231` | n/a |
| 1 | reference-repo write gate | `src/crux/proxy/github_proxy_mitm.go:89-100` | `reference_repo` |
| 2 | repo paused | `src/crux/proxy/github_proxy_mitm.go:102-106`, `RepoPauseRefusal` in `src/crux/proxy/rules.go:478-489` | `repo_paused` |
| 3 | agent repo scope | `src/crux/proxy/github_proxy_mitm.go:107-110`, `AgentRepoScopeRefusal` at `src/crux/proxy/rules.go:542-552` | `agent_repo_scope` |
| 4 | repo auto-merge off (REST) | `src/crux/proxy/github_proxy_mitm.go:111-116`, `RepoAutoMergeRefusal` at `src/crux/proxy/rules.go:496-506` | `repo_auto_merge_disabled` |
| 5 | write-surface enforcement for a listed lane (REST, git push advert) | `src/crux/proxy/github_proxy_mitm.go:117-123`, `WriteSurfaceEnforceRefusal` at `src/crux/proxy/write_surface_enforce.go:48-73` | `write_surface_enforce` |
| 6 | Linear classifier | `src/crux/proxy/github_proxy_mitm.go:144-147`, `LinearGraphQLAllowed` at `src/crux/proxy/linear_rules.go:212-283` | `linear_policy` |
| 7 | GraphQL: auto-merge off, write-surface, mode classifier | `src/crux/proxy/github_proxy_mitm.go:158-182`, `GraphQLAllowedCaps` at `src/crux/proxy/rules.go:794-828` | `repo_auto_merge_disabled`, `write_surface_enforce`, `graphql_mutation`, `graphql_denied` |
| 8 | REST mode table with capability OR-grant and per-repo autonomy | `src/crux/proxy/github_proxy_mitm.go:196-214`, `AllowedByModeCaps` at `src/crux/proxy/rules.go:248-262` | `mode_denied`, or `hard_deny` when a deny rule matched |
| 9 | allowed-repo filter | `src/crux/proxy/github_proxy_mitm.go:215-219` | `repo_not_in_config` |
| 10 | issue shape | `src/crux/proxy/github_proxy_mitm.go:227-240` | `issue_shape` |
| 11 | canary egress | `src/crux/proxy/github_proxy_mitm.go:242-250` | `canary_egress` |
| 12 | write counters, then one 403 with `X-Hive-Proxy-Reason` | `src/crux/proxy/github_proxy_mitm.go:252-318` | (the code above) |
| 13 | per-agent write budget, a 429 | `githubBudgetRefusal` at `src/crux/proxy/agent_github_budget.go:143-170` | not a deny code |
| 14 | credential rewrite, then forward | `src/crux/proxy/github_proxy_mitm.go:347-355` | n/a |

The inputs the mode gates read:

- **Mode** comes from `/tmp/.hive-mode-<agent>`, read per request by
  `readAgentMode` (`src/crux/proxy/github_proxy_identity.go:168-181`). A
  missing, unreadable or unparseable file, or an unidentified agent, is
  `ModeAdvisory`: fail closed on that input. The four modes are
  `src/crux/agentmode/agentmode.go:13-20`.
- **Capabilities** come from `/tmp/.hive-caps-<agent>`. A missing file means
  none (`src/crux/proxy/github_proxy.go:30-36`). Today only `converse` exists
  (`src/crux/agent/capabilities.go:68-70`).
- **Per-repo autonomy** can swap the mode for one repository
  (`autonomyModeForRepo`, `src/crux/proxy/autonomy_levels.go:23-34`). For
  GraphQL it matches a `repository(owner:, name:)` literal in the query, and
  when repo levels exist but none matches, a merge or PR-write mutation is
  decided at `ModeAdvisory` (`src/crux/proxy/autonomy_levels.go:36-57`).

The rule tables:

- **Mode table:** first match wins over `(method, path regexp)`, deny by
  default on no match (`src/crux/proxy/rules.go:135-224`, evaluation
  `src/crux/proxy/rules.go:248-262`). Rows cover GitHub, GitLab and Gitea
  shapes. A `Capability` is an OR-grant that never narrows.
- **Hard denies:** PR or MR create and merge for every mode, checked first
  inside the mode function (`src/crux/proxy/rules_deny.go:43-93`,
  `DeniedMessage` at `src/crux/proxy/rules_deny.go:96-103`).
- **GraphQL:** a separate classifier keyed on mutation names
  (`src/crux/proxy/rules.go:584-600`, `src/crux/proxy/rules.go:794-828`).
- **Linear:** a separate table keyed on the GraphQL operation, deny on
  unknown, unparseable or truncated (`src/crux/proxy/linear_rules.go:212-283`).

Evidence: a block increments a per-agent counter and logs once per distinct
`(agent, method, path)` (`src/crux/proxy/github_proxy_violations.go:233-270`);
a write-surface refusal also writes an `agent_write_refused` audit record via a
setter wired in `src/cmd/hive/main.go:4281-4286`. **An allowed proxy request
leaves no per-decision record** beyond the write counters.

### B. The push broker

`Broker.Run` (`src/crux/pushbroker/pushbroker.go:174-300`) runs a fixed
sequence, each step an early return through `fail`
(`src/crux/pushbroker/pushbroker.go:603-610`), which logs and returns the error:

1. required fields (`validate`, `:302`);
2. hostile workspace config (`:181`, `src/crux/pushbroker/workspace_config.go:111`);
3. empty outgoing commits (`:189`);
4. fast-forward against remote and base (`:195-203`);
5. descend-from-lease when updating an existing PR branch (`:205`);
6. forged lane sign-off (`:209`, `:438`);
7. changed-files guard: empty push, then protected paths (`:213`,
   `src/crux/pushbroker/protected.go:15-33`);
8. outgoing-diff secret scan (`:244`);
9. mint token (`:248`), then push inside `effects.Execute` with
   `Kind: KindBranchPush` (`:283-288`).

The protected-path list is `DefaultProtectedPaths`
(`src/crux/pushbroker/pushbroker.go:29-37`). `Broker.ProtectedPaths` overrides
it per call (`:129`, `src/crux/pushbroker/protected.go:54-59`), but the only
production construction, `src/crux/agent/sandbox_executor.go:377-389`, does not
set it, and a grep found no config key feeding it. So the list is effectively a
code constant today. **(unverified outside the files searched.)**

The broker records a `Result` (`src/crux/pushbroker/pushbroker.go:156-172`)
with `ProtectedReject`, `SecretRejected` and `Error`. It carries no rule id, no
policy version and no credential descriptor. The `effects.Claim` it submits has
an `Actor` field (`src/crux/effects/effects.go:182-188`) that the broker leaves
empty.

### C. The hive-side relays

Agents write through request files that the hive fulfils with the App token.
Each relay has its own authorizer type and the same shape: a nil authorizer
denies everything.

| Relay | Authorizer and gates | Where |
|---|---|---|
| open PR | forge-resistance and ACMM push gate, repo policy gate, lane allowlist, repo pause, agent scope | `src/pkg/github/pr_request_watcher.go:152`, `:394-427` |
| push branch | authorizer, lane allowlist, default-branch refusal | `src/pkg/github/push_branch_request_watcher.go:85`, `:342`, `:390` |
| merge | agent and CanMerge, then pinned SHA, then merge-eligible binding, then lane policy gate | `src/pkg/github/merge_request_watcher.go:84-100`, `src/cmd/hive/merge_eligibility.go:399-432` |
| issue, review | authorizer and lane allowlist | `src/pkg/github/issue_request_watcher.go:248`, `src/pkg/github/review_request_watcher.go:126` |

A refusal writes the result file, renames the request to `.denied` and logs
(`src/pkg/github/pr_request_watcher.go:874-881`). A **success** is audited
through `recordWriteAudit` (`src/pkg/github/write_surface.go:217-222`, for
example `src/pkg/github/pr_request_watcher.go:743`). That is the one place an
allow already has a record, in `audit.jsonl`. Whether every relay audits its
refusals as well as its successes is **unverified**.

### D. Tool approval (the declarative layer)

- Base verdict: hard guardrails (direct PR create and merge by tool name),
  explicit tool deny patterns, repo allowlist, mode restrictions, read-only
  auto-approve, then an ACMM level ladder
  (`src/crux/toolapprove/decide.go:14-180`).
- Operator rules: CEL, compiled at load (`CompileRules`,
  `src/crux/toolapprove/rules.go:170-224`), cost-bounded
  (`maxRuleEvalCost`, `src/crux/toolapprove/rules.go:42`), typed
  `RuleRequest` variable (`:51-102`, `:126-143`). Error, non-bool and cost
  overrun are **no-match** (`Match`, `src/crux/toolapprove/rules.go:233-260`).
- Rule effects are only `auto-approve`, `security-scan` and
  `operator-approve` (`src/crux/toolapprove/desk.go:49-76`). **There is no deny
  effect.** A rule may move a verdict toward more authority; the ACMM ceiling
  clamps it afterwards (`src/crux/toolapprove/desk.go:301-355`,
  `ACMMCeiling` at `:122`). A base deny is terminal.
- Record: one `tool_approval` audit event per `EvaluateAndAudit` call with
  decision, rule, ceiling flag and rationale
  (`src/crux/toolapprove/decide.go:235-243`,
  `src/crux/toolapprove/types.go:233-260`).
- Default off (`src/crux/toolapprove/wire.go:28-31`). Wired in
  `src/cmd/hive/approvalwire.go:35`. The proxy does not call it **(unverified:
  no call from `src/crux/proxy` was found)**.

### E. Credentials

- **Forge tokens.** `ScopedTokenForRepos(tier, repos)` narrows an installation
  token to a permission set per tier and, when repos are named, to those
  repositories (`src/pkg/github/app.go:291-345`, `:440-444`). The
  `contributor` tier carries issues, contents and pull-requests write
  (`src/pkg/github/app.go:331-337`). The broker uses `DefaultTier = "contributor"`
  (`src/crux/pushbroker/pushbroker.go:26`), so a push token also carries issue
  and PR write that a push does not need.
- **Callback mint.** `MintToken` returns a response with an `ExpiresAt` for
  the minted token (`src/pkg/cruxhost/cruxhost.go:220-241`). The relation
  between that value and the forge's fixed one hour token life is a
  question for phase 5, not a finding here.
- **Mint JWT.** `pkg/mint` signs a scoped JWT for downstream verifiers:
  `tierScopes` (`src/pkg/mint/agent.go:43-55`), unknown tier gets read-only
  (`:60-66`), empty agent name is an error (`:96-104`). TTL is clamped to a
  15 minute default and a one hour hard cap
  (`src/pkg/mint/mint.go:35-43`, `:172-186`). Each token has a `jti`
  (`src/pkg/mint/mint.go:138-170`). It is opt-in and additional to the App token
  (`src/pkg/mint/agent.go:8-16`).

### F. Per-agent permission modes

The mode is a function of agent name and ACMM level (`DefaultAgentMode`,
`src/crux/agentmode.go:25-60`), with an optional per-agent `Config.Mode`
override, written to the mode file by `SyncModeFiles`
(`src/crux/agent/manager_modes.go:38-90`). The token tier is a fixed mapping
from mode (`TokenTier`, `src/crux/agentmode/agentmode.go:73-86`). The tool
approval desk uses the ACMM **level** ladder; the proxy uses the **mode**
ladder. Two ladders are in force.

### G. The audit log

`/data/audit.jsonl` is a JSONL file with `ts`, `user`, `action`, `detail`,
`agent`, `repo`, `target` (`src/docs/audit-log.md`, `logRecordAt` at
`src/pkg/dashboard/audit.go:263-300`). It rotates at 5 MB with 3 backups and a
90 day age, and keeps a 500 entry in-memory ring
(`src/pkg/dashboard/audit.go:26-33`). A failed write is logged and dropped
(`:290-294`). Runtime code reaches it through `crux.AuditSink`, a one-method
interface the runtime defines and Hive implements
(`src/crux/audit.go:84-90`). There is no hash chain; #17091 asks for one.

## Where they disagree or duplicate

Each item is a reading of the code above, not a bug report. Items marked
"decision" are ones the seam must resolve explicitly.

1. **The same action is decided in up to four places with different inputs.**
   A branch push can be a proxy `git-receive-pack` (mode and write-surface), a
   `hive-push-branch` relay (authorizer, lane allowlist, default-branch
   refusal) or a sandbox push through the broker (protected paths, secret
   scan, sign-off). Protected paths and the secret scan are in the broker only;
   no protected-path check was found in the relay file **(unverified beyond
   that file)**.
2. **PR create and merge are denied in three copies.** REST `denyRules`,
   the tool-name guardrails in `src/crux/toolapprove/decide.go:14-40`, and
   the relay authorizers. The wording is duplicated, not shared.
3. **Decision parity across wire shapes.** The same operation can reach the
   proxy as REST or as GraphQL, through different classifiers. Verifying
   decision parity between the two paths is tracked separately and is not
   argued in this record.
4. **Hard deny is not a first stage.** It runs inside the REST mode check,
   after pause, scope, auto-merge and write-surface, so a paused repo returns
   `repo_paused` for a PR create. Reason codes are machine readable, so the
   order is observable behaviour (see crux-tool-policy.md).
5. **Pause and agent scope exist twice** (proxy gates 2 and 3, and the PR relay
   at `src/pkg/github/pr_request_watcher.go:411-427`), so a change to either
   has to be made in two places.
6. **Two ladders.** AgentMode (4 rungs, proxy and tokens) and ACMM level (7
   levels, tool approval) are related by `DefaultAgentMode` and not by one
   function either side calls.
7. **Allow is recorded in some paths and not others.** Relay successes and tool
   approvals are audited. Proxy allows, broker pushes and relay refusals have
   no decision record. A broker success shows up only as the downstream PR
   audit, not as a record of which checks passed.
8. **Error posture differs by layer.** Tool approval: error is no-match, which
   is safe for rules that grant. Proxy classifiers: unparseable bodies are
   denied (`src/crux/proxy/linear_rules.go:212-235`,
   `src/crux/proxy/rules.go:804-808`); issue-shape treats an unreadable body
   as a refusal to continue (`src/crux/proxy/github_proxy_mitm.go:227-236`).
   Audit failure is logged and dropped. There is no single stated rule.
9. **Credentials are tier-shaped, not action-shaped.** A push token carries
    more than a push needs, and the decision does not record what was
    granted.
10. **Operator rules cannot be added for the write path at all.** The only
    operator-authored rule layer is the approval desk, and it cannot deny.

## Proposed design

### The request

A typed, flat, documented struct. Flat on purpose: it is the stable surface
operator rules are written against, as `RuleRequest` is for approvals
(`src/crux/toolapprove/rules.go:47-50`).

| Group | Fields |
|---|---|
| actor | `agent` (name or `hive-internal`), `mode`, `acmm_level`, `capabilities`, `identified` (bool; false when UID attribution failed) |
| action | `kind` from one vocabulary (see below), `surface` (`proxy`, `relay`, `broker`), `forge` |
| resource | `repo` (`owner/name`), `branch`, `number`, `paths` (changed files, capped), `labels`, `target_state` (draft, default branch, protected) |
| context | `lane`, `write_surface_enforced`, `repo_paused`, `repo_auto_merge`, `request_id`, `policy_inputs_version` |

The action vocabulary is the existing `effects` kinds
(`pull_request_create`, `branch_push`, `issue_create`, and so on, from
`src/crux/effects/effects.go`) extended by the operation names
[crux-tool-policy.md](crux-tool-policy.md#1-policy-model-shared-operations-per-forge-adapters)
already proposes (`ref.write`, `branch.protect`, `git.fetch`). **One
vocabulary, not two.** The proxy adapter that crux-tool-policy.md describes
is the producer of `kind` for the proxy surface; this design adds the broker
and relay producers and the evaluator behind them.

Request bodies, diffs and file contents are never in the request. Only
attributes derived from them (path list, a boolean `secret_found`) are.

### The decision

```
Decision {
  Effect         allow | deny | hold
  RuleID         stable id, e.g. "floor.proxy.hard_deny.pr_merge" or "op.<name>"
  ReasonCode     existing proxy codes retained, new ones namespaced
  PolicyVersion  hash of built-in set version + operator bundle
  Obligations    optional: credential scope the caller must request
  Shadow         true when computed but not enforced
}
```

`hold` means "a human must decide". It reuses the tool-approval inbox
(`toolapprove.Inbox`) rather than adding a second queue.

### Layers and order

Written down once, replacing "order of statements in `proxyHTTPHost`":

1. **Floor** (built-in, Go). Today's gates, each registered under a stable
   rule id with an explicit stage number: attribution, pause, scope, hard
   deny, write-surface, mode table, repo allowlist, shape, protected paths,
   secret scan, merge binding. A floor deny is terminal.
2. **Operator narrowing rules** (CEL). Evaluated only after the floor
   allowed. Effects: `deny`, `hold`. No `allow` effect exists, so a rule
   cannot widen.
3. **Ceiling.** The ACMM ceiling is applied last, as the approval desk does
   (`src/crux/toolapprove/desk.go:330-344`).

Because a rule cannot widen, the first matching `deny` wins and `hold` loses
only to `deny`: most restrictive wins, which is also the precedence the
runtime guardrails RFC settled on. Order among operator rules then does not
change the outcome, only which rule id is credited.

This differs deliberately from tool approval, where a rule may move toward
more authority up to the ceiling. The two layers stay separate (next
section), and their different directions are documented rather than hidden.

### Where it runs

In the Crux module, as a package (working name `src/crux/authz`), because the
proxy and the broker are in that module and must not import Hive. The relays
are in the Hive module and may import Crux. This is the existing seam shape:
the runtime defines an interface and Hive supplies the implementation, as it
does for `crux.AuditSink`.

- **Evaluation is in-process at each enforcement point.** No network hop on the
  proxy path.
- **Policy input** reaches the evaluator through a `PolicySource` interface
  Hive implements (operator file, later a delivered bundle). The evaluator
  keeps the last good compiled set.
- **Decision events** leave through a `DecisionSink` interface Hive implements,
  which is the only way a decision crosses the module boundary, so it adds no
  new trust path: it is the same direction as `AuditSink` and
  `SetWriteRefusedAuditFunc` (`src/cmd/hive/main.go:4283-4286`).
- The evaluator never receives a credential and never mints. Minting stays with
  the existing `TokenScoper` seam.

### Policy language: CEL for the operator layer

| Option | For | Against |
|---|---|---|
| **A. CEL, same posture as approvals and triggers** (recommended) | Already a dependency and already reviewed; compile at load, typed native struct, cost cap, deny on error are all existing patterns (`src/crux/toolapprove/rules.go`, `src/pkg/celtrigger/celtrigger.go:141-153`); operators who wrote approval rules already know it | Two CEL environments to keep in step; CEL is expression-only so a rule is a predicate, not a program; error posture needs the per-effect inversion below |
| B. Go tables only, versioned and logged | No new language; smallest change | Fails the RFC's second gap: an operator still needs a release to add a path rule |
| C. A new Hive YAML policy DSL | Friendly for the common case (a path list) | A second language and compiler to maintain; the guardrails RFC chose a YAML language for host actions, but its compile target is a separate engine, which does not apply here |
| D. An external policy engine | Mature tooling | A new runtime dependency and process on the proxy hot path; rules and Go types must be kept in agreement; heavy for a four-field request |

**Recommendation: A.** Share the environment builder (`hasLabel`, cost cap,
native-type registration) as a small helper so approvals, triggers and authz
agree on it, but keep three typed request structs. For the very common case
(extra protected paths, extra denied branches) offer **data-shaped sugar** in
the policy file that compiles to the same CEL, so most operators never write
an expression.

### Error and fail-closed semantics, per effect

The RFC's gap 5 is that "error is no-match" is safe for a grant and unsafe for
a deny. The seam makes the rule explicit.

| Situation | Result |
|---|---|
| Floor gate errors, panics or times out at an enforcement point | **deny**, reason `authz_error` |
| Operator `deny` or `hold` rule evaluation error, cost overrun or non-bool result | **treated as matched**: deny (or hold) with reason `rule_error` and the rule id |
| A required attribute is missing (repo, branch, agent) | deny, reason `missing_attribute` |
| Agent could not be identified | existing behaviour kept: decided at `ModeAdvisory` (`src/crux/proxy/github_proxy_identity.go:168-181`) |
| Whole evaluation exceeds its time budget | deny, reason `authz_timeout` |
| Policy file fails to load or compile | **keep the last good version**, raise an alert, never "no rules"; the built-in floor always runs |
| Decision sink unavailable | decision still enforced; event spooled to a bounded local buffer; drop counter exported. Never fail open, and never block a deny on the sink |
| Hold on the proxy path | **deny with reason `hold_required`** plus an inbox item (see open questions) |

Inverting the error posture for deny rules means a broken narrowing rule can
over-block, which is the safe direction and shows up immediately in the
decision stream as `rule_error`.

### Decision events

One event per decision, allow and deny, on a dedicated stream.

```json
{
  "v": 1,
  "ts": "2026-11-18T14:31:02.123Z",
  "decision_id": "dec_01J...",
  "surface": "proxy",
  "effect": "deny",
  "shadow": false,
  "rule_id": "floor.proxy.repo_paused",
  "reason_code": "repo_paused",
  "policy_version": "b7e1c0...",
  "actor": {"agent": "reviewer", "mode": "ISSUES_AND_PRS", "acmm_level": 5, "identified": true},
  "action": {"kind": "ref.write", "forge": "github"},
  "resource": {"repo": "org/name", "branch": "topic/x", "paths_count": 3},
  "latency_us": 412,
  "credential": {"kind": "installation", "repos": ["name"], "permissions": ["contents:write"], "expires_at": "2026-11-18T15:31:02Z"}
}
```

Rules for the record:

- **Attributes, never payloads.** No bodies, diffs, file contents, tokens or
  token digests. Paths are listed only up to a cap, otherwise counted.
- **Stable schema, versioned** with `v`. Reason codes keep today's spellings.
  New fields are additive.
- **`credential` is present only when one was minted**, and for an
  installation token records repos, permission names and expiry. Installation
  tokens have no id of their own, so `decision_id` is the join key. For a
  mint JWT the `jti` is recorded.
- **Allows on high-volume reads are aggregated, not dropped.** A documented
  rule: reads (`git.fetch`, `read`) are counted per
  `(agent, kind, rule_id, effect)` per minute and one summary event is
  emitted; all writes, holds and denies are individual events. The summary
  says it is a summary.
- **Separate stream, not `audit.jsonl`.** The audit log rotates at 5 MB
  with 3 backups and a 500 entry ring
  (`src/pkg/dashboard/audit.go:26-33`); allow events at proxy request rates
  would evict the human-action history it exists to keep. The dashboard
  Audit Log can read both and merge on `ts`. Writes of individual decisions
  to `audit.jsonl` stay as they are.
- **Integrity.** The writer is the Hive process; the agent has no path to the
  file. **(unverified: file mode and mount of the decision stream are to be
  specified in phase 2.)** Tamper evidence (hash chain, off-host shipping) is
  owned by #17091 Pillar F, and this stream adopts it when it lands.

Volume has not been measured **(unverified)**. Phase 2 exists to measure it
before anything depends on it.

### Credentials and `pkg/mint`

Two credential families exist and they differ in how far "per decision" can go.

**Forge installation tokens (what the broker and relays use).** The forge
sets the floor: a repository set plus a permission set, and a fixed one hour
life. Hosted installation tokens cannot be narrowed to a branch or a single
operation. So per-decision here means three concrete things and no more:

1. **Mint after allow, from the decision.** The caller asks for a token only
   once `Decision.Effect == allow`, and `Obligations` states the repository and
   the permission names.
2. **Narrow the permission set to the action.** Add action-shaped tiers
   beside the mode-shaped ones, for example a push-only set (contents write,
   metadata read) so a push token no longer carries issue and PR write
   (`src/pkg/github/app.go:331-337`). Whether contents write alone is enough
   for every push path, including branches with workflow files, needs a live
   check **(unverified)**.
3. **Require a complete scope.** A missing repo attribute is
   `missing_attribute`, not a request for a broader token.

Beyond that it is **record-keeping**: the decision event says what was
requested and when it expires. It does not make the token narrower than the
forge allows, and the design does not claim it does. The expiry the
callback mint reports should be reconciled with the forge's token life
before the event repeats it (`src/pkg/cruxhost/cruxhost.go:220-241`).

**Mint JWTs (what downstream verifiers use).** Here Hive controls the claims,
so per-decision is real. Phase 5 extends `MintAgentToken`
(`src/pkg/mint/agent.go:96-104`) with a request-scoped variant whose scope is
one action on one repository (for example `contents:write:org/name`), whose
`jti` is the `decision_id`, and whose TTL is the shortest the clamp allows
(`src/pkg/mint/mint.go:172-186`). It stays opt-in and additional to the App
token, as today. A verifier can then reject a token that was not minted for
the action it is being used for, and the decision stream joins to the
verifier's logs on `jti`.

### Relationship to #12815 and #17091

- **#12815 (closed):** the guardrails decide host tool calls inside the agent
  container with an ALLOW, ASK or DENY vocabulary. This design uses `hold`
  where it says ASK. The two keep **separate vocabularies and separate
  evaluators**, because their inputs, their enforcement points and their
  trust positions differ, and a **common decision record** (the event above,
  with a distinct `surface` value such as `host_tool`). A shared record gives
  one query for "everything refused for agent X today" without forcing the
  host actions into the forge action vocabulary.
- **#17091 (open):** its Option 2 delivers signed policy bundles that spokes
  verify and apply, keeping the last known good bundle on failure. That is the
  `PolicySource` contract here: signature verification belongs to the source,
  and this evaluator only consumes a verified compiled set. Narrowing-only
  below the org is the same rule as operator rules here. Hash-chained audit is
  adopted by the decision stream when it exists. Nothing in this design needs
  #17091 to ship first.

## Migration

Five phases. Each is independently shippable, default off, and reversible.
Phases 2 to 4 run in **shadow mode** first: the seam computes a decision and
logs it, the legacy gate still decides, and nothing is enforced by the seam.

| Phase | Ships | Behaviour change | Exit criterion |
|---|---|---|---|
| 1. Types and schema | `authz.Request`, `authz.Decision`, event schema `v1`, rule id registry for existing gates, reason-code table. No callers | none | schema reviewed; every existing reason code has a rule id |
| 2. Decision events from existing gates, behind a flag | Each gate emits a `Decision` event when the flag is on (allow and deny), including relay refusals and broker results. Legacy code decides | none when off; one extra write per decision when on | measured volume and size on a maintainer hive; aggregation rule confirmed |
| 3. Seam in shadow | Gates are moved behind `authz.Decide` for evaluation; the legacy result is still enforced; a mismatch between legacy and seam is logged as `parity_mismatch` and counted | none | zero mismatches on the corpus and a soak period with no unexplained live mismatch |
| 4. Enforce through the seam, per gate group | Flip one group at a time (see order below); legacy code is kept for one release as the comparison oracle | none by construction, since parity held | parity mismatch counter flat through a release |
| 5. Operator narrowing rules, then credentials | Policy file and CEL layer in report-only (`shadow: true`) then enforce; then action-shaped tiers, complete-scope requirement, mint JWT per decision | operators can add deny and hold rules; tokens narrow | operator rules shown in shadow for a release before enforce |

Flip order for phase 4, lowest risk first: broker checks (small, one caller,
pure functions of the workspace), then proxy hard denies and write-surface,
then proxy REST mode table, then relays, then GraphQL (decision parity with
the REST path is verified first, see open questions).

### Phase status

| Phase | Status |
|---|---|
| 1. Types and schema | **Shipped for one gate.** `src/crux/authz` has `Request`, `Decision`, the `v1` event, and a rule id and reason code for the push broker's changed-files guard only. The registry for the other gates is not written |
| 2. Decision events behind a flag | **Started, one gate.** With `HIVE_AUTHZ_SHADOW=true` the push broker's changed-files guard emits one event per call to a bounded in-memory stream (`HIVE_AUTHZ_SHADOW_BUFFER`, default 1000, evictions counted). The proxy, the relays, the rest of the broker and the volume probe are not done, and there is no persistent stream or sink wiring yet, so the volume question stays open |
| 3. Seam in shadow | **Started, one gate.** `authz.Decide` re-derives the broker's changed-files decision after the guard has decided, and a difference is counted and logged as a divergence. Parity is covered by `TestParityPushBrokerChangedFiles` and `TestParityPushBrokerPathShapes` over the broker's existing cases. The proxy mode table, GraphQL, Linear, the relays and tool approval have no evaluator yet |
| 4. Enforce through the seam | Not started. Nothing reads a `Decision` to allow or refuse an action |
| 5. Operator rules and credentials | Not started |

What this slice does and does not do, as built:

- **Default off.** Unset, empty or unparseable `HIVE_AUTHZ_SHADOW` is off. Off
  records nothing and allocates nothing: the broker holds a nil recorder.
- **Enforces nothing.** The broker calls the shadow after the guard has
  returned its verdict and never reads a result back. A recorder panic is
  recovered and counted. A test pins that the broker's `Result`, error and git
  commands are identical with the shadow on and off.
- **Fail closed is written down, not applied.** `authz.Decide` already returns
  a deny with `authz_error` for an evaluator panic, `missing_attribute` for a
  missing repo or branch, and `unsupported_action` for an action with no rule,
  and tests pin that. In this phase a shadow deny that the guard did not issue
  is a divergence log line only.
- **Independent mirror.** The evaluator's path matching is written separately
  from `pushbroker.ProtectedPathViolations`, so drift between the two is
  caught by the parity tests and by the live divergence counter rather than
  hidden by a shared function.
- **Attributes only.** An event carries the agent name, repo, branch, a count
  of changed paths and a count of matched paths. Free-text attributes are
  scrubbed for credential shapes, stripped of control characters and cut to 256
  bytes. No path list, body, diff or credential is in the event type.
- **Policy version.** The event's `policy_version` is the floor version plus a
  short digest of the protected path list in force, so a per-call override of
  the list is visible in the record.
- **Not yet an audit trail.** The stream is in memory, bounded and lost on
  restart. It exists to measure parity. The dedicated durable stream and its
  integrity are phase 2 work and open question 6.

Proposed task-sized issues (not filed; for the RFC's acceptance criteria):

- `authz`: request, decision and event types plus the rule id registry (phase 1)
- `authz`: decision event emitter and flag in the proxy, with a volume probe (phase 2)
- `authz`: the same for the push broker and relays, including relay refusals (phase 2)
- `authz`: evaluator and built-in floor adapters, shadow comparison counter (phase 3)
- `authz`: enforce the broker group through the seam (phase 4)
- `authz`: enforce proxy groups through the seam, one group per issue (phase 4)
- `authz`: operator rule file, CEL environment helper, report-only mode (phase 5)
- `authz`: action-shaped token tiers and complete-scope requirement (phase 5)

### Parity tests, by name

Parity means the same `(allow or deny, reason code)` for the same request.
The corpus is built from the table-driven tests that already encode today's
decisions, not rewritten.

- `TestParityProxyModeTable`: every row of `rules` and `denyRules` crossed with
  the four modes and the capability on and off, through `AllowedByModeCaps`
  and through the seam. Source: `src/crux/proxy/rules_test.go`.
- `TestParityProxyGateOrder`: pause, scope, auto-merge, write-surface, mode and
  allowlist combinations, asserting the same first-denying reason code.
  Source: `src/crux/proxy/write_surface_enforce_test.go`,
  `src/crux/proxy/denial_message_test.go`.
- `TestParityGraphQL`: the mutation corpus in
  `src/crux/proxy/graphql_mutation_prefix_test.go` and
  `src/crux/proxy/graphql_repo_policy_test.go`, and any case where the REST
  and GraphQL paths differ is recorded as an explicit expected divergence,
  not smoothed over.
- `TestParityLinear`: `src/crux/proxy/linear_rules_test.go` and the parser edge
  tests.
- `TestParityPushBroker`: protected paths, empty commits, non-fast-forward,
  lease, forged sign-off and secret scan over the cases in
  `src/crux/pushbroker/pushbroker_test.go`.
- `TestParityToolApproval`: the existing `src/crux/toolapprove/parity_test.go`
  and `src/crux/toolapprove/bulk_parity_test.go` already pin the desk against
  the legacy gates; they stay the oracle for that surface.
- `TestParityRelays`: one case per relay authorizer, including the nil
  authorizer denying.

## Test strategy

- **Parity tests** above are the primary safety net and run in CI on every
  phase after 3.
- **Shadow soak.** The `parity_mismatch` counter is the live test. A phase 4
  flip is gated on it being flat.
- **Fail-closed tests per row** of the error table: injected evaluator error,
  panic, timeout, missing attribute, a deny rule that errors, an unloadable
  policy file (last good kept, alert raised, floor still running).
- **Narrowing-only property test.** For random floor outcomes and random
  operator rule sets, an operator rule never turns a floor `deny` into
  `allow`. This is the central invariant and gets a property test, not only
  examples. Per the project rule that security tests assert the invariant,
  the assertion is on the effect, not on a message string.
- **Event schema tests.** Golden JSON for each surface; a test that no event
  field can carry a token, body or diff (fuzz the request attributes with
  secret-shaped strings and assert the redaction filter).
- **Volume test.** A synthetic request rate through the emitter with the sink
  slowed down, asserting that the bounded buffer drops are counted and that
  enforcement latency does not grow.
- **Credential tests.** A missing repo attribute is refused; action
  tier permission sets are exactly the expected names; a mint JWT carries the
  decision id as `jti` and the clamped TTL.
- No test in this plan needs a live GitHub. Live verification of the push-only
  permission set is a separate manual checklist item before phase 5 ships.

## What this cannot cover

- **Actions that bypass the mediation points.** Anything that does not traverse
  the proxy, a relay or the broker is not decided here. The forced-egress
  redirect and per-agent UIDs are what make the proxy the path; if attribution
  fails the agent is treated as advisory, but it is still not authorized by a
  rule it never meets.
- **Host-level actions** of an agent (shell, file, fetch): #12815.
- **What the forge permits.** A token cannot be narrower than repository plus
  permission set, so a decision to allow one push is not enforced by the forge
  as one push.
- **Content-level judgment.** Rules see attributes, not meaning. A secret the
  scan does not recognise is still pushed.
- **The agent's own container boundary and the hive host.**
- **Non-repudiation.** A decision log is evidence, not proof; tamper evidence
  comes from #17091.
- **Contributor agents** running on contributor machines, where Hive cannot
  guarantee the evaluator is in the path.

## Open questions

1. **Decision parity.** Verify decision parity between the REST and GraphQL
   proxy paths (tracked separately). Phase 4 for the GraphQL group is gated on
   that result.
2. **Hold on the proxy path.** A request cannot wait for a human. The proposal
   is deny with `hold_required` plus an inbox item the agent's next kick can
   retry. Is hold in scope for the proxy at all, or only for the broker and
   relays, which are asynchronous already?
3. **Where hold and operator rules apply for relays.** The relay already has a
   file-in, result-out model, so `hold` fits naturally; confirm the result
   file shape for a held request.
4. **Operator policy location and integrity.** An operator-owned file outside
   every agent workspace, a hub-delivered bundle, or both? Who may write it,
   and how are changes reviewed and versioned? This is #17091 Pillar C and is
   not decided here.
5. **Allow volume.** What do proxy allows cost in events and bytes? The
   aggregation rule above is a guess until phase 2 measures it.
6. **Decision stream integrity.** Where does it live so that no agent UID can
   write or delete it, and is the PVC authoritative? Needs the deployment
   facts per distribution **(unverified)**.
7. **Quotas.** Should the per-agent GitHub budget emit a decision event with a
   `budget_exhausted` reason, or stay out of this stream entirely?
8. **Rule widening for operators.** Narrow-only is the RFC's stance and this
   design keeps it. Operators who want to widen (for instance allow one agent
   to touch one protected path) currently have no route. An explicit,
   scoped, expiring, audited exception is the guardrails RFC's model; do we
   want it here, and who approves?
9. **Shared CEL environment.** Is one helper for approvals, triggers and authz
   worth the coupling, or should each keep its own environment?
10. **Credential reality.** Given the forge limits, is the action-shaped tier
    set worth its complexity, or is the honest minimum "a complete-scope requirement
    plus a decision record"? Credential expiry reporting should be reconciled first.
11. **Per-agent mode file as an input.** The mode and capability files are
    plain files in `/tmp`. The seam would read them through the same path.
    Should the evaluator take mode from a Hive-supplied value instead, and is
    the file ever writable by an agent UID? **(unverified.)**

## Risks

- **Parity gaps hide behaviour.** The proxy order is implicit in code and
  may differ from what its comments say. Mitigation: shadow mode and the named corpus, with divergences
  recorded rather than normalised.
- **Hot path cost.** CEL evaluation on every proxy request. Mitigation: the
  floor is plain Go and CEL runs only when operator rules exist; the existing
  approval engine's cost cap and a time budget bound it.
- **Over-blocking from inverted error posture.** A broken deny rule blocks. That
  is intended, but an operator can lock an agent out by typo. Mitigation:
  compile-time type checks, a report-only phase, and `rule_error` in the event.
- **A second source of truth.** Rule ids and the legacy gates must not drift
  during phases 3 and 4. Mitigation: the registry is generated from the same
  table the gates use, and the comparison oracle stays for a release.
- **Event stream as a new sensitive store.** It records who did what to which
  repo and branch. Mitigation: attributes only, no payloads, redaction test,
  retention set with #17091.
- **Scope creep into one big refactor.** Mitigation: each phase is a separate
  issue and no phase removes legacy code until the next has soaked.

## Exit criteria for this RFC

Mapped to the acceptance criteria in #18988:

- A decision on the seam and the record schema: sections "The request", "The
  decision" and "Decision events". Review asks for objections to the flat
  request shape and the dedicated stream.
- A decision on the language and the error semantics per effect: "Policy
  language" and the error table. Review asks for objections to CEL for the
  narrowing layer and to the inverted deny-rule error posture.
- A gate inventory: this page, which is the inventory attached to the issue.
- Phase 2 and 3 split into task-sized issues with named parity tests: the list
  and the parity test names above.
