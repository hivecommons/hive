# Agent state inventory — where in-process suspended state lives today

Status: stage 1 of the [RFC #4002](https://github.com/hivecommons/hive/issues/4002)
sequence (re-entrant conversation-as-state turn model), per the
[maintainer response](https://github.com/hivecommons/hive/issues/4002#issuecomment-5321618936):
**inventory (this document) → journaled prototype with a kill-mid-turn replay
test → handoff evaluation**. Docs only; nothing here changes behavior. Every
file/symbol reference below was verified against the `v5` branch at the time of
writing (line numbers drift; symbol names are the stable citation).

## The problem

RFC #4002 proposes making the conversation the durable state and each agent
turn a re-entrant function call, so an agent survives a pod roll because
nothing about it is suspended in-process. Before that can be prototyped, we
need an honest answer to the prior question: **where does in-process suspended
state actually live in hive today, and what happens to each piece when the pod
rolls?**

We already know the shape of the answer from operational history — the last
month re-taught "in-process state dies on a pod roll" one subsystem at a time
(#3771, #3817, #3968, #3792, #3989, #3997, #3876), each as its own
incident-and-fix cycle. This document consolidates those lessons, verifies each
one against current code, and sweeps for the state nobody has been burned by
yet. Section 4 classifies every row for the RFC: absorbed by the
conversation-as-state model, kept as infrastructure state, or owned by a
backend CLI that hive cannot externalize without headless turns.

## 1. The current agent-loop model

### 1.1 Hosted agents: a tmux pane owned by the manager

A hosted agent is not a process hive controls turn-by-turn. It is a backend
CLI (`claude`, `codex`, `copilot`, `agy`, `goose`, `bob`, …) launched into a
detached tmux session that the spoke's agent manager owns
(`src/pkg/agent/manager.go`, `Manager` / `AgentProcess`; sessions are named
`hive-<agent>` — `manager.go:846`). The manager's contract with the agent is
deliberately thin:

- **Launch**: `launchInTmux` creates the session with a deep scrollback
  (`defaultTmuxHistoryLimit` = 50,000 lines, raised from tmux's 2,000 default
  by #3790 because the default silently capped both browser scrollback (#3694)
  and the full-log endpoint (#3693)) and a 500-column pane
  (`defaultTmuxPaneWidth`, #3878 — the CLI truncates tool-call lines at render
  time, so a narrow pane destroys log content before it ever reaches
  scrollback).
- **Kick**: the governor decides an agent is due (`src/pkg/governor/governor.go`,
  `State.LastKick`) and the manager types a prompt into the pane
  (`deliverKickLocked`).
- **Observe**: a 3-second poller captures the pane
  (`pollTmuxOutputForAgent` → `AgentProcess.lastPaneCapture` /
  `LastPaneChange`) and watchdogs infer progress from what the pane *looks
  like* — stall nudges, consent-screen dismissal, login detection
  (`NeedsLogin`), no-action detection via tool-marker counting
  (`lastInferKickMarks`).

Everything the manager knows about a running turn is therefore an inference
over terminal text. The turn itself — the conversation, the tool calls, the
partial work — is suspended inside the backend CLI's process and rendered into
a tmux buffer. Neither is a structure hive can checkpoint, hand off, or
re-enter.

### 1.2 The backend CLI owns the conversation

Each backend records its own transcript, in its own format, in its own home:

- Claude Code: `~/.claude/projects/<project-hash>/*.jsonl`
  (`src/pkg/tokens/claude_scanner.go`, `ScanClaudeSessions`)
- Copilot CLI: `~/.copilot/session-state/`
  (`src/pkg/tokens/copilot_scanner.go`, `ScanCopilotSessions`)
- bob: `~/.bob/tmp/<uuid>/chats/<session-id>.json`
  (`src/pkg/tokens/bob_scanner.go`)

Hive reads these files today — but only for **token accounting**
(`src/pkg/tokens/collector.go`), never for resume. A subtle and important
consequence of the deployment layout: agent homes live on the PVC
(`HOME=/data/home` — `manager.go:1972`; `/data` is the `hive-data`
PersistentVolumeClaim, `src/pkg/hub/saas_provision.go` deployment template),
so **the transcript bytes survive a pod roll. What dies is the session**: the
CLI process, its in-flight turn, the tmux server whose scrollback was the
observable surface, and any backend-internal suspended state. Hive has no
contractual way to turn the surviving bytes back into a running conversation —
each CLI's resume semantics are its own, vary by backend, and are exactly the
"(b) backend-native session resume" path the maintainer response classifies as
a per-backend optimization, not a foundation.

### 1.3 The relay/scrape layer

The contributor relay (`bin/contributor-relay.sh` — a Node program despite the
extension) is the WebSocket client that connects a contributor agent to a
hive's contribute plane. In interactive mode it drives the same pane-shaped
contract from the outside: it types tasks into a tmux session and classifies
the pane text to decide whether the agent is `working`, `idle_complete`, or
`blocked_on_human` (`classifyTmuxPane`, `contributor-relay.sh:1193`, with
per-backend regex branches).

This layer is where scrape brittleness becomes a bug class of its own. The agy
branch of `classifyTmuxPane` carries the freshest scar (#4026): agy narrates
in prose, a leftover "Running…" line from a *previous* task pinned the
classifier to `working` forever, the relay kept renewing the task lease, and
the task was never reclaimed. The fix — scope the activity scan to the pane
tail — is a regex-window adjustment, which is the point: when the conversation
is a terminal buffer, correctness lives in regexes over prose.

### 1.4 The contrast case: contribute headless mode

The same relay also has a headless mode (`MODE_HEADLESS`,
`contributor-relay.sh:85`, added for #2538; per-backend one-shot invocations —
`claude -p "<prompt>"`, `codex exec "<prompt>"` — verified per backend as
recently as #4007). In headless mode there is no pane and no scraping: hive
constructs the prompt, the backend runs one bounded non-interactive turn, and
completion is reported as a **structured verdict** — since #3997 including
`no_work_needed`, which is a structured turn return in exactly the RFC's
sense. Lifecycle state is recorded as JSON
(`HEADLESS_STATUS_FILE`, default `/tmp/contributor-headless-status.json`).

This is why the maintainer response targets the contribute plane for the
prototype: the headless path already demonstrates hive-owned turns with
structured returns. What it does not yet have is a journaled, re-enterable
operation list — its status file is a coarse lifecycle marker in `/tmp`, not a
recoverable turn journal.

## 2. Inventory: volatile state and its blast radius

Scope note: "volatile" here means *lost or reset when the owning process
dies* — whether the state is a Go map, a process's heap, or a file on a
non-PVC path (`/tmp`, `/var/run`). Rows marked **fixed** are the incident
history: state that was volatile, burned us, and is now persisted or
re-derived; they stay in the inventory because they map the pattern the RFC
generalizes. Verdicts: **volatile** (lost, with consequences), **benign**
(lost, self-healing by design), **fixed** (was volatile, now durable or
re-derived).

### 2.1 Hub process (`src/pkg/hub`)

| # | State | Where (v5) | On a hub pod roll | Verdict |
|---|-------|------------|-------------------|---------|
| 1 | `heartbeatSwitchTag` — pending channel/branch switches delivered via heartbeat | `server.go:984`, guarded by `s.mu` | Historically: every in-flight channel switch silently dropped mid-fleet-migration; the pill kept promising the channel while the directive was gone. Fixed by #3771: re-armed from the persisted `SaaSHive.TrackedChannel` whenever a spoke's reported image tag disagrees with its tracked channel (`server.go:2230`, guarded on `ImageRef` being reported and the spoke not mid-upgrade) | **fixed** (re-derived) |
| 2 | `heartbeatUpgrade` — pending SHA upgrades for spokes the hub cannot kubectl-reach | `server.go:907`, guarded by `s.mu` | Re-populated by the stale-upgrade recovery sweep in `triggerAutoUpgrades` (`saas.go`, "Re-populate the heartbeatUpgrade map in case the hub restarted") from registry latches | **fixed** (re-derived) |
| 3 | `pendingWebhooks` — GitHub App installation webhooks that arrived before the matching spoke heartbeat | `server.go:988` | Dropped. GitHub does not re-deliver on its own, so an installation completed during a hub roll waits for manual repair or a later delivery | **volatile** |
| 4 | `pendingGateways` — funded OpenRouter gateways awaiting heartbeat delivery, carrying the secret key value | `server.go:1029` | Dropped; documented as "retried by the sponsor, not persisted". Deliberate: it is secret-bearing and drained on delivery | **benign** (by choice) |
| 5 | `pendingGitHubAppConfigs` — App config queued for heartbeat delivery | `server.go:993` | Dropped; re-queued by the flows that populate it | **benign** |
| 6 | `liveHiveUsers` / `engagedHiveUsers` — presence sets | `server.go:884` | Re-learned "within a beat or two" (comment says so, and it is true) | **benign** |
| 7 | `usageHistory` — sampled fleet token trend | `server.go:900` | Trend history lost; re-accumulates from the next beat | **volatile** (cosmetic) |
| 8 | `reporterSeen` / `statusFlipSeen` — flip/oscillation detectors | `server.go:968`, `:973` | Detector memory lost; a flip in progress across the roll goes unobserved | **volatile** (observability) |
| 9 | `appKeyDelivery` — per-hive consecutive undelivered-key counter (#2496 backoff) | `server.go:979` | Backoff resets; a broken reporter gets key material re-sent until the counter rebuilds | **volatile** (bounded) |
| 10 | `clusterUnreachableUntil` + reconcile throttles (`lastNetAdminReconcile`, `lastPerHiveEnvReconcile`, `lastGenerationRetire`) | `server.go:916` | One redundant dial-timeout burst / early sweep after boot | **benign** |
| 11 | `spokeProxyAuthCache` — memoized per-hive dashboard tokens | `server.go:1006` | Cache refills on demand (one kubectl exec per hive) | **benign** |
| 12 | `openRouterStateStore` — in-progress PKCE flows (`sync.Once`-lazily built) | `server.go:1034` | In-flight OAuth flows die; user restarts the flow | **volatile** (UX) |
| 13 | `lastHubUpgradeTrigger` / `hubUpgradeTarget` / `hubUpgradeFault` — self-upgrade debounce + fault surface | `server.go:836` | A roll mid-self-upgrade clears the "why is this stuck" surface (`hubUpgradeFault` exists because of the `target1` incident) | **volatile** (observability) |
| 14 | `perHiveEnvSeen` / considered / skipped / unreachable — env-convergence observation | `server.go:945` | Rebuilt by the next sweep; until then the convergence surface under-reports | **benign** |
| 15 | Hub-roll incident class as a whole: `heartbeatHealth`, vanity/claim in-flight guards (`sync.Map`s) | `server.go:875`, `:867`, `:874` | Health staleness re-learned per beat; in-flight background kubectl work is abandoned and restarted | **benign** |

Durable-by-design hub state that shares the same file (`s.mu` → `saveCh` →
`saveLoop`, `server.go:3453`): the registry itself (row: none — see §3).
Already-persisted leaf stores: `timeline` (`/data/saas/timeline/`), `journey`
(`/data/saas/journey-state.json`), alert acks
(`/data/saas/hub-alert-acks.json`), `revokedSessions` (PVC, audit F10 —
in-memory-only would *un-revoke sessions* on a roll), hub banners
(`/data/saas/hub-banners.json`), `reachHistory` (`/data/reach-history.json`),
key generations (`/data/saas/hub-generations.json`).

### 2.2 Spoke process (`src/cmd/hive`, `src/pkg/agent`, `src/pkg/governor`)

| # | State | Where (v5) | On a spoke pod roll | Verdict |
|---|-------|------------|---------------------|---------|
| 16 | The in-flight agent turn: backend CLI process + suspended conversation | the CLI's process; transcript files under `/data/home` (§1.2) | The turn dies mid-flight. Transcript bytes survive on the PVC; hive cannot re-enter them. Side effects already performed (a PR opened, a comment posted) are not journaled anywhere hive can consult on restart — this is the RFC's hard problem 1 | **volatile** (the RFC's target) |
| 17 | tmux server + scrollback — the de-facto conversation surface | tmux process memory; created 50,000 lines deep (#3790), 500 columns wide (#3878) | Entire observable history of every agent gone. `CaptureFullLog`, the dashboard log endpoint, and every pane-classification watchdog start blind | **volatile** |
| 18 | `AgentProcess` runtime fields: `Paused`+reason/trigger, `PinnedCLI`/`PinnedModel`, `ModelOverride`/`BackendOverride`, `RestartCount`, `KickHistory` (50 entries), `LastKick` | `manager.go:121` | Persisted to `/data/hive-state.json` once per eval cycle and on clean shutdown (`persistState`, `cmd/hive/main.go`; shape: `src/pkg/snapshot/state.go`), replayed at boot by `restoreAgentRuntimeState` (`cmd/hive/state_restore.go`). Overrides silently failed to survive until #3961/#3968 — the restore ran before the gateway predicate was wired, the rejection was swallowed, and "restored" was logged for a value already gone | **fixed** (with a persist window — see residuals) |
| 19 | Governor `State.LastKick` + budget window state | `governor.go` (`State`), persisted via `PersistedState.LastKicks` / `Budget*` | Historically: boot *wiped* persisted LastKick so every roll re-kicked every cadenced agent — silent token burn at roll frequency. Fixed by #3817 (`SeedLastKicks`, `governor.go:865`; boot now "kicks only agents whose cadence has elapsed") | **fixed** |
| 20 | Fleet breaker (`breakerEngaged` / `breakerPaused`) | `manager.go:329` | Persisted into `hive-state.json` (`BreakerState`) and re-associated at boot (`RestoreBreaker`) — an engaged kill-switch survives the roll | **fixed** |
| 21 | Watchdog inference state: `lastPaneCapture`, `LastPaneChange`, stall/action nudge counters, consent timers, `launching`, thrash-breaker map (`Manager.thrash`) | `manager.go:140`, `:165`–`:183`, `:252` | All reset. Mostly self-healing, with one sharp edge: `LastPaneChange` is "the spoke's only evidence of an agent actually doing something", and it reads "unknown" until the poller sees two differing captures | **benign**/**volatile** (observability) |
| 22 | `OutputBuffer` ring (500 lines/agent) + prompt-history in-memory ring | `manager.go:139`; `src/pkg/dashboard/prompt_history.go` | Ring lost; durable copies exist (`/data/prompt-history.jsonl`; pane scrollback until row 17 takes it) | **benign** |
| 23 | Spoke heartbeat last-good-collect cache | `src/pkg/hub/heartbeat.go:104` (`lastGoodPayload`) | Historically: a just-restarted spoke with real repos could *never* finish a collect inside the budget, so it had no cache and skipped every beat — a healthy hive read OFFLINE permanently. Fixed by #3876: collect-independent identity published before the loop, `minimalLivenessPayload` when collect times out with no cache | **fixed** |
| 24 | Agent mint-token cache | `/var/run/hive-metrics/agent-tokens/` (`manager.go:632`) — container filesystem, not the PVC | Re-minted on demand; by design a cache | **benign** |
| 25 | Sandbox executor in-flight runs + `sandboxResumeAfterCancel` | `src/pkg/sandbox` (`PodmanLauncher`), `manager.go:188` | A sandbox one-shot dies with the pod; its bounded-lifetime design assumes exactly this | **benign** |
| 26 | Runtime config overlay | `/data/hive.yaml.runtime` (`config.go:4287`); note `/etc/hive` is an emptyDir in the hosted deployment | Durable — the overlay exists precisely because the ConfigMap mount is unwritable | **fixed** |

### 2.3 Contribute plane (`src/pkg/dashboard/contribute_ws.go` + relay)

| # | State | Where (v5) | On a spoke (dashboard) pod roll | Verdict |
|---|-------|------------|--------------------------------|---------|
| 27 | Completed-task ledger + per-task cooldowns + PR URLs | `completedTasks` / `completedTaskCooldown` / `completedTaskPRURL` (`contribute_ws.go:353`), persisted to `/data/contributors/completed-tasks.json` | Survives; was moved to the PVC ledger through the #3792 → #3989 lineage after re-offer loops | **fixed** |
| 28 | Failure cooldowns + quarantine counters | `failedTasks` / `consecutiveFailures` (`:373`, `:381`), persisted to `/data/contributors/failed-tasks.json` | Survives (#2435 livelock fix, made durable with the ledger) | **fixed** |
| 29 | No-PR completion streaks | `noPRStreaks` (`:394`), `/data/contributors/no-pr-streaks.json` | Survives (#3980; geometric backoff must outlive the completion entry it escalates) | **fixed** |
| 30 | `no_work_needed` verdicts | `noWorkVerdicts` (`:416`), `/data/contributors/no-work-verdicts.json` | Survives (#3987/#3997) — "persisted in the same PVC-backed ledger dir as the cooldowns so a pod restart does not forget the verdict" | **fixed** |
| 31 | Task leases — the server-authoritative record of what was issued to whom | `leases`, persisted to `/data/contributors/task-leases.json` (0600) | Survives ([#5681](https://github.com/hivecommons/hive/issues/5681)). Previously **all active leases went void**: a reconnecting relay could only re-adopt against an exact unexpired lease (the C4 fix), so after a roll no in-flight contributor task could resume — the agent was interrupted mid-turn and handed the identical issue back seconds later. The durability cost was accepted as a security posture, but the posture never required volatility: the restored record is one the *server* wrote, matched exactly as before, so nothing is rebuilt from client-supplied fields. Expired records are dropped at load rather than restored | **fixed** |
| 32 | Assignment generation counter (`taskGen`) — fencing tokens | `:341`, `atomic.Uint64`, in-memory; high-water mark re-derived from row 31 at boot | Restarts at zero, then `loadLeases` advances it past every restored lease's generation ([#5681](https://github.com/hivecommons/hive/issues/5681)). This is what residual 2 below warned about: making row 31 durable without this would let a post-roll assignment mint a generation that ALIASES a restored one, and the #2568 Gate would accept a pre-roll straggler against a brand-new task. The fence no longer depends on two structures dying together | **fixed** (re-derived, not persisted) |
| 33 | Per-tier rate-limit ledger (`assignmentTimes`, #2436/#2566) | `:435`, in-memory only | **Every contributor's rolling per-hour/per-day assignment count resets to zero.** The tier limits an operator set are silently un-enforced for up to a day's window after each roll — same "admin-visible number is inert" class that #2566 fixed, reintroduced at roll frequency | **volatile** |
| 34 | Live connections + `currentTask` + pending-auth counter | `connections` / `ContributorConnection.currentTask` (`:73`), `pendingConns` | Connection-scoped by nature; relays reconnect with backoff | **benign** (given row 31) |
| 35 | Activity feed (50 entries) + SSE fan-out registry | `activity` (`:351`), `contribute_sse.go` | Operations view starts empty | **benign** |
| 36 | Yank self-exclusions | `yankExclusions` (`:450`) | A just-yanked clanker can be re-handed the yanked issue after a roll | **volatile** (small) |
| 37 | Relay-side task state: `currentTask`, `cliReady`, seq, token expiry, headless status file | `contributor-relay.sh:197`; `/tmp/contributor-headless-status.json` | Dies with the relay process/container; the hub's lease TTL (`wsTaskTimeout`, 30 min) is the backstop that reclaims the task | **volatile** |
| 38 | Duplicate-PR claim ledger | `github.ClaimLedgerPath` = `/data/pr-claims.json` (`src/pkg/github/prclaims.go:53`) | Survives (#3768 lineage); feeds both agent-side filtering and contribute admission (`contribute_admission.go`) | **fixed** |

**Count: 38 rows.** The pattern across the "fixed" rows is uniform and worth
stating because it is the RFC's whole argument: every one was in-process state
whose loss was discovered as a production incident, and every fix was either
(a) write it to the PVC, or (b) re-derive it from something that already was.
The conversation-as-state model is the claim that rows 16, 17, 31, 32 and 37 —
the ones that are *not* fixable by pattern (a) or (b) because the state is a
suspended process, not a value — need a third pattern.

## 3. The durable stores that already exist

The maintainer response cautions that the fleet "already has four
durable-state stores that grew this way" and the RFC's envelope "should
consolidate or at least map onto these, not become store number five." The
verified list is actually longer, which sharpens the caution:

| Store | Path | Owner / write path |
|-------|------|--------------------|
| Hub registry | `/data/hub-registry.json` (`server.go:41`) | Hub; every mutation under `s.mu`, coalesced through `saveCh` → `saveLoop` (`server.go:3453`) |
| SaaS hive meta | `/data/saas/hives/` per-hive JSON (`saas_provision.go:24`) | Hub; `listSaaSHives`/save on provisioning and admin actions. Holds `TrackedChannel` — the persisted fact #3771 re-derives delivery from |
| Spoke runtime state | `/data/hive-state.json` (`cmd/hive/main.go`, `persistState`; shape `src/pkg/snapshot/state.go`) | Spoke; written once per eval cycle + shutdown, replayed by `state_restore.go`. Stale after 7 days (`maxStateAge`) |
| Contribute ledger dir | `/data/contributors/*.json` (§2.3 rows 27–30, plus contributor profiles) | Spoke dashboard; each map saved atomically (tmp+rename) on mutation |
| Upgrade kill-switch | `/data/saas/upgrade-pause.json` (`upgrade_pause.go:41`) | Hub; lazily loaded, consulted on every heartbeat and reconcile cycle |
| Audit + prompt logs | `/data/audit.jsonl` (`dashboard/audit.go:17`), `/data/prompt-history.jsonl` | Spoke dashboard; append-only JSONL, lumberjack-rotated |
| PR-claim ledger | `/data/pr-claims.json` (`prclaims.go:53`) | Spoke; shared by governor dedupe and contribute admission |
| Bead stores | `/data/beads/<agent>/` (`src/pkg/beads`) | Per-agent work-item store |
| Hub leaf stores | timeline, journey, alert-acks, banners, revocations, reach-history, key generations (§2.1) | Hub; each its own file, own mutex, atomic writes |

Three properties recur, and any RFC envelope that joins this list must keep
them: **single writer** (every file has exactly one owning process and lock),
**atomic replace** (tmp+rename everywhere; a crash mid-write never leaves a
torn file), and **fail-open loads with explicit staleness** (`maxStateAge`,
banner pruning, streak expiry on load — old state is discarded, not trusted).
The counter-lesson is also on record: `hub-generations.json` deliberately
fails *closed* on an unreadable file because re-deriving from a default would
re-install superseded key material (`master-key-rotation.md`). State that
changes behavior when lost must decide, per store, which failure mode is the
safe one — a decision the envelope design cannot skip.

And the caution stands: nine stores grown one incident at a time is the
argument *for* mapping the envelope onto existing stores (the contribute
ledger dir is the natural home for turn envelopes of contribute tasks) and
*against* minting `/data/turn-envelopes.json` as store number ten.

## 4. Classification for RFC #4002

### 4.1 Absorbed: turn-scoped state the conversation-as-state model owns

Rows 16, 31, 32, 34, 36, 37 — plus the verdict/cooldown rows (27–30) at the
boundary. The maintainer response's minimal envelope — *task ref +
conversation + verdict + cooldown clocks* — falls directly out of this table:

- **task ref**: what `leases` + `currentTask` + generation fencing express
  today across three in-memory structures whose safety depends on dying
  together (rows 31/32/34). In the turn model the task ref is *in* the
  envelope, the claim machinery stays authoritative for *who may hold it*, and
  a re-entering process proves possession of the envelope instead of a live
  socket.
- **conversation**: what the tmux pane and the backend transcript are today
  (rows 16/17). Absorbed only on the headless path, where hive constructs the
  prompt and owns the transcript (§1.4).
- **verdict**: already structured and already durable (row 30) — #3997 built
  the RFC's turn-return shape without calling it that.
- **cooldown clocks**: rows 27–29 stay in the contribute ledger, but the
  envelope must *reference* them, because a re-entered turn's admission
  decision depends on them.
- The **ops journal** — the piece nothing in this inventory provides — is what
  makes row 16's side-effect problem tractable: a turn that died after opening
  a PR must find that fact in its own journaled history on re-entry, not
  discover it by opening a second PR. That is stage 2's kill-mid-turn replay
  test, and no current store records it.

Row 33 (`assignmentTimes`) is a judgment call: it is not turn state, but it is
the same "enforcement that silently resets on a roll" class. It should either
ride the contribute ledger like rows 27–30 or be explicitly documented as
best-effort; today it is neither.

### 4.2 Infrastructure state: stays exactly where it is

Rows 1–15, 18–20, 23, 26, 38 and every store in §3. Registry, hive meta, key
material and generations, channel/upgrade delivery, governor cadence and
budget, breaker, heartbeat liveness, config overlays, claim dedupe. None of
this is conversation; all of it already has a durability story (some of them
paid for in incidents). The RFC should not touch these — and conversely, the
envelope should not quietly absorb any of them, because their writers, locks
and failure modes (fail-open vs fail-closed) were chosen per store.

### 4.3 Backend-owned: cannot be externalized without headless turns

Rows 17, 21, 24 and the backend-format transcript files of §1.2. For hosted
tmux agents, the conversation belongs to the CLI: hive can read the bytes
(token scanners do) but cannot construct, checkpoint, or re-enter the session,
and every observation of it is a regex over pane text (#4026 being the
freshest reminder of what that costs). These rows are why the prototype target
is a **contribute headless turn** and not a hosted agent: on the headless path
the pane-ownership problem does not exist, so the spike measures the turn
model, not the scraping. Backend-native resume stays what the maintainer
response called it — a per-backend optimization to evaluate after the
envelope survives the replay test.

## 5. Residuals and open questions

Honest gaps in the current state of things, and in this inventory:

1. **The persist window.** `/data/hive-state.json` is written once per eval
   cycle and on clean shutdown. A SIGKILL between ticks loses up to one
   cycle's worth of pause/override/LastKick mutations. Nobody has been burned
   hard enough to make it write-through; the RFC's journal, if it lands,
   should not inherit this cadence.
2. ~~**Row 32's coupling is undocumented elsewhere.**~~ **Resolved by
   [#5681](https://github.com/hivecommons/hive/issues/5681).** The warning was
   accurate and it was load-bearing: leases were made to survive restarts, and
   the fence would have broken silently had `taskGen` not been re-derived from
   the restored leases in the same change. The coupling is now stated in
   `loadLeases` itself and pinned by a test
   (`TestLeaseRestart_GenerationAdvancesPastRestoredLeases`), so it is no longer
   carried only by this document.
3. **Rate limits reset on every roll** (row 33). Known now; not yet an
   incident; cheap to fix inside the existing ledger dir.
4. **Webhook loss** (row 3) has no re-derivation path. It predates this
   effort and is unrelated to the turn model, but it is the one hub row where
   real external input is dropped with no retry on either side.
5. **This inventory is a snapshot.** It was compiled by reading v5 at one
   commit, seeded by the incident history and a sweep of `sync.`-guarded
   fields in the three big processes. The sweep was systematic but not
   mechanical; a lint that flags new in-memory maps in `HubServer` /
   `Manager` / `ContributeWSHub` without a durability comment would keep it
   from rotting. Line numbers cited will drift; symbols are the stable
   reference.
