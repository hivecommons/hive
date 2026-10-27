# Crux: a generic tool policy for the MITM proxy (#14228)

Status: **Proposed (design only).** Spec for #14228, a child of the Crux
stage 2/3 tracker #14138. No code changes with this record. Implementation
is cut into the child issues listed under
[Exit criteria](#exit-criteria-and-implementation-children). The parent
record is [crux.md](crux.md). Its
[Credentials and hive-provided capabilities](crux.md#credentials-and-hive-provided-capabilities)
section sets the engine/hive split this record relies on.

Scope: the request-level policy that `crux/proxy` applies to agent traffic
it intercepts. That means forge APIs (GitHub REST and GraphQL today, and
GitLab, Gitea and Forgejo next: #14211, #14116), Linear's GraphQL API, git
smart-HTTP, and the classification of inference endpoints. Out of scope: the
tool-approval desk (`pkg/toolapprove`, [tool-approval-desk.md](tool-approval-desk.md)),
which decides approval-shaped requests before an agent acts, and
`pkg/pushbroker`, which pushes for the agent with a hive-minted token. Both
sit next to this policy. Neither one is part of it.

Line references are to `src/crux/proxy/` on v5 at `6350270f1` and are given
as orientation only. The identifiers are the stable reference.

## The problem: three classifiers and an implicit order

The proxy's policy is GitHub-shaped. It is split across three classifiers
with three different postures:

| Mechanism | File | Keyed on | Posture on unknown |
|---|---|---|---|
| `ProxyRule` table (`rules`) + `denyRules` | `rules.go` | `(method, path regexp)` on any inspected host, `MinMode` + optional `Capability` OR-grant | **deny** (`AllowedByModeCaps` falls through to `false`) |
| `GraphQLAllowedCaps` | `rules.go` | substring match of mutation names (`graphQLMergeMutationRe`, `graphQLPRWriteMutationRe`) | **permit at `ModeIssuesOnly`** (the `default:` branch, line ~691) |
| `LinearGraphQLAllowed` / `linearMutations` | `linear_rules.go` | top-level GraphQL operation name on the single `POST /graphql` | **deny** |

Several gates compose with these classifiers but are not part of them:
write-surface enforcement (`WriteSurfaceEnforceRefusal`,
`WriteSurfaceGraphQLRefusal` in `write_surface_enforce.go`), repo pause /
agent repo scope / repo auto-merge-off (`RepoPauseRefusal`,
`AgentRepoScopeRefusal`, `RepoAutoMergeRefusal`, `GraphQLAutoMergeRefusal`),
per-repo autonomy levels (`autonomyModeForRepo`, `autonomyGraphQLMode` in
`autonomy_levels.go`), the allowed-repo filter (`RepoFilterAllowed`), the
issue-shape gate (`enforceIssueShapeFor`), canary egress
(`inspectCanaryEgress`) and the per-agent write budget
(`githubBudgetRefusal` in `agent_github_budget.go`).

The only place the order of these gates is written down is the body of
`proxyHTTPHost` in `github_proxy.go`. At `6350270f1` the order is:

| Step | Where | Deny code (`denial_message.go`) |
|---|---|---|
| internal caller (`internalCallerName`) skips the agent gates | throughout | n/a |
| repo paused | `p.repoPauseRefusal`, line ~1385 | `repo_paused` |
| agent repo scope | `p.agentRepoScopeRefusal`, line ~1389 | `agent_repo_scope` |
| repo auto-merge off (REST) | `p.autoMergeRefusal`, line ~1393 | `repo_auto_merge_disabled` |
| write-surface (REST, git) | `WriteSurfaceEnforceRefusal`, line ~1399 | `write_surface_enforce` |
| Linear classifier | `p.enforceLinear`, line ~1426 | `linear_policy` |
| GraphQL: auto-merge-off, write-surface, classifier | lines ~1439–1464 | `repo_auto_merge_disabled`, `write_surface_enforce`, `graphql_mutation` / `graphql_denied` |
| REST mode (+capability, per-repo autonomy). Hard deny is checked *inside* it and only re-surfaced as the reason | `AllowedByModeCaps` / `DeniedMessage`, lines ~1472–1489 | `mode_denied`, `hard_deny` |
| allowed-repo filter | `RepoFilterAllowed`, line ~1490 | `repo_not_in_config` |
| issue shape | `p.enforceIssueShapeFor`, line ~1503 | `issue_shape` |
| canary egress | `p.inspectCanaryEgress`, line ~1517 | `canary_egress` |
| write counters, then the one-line 403 (`ProxyDenialBody`, `X-Hive-Proxy-Reason`) | lines ~1530–1590 | |
| write budget, as a 429 | `p.githubBudgetRefusal`, line ~1598 | (429, not 403) |
| credential injection, then forward | `rewriteGitHubAuth` / `injectLinearCredential`, line ~1622 | |

Three facts follow, and the decisions below rest on them:

1. **A forge is keyed on its wire shape, so a second forge means
   duplicating policy.** #14211 adds GitLab and Gitea rows to the shared,
   host-blind `rules` slice and copies the hard-deny prose per forge. Its
   contract test (`ForgeRulesContract`) already invents a shared operation
   name (`CreateChangeRequestHardDeny`, `CommentIssue`, …) to keep the
   copies in step. That operation vocabulary is real. It is just not in the
   code.
2. **The same operation gets different verdicts depending on its wire
   shape.** REST `PUT …/pulls/{n}/merge` and `POST …/pulls` are hard-denied
   for every mode (`denyRules`), and the comment above `denyRules` says the
   GraphQL merge path matches. It does not. `GraphQLAllowedCaps` returns
   `mode >= ModeIssuesPRsMerge` for `mergePullRequest` (line ~677), and
   `createPullRequest` is gated at `ModeIssuesAndPRs`. Neither is a relay.
3. **Hard deny is not a first stage.** It runs inside the REST mode check,
   after pause, scope, auto-merge and write-surface. A paused repo returns
   `repo_paused` for a PR-create, not `hard_deny`. #12127 made reason codes
   machine-readable, so this order is now observable behaviour.

## Decisions

Hive filed this issue, so the engineering questions are decided here. Each
answer below names the alternative it rejects. The rule used throughout is
to pick the simplest option that is additive and backward-compatible.

### 1. Policy model: shared operations, per-forge adapters

**Decision:** the unit of policy is an **operation** from one shared
vocabulary (`pr.create`, `pr.merge`, `issue.comment`, `label.add`, …). Each
forge has an **adapter** that maps a request's wire shape (method + path,
GraphQL field names, git service) to zero or more operations plus the
target repo. One shared **evaluator** applies one policy table to those
operations. *Rationale:* one ACMM ladder holds across every forge, so a
relay or tier change is made once and every forge picks it up. #14211's
`ForgeRulesContract` already shows the vocabulary is needed. *Rejected:*
a `(method, path)` table per forge behind a shared evaluator. Each table is
simple to audit for its own host, but the ladder gets copied N times, and
`linear_rules.go`'s header explains why `(method, path)` cannot express a
single-endpoint GraphQL API at all.

The vocabulary starts as exactly the set of distinct things today's GitHub
and Linear tables gate. Defaults equal today's verdicts:

| Operation | GitHub REST / git (today's rule) | GitHub GraphQL (today) | Default policy |
|---|---|---|---|
| `read` | `GET`/`HEAD`/`OPTIONS` `.*` | non-mutation document | `ModeAdvisory` |
| `auth.login` | `POST /login/device/code`, `/login/oauth/access_token` | — | `ModeAdvisory`, write-surface exempt |
| `git.fetch` | `POST …\.git/git-upload-pack` | — | `ModeAdvisory` |
| `git.push` | `POST …\.git/git-receive-pack`; `GET …/info/refs?service=git-receive-pack` (`gitServiceIsReceivePack`, write-surface only) | — | `ModeIssuesAndPRs` |
| `ref.write` | `POST …/git/refs`, `POST …/git/commits`, `DELETE …/git/refs/…` | `createCommitOnBranch`, `createRef`, `updateRef`, `deleteRef` | `ModeIssuesAndPRs` |
| `branch.protect` | — | `createBranchProtectionRule`, `updateBranchProtectionRule` | `ModeIssuesAndPRs` |
| `issue.create` | `POST …/issues` | (default bucket) | `ModeIssuesOnly` |
| `issue.edit` | `PATCH …/issues/{n}` | (default bucket) | `ModeIssuesOnly` |
| `issue.comment` | `POST …/issues/{n}/comments` | conversational fields (`converseMutations`) | `ModeIssuesOnly` or `converse` |
| `label.add` | `POST …/issues/{n}/labels` | (default bucket) | `ModeIssuesOnly` |
| `pr.create` | `POST …/pulls` | `createPullRequest` | **`relay(hive-open-pr)`** |
| `pr.edit` | `PATCH …/pulls/{n}` | `markPullRequestReadyForReview`, `convertPullRequestToDraft` | `ModeIssuesAndPRs` |
| `pr.review` | `POST …/pulls/{n}/reviews` | `addPullRequestReview`, `submitPullRequestReview` | `ModeIssuesAndPRs` or `converse` |
| `pr.update_branch` | `PUT …/pulls/{n}/update-branch` | — | `ModeIssuesPRsMerge` |
| `pr.merge` | `PUT …/pulls/{n}/merge` | `mergePullRequest`, `mergeBranch`, `enablePullRequestAutoMerge` | **`relay(hive-merge)`** |
| `agent.session` | — (Linear: `agentActivityCreate`, `agentSessionUpdate`) | — | `ModeAdvisory` or `converse` |
| `attachment.link` | — (Linear: `attachment*`) | — | `ModeIssuesAndPRs` |
| `inference.call` | inference hosts (see question 6) | — | allow (mode-independent) |
| `unknown` | anything an adapter cannot classify | unrecognised mutation | **deny** (see question 2) |

A request whose GraphQL document selects several top-level fields maps to
several operations. The request is allowed only if **every** operation is
allowed, and the verdict that gets reported is the strictest one (relay
beats deny-by-mode, which beats allow). This is the same "the whole
document, not one substring" rule that `isConversationOnlyMutation`
already applies for `converse`.

The adapters stay Go code. GitHub's GraphQL and Linear's operation parsing
(`topLevelMutationFields`, `linearOperationFields`) are parsers with body
limits (`graphQLBodyLimit`, `linearBodyLimit`, `linearMaxOperations`), not
regex rows.

### 2. Default posture for an unrecognised operation: deny

**Decision:** one rule for every adapter. A request the adapter cannot map
to a known operation becomes `unknown`, and `unknown` is **denied** with
reason code `mode_denied` (REST) or `graphql_mutation` / `linear_policy`
(GraphQL hosts), so today's codes do not change. GitHub GraphQL's
permissive `default:` is **a bug to close, not a documented exception**.
It is closed with a ratchet rather than a flag day. During migration, the
GitHub GraphQL adapter maps each unrecognised mutation field to the named
legacy operation `legacy.graphql_mutation`, whose default policy is
`ModeIssuesOnly`. That is exactly today's verdict, so the parity test
holds. A follow-up child then (a) maps the mutation names that agents
actually send, read from the `graphql_mutation` violation and write-counter
logs, onto real operations, and (b) changes `legacy.graphql_mutation` to
deny. *Rationale:* fail closed is already the posture for REST and Linear,
and "REST is the primary gate" does not hold for GraphQL-only operations.
*Rejected:* keeping the permissive default as a documented GitHub
exception. It is the one place where an unknown write gets through, and
every new GraphQL forge would inherit the question.

The same ratchet closes the divergence noted in fact 2. Under the parity
phase, GraphQL `pr.merge` and `pr.create` keep today's mode-gated verdicts
through two named entries in the evaluator's `legacyExceptions` list. The
child that empties that list makes them `relay`, which matches what the
`denyRules` comment already claims. The list can only shrink, and a test
pins its contents.

### 3. Hard deny becomes a `relay(<command>)` verdict

**Decision:** the evaluator returns one of three verdicts: `allow`,
`deny(code, reason)`, or `relay(command)`. `relay` is a deny that names the
sanctioned path. The 403 body is rendered from one shared template per
relay command (what was refused, the relay invocation, what not to use).
The reason code is still `hard_deny`. A forge adapter can add a short list
of forge-specific "do not use" hints (for GitHub: `gh pr create`, the
GitHub MCP `create_pull_request`). It does not write its own prose.
*Rationale:* this removes the per-forge message copies seen in #14211
("use the request-file path…") and keeps the two GitHub messages in
`denyRules` byte-identical. The compiled-in relay templates reproduce them,
and the parity test asserts that. *Rejected:* a plain `deny` with a
per-rule message string (today's `denyRule.Msg`). That copies the relay
contract into every adapter, and the copies drift.

### 4. Where the tables are declared: engine vocabulary and adapters in Go, hive-shaped policy as config with compiled-in defaults

**Decision:** the policy has three parts, and each lives in a different
place:

| Part | Shape | Lives in | Why |
|---|---|---|---|
| Operation vocabulary (`Operation` constants) | engine | `crux/proxy`, Go | a stable contract between adapters and policy |
| Adapters (wire shape → operations) | engine | `crux/proxy`, Go, one file per forge | parsers with body limits, unit-tested, reviewed as code |
| Policy table (operation → `MinMode`, `Capability`, `relay` command and its usage line) | **hive-shaped** | supplied by the host. Compiled-in default = today's tables | relay names (`hive-open-pr`, `hive-merge`) and ladder levels are hive policy |

The host installs a table in-process with `(*GitHubProxy).SetToolPolicy(PolicyTable)`.
When stage 3 lands, the same table arrives as config through the `crux serve`
API, as the stage-2 capability table in crux.md already says for
`SetRepoPausedFunc` and the other setters. If nothing is installed, the
compiled-in `DefaultPolicyTable` applies. It reproduces `rules`, `denyRules`,
`GraphQLAllowedCaps` and `linearMutations` exactly, so hives that configure
nothing see no change. Any table that is installed is validated against
the vocabulary: an unknown operation name is a load error, and a missing
operation falls back to the default row, never to allow. *Rationale:* this
follows the stage-2 rule (hive policy answers on the hive side, engine
mechanism in the engine) without forcing every host to ship a table.
*Rejected:* (a) a data file of regex routes shipped with the runtime. It
moves classification out of reviewed, tested code and still cannot express
GraphQL. (b) Keeping everything compiled in. That leaves `hive-open-pr` and
`hive-merge` hard-coded inside an engine that other consumers are meant to
use.

### 5. Composition order: one documented pipeline

**Decision:** every adapter runs through the single pipeline below. Its
**gate order matches the order `proxyHTTPHost` applies today**, so reason
codes do not change. Classification moves to the front, and hard deny
becomes the `relay` verdict of the policy stage, which is where it already
takes effect.

```text
                  agent request (CONNECT / transparent TLS / plain)
                                   │
      ┌────────────────────────────▼────────────────────────────┐
  0   │ host → adapter        RegisterForgeHost table (Q7)      │── no adapter ─▶ opaque tunnel (as today)
      └────────────────────────────┬────────────────────────────┘
      ┌────────────────────────────▼────────────────────────────┐
  1   │ classify              adapter: wire → []Operation, repo │── unparseable / over limit ─▶ unknown
      └────────────────────────────┬────────────────────────────┘
                                   │ internal caller (hive control plane)? ── yes ─▶ skip 2–5, go to 6
      ┌────────────────────────────▼────────────────────────────┐
  2   │ run-state gates       repo paused → agent repo scope →  │── deny repo_paused / agent_repo_scope /
      │                       repo auto-merge off               │        repo_auto_merge_disabled
      └────────────────────────────┬────────────────────────────┘
      ┌────────────────────────────▼────────────────────────────┐
  3   │ write-surface         lane under write_surface.enforce, │── deny write_surface_enforce (+ audit)
      │                       op is a write (not auth.login)    │
      └────────────────────────────┬────────────────────────────┘
      ┌────────────────────────────▼────────────────────────────┐
  4   │ policy                per op: relay? → mode(+per-repo   │── relay(cmd) → hard_deny
      │                       autonomy) OR capability; unknown  │── deny mode_denied / graphql_* / linear_policy
      │                       → deny; strictest op wins         │
      └────────────────────────────┬────────────────────────────┘
      ┌────────────────────────────▼────────────────────────────┐
  5   │ allowed-repo filter   RepoFilterAllowed                 │── deny repo_not_in_config
      └────────────────────────────┬────────────────────────────┘
      ┌────────────────────────────▼────────────────────────────┐
  6   │ body gates            issue shape (agents only) →       │── deny issue_shape / canary_egress
      │                       canary egress scan                │
      └────────────────────────────┬────────────────────────────┘
                                   │ write counters; first deny above → one-line 403 + X-Hive-Proxy-Reason
      ┌────────────────────────────▼────────────────────────────┐
  7   │ write budget          per-agent window (agents only)    │── 429
      └────────────────────────────┬────────────────────────────┘
      ┌────────────────────────────▼────────────────────────────┐
  8   │ credential injection  adapter-owned (GitHub, Linear)    │
      └────────────────────────────┬────────────────────────────┘
                                   ▼
                                forward
```

Each stage gets the classified operations and the target repo, not the raw
`(method, path)`. So the run-state gates and the write-surface gate are
written once and work for every forge, including GraphQL hosts. Today they
are duplicated across the REST and GraphQL branches. *Rationale:* moving
gates is observable through reason codes and the write-surface audit
entry. Documenting the current order and routing it through one function
gives a single pipeline without any behaviour change. *Rejected:* the order
listed in the issue (hard deny → write-surface → repo scope/paused → mode).
It would make a PR-create on a paused repo report `hard_deny` instead of
`repo_paused`, and would replace the write-surface audit entry for an
enforced lane's PR-create with a relay denial. Both are behaviour changes
with no safety gain, because every path still ends in deny.

### 6. git smart-HTTP and inference are in scope as small adapters

**Decision:** "tool policy" covers everything the proxy inspects, not only
forge JSON APIs. git smart-HTTP gets one forge-neutral sub-adapter
(`gitsmart`) that every forge adapter delegates to for `…\.git/…` and
`info/refs` paths. It produces `git.fetch` or `git.push`, using
`gitServiceIsReceivePack` and the `gitUploadPackPath` / `isGitReceivePack`
matchers. Inference hosts (`IsCopilotAPIHost`, `SetInferenceRoute` routes)
get an `inference` adapter that produces `inference.call`. Its default
policy is allow at every mode, so the only new behaviour is that inference
requests use the same verdict type and denial format. Inference routing,
translation, entitlement and `inference_budget.go` stay where they are and
are not part of the pipeline stages. *Rationale:* git push is already a
write in the write-surface gate and the mode table. Having a separate model
for it is how GraphQL ended up with its own posture. *Rejected:* limiting
the policy to forge APIs. That leaves `git-receive-pack` in a third
classifier, and every new forge would have to re-derive push handling.

### 7. Host identification: host → adapter mapping from config

**Decision:** `githubHosts` is generalised to a mutex-guarded
`forgeHosts map[string]AdapterID`, populated by
`RegisterForgeHost(host string, adapter AdapterID)`. `RegisterGitHubHost(h)`
remains as a wrapper for `RegisterForgeHost(h, AdapterGitHub)`, and
`IsGitHubHost` / `IsLinearHost` / `NeedsInspection` become lookups in the
same map. Defaults: `api.github.com`, `github.com` → `github`, and
`api.linear.app` → `linear`. Each adapter also declares which of its hosts
need MITM (today `NeedsMITM`, `hostNeedsMITM`, `hostNeedsMITMFor`), so
adding a host does not quietly leave it in an opaque tunnel (the trap that
`NeedsInspection`'s comment describes). The host gets these mappings from
the same config that supplies forge URLs. Under stage 3 they arrive with
the policy table. *Rationale:* this is deterministic, auditable and
additive, and it is how GHE hosts are added today. *Rejected:* (a) probing
the host (`/api/v4/version`, `/api/v1/version`). It adds latency and a
time-of-check/time-of-use gap, and an attacker-reachable endpoint would
choose the policy. (b) Using the agent's declared `forge`. That is
agent-influenced input selecting the agent's own policy.

### 8. Testing contract every adapter ships

**Decision:** an adapter is merged only with all of the following, each a
table test parameterised over the registered adapters:

1. **Completeness.** Every operation in the vocabulary has at least one
   wire example in the adapter's fixture table, or appears in the adapter's
   explicit `Unsupported` list with a reason. A new operation with no row
   for an existing adapter fails the build. This generalises #14211's
   `ForgeRulesContract`.
2. **Classification round-trip.** Each fixture classifies to the operation
   it claims. Near-miss fixtures (for example GitLab
   `POST /projects/:id/labels`, which *creates a project label* and is not
   `label.add`) classify to the right operation or to `unknown`.
3. **Relay coverage.** For every adapter and every mode, each operation the
   default table marks `relay` (`pr.create`, `pr.merge`) is refused with
   `hard_deny` and the shared relay message.
4. **Fail closed.** An unparseable body, an over-limit body, or an
   unmatched route is denied at every mode.
5. **Denial message.** The `denial_message_test.go` assertions (one line,
   ends `reason=<code>`, code in `X-Hive-Proxy-Reason`, what/why/instead
   preserved) run for one refused request per adapter.
6. **Parity (GitHub and Linear, migration only).** For a fixed corpus of
   `(mode, caps, method, path, body)` built from every row of `rules`,
   `denyRules`, `GraphQLAllowedCaps`'s buckets and `linearMutations`, plus
   the existing `rules_test.go` / `linear_rules_test.go` cases, the
   evaluator returns the same allow/deny decision **and the same reason
   code** as the legacy functions. The test is deleted together with the
   legacy path.
7. **Ratchet.** `legacyExceptions` and the
   `legacy.graphql_mutation` mapping are pinned exactly, and the only
   allowed change is to shrink them.

*Rationale:* (1)–(5) are the properties #14211 and the Linear gate each
tested ad hoc. (6) is what makes the migration a pure refactor. *Rejected:*
tests for each adapter only. Those cannot catch an operation missing from
one forge, which is the failure #14211's contract was written to prevent.

### 9. Migration: evaluator first behind parity, then new forges, then delete legacy

**Decision:**

1. **Evaluator + GitHub adapter, no behaviour change.** Add the
   vocabulary, `Verdict`, `PolicyTable` with `DefaultPolicyTable`, the
   evaluator, and the `github` and `gitsmart` adapters. `proxyHTTPHost`
   calls the evaluator. `AllowedByMode`, `AllowedByModeCaps`,
   `DeniedMessage`, `GraphQLAllowed` and `GraphQLAllowedCaps` stay as thin
   wrappers over it, because existing tests and `pkg/apiproxy` call them.
   The parity test gates this step.
2. **Linear onto the evaluator.** Add a `linear` adapter. Its posture is
   already deny, so parity is direct.
3. **New forges only on the generic model.** Add `RegisterForgeHost`
   (question 7) and the GitLab and Gitea/Forgejo adapters. No non-GitHub
   row is ever added to the host-blind `rules` slice.
4. **Close the GraphQL exceptions** (question 2): map observed mutations,
   empty `legacyExceptions`, and make `legacy.graphql_mutation` deny.
5. **Hive-side policy table** through `SetToolPolicy` in-process, then
   through the stage-3 API.
6. **Delete the legacy path:** `rules`, `denyRules`, the classifier bodies
   in `GraphQLAllowedCaps` and `LinearGraphQLAllowed`, and the parity test.

*Rationale:* GitHub moves first, but only as a verified refactor. Otherwise
the generic evaluator would govern only forges that have no production
traffic, and the GitHub table would stay the de facto spec. *Rejected:*
(a) using the generic evaluator only for new forges and moving GitHub "in
a later ratchet". That leaves two evaluators with different postures, which
is the problem this record is meant to fix. (b) A single PR that both moves
GitHub and changes verdicts. Parity cannot be reviewed if the expected
results change in the same diff.

**#14211 is the first non-GitHub adapter, not a stop-gap.** Its GitLab and
Gitea routes, its hard-deny set (MR/PR create and merge for every mode) and
its `ForgeRulesContract` are the content of the `gitlab` and `gitea`
adapters (step 3) and the seed of the completeness test (question 8.1). It
should be rebased onto the evaluator rather than merged into the shared
`rules` slice, for three reasons. The slice is host-blind, so GitLab and
Gitea rows would also match requests to GitHub hosts. Widening the GitHub
rows with `(?:/api/v1)?` changes GitHub coverage with no GitHub reason. And
its hard-deny prose ("use the request-file path") becomes the shared
`relay(hive-open-pr)` / `relay(hive-merge)` messages. Its fixtures carry
over unchanged.

## Exit criteria and implementation children

This record answers questions 1–9 of #14228 and draws the pipeline once
(above). It also records the decision on #14211 (first adapter, rebased).
#14228 stays open until the following children are filed **as children of
#14228**, not of #14138:

1. `crux/proxy`: operation vocabulary, `Verdict` (`allow` / `deny` /
   `relay`), `PolicyTable` + `DefaultPolicyTable`, evaluator, and the
   GitHub + gitsmart adapters behind a parity test (no behaviour change)
2. `crux/proxy`: Linear adapter on the evaluator (parity with
   `linearMutations`)
3. `crux/proxy`: `RegisterForgeHost(host, adapter)` and adapter-declared
   MITM hosts. `RegisterGitHubHost` becomes a wrapper
4. `crux/proxy`: GitLab and Gitea/Forgejo adapters (rebase of #14211 onto
   the evaluator) with the adapter contract tests
5. `crux/proxy`: close GitHub GraphQL's permissive default and the
   `legacyExceptions` (GraphQL `pr.merge` / `pr.create` → relay) by ratchet
6. `crux/proxy`: inference adapter (`inference.call`) on the shared verdict
   and denial format
7. `crux/proxy` + `cmd/hive`: `SetToolPolicy` and the hive-side policy
   table (relay commands, ladder levels). Stage-3 API transport follows
   with #14138's sidecar work
8. `crux/proxy`: delete the legacy `rules` / `denyRules` / classifier
   bodies and the parity test

## References

- Spec issue: #14228. Tracker: #14138. Parent record: [crux.md](crux.md)
- Forge pressure: #14211 (GitLab/Gitea rule table), #14116 (Forgejo smoke)
- Code: `src/crux/proxy/rules.go`, `linear_rules.go`,
  `write_surface_enforce.go`, `agent_github_budget.go`,
  `autonomy_levels.go`, `github_proxy.go` (`proxyHTTPHost`),
  `denial_message.go`, `denial_message_test.go`
- Related decisions: #4492 (capabilities as OR-grants, Linear gate), #9772
  (write-surface enforcement), #12127 (one-line denials with reason codes),
  [tool-approval-desk.md](tool-approval-desk.md)
