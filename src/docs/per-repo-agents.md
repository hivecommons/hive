# Per-repo agents

Scope an agent to the repositories it serves.

A hive runs one fleet of agents against several repositories at once. Until now
which agents existed was a property of the **hive**: define a specialist and
every repo got it. In a hive holding a Go service, a Rust CLI and a Terraform
module, a schema-migration reviewer added for one of them woke on cadence and
went hunting for schema migrations in repos that have no database — inference
spend and issue noise on repos that never wanted it.

Setting `repos:` on an agent makes the roster a **per-repo** answer:

```yaml
agents:
  schema-reviewer:
    backend: claude
    model: claude-opus-4-6
    mode: ISSUES_ONLY
    repos: [console]      # ← this agent is for `console` and nothing else
```

Leave `repos:` off and the agent serves the whole hive — that is the default,
and what every agent did before this setting existed. **A hive with no scoped
agents behaves exactly as it did before.**

## When to use it

- **Polyglot hive.** A Go repo and a Rust repo want different specialist
  reviewers. Without scope, both get both — or neither.
- **Domain agent on one repo.** A schema-migration reviewer, a protocol
  compatibility checker, an image-build auditor: valuable on the one repo with
  that concern, pure noise on the other six.
- **Cost control by repo.** `backend` and `model` are already per-agent; making
  the agent per-repo makes that spend targetable. A heavyweight model can be
  justified on the flagship repo and absent from a low-traffic one.
- **A repo whose code cannot leave a boundary** may need a specific backend
  while the others do not.
- **Onboarding a repo with its own reviewers** without changing what every other
  repo receives.

## Setting the scope

### In config

```yaml
project:
  org: acme
  repos: [console, dashboard, laredo/cuga-agent]

agents:
  schema-reviewer:
    repos: [console]
  protocol-checker:
    repos: [console, laredo/cuga-agent]
  scanner: {}                      # no repos: → serves every repo
```

Entries are spelled as `project.repos` spells them — a bare name (`console`) or
an explicit cross-org reference (`laredo/cuga-agent`) — and matched
org-qualified and case-insensitively, so `Console`, `console` and `acme/console`
are the same repository.

### From the dashboard

The agent config dialog's **General** tab has a **Repos** field (owner only).
Comma-separated; empty means the whole hive. It also shows the hive's repo list
so you are not spelling names from memory. Saving stamps the field
operator-owned, the same contract model, backend and pause state carry.

### From the API

```bash
curl -X PUT http://localhost:8080/api/config/agent/schema-reviewer/general \
  -H 'Content-Type: application/json' \
  -d '{"repos": ["console"]}'

# [] clears the scope and returns the agent to hive-wide
curl -X PUT http://localhost:8080/api/config/agent/schema-reviewer/general \
  -H 'Content-Type: application/json' -d '{"repos": []}'
```

`GET /api/agents` reports `repos` (what the agent declares) and `watchedRepos`
(that scope intersected with `project.repos`) for scoped agents. They are
separate on purpose: an entry that is declared but **not** watched is exactly
the misconfiguration worth seeing, and collapsing them would hide it.

### In a BYO agent spec

`skillreg.AgentSpec` — the bring-your-own-agent contract — gained an optional
`repos:` key:

```yaml
name: schema-reviewer
backend: claude-code
model: opus
mode: suggest
repos: [console]
skills: [sql-migrations]
```

The `AgentSpec` **interface is unchanged**. Scope is a separate optional
interface (`RepoScoped`) read through `skillreg.SpecRepos`, because adding a
sixth method would break every existing third-party implementation at compile
time — a spec that compiled last release would stop compiling. A spec with no
scope is hive-wide, exactly as every spec written before the field was. On
config load and reload, a spec `repos:` list is copied into the agent config
and marked `repos_owner: spec`, so the proxy, relays and kick filtering enforce
it through the same `Config.AgentServesRepo` predicate as dashboard-defined
scopes. If an operator explicitly sets `repos_owner: operator`, that operator
choice wins over the spec until the owner marker is cleared.

A `repos:` key that is present but names nothing (`repos: ["", "  "]`) is
rejected at parse time: it would read as "scoped" while silently widening the
agent to the whole hive, which is the opposite of what writing the key meant.

## What a scope actually does

Enforcement is **deterministic**, not a prompt instruction. Hive's design rule —
if a human would give the same answer every time, it belongs in infrastructure,
not a prompt — applies here: an out-of-scope agent is *refused*, not asked not
to go. A prompt-level scope has already been observed to fail when an agent's
model changes.

| Layer | Behaviour for an out-of-scope repo |
| --- | --- |
| MITM GitHub proxy | Every **write** (`POST`/`PUT`/`PATCH`/`DELETE`, `git push`) is answered `403` with a message naming the agent and the repo — whatever ACMM mode the agent holds, including `ISSUES_PRS_MERGE`. |
| [`hive-open-pr`](hive-open-pr.md) relay | The request is rejected before any GitHub call and quarantined, with the reason in its result file. |
| [`hive-merge`](hive-merge.md) relay | Same, checked before the optional branch update so the repo receives no write at all. |
| [`hive-open-issue`](hive-open-issue.md) relay | Same. Issue noise on repos that never wanted the agent is the specific cost this feature is about. |
| Kick assembly | The agent's `${ISSUE_LIST}`, PR list, hold list, clusters, queue counts, `${MERGE_ELIGIBLE}` and `${CI_FAILING}` are filtered to its repos, and `AUTHORIZED REPOS` lists only those. |
| Agent environment | `$HIVE_REPOS` lists only the agent's repos, and `$HIVE_REPO` names its own primary. |

The proxy and relay gates are both required, and neither is redundant. The proxy
hard-denies direct `POST /pulls` and `PUT /pulls/{n}/merge` for *every* agent
mode precisely so those operations route through the hive — and a request the
hive fulfils never traverses the proxy. Scope enforced only at the proxy would
have left a specialist able to open PRs and merge them on repos it was never
defined for.

### Cost: task filtering, not cadence

Cadence stays hive-wide per agent, as [#6111](https://github.com/hivecommons/hive/issues/6111)
describes — a scoped agent still wakes on its schedule. What changes is what it
wakes *to*: filtered work instead of a fleet-wide backlog it has to read through
and discard. That task filtering is the cheap fix, and it is the one that
matters — an agent shown a schema-migration issue in a repo with no database
will look at it.

### What a scope does **not** do

- **Block reads.** Scope says which repos an agent is *for*, not which repos it
  may look at. A reviewer scoped to the Go service may legitimately read the
  Rust CLI to understand a shared protocol, and `git clone`/`git fetch` keep
  working. This matches the pre-existing repo filter, which has always gated the
  "repo not in hive config" case on writes alone.
- **Touch the hive's control plane.** App-token minting and heartbeats are
  exempt: scope composes the *agent* roster, and refusing the hive itself would
  take the spoke down rather than narrow an agent.
- **Re-point the hive.** `project.primary_repo` is unchanged. Only the *agent's*
  `$HIVE_REPO` follows its scope, and only when the hive primary is outside it —
  handing a specialist a `$HIVE_REPO` it may not write to would aim every
  shipped template example (`gh ... --repo "$HIVE_REPO"`) at a refused write.
- **Cover GraphQL writes whose target repo is only in the request body.** Repo
  attribution reads the request *path*, so `POST /graphql` is not matched — the
  same blind spot the pre-existing repo-scope filter has. Those mutations stay
  gated by ACMM mode.
- **Change the ACMM level.** Level is how much autonomy an agent has; scope is
  whether the agent is there at all. Lowering a repo's autonomy does not remove
  an agent that is irrelevant to it.

## Surviving the pack sweep

ACMM packs reconcile the roster on **every** restart, and the ownership marker
family (#5632/#5706) exists because a pack that does not know a field was chosen
by a human silently reverts it on the next pod roll. `repos` carries the same
marker: an operator edit stamps `repos_owner: operator`, while a BYO spec
stamps `repos_owner: spec` when its scope is loaded. Operator-owned scopes are
left alone on subsequent spec reloads; otherwise the spec is authoritative.

No pack ships a repo scope today, so nothing reconciles it away today. The
marker is there so a pack that someday does cannot widen a specialist back to
the whole hive without anyone noticing.

An agent you create by hand in the dashboard is also stamped
`pause_owner: operator` at birth, which is what stops the pack visibility sweep
pausing it as "agent not in pack level N" on every restart. That is pre-existing
behaviour and it is what makes a custom per-repo agent survive.

## Misconfiguration is reported, not fatal

Two states are inert, and both are silent without a warning:

- an entry naming a repo the hive does not watch (a typo, or a repo removed from
  `project.repos` after the agent was scoped to it), and
- a scope whose entries **all** miss, leaving the agent with no repos at all.

Both are logged at boot (`per-repo agent scope`) and neither refuses to start:
refusing to boot over a stale scope entry would be a worse failure than an
ignored one, and keeping the entry means re-adding the repo restores the scope
rather than silently widening the agent. The dashboard also refuses to *save* an
entry that cannot name a repository at all (a URL, an `owner/name/extra` path),
so you see it on the request that caused it.

Scoped agents are named individually in the boot log, so an operator debugging
"why did nothing happen on that repo" can read the roster composition out of the
log they already have.

## Alternatives, and why they are not this

- **Per-repo `AGENTS.md` telling an agent to ignore repos.** Works today and is
  the current best effort, but it is prompt-level: the agent still exists, still
  wakes on cadence, and still costs inference.
- **Per-repo skills.** Already supported and genuinely useful, but it tunes
  behaviour rather than roster membership.
- **One hive per repo.** Duplicates the process, dashboard, tmux sessions and
  fixed-cadence spend, and fragments the governor's view.
- **A lower ACMM level for the repo.** Reduces autonomy; does not remove an
  irrelevant agent.

## What to read next

- [Agent configuration](agent-configuration.md) — every field on an agent.
- [Skill registry and BYO agents](skills.md) — the `AgentSpec` contract this
  extends.
- [Per-repo `AGENTS.md`](agents-md.md) — per-repo *instructions*, the
  complementary mechanism.
- [`hive-open-pr`](hive-open-pr.md), [`hive-merge`](hive-merge.md),
  [`hive-open-issue`](hive-open-issue.md) — the three hive-mediated write paths
  that enforce the scope alongside the proxy.
