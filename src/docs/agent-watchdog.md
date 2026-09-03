# Agent Self-Healing Watchdog

Implements [RFC #4665](https://github.com/hivecommons/hive/issues/4665):
k8s-style liveness/readiness reconciliation for hive agents. Before the
watchdog, an agent's dashboard state was a config echo — an agent could sit
dead at a shell prompt (or at a login screen, or behind a first-run modal)
while the UI said `running`. The watchdog observes the actual tmux pane and
reconciles.

## What it does

On every governor evaluation tick (self-gated to `probe_interval_s`), for each
launched, running, unpaused agent:

1. **Liveness** — the visible pane is classified as a state machine:
   `ready`, `shell-prompt`, `auth-required`, `stuck-overlay`, `no-output`,
   `no-session`, or `unknown`. Classification reuses the same CLI markers and
   login-prompt patterns the agent manager uses for sensing. `unknown` is
   never treated as healthy — it simply takes no action.
2. **Restart with backoff** — dead panes (`no-session`, `shell-prompt`,
   `stuck-overlay`, stale `no-output`) are restarted with exponential backoff
   (default `1m, 2m, 4m, 8m, 16m`; the last entry is the cap). Restarts run
   detached with a hard timeout, so a wedged tmux/kick path can never block
   the governor tick; a wedge is surfaced as an alert on the next sweep.
3. **Crash-loop escalation** — after `crash_loop_after` consecutive failed
   restarts (default 5) the agent is paused with trigger `watchdog-crashloop`,
   a dashboard banner alert is raised, and an error is journaled. No more
   restarts until a human (or the resume API) intervenes. A continuous-ready
   window of `healthy_reset` (default 30m) clears the failure counter.
4. **Auth probing** — a pane sitting at a login screen sets
   `Authenticated=False` and raises an alert but deliberately does **not**
   restart: restarting into a dead credential is the 1042-restart loop the
   RFC documents. Provider-level credential probes reuse the rotation
   package's probers (Claude OAuth usage, Codex app-server handshake, Agy
   headless usage, DeepSeek balance) and the owner-aware per-agent credential
   check (per-agent `$HOME` layouts, 0600 session files).
5. **Readiness** — production evidence (pane activity plus newest mtime under
   the backend's state directories in the agent's `$HOME`) feeds a
   `Producing` condition. No evidence for `no_production_for` (default 6h)
   degrades to `Producing=False` plus a warning alert — never a pause or
   restart.

## Conditions

Each agent carries k8s-style conditions in the `/api/agents` payload:

```json
"conditions": [
  {"type": "Ready", "status": "True", "reason": "PaneReady", "lastTransitionTime": "..."},
  {"type": "Authenticated", "status": "True", "reason": "CredentialPresent", "lastTransitionTime": "..."},
  {"type": "Producing", "status": "True", "reason": "RecentProduction", "lastTransitionTime": "..."}
]
```

`lastTransitionTime` only moves when the status flips; the reason/message may
refresh in place. A probe that cannot tell reports `Unknown`.

### When most of the fleet goes `auth-required` at once

A burst — every Claude agent but ONE flipping to `auth-required` /
`PaneShowsLogin` within a few minutes, hours after a healthy boot — is almost
never an expired login, and re-authenticating will not fix it
([#5730](https://github.com/hivecommons/hive/issues/5730)).

Agents share one operator login: each runs as its own uid, but their
`~/.claude` all symlink to `/data/home/.claude` (#4619). Claude Code refreshes
that OAuth credential roughly every 8 hours and rewrites it as whichever agent
refreshed it, mode `0600`. The surviving agent is the one that owns the file;
every other uid, which reaches it through the shared `node` group, is locked
out of a credential that is otherwise perfectly healthy.

Check the mode before you touch the login:

```console
$ podman exec hive ls -l /data/home/.claude/.credentials.json
-rw-------. 1 hive-supervisor node 508 Sep  2 19:06 /data/home/.claude/.credentials.json
```

Owner-only (no group `r`) is the failure. One command fixes it, with no restart
and no re-login:

```console
$ podman exec hive chmod g+r /data/home/.claude/.credentials.json
```

Agents recover within seconds — the log shows `auto-restarting agent after
token detected in shared config`, then `credential watchdog: durable credential
restored`.

**Do not restart the container.** It is unnecessary, and it only re-runs the
boot-time repair that made the file readable in the first place, which hides
the cause.

The same shape applies to antigravity (`agy`), whose token lives one directory
deeper, at `/data/home/.gemini/antigravity-cli/antigravity-oauth-token`
([#5734](https://github.com/hivecommons/hive/issues/5734)) — check and fix it
the same way.

Two layers keep this from recurring, and both report rather than hide a
failure: the entrypoint's permission guards reopen the credential on every
write and on a 5s poll (they are the only actor that can, since they run as
root and the file is owned by an agent uid), and the in-process permissions
watcher logs at ERROR — naming the mode, the owner and the `chmod` — when it
finds a shared credential it cannot repair itself. If the watchdog reports

```
reason=unreadable by the hive process (permission denied)
recovery=chmod g+r /data/home/.claude/.credentials.json
```

that is this condition, stated plainly. A report of `login expired (no usable
refresh grant)` is the other condition and does need an operator login.

## Configuration

Everything lives under `governor.watchdog` and is optional; an absent block
means the defaults below. Invalid values fall back to the default for that
field with a logged warning — a config typo never silently disables healing.

```yaml
governor:
  watchdog:
    mode: observe          # observe (default) | heal | off
    probe_interval_s: 300
    liveness:
      stuck_overlay_after: 10m
      shell_prompt_after: 5m
    restart:
      backoff: [1m, 2m, 4m, 8m, 16m]
      crash_loop_after: 5
      healthy_reset: 30m
    readiness:
      no_production_for: 6h
    auth_probe: true
```

`mode` is the authority level, and the default is deliberately `observe`:

| mode | classifies, publishes conditions, alerts | restarts / pauses |
|---|---|---|
| `observe` (default) | yes | **no** |
| `heal` | yes | yes |
| `off` | no | no |

This reconciler restarts agents across the fleet, so it does not ship with that
authority. In `observe` it records what healing *would* have done — as audit
entries suffixed `-observed` — on your own agents, so promotion to `heal` is a
decision made on evidence rather than trust. Read the Audit Log, then switch.

The pre-mode `enabled` flag still works: `false` maps to `off`, and `true` or
absent maps to `observe` — never straight to `heal`, because a config written
before modes existed never consented to fleet-wide restarts. An unrecognized
`mode` falls back to `observe` and logs; a typo never grants authority.

Settings are editable in the dashboard under **Settings → Governor → Health**,
beside the escalation breaker, so nobody has to hand-edit `hive.yaml`. The
backoff ladder is shown there read-only: it is a derived progression, and an
arbitrary ladder invites a one-second cap.

### Fleet-wide kill switch

Set `HIVE_WATCHDOG_PAUSE=true` on the deployment to downgrade every hive that
reads it from `heal` to `observe` — conditions and alerts keep flowing, but no
agent is restarted or paused. It is read at every config resolve, so engaging
it needs no restart, and it can only ever *reduce* authority: it never turns a
watchdog on and never promotes `observe` to `heal`. This is the analogue of the
hub's spoke-upgrade pause, for the case where an operator needs the automated
actor to stop across the fleet without editing 55 configs.

Backoff and crash-loop state persist in the hive state file (`watchdog` key),
so a pod restart does not reset an escalated agent back into a restart storm.

## Audit trail

Every action the watchdog takes is written to the durable audit log
(`/data/audit.jsonl`, 90-day retention) under the user `watchdog`, visible in
the dashboard Audit Log:

| action | when |
|---|---|
| `watchdog-restart` | a dead agent was restarted (detail carries the pane class, failure count and backoff step) |
| `watchdog-crashloop-pause` | the give-up threshold was reached and the agent was paused |
| `watchdog-giveup` | recorded alongside the pause: no further restarts will be attempted |
| `watchdog-healthy-reset` | a continuously healthy agent's failure counter was cleared |

In `observe` mode the same decisions are recorded with an `-observed` suffix
(`watchdog-restart-observed`), so a reader can never mistake "would have
paused" for "paused". Sweeps and probes write nothing — only actions and
transitions — because the in-memory ring is small and per-sweep noise would
evict the entries that matter. Entries carry the reasoning, never pane content.

The `watchdog` user is deliberately not one of the audit log's pseudo-users:
its actions must be auditable, but they must not count as *human* engagement in
the activity signal the hub reports.

## Coordination with existing machinery

- Restarts go through the agent manager's `Restart` (same path as the manual
  kick), so restart counts stay in one ledger. The watchdog only reconciles
  launched, running agents; crashed/stopped agents remain the crash-restart
  loop's job, and auth-required panes are never restarted, respecting the
  capped token-restart accounting from #4606.
- Provider auth probes are the rotation package's probers (#4608), so probe
  improvements there (e.g. #4645) apply here unchanged.
- Dead-session and bare-pane recovery is the watchdog's ONLY in `heal` mode:
  the agent manager's crash loop stops restarting those two conditions so the
  bounded ladder is the single throttle. In `observe` (and `off`) the manager
  keeps that job, so there is never a window in which neither restarts a dead
  agent. Consent-screen dismissal and the inference stall nudge are not
  restarts and always stay with the manager.
- `Producing=False` requires work to actually be queued. An agent with an empty
  queue producing nothing is behaving correctly, and reporting it would light
  the condition on every healthy quiet hive — the same rule the hub applies to
  agent inactivity. Advisory-tier agents, which cannot drain a queue by design,
  are never marked down for a backlog they may not touch.
- The shared-`$HOME` 0600 permission clobber (RFC failure mode 3) was fixed
  at the source by the `umask 007` launch change (#4668) and is not
  re-implemented here.
