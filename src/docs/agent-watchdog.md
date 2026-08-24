# Agent Self-Healing Watchdog

Implements [RFC #4665](https://github.com/kubestellar/hive/issues/4665):
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

## Configuration

Everything lives under `governor.watchdog` and is optional; an absent block
means the defaults below. Invalid values fall back to the default for that
field with a logged warning — a config typo never silently disables healing.

```yaml
governor:
  watchdog:
    enabled: true
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

Backoff and crash-loop state persist in the hive state file (`watchdog` key),
so a pod restart does not reset an escalated agent back into a restart storm.

## Coordination with existing machinery

- Restarts go through the agent manager's `Restart` (same path as the manual
  kick), so restart counts stay in one ledger. The watchdog only reconciles
  launched, running agents; crashed/stopped agents remain the crash-restart
  loop's job, and auth-required panes are never restarted, respecting the
  capped token-restart accounting from #4606.
- Provider auth probes are the rotation package's probers (#4608), so probe
  improvements there (e.g. #4645) apply here unchanged.
- The shared-`$HOME` 0600 permission clobber (RFC failure mode 3) was fixed
  at the source by the `umask 007` launch change (#4668) and is not
  re-implemented here.
