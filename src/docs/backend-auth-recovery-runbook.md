# Agent backend-auth failure — recovery runbook

An agent whose model backend will not authenticate produces no output, so it
never shows up as a failing task — only as silence. Hive tracks this per agent
as `backend_auth` (see the
[health canary](fleet-health.md#agent-backend-auth-health-canary-6558)) and
rolls it up per hive as `auth_health`. This runbook is the response path for
those signals, including a fleet report that names the `backend-auth`
component.

## 1. Detect and size the impact

```bash
curl -s http://<dashboard>/api/status \
  | jq '.agents[] | select(.backendAuthStatus) | {name, backendAuthStatus, backendAuthSince, backendAuthLastError}'
```

Agents that are `ok` are omitted from those fields. Then size it:

| What you see | Impact | Urgency |
|---|---|---|
| One agent, `backendAuthSince` minutes old | That lane is paused; others are unaffected | Check within the hour |
| Some agents (`auth_health: degraded`) | Reduced throughput | Same day |
| Every enabled agent (`auth_health: down`, red row on `/fleet`) | Fleet is producing nothing; the owner gets one Slack DM | Now |

A fleet report with a count of 1 and `self-recovered: no` means one agent was
failing when the report was built, not that the fleet is down. Confirm with the
command above before treating it as an outage; if every agent is `ok`, it
recovered and no action is needed.

## 2. Pick the remedy by status

| `backendAuthStatus` | First action |
|---|---|
| `unlicensed` | Check the seat at <https://github.com/settings/copilot>. If it is active, the hive is presenting a token the account no longer owns: follow the `/login` recovery in [troubleshooting.md](troubleshooting.md). |
| `token-expired` | Run `/login` in any agent's Terminal with a licensed identity and wait about 30 seconds for the reconciler; affected agents relaunch onto the new token. |
| `forbidden` | Cause is undetermined (bare 403). Read which credential was refused (dashboard model dropdown hover, or the `copilot model discovery rejected by upstream` log line's `credential` field) before re-logging in; an org policy denial is not fixed by a new login. |
| `unreachable` | The inference backend is down or unroutable. Check the backend's own health and network path from the pod; re-login will not help. |
| `quota` | The provider is refusing calls on quota. Wait for the window to reset, add quota, or switch the agents to another backend. Do not restart agents in a loop. |

## 3. Verify recovery

1. Within one reconciler tick (about 30 seconds) the affected agents drop
   their `backendAuthStatus` field from `/api/status`.
2. `auth_health` returns to `ok` on `/fleet`.
3. An agent completes a turn (check its pane or the activity feed). `ok` is
   only set after a turn succeeds, so a clean status plus a completed turn is
   the evidence that matters.

If the status is still failing after two ticks, do not keep re-running
`/login`: collect `backendAuthLastError` for each agent and escalate.

## 4. What not to do

- Do not clear or hide the status to make the fleet row green. The canary
  is deliberately checked before every other verdict signal.
- Do not paste tokens into issues, fleet reports or chat. `backendAuthLastError`
  is a truncated pane line; review it before sharing.

After an outage lasting more than one tick, record it with the incident
write-up your team uses.
