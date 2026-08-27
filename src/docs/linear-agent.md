# Linear agent integration

Part 2 of [RFC #4492](https://github.com/kubestellar/hive/issues/4492)
("converse capability + Linear agents as first-class workspace members").
With this integration a hive joins a Linear workspace **as an agent**: it can
be assigned or @-mentioned on issues, it acknowledges each agent session
within Linear's 10-second budget, kicks a configured hive agent to do the
work, and narrates completion back into the session as agent activities.

Part 1 ([#4515](https://github.com/kubestellar/hive/pull/4515)) added the
`converse` capability; the `api.linear.app` proxy enforcement (component F)
merged in [#4522](https://github.com/kubestellar/hive/pull/4522).

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
3. **Connect the workspace**: as an owner, `POST /api/linear/agent/install`
   returns an `authorize_url`; open it in a browser and approve the app for
   the workspace (the flow uses `actor=app` and the
   `app:assignable,app:mentionable` scopes, so the app becomes an assignable,
   mentionable workspace member). Linear redirects back to
   `/linear/callback`, and the hive stores the workspace grant + its
   per-workspace app user id at `LINEAR_AGENT_STORE`
   (default `/data/linear-agent.json`).
4. **Pick the session agent** in `hive.yaml` when the hive runs more than one
   agent:

   ```yaml
   governor:
     work_source:
       linear:
         session_agent: scanner   # agent that takes Linear sessions
   ```

   With exactly one configured agent this is implicit.
5. **Optional — enumerate only delegated/assigned issues**: when the Linear
   work source is active, `assigned_only: true` narrows backlog enumeration to
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
   startup error, never "enumerate everything".

`GET /api/linear/agent/status` (owner) reports configuration, the connected
workspace, the resolved session agent, and recent sessions.
`POST /api/linear/agent/disconnect` (owner) forgets the stored grant (revoke
the app itself from Linear's settings).

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
