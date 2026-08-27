# The agent turn model and where in-process state lives (#4002)

Status: **spike / investigation — informational, no decision taken.**

This is step 1 of the four steps proposed in RFC #4002 ("re-entrant
conversation-as-state agent turn model"). It documents how hive drives an agent
turn **today** and where per-agent state lives that would not survive a process
restart. It prototypes nothing, proposes nothing, and takes no decision — steps
2 (prototype), 3 (handoff path), and 4 (feasibility + migration cost) are
untouched.

Every claim below carries a `file:line` citation against `origin/v4` at the time
of writing. Line numbers drift; the function names are the durable handle. Where
the RFC's framing does not survive contact with the code, this page says so
plainly — that is the main thing a spike is for.

---

## Summary of findings

1. **There is no "turn" in the code.** No `Turn` type, no per-turn context, no
   per-turn goroutine, no turn return value. A turn is an emergent property of
   pane text: it begins when hive types a prompt into a tmux pane and ends when
   the pane matches an idle-input-prompt marker again.
2. **There is no agent-owned conversation to externalize.** Hive never holds the
   conversation. The backend CLI does, in its own private on-disk format, and
   hive treats it as opaque. Hive durably records the **prompts it sent** and a
   **rendered-text transcript of the terminal**, and neither is a conversation.
3. **Hive already externalizes a lot of per-agent state** — via
   `/data/hive-state.json` — but that state is *control-plane* state (paused,
   pins, restart counts, backoff ladders), not *conversation* state.
4. **A restart does not resume a turn; it abandons one.** `Restart` kills the
   CLI and the tmux session outright, and the replacement receives no prompt at
   all until the governor's next tick happens to find it due — the kick that
   arrives carries no signal that a restart occurred.
5. **The one thing that does survive is accidental, not designed**, and it works
   only when the tmux server outlives the hive process — which a pod roll or an
   image upgrade rules out.

---

## 1. How a turn is actually driven

### 1.1 The CLI is long-lived; the turn is not a process

The backend CLI is launched **once per tmux session**, as an interactive
foreground process typed into a shell pane, and it stays there across many
turns. `launchInTmux` creates a detached shell session
(`src/pkg/agent/manager.go:1822`, `newSessionCommands`, which issues a plain
`tmux new-session -d` at `src/pkg/agent/manager.go:1838`) and then *types the CLI
invocation into that shell*:

```go
m.tmuxSendLiteralForAgent(agent, fullCmd)   // src/pkg/agent/manager.go:2634
time.Sleep(textToEnterDelay)
m.tmuxSendEntersForAgent(agent)             // src/pkg/agent/manager.go:2636
```

The composed command line is per-backend and is built as a string —
`claude --model … --dangerously-skip-permissions`
(`src/pkg/agent/manager.go:2429`), `copilot … --allow-all`
(`src/pkg/agent/manager.go:2456`), and so on.

Nothing in the kick path ever spawns a process. `deliverKickLocked`
(`src/pkg/agent/manager.go:4647`) only manipulates the existing pane.

### 1.2 A turn starts with simulated keystrokes

`SendKick` (`src/pkg/agent/manager.go:4569`) is the entry point; it delegates to
`deliverKickLocked` (`src/pkg/agent/manager.go:4647`), which:

- clears any stale input with `C-c` then `C-u`
  (`src/pkg/agent/manager.go:4656`),
- types the prompt literally, chunked, via
  `tmuxSendLiteralForAgent` (`src/pkg/agent/manager.go:4683`, implementation at
  `src/pkg/agent/manager.go:4974` → `tmux send-keys -l`),
- submits with `tmuxSendEntersForAgent` (`src/pkg/agent/manager.go:4697`).

The prompt reaches the model the same way a human's keyboard would. There is no
API call, no stdin pipe, and no structured request that hive could inspect,
replay, or hand to another process.

### 1.3 A turn "ends" when the pane looks idle

Nothing calls `Wait()` on the CLI for a turn — the process does not exit between
turns, so there is no exit to wait for. Turn completion is **inferred from pane
text**, by two independent mechanisms:

**The readiness gate before the next kick.**
`waitForInputPromptForAgent` (`src/pkg/agent/manager.go:3885`) polls
`captureTmuxPaneForAgent` and returns once `paneShowsInputPrompt`
(`src/pkg/agent/manager.go:3814`) matches a per-backend marker string. `SendKick`
calls it at `src/pkg/agent/manager.go:4627`. This gate is the only thing that
stops hive typing a new prompt on top of an in-flight response.

**The pane poller.** `pollTmuxOutputForAgent`
(`src/pkg/agent/manager.go:2847`) runs a `3 * time.Second` ticker
(`src/pkg/agent/manager.go:2848`) for the agent's whole lifetime, diffing
captured pane content to maintain `agent.LastPaneChange`
(`src/pkg/agent/manager.go:2906`) — hive's only evidence that a running,
authenticated CLI is actually doing something.

Stall recovery is likewise textual: `nudgeIfKickStalled`
(`src/pkg/agent/manager.go:5597`) compares `paneContentHash(pane)` against
`agent.lastInferKickPane` (`src/pkg/agent/manager.go:5611`), and
`paneShowsActiveWork` (`src/pkg/agent/manager.go:5545`) matches a spinner or a
token-counter marker.

### 1.4 Scheduling: one shared ticker, not a per-turn continuation

**Correction to the task framing: `src/pkg/agent/scheduler.go` does not exist.**
Kick *timing* lives in `pkg/governor`; kick *text* is built in `pkg/scheduler`.

- `Governor.agentsDueForKick` (`src/pkg/governor/governor.go:632`) compares
  `now - LastKick` against each agent's configured cadence
  (`AgentCadence`, `src/pkg/governor/governor.go:24`).
- `Governor.Evaluate` (`src/pkg/governor/governor.go:311`) calls it at
  `src/pkg/governor/governor.go:389` and returns the due list.
- The driving loop is a single ticker in `main`:
  `time.NewTicker(… EvalIntervalS …)` at `src/cmd/hive/main.go:4700`, loop at
  `src/cmd/hive/main.go:4754`, evaluation at `src/cmd/hive/main.go:5791`,
  message assembly via `sched.BuildKickMessages` at `src/cmd/hive/main.go:5960`
  (`src/pkg/scheduler/scheduler.go:425`), and delivery via
  `agentMgr.SendKick` at `src/cmd/hive/main.go:6034`.

This matters for the RFC: the scheduler is already **stateless with respect to
turns**. It does not hold a continuation, does not await turn *N* before
planning turn *N+1*, and reconstructs the entire decision each tick from
`LastKick` timestamps. The control loop is arguably closer to the RFC's model
than the RFC assumes. The suspended state is not in the scheduler — it is in the
CLI subprocess.

---

## 2. Where per-agent state lives

### 2.1 Durable — survives a process restart

| State | Where | Citation |
|---|---|---|
| Pause flag (one bool per agent) | `/data/hive.yaml` via `AgentConfig.Paused` | `src/pkg/config/config.go:856`; writer `SetAgentPausedAndSave` `src/pkg/config/config.go:5040` |
| Pause provenance (`PausedAt`, `PausedReason`, `PausedTrigger`, `PausedBy`), CLI/model pins, model/backend overrides, restart count, `LastKick`, truncated kick history | `/data/hive-state.json` via `snapshot.AgentState` | `src/pkg/snapshot/state.go:78-101`; path `src/cmd/hive/main.go:1738` |
| Watchdog failure count, crash-loop latch, backoff deadline, healthy-since, conditions | same file, `snapshot.PersistedState.Watchdog` | `src/pkg/snapshot/state.go:41`; `watchdog.PersistedAgent` `src/pkg/watchdog/reconciler.go:187` |
| Fleet-breaker engagement + held set | same file, `BreakerState` | `src/pkg/snapshot/state.go:49` |
| Governor budget/spend/eval history, cadence overrides, ACMM level | same file | `src/pkg/snapshot/state.go:16-42` |
| **Full text of every delivered prompt** | `/data/prompt-history.jsonl` (lumberjack-rotated JSONL) | `src/pkg/dashboard/prompt_history.go:44`; writer `Server.RecordPrompt` `src/pkg/dashboard/prompt_history.go:365`, wired at `src/cmd/hive/main.go:3077` |
| **Rendered terminal scrollback, per kick** | `/data/logs/kicks/<agent>/<ts>-<reason>.log` | `src/pkg/agent/kick_logs.go:43` (`defaultKickLogDir`); writer `archiveKickLogLocked` `src/pkg/agent/kick_logs.go:196` |
| Token-usage summary | `/data/metrics/token-summary.json` | `src/pkg/tokens/collector.go:85`, `:121` |
| Structured audit trail | `/data/audit.jsonl`, reloaded into a ring at boot | `src/pkg/dashboard/audit.go:22`, `loadFromDisk` `:88` |
| Agent-name → UID allocation | `/var/run/hive/uid-map.json` | `UIDMapPath` `src/pkg/agent/uidmap.go:17`; load `src/pkg/agent/manager.go:1467` |
| Backend CLI's own session/credential files | the CLI's own `HOME` / `CODEX_HOME`, rooted at `/data/home` | per-agent `CODEX_HOME` `src/pkg/agent/manager.go:8490`, helper `:7254`; per-agent HOME `src/pkg/agent/interactive_home.go:57`; shared `.claude` bridged by symlink `interactive_home.go:74` |

Two caveats on that table:

- The UID map is the one entry **not** rooted at the `/data` PVC. `/var/run` is
  conventionally ephemeral, so whether it survives a pod restart depends on the
  deployment's volume configuration rather than on the code. The load path
  treats absence as a recoverable fallback
  (`src/cmd/hive/main.go:1472`), so this is a durability *asymmetry* rather than
  a known failure — noted here because it is the only place the otherwise
  consistent "durable means `/data`" rule does not hold.
- The last row is the important one for the RFC and is discussed in §5.

### 2.2 In-process — lost on restart

All of the following are fields of `AgentProcess`
(`src/pkg/agent/manager.go:226`) or goroutines owned by the manager. None is
written anywhere.

| State | Field / mechanism | Citation |
|---|---|---|
| Live output ring buffer | `OutputBuffer *RingBuffer` — pure in-memory circular slice, no persistence path | `src/pkg/agent/manager.go:251`; `src/pkg/agent/ringbuffer.go` |
| Last captured pane content | `lastPaneCapture []string`, guarded by `paneMu` | `src/pkg/agent/manager.go:252-253` |
| Activity clock | `LastPaneChange` (guarded by `paneMu`) | `src/pkg/agent/manager.go:300` |
| Most recent kick text | `LastKickMessage` — replaced on every kick; the durable copy is truncated (`KickRecord.Snippet`) or lives in prompt-history | `src/pkg/agent/manager.go:254`; explicitly called out at `src/pkg/dashboard/prompt_history.go:25-30` |
| tmux session/socket identity | `tmuxSession`, `tmuxSocket` | `src/pkg/agent/manager.go:260-261` |
| Per-launch cancel func and launch generation | `cancel context.CancelFunc`, `launchGen int` | `src/pkg/agent/manager.go:262`, `:319` |
| Launch-serialization flag | `launching bool` (guarded by `m.mu`) | `src/pkg/agent/manager.go:272` |
| One-shot bootstrap override | `BootstrapOverride` | `src/pkg/agent/manager.go:274` |
| Token-restart backoff ladder | `lastTokenRestart`, `tokenRestartAttempts`, `tokenRestartGaveUp` | `src/pkg/agent/manager.go:276`, `:285`, `:287` |
| Login / quota observations | `NeedsLogin`, `QuotaExhausted` | `src/pkg/agent/manager.go:288-289` |
| Consent-screen watcher timers | `consentSeenAt`, `lastConsentDismiss` | `src/pkg/agent/manager.go:308-309` |
| Stall-watchdog per-kick state | `lastInferKickAt`, `lastInferKickPane`, `stallNudgeSent`, `lastInferKickMarks`, `actionNudgeSent` | `src/pkg/agent/manager.go:310-312`, `:320`, `:328` |
| Transient-API-error nudge cooldown | `lastTransientNudge`, `transientNudgesThisKick` | `src/pkg/agent/manager.go:316-317` |
| Un-archived-scrollback flag | `kickLogPending` (guarded by `m.mu`) | `src/pkg/agent/manager.go:327` |
| Sandbox / bob-key latches, last launch banner | `sandboxResumeAfterCancel`, `awaitingBobKey`, `lastLaunchFailureBanner` | `src/pkg/agent/manager.go:330`, `:337`, `:346` |
| Poller goroutines themselves | `go m.pollTmuxOutputForAgent(agent, agentCtx)` and siblings, tied to a per-launch context | `src/pkg/agent/manager.go:2567`, `:2571`, `:2577` |
| Blocked-action thrash windows | `Manager.thrash map[string]*thrashState` under its own `thrashMu` — deliberately *not* `m.mu`, to avoid re-entrancy from the output-capture goroutines | `src/pkg/agent/manager.go:421-425`; `thrashState` `:3655`; trip logic `recordBlockedAndCheck` `:3660` |

Note the asymmetry: several counters that exist precisely to *stop a runaway
loop* (`tokenRestartAttempts`, `transientNudgesThisKick`, `stallNudgeSent`) are
in-process only. A restart resets each of them to zero. The watchdog's own
backoff ladder is persisted (`src/pkg/watchdog/reconciler.go:345`) specifically
so that it doesn't have this property; the manager-side nudge counters were not
given the same treatment. This is an observation about current behaviour, not a
bug report — see Open questions.

### 2.3 tmux state, and the one accidental survival path

tmux is a separate process tree with its own server per agent socket
(`src/pkg/agent/manager.go:1767` builds `tmux -L <socket>`). What tmux holds —
the running CLI process and its scrollback — is therefore outside hive's memory
and *can* outlive a hive process restart.

`launchInTmux` exploits this. If the pane already shows a CLI marker and no
relaunch was forced, hive **adopts the surviving CLI instead of launching one**:

```go
if !agent.forceRelaunch && m.tmuxPaneHasCLIForAgent(agent) {
    m.logger.Info("CLI already running in tmux pane, skipping launch", …)
    …
    go m.pollTmuxOutputForAgent(agent, agentCtx)
    return nil
}
```
(`src/pkg/agent/manager.go:2557-2579`)

Three things about this are worth stating precisely:

- **It is one of two reattach points, and both are early returns rather than a
  recovery routine.** The other is `ensureTmuxSession`
  (`src/pkg/agent/manager.go:1922`), whose first act is
  `if m.tmuxSessionExistsForAgent(agent) { return nil }`
  (`src/pkg/agent/manager.go:1923-1925`) — a surviving session is reused, not
  recreated. Boot reaches both: `main` unconditionally calls
  `agentMgr.Start(ctx, name)` for every enabled agent
  (`src/cmd/hive/main.go:3457`) and the reuse-vs-relaunch decision is taken
  inside. There is no `Adopt`, `Reattach`, or `RecoverAgents` function;
  searching for one finds only `RestoreBreaker`
  (`src/pkg/agent/manager.go:8848`), which restores control metadata and is
  explicitly documented as *not* touching agent state: "a boot restore must
  never change agent state, only reattach the breaker"
  (`src/pkg/agent/manager.go:8845`).

  Because reattachment is emergent from two independent early returns rather
  than an explicit path, nothing in the codebase names it, tests it end to end,
  or reports whether it happened. The only trace is a log line
  (`src/pkg/agent/manager.go:2558`).
- **The adoption test is a screen-scrape.** `tmuxPaneHasCLIForAgent`
  (`src/pkg/agent/manager.go:2185`) is one line: `paneHasCLIMarker(
  m.captureVisiblePaneForAgent(agent))`. Hive decides whether an agent's
  conversation is still alive by pattern-matching rendered terminal output.
- **It does not survive the deployment's actual restart mode.** In hosted
  operation hive restarts by rolling the pod or replacing the image, which
  destroys every tmux server in the container. The `kick_logs.go` header states
  this outright: graceful shutdown must archive scrollback because "a pod roll
  or hive image upgrade … destroys every tmux server in the container"
  (`src/pkg/agent/kick_logs.go:17-19`, `:246`).

So the reattach path helps only when the hive **process** restarts inside a
surviving **container**. For the case the RFC names — spoke rolls — it does not
apply.

---

## 3. What a restart actually does to a turn

`Restart` (`src/pkg/agent/manager.go:9182`) does not resume anything. In order:

1. `C-c` into the pane and cancel the launch context
   (`src/pkg/agent/manager.go:9209-9213`).
2. Archive the scrollback *before* it is destroyed —
   `archiveKickLogLocked(agent, "restart")`
   (`src/pkg/agent/manager.go:9218`), with the comment that `kill-session`
   destroys the only record of the previous run
   (`src/pkg/agent/manager.go:9215-9217`).
3. Reap the CLI process and any UID-owned helpers
   (`src/pkg/agent/manager.go:9229-9233`).
4. `tmux kill-session` (`src/pkg/agent/manager.go:9240`).

The replacement CLI starts with **no prompt at all** in the default case.
`buildBootstrapPrompt` (`src/pkg/agent/manager.go:3352`) computes a list of
candidate policy paths and then unconditionally returns `""`
(`src/pkg/agent/manager.go:3383`), with the reasoning recorded inline: the
governor's first eval cycle kicks all due agents with fully substituted
templates, whereas sending a boot prompt here leaked unsubstituted `${ISSUE_LIST}`
placeholders to the agent (`src/pkg/agent/manager.go:3379-3382`).

So a restarted agent sits idle at its input prompt until the shared governor
ticker (§1.4) next finds it due. Only the explicit override paths supply text
directly: `RestartWithBootstrap` (`src/pkg/agent/manager.go:8887`) sets
`BootstrapOverride`, and `RestartThenSendKick`
(`src/pkg/agent/manager.go:8965`) is documented as restarting "with a clean
slate". Whichever path runs, delivery is gated on the new pane reaching its
input marker (`deliverStartupKick`, `src/pkg/agent/manager.go:4932`).

**Restart is a reset, not a resume.** Whatever the agent had established in its
conversation — plan, working context, partial tool results — is gone, and the
next thing it receives is an ordinary scheduled kick that carries no knowledge
that a restart happened or that earlier work exists.

---

## 4. The watchdog

`src/pkg/watchdog/` observes liveness and, in acting modes, restarts.

Its input is an `Observation` (`src/pkg/watchdog/classify.go:47`) whose
substantive fields are `Pane string` — the visible pane, deliberately excluding
scrollback so "stale markers in scroll history must not vouch for a dead CLI"
(`src/pkg/watchdog/classify.go:51-52`) — plus boolean verdicts derived from that
pane and a `LastChange` timestamp from the poller.

It classifies each pane into one `PaneClass`
(`src/pkg/watchdog/classify.go:13-31`): `ready`, `shell-prompt`,
`auth-required`, `stuck-overlay`, `no-output`, `no-session`, `unknown`.
`Dead()` (`src/pkg/watchdog/classify.go:37`) returns true for `shell-prompt`,
`stuck-overlay`, `no-output`, and `no-session`. `auth-required` is deliberately
excluded, because restarting into a dead credential produces a restart loop
(`src/pkg/watchdog/classify.go:33-35`). The pattern tables it matches against are
literal CLI UI chrome — `"Login expired"`, `"Please run /login"`
(`src/pkg/watchdog/classify.go:82-87`), `"[Next]"`, `"Choose an accent"`
(`src/pkg/watchdog/classify.go:92-102`).

Its action is `r.fleet.Restart(ctx, name)` (`src/pkg/watchdog/reconciler.go:632`,
via `planRestartLocked` at `:547` and `restartDetached` at `:628`) — i.e.
exactly the destructive path in §3.

**What the watchdog does about lost in-process state: nothing, and by design it
cannot.** It restores its *own* ladder across restarts
(`Snapshot`/`Restore`, `src/pkg/watchdog/reconciler.go:345` and `:379`), so it
"neither forgets a crash-loop nor re-runs a backoff ladder from the top"
(`src/pkg/watchdog/reconciler.go:373-374`). It has no mechanism to restore the
agent's conversation, because none exists to call. From the watchdog's point of
view a restarted agent is a fresh agent that happens to keep its name and its
failure count.

This is the sharpest way to state hive's current position: **the control plane
is already durable; the agent is not.**

---

## 5. Honest feasibility notes

These are constraints observed in the code, not arguments for or against the
RFC.

### 5.1 Hive does not own the conversation, and cannot read it

Every supported backend is an opaque interactive subprocess. Hive composes a
command line as a string (`src/pkg/agent/manager.go:2429`, `:2456`, `:2479`),
types it into a shell, and from then on interacts only through keystrokes in and
rendered characters out.

Hive sets `HOME` and a per-agent `CODEX_HOME`
(`src/pkg/agent/manager.go:8490`, helper at `src/pkg/agent/manager.go:7254`) so
that each agent's CLI writes its session files somewhere hive controls the
*location* of. That is location control, not format control: nothing in
`src/pkg/agent/` parses, writes, or migrates a backend session file.

Notably, **no launch path passes a resume/continue/session-id flag** to any
backend. The composed command lines carry model and permission flags only. There
is therefore no existing mechanism by which hive could ask a relaunched CLI to
pick up where the previous one stopped, even for a backend that supports it.

Consequence for the RFC: "the conversation is the durable state" presumes the
orchestrator can serialize and reload the conversation. Hive today can do
neither. Any conversation-as-state design must either (a) confine itself to
backends that expose a documented, stable resume mechanism and accept that the
envelope is backend-specific and not portable, or (b) move agents off opaque
interactive CLIs onto an API-shaped backend hive drives itself. Both are
substantially larger than "externalize hive's state", and the choice between
them is a real fork the RFC does not currently name. This spike takes no
position on which.

### 5.2 The two durable records hive does keep are not conversations

It is easy to mistake either for one; neither is.

- `/data/prompt-history.jsonl` holds the **input half only**. It is the
  fully-expanded prompt text of every kick
  (`src/pkg/dashboard/prompt_history.go:44`, `:365`). The file's own header
  explains it exists because the alternatives — in-memory `LastKickMessage` and
  the 120-character `KickRecord.Snippet` — could not answer "what was my agent
  asked to do?" (`src/pkg/dashboard/prompt_history.go:21-34`). No model
  response, tool call, or tool result is recorded.
- `/data/logs/kicks/…` holds **rendered terminal text**, captured with
  `tmux capture-pane -p -J` (`src/pkg/agent/kick_logs.go:167-171`). It is a
  human debugging artifact, retained to 10 files / 64 MiB per agent
  (`src/pkg/agent/kick_logs.go:48`, `:59`). It contains ANSI-rendered,
  width-wrapped, spinner-animated output with no message boundaries and no
  role structure. It is not machine-replayable into any backend.

Together these mean hive can already tell you *what it asked* and *what the
screen looked like*, and still cannot reconstruct *what the model's state was*.

### 5.3 The state envelope precedent already exists

Whatever shape a handoff envelope takes, hive has a working pattern for it:
`snapshot.PersistedState` (`src/pkg/snapshot/state.go:16`), saved atomically via
a `.tmp` + rename (`src/pkg/snapshot/state.go:113`), aged out at seven days
(`src/pkg/snapshot/state.go:14`), restored at boot by
`restoreAgentRuntimeState` (`src/cmd/hive/state_restore.go`). The watchdog's
`PersistedAgent` (`src/pkg/watchdog/reconciler.go:187`) shows how a subsystem
contributes a durable subset of its in-memory record to that envelope without
persisting the whole thing, including the judgement call about *which* fields
are safe to restore under which mode
(`src/pkg/watchdog/reconciler.go:376-382`).

If step 2 externalizes manager-side state, this is the mechanism to extend
rather than a new one to invent.

### 5.4 Everything hive knows about an agent's progress is screen-scraped

This is the load-bearing constraint behind all of the above and is worth
isolating. Turn completion (§1.3), liveness classification (§4), stall
detection (`src/pkg/agent/manager.go:5597`), auth failure
(`src/pkg/watchdog/classify.go:82`), quota exhaustion
(`src/pkg/agent/manager.go:289`), and even whether a surviving CLI can be
adopted (`src/pkg/agent/manager.go:2185`) are all decided by matching substrings
against rendered terminal output.

A structured turn return value — the RFC's testability and subagent-sync
benefits — is not a refactor of an existing structured signal. There is no
structured signal. It would be a new capability, and it requires a channel to
the backend that hive does not currently have.

---

## Open questions

Things this spike did not establish, and what would settle each.

1. **Do the supported backends expose a stable resume mechanism, and is its
   session format documented?** Not investigated here — it is a property of
   claude / copilot / bob / agy, not of hive, and no hive code exercises it.
   Settling it requires per-backend version-pinned testing against the actual
   binaries, since an undocumented on-disk format is not a contract.
2. **Should the manager-side nudge and restart counters join the persisted
   envelope?** `tokenRestartAttempts` (`src/pkg/agent/manager.go:285`),
   `transientNudgesThisKick` (`src/pkg/agent/manager.go:317`), and
   `stallNudgeSent` (`src/pkg/agent/manager.go:312`) are loop-breakers that
   reset to zero on restart, while the watchdog's equivalent ladder is
   deliberately persisted (`src/pkg/watchdog/reconciler.go:373`). Whether the
   difference is intentional (these counters are per-launch by design, and a
   fresh launch legitimately deserves a fresh budget) or an oversight is not
   determinable from the code alone and was not raised as a defect by this
   spike. Confirming it needs the author's intent, not more reading.
3. **What is the actual observed restart rate, and how much work is lost per
   restart?** The RFC's motivation is "frequent spoke rolls", but this spike
   found no metric that quantifies work discarded by a restart. `RestartCount`
   (`src/pkg/snapshot/state.go:92`) counts restarts but says nothing about what
   each one cost. Sizing the problem would need that measurement, and step 4's
   feasibility judgement arguably depends on it.
4. **Is `buildBootstrapPrompt`'s path-building dead code, or a staging post?**
   The function assembles a candidate policy-file list
   (`src/pkg/agent/manager.go:3366-3378`) and then discards it by returning `""`
   (`src/pkg/agent/manager.go:3383`). The `return ""` is deliberate and
   explained, but whether the now-unused path construction above it should be
   removed or is being kept for a planned re-enable is not answerable from the
   code. Flagged as an observation, not filed as a defect.
5. **Are there non-tmux agent execution paths with different properties?**
   `src/pkg/agent/sandbox_executor.go` runs a different shape of execution
   (`src/pkg/agent/sandbox_executor.go:309` composes its own command line) and
   inference backends are handled separately
   (`IsInferenceBackend`, `src/pkg/agent/manager.go:672`). This spike scoped
   itself to the tmux CLI path, which is the fleet's normal mode; the sandbox
   and inference paths were not mapped and may not share these constraints.

---

## What this page does not say

It does not say the RFC is infeasible, and it does not say it is worthwhile.
Step 4 is where that judgement belongs, and it should be made with §5.1's fork
named explicitly and Open question 3 answered.
