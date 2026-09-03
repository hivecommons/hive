# Linear agent integration

Part 2 of [RFC #4492](https://github.com/hivecommons/hive/issues/4492)
("converse capability + Linear agents as first-class workspace members").
With this integration a hive joins a Linear workspace **as an agent**: it can
be assigned or @-mentioned on issues, it acknowledges each agent session
within Linear's 10-second budget, kicks a configured hive agent to do the
work, and narrates completion back into the session as agent activities.

Part 1 ([#4515](https://github.com/hivecommons/hive/pull/4515)) added the
`converse` capability; the `api.linear.app` proxy enforcement (component F)
merged in [#4522](https://github.com/hivecommons/hive/pull/4522).

## Architecture

```
Linear                          hive spoke
──────                          ──────────
issue delegated to app ──webhook──▶ POST /api/linear/webhook   (public; HMAC = credential)
                                      │  verify Linear-Signature + ±60s replay window
                                      │  respond 200 immediately (5s budget)
                                      ▼
                                  Responder (pkg/linearagent)
                                      │  1. post `thought` activity  ◀── SYNCHRONOUS, ≤8s deadline
                                      │     (never waits on the governor loop or tmux)
                                      │  2. resolve session agent (config, no I/O)
                                      │  3. SendKick(agent, issue context)
                                      │  4. post `action` activity ("Delegated")
                                      ▼
                                  agent works (tmux CLI session)
                                      │
                              kick log archived (next rotation point, #4296)
                                      ▼
                                  kick observer → `response` activity, session finished
```

The <10s acknowledgement never depends on the governor's `eval_interval_s`
(300s) or on agent startup: the thought is posted synchronously on webhook
receipt, before the kick is even attempted. A kick refusal (paused agent,
unknown agent) is reported into the session as an `error` activity.

Completion detection is intentionally coarse: a run "ends" at the next kick-log
rotation point (a newer kick, an agent restart, shutdown). There is no
mid-run streaming of agent output into Linear yet.

## Setup

1. **Create a Linear OAuth application** (Linear → Settings → API →
   Applications) with:
   - Callback URL: `https://<your-hive>/linear/callback`
   - Webhooks enabled, URL `https://<your-hive>/api/linear/webhook`,
     with the **Agent session events** category checked. Copy the signing
     secret.
2. **Set the environment variables** on the hive (see
   [env-vars.md](env-vars.md#linear-agent-integration)):
   `LINEAR_CLIENT_ID`, `LINEAR_CLIENT_SECRET`, `LINEAR_WEBHOOK_SECRET`.
3. **Tell the hive its public origin** when the dashboard is not reached at
   `https://<your-hive>` directly. The `redirect_uri` the hive sends Linear
   is built from, in order: `dashboard.public_url`, then `hub.dashboard_url`
   (hub-hosted spokes only), then the request's
   `X-Forwarded-Proto`/`X-Forwarded-Host`/`Host`. Set the first one on a
   standalone (`hub.enabled: false`) hive whose dashboard is private but
   whose `/linear/callback` is published on another hostname, or that sits
   behind an ingress that rewrites the `Host` header (Traefik with a fixed
   upstream `Host`, a Cloudflare Tunnel "HTTP Host Header") — otherwise the
   install request and the callback request derive different origins and
   Linear refuses the code exchange with `redirect_uri is invalid`:

   ```yaml
   dashboard:
     public_url: https://hive.example.com   # origin only: no path or query
   ```

   It must be an absolute `http(s)://` origin; a path, query, fragment or
   credentials fail config load with a clear error, and a trailing slash is
   trimmed. It is honored from the seed config (`hive.yaml`). Do **not** reach
   for `hub.dashboard_url` on a hub-less hive — it is the hub's notion of the
   spoke's dashboard link and stays only as the fallback for hub-hosted
   spokes.
4. **Connect the workspace**: as an owner, `POST /api/linear/agent/install`
   returns an `authorize_url` and the `redirect_uri` it was built with — that
   `redirect_uri` is the exact value the Linear app's Callback URL must match,
   so check it here rather than decoding the authorize URL. Open the
   `authorize_url` in a browser and approve the app for the workspace (the
   flow uses `actor=app` and the `app:assignable,app:mentionable` scopes, so
   the app becomes an assignable, mentionable workspace member). Linear
   redirects back to `/linear/callback`, and the hive stores the workspace
   grant + its per-workspace app user id at `LINEAR_AGENT_STORE`
   (default `/data/linear-agent.json`).
5. **Pick the session agent** when the hive runs more than one agent — from
   the dashboard (Governor → Work Source → Linear → *Session agent*) or in
   `hive.yaml`:

   ```yaml
   governor:
     work_source:
       linear:
         session_agent: scanner   # agent that takes Linear sessions
   ```

   With exactly one configured agent this is implicit. The dashboard rejects
   (400) a name that matches no configured agent.
6. **Optional — enumerate only delegated/assigned issues**: when the Linear
   work source is active, *Assigned only* in the dashboard (or
   `assigned_only: true` in `hive.yaml`) narrows backlog enumeration to
   issues assigned **or delegated** to the app user (Linear sets `delegate`,
   not `assignee`, when an issue is handed to an agent):

   ```yaml
   governor:
     work_source:
       type: linear
       linear:
         api_key: ${LINEAR_API_KEY}
         assigned_only: true
   ```

   This fails closed: `assigned_only` without a connected Linear agent is a
   startup error, never "enumerate everything" — and the dashboard refuses to
   save it (400) until step 4 has been completed.
7. **Map teams to repos.** The whole `work_source.linear` block — API key,
   hold labels, session agent, assigned-only, and the team list with each
   team's `repo`, `states`, `cycles` and per-project repo overrides — is
   settable from the dashboard's Work Source tab, backed by
   `GET`/`PUT /api/config/governor/work-source` (owner-only). Set `states`
   (e.g. `Todo, In Progress`) on each team: without it every open issue in
   the team counts toward the governor's backlog and a large backlog will put
   the hive into SURGE. `GET` never returns the API key value, only
   `api_key_set`; a `PUT` that omits `api_key` keeps the stored one. The
   `teams` list is replaced when present and untouched when absent:

   ```json
   {
     "type": "linear",
     "linear": {
       "session_agent": "scanner",
       "assigned_only": false,
       "hold_labels": ["hold"],
       "teams": [
         {"key": "ENG", "repo": "my-org/app", "states": ["Todo", "In Progress"],
          "cycles": "current", "projects": [{"name": "Billing", "repo": "my-org/billing"}]}
       ]
     }
   }
   ```

With `type: linear` the GitHub App does **not** need the Issues read
permission: a failed GitHub issue enumeration is logged as a warning and the
cycle continues, with the Linear backlog supplying issues and GitHub supplying
PRs when it can.

`GET /api/linear/agent/status` (owner) reports configuration, the connected
workspace, the resolved session agent, and recent sessions.
`POST /api/linear/agent/disconnect` (owner) forgets the stored grant (revoke
the app itself from Linear's settings).

## GitHub-issue parity: agents writing to Linear

With the pieces above a hive could *read* Linear and *acknowledge* sessions,
but an agent still had no way to do the tracker half of its policy —
file an issue, comment, cite the issue from a PR — against Linear. The
policy templates are written for GitHub Issues (`gh issue create`,
`Fixes #N`, the `hold` label), and the proxy's mutation allowlist was gated
but nothing reachable: no agent held a Linear credential. Parity is built
the same way the GitHub path is, from the config that already exists:

| GitHub Issues | Linear | Mechanism |
|---|---|---|
| App installation token, tier-scoped, pushed as `GITHUB_TOKEN` to push-capable agents and refreshed hourly | The connected app's OAuth token pushed as `LINEAR_ACCESS_TOKEN` (Bearer) to **ISSUES_ONLY+** agents; falls back to `work_source.linear.api_key` as `LINEAR_API_KEY`; re-pushed on the same hourly refresh tick | `agent.Manager.SetLinearCredentialResolver`, wired in `main.go` from `dashboard.Server.LinearAgentAccessToken` |
| Advisory agents have `GH_TOKEN`/`GITHUB_TOKEN` stripped | Advisory agents have both Linear variables stripped from the tmux session | `ensureTmuxSession` |
| Writes authored by the App bot | Writes authored by the Hive app user (the same identity that acknowledges sessions) | `actor=app` grant |
| `${GH_AUTH}` explains auth; templates give `gh issue create` and `Fixes #N` | A **Work Tracker: Linear** section rendered from `work_source.linear` (team → repo map, states, hold labels, `assigned_only`) and injected into every kick at the same post-resolution seam as held-PR coordination, so customized templates cannot omit it; `${WORK_TRACKER}` places it explicitly | `pkg/scheduler/work_tracker.go` |
| `Fixes #N` auto-closes on merge | Linear's GitHub integration: identifier in the branch name or `Fixes TEAM-123` in the PR body links the PR, moves the issue to In Progress when it opens and Done when it merges; `Part of` / `Refs` / `Contributes to` are the non-closing forms | Linear-side, nothing hive-specific |
| REST route table gates writes by tier | GraphQL operation allowlist gates writes by tier (below) | `pkg/proxy/linear_rules.go` |

Nothing changes for a GitHub-sourced hive: with no Linear credential the
resolver injects nothing and the tracker section is empty.

Agents are told **not** to change the state of PR-driven issues by hand —
Linear's GitHub integration owns that transition, exactly as GitHub's
`Fixes` keyword owns issue closure. Make sure the integration is enabled
for the workspace and the repos in `work_source.linear.teams[].repo` are
connected to it.

### Session kicks and governor kicks

A delegated issue reaches the hive twice: the webhook opens an agent
session (kicked immediately to the session agent), and — with
`assigned_only: true` — the same issue is enumerated into the governor's
backlog on the next sweep. Kicks never interrupt a running agent
(`SendKick` waits for the CLI's input prompt), so the risk is a *re-hand*:
the governor kicking the same issue again the moment the session's run
ends, or a second agent in the lane taking it in parallel.

The session tracker is therefore the in-flight ledger. While a session is
`working`, the scheduler withholds its issue from every governor kick's
`${ISSUE_LIST}` and `IssueRefs`, and says so in an **In Flight** note
appended at the same seam as the tracker section (`${IN_FLIGHT}` places it
explicitly). The hold releases when the session finishes — its kick log
archives — or fails. GitHub-sourced items are never session-held.

### Which agent takes sessions

`work_source.linear.session_agent` when set; otherwise the sole configured
agent; otherwise the sole enabled agent whose ACMM mode allows tracker
writes (`ISSUES_ONLY` and above) — which is what makes the L3 pack (six
agents, quality the only writer) work without extra config. Two or more
writers is ambiguous and the session is acknowledged with an error naming
the setting.

### PRs in the session

When the hive's `hive-open-pr` watcher opens a PR for an agent with an
active session, the PR is narrated into the session as an `action`
activity and attached to the session's external links
(`agentSessionUpdate.externalUrls`), so the person who delegated the issue
sees where the work landed before the run ends. Linear's GitHub integration
attaches the same PR to the *issue* on its own; this is the session
surface.

## Proxy enforcement

Agent-side calls to `api.linear.app` go through the ACMM proxy
(`pkg/proxy/linear_rules.go`, merged in #4522): a deny-by-default GraphQL
mutation allowlist where `agentActivityCreate`/`agentSessionUpdate` are
reachable at every tier (they carry the 10-second session invariant),
`issueUpdate`/`commentCreate` require ISSUES_ONLY, and anything unknown or
unparseable is denied. The control-plane client in `pkg/linearagent` (webhook
ack, identity query, token refresh) runs in the hive process itself, not
through the agent proxy.

## What requires a live workspace to verify

CI exercises every component against recording fakes; these behaviors depend
on Linear's side of the contract and need a manual pass against a real
workspace:

1. **OAuth install**: authorize with `actor=app` and confirm the app appears
   as an assignable, mentionable member; callback lands with
   `?linear=connected` and status shows the workspace + `viewer_id`.
2. **Webhook signatures**: real deliveries verify against
   `LINEAR_WEBHOOK_SECRET` (bare hex HMAC in `Linear-Signature`, no
   `sha256=` prefix) and pass the ±60s `webhookTimestamp` window.
3. **Ack timing**: delegate an issue to the app and confirm the session shows
   the thought within 10s (the responder's deadline is 8s).
4. **Prompt field positions**: `prompted` events are parsed from
   `agentActivity.content.body` with a fallback to `agentActivity.body`, and
   issue context from `promptContext` at both the top level and under
   `agentSession` — confirm real payloads land in one of those.
5. **Delegate filter**: with `assigned_only: true`, confirm delegated issues
   (Linear sets `delegate`) are enumerated and unrelated backlog is not.
6. **Token refresh**: after ~24h, confirm the stored access token refreshes
   transparently (30-minute grace window; the old refresh token is kept when
   the response omits a new one).
7. **Agent writes**: from an ISSUES_ONLY+ agent session, confirm
   `LINEAR_ACCESS_TOKEN` is set (and absent in an advisory session), that an
   `issueCreate` lands authored by the Hive app user, and that an `issueDelete`
   is refused by the proxy with a 403 naming the operation.
8. **PR auto-link**: open a PR on a branch named `<agent>/team-123-slug` with
   `Fixes TEAM-123` in the body and confirm Linear attaches it and moves the
   issue to In Progress, then Done on merge.
9. **Session PR link**: with a session `working`, have the agent open a PR
   through `hive-open-pr` and confirm the session shows an "Opened pull
   request" activity and the PR under its external links.
10. **In-flight withholding**: while a session is `working`, trigger a
    governor kick for the same agent and confirm the delegated issue is
    absent from its work list and named under "In Flight"; after the run
    ends, confirm it is handed out again on the next sweep (or has left the
    enumerated states via the PR).
