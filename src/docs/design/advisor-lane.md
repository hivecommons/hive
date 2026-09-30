# The advisor lane

**Status: phase 1 implemented** (roles, records, and the Claude Code adapter —
[#9722](https://github.com/hivecommons/hive/issues/9722)); phases 2–4 are design
only. It belongs to the v6 line
([#7563](https://github.com/hivecommons/hive/issues/7563)) and, per that line's policy, lands on
the `v6` branch only. Tracked by [#9638](https://github.com/hivecommons/hive/issues/9638).

Read the trajectory-review section of the governor configuration first
(`governor.trajectory` in [operator-reference.md](../operator-reference.md)). This page is
written as the turn-synchronous complement to that lane and does not repeat its reasoning.

## What this is

A second, independently chosen model that reads each turn a hub-launched agent finishes and can
object **before the agent takes its next step**. Hive owns the advisor: the model, the standing
instructions, the decision, and the record of what it said. Each supported agent backend gets a
thin adapter that registers a turn-end hook at launch and relays the verdict back into the
session in that backend's own dialect. Four backends are in scope — Claude Code, OMP, Copilot
CLI and Codex CLI — because those four expose a synchronous turn boundary today.

Every advisor opinion is a first-class hive record: which agent, which turn, what severity, what
was said, whether the agent acted on it, what it cost. Records surface on the agent's dashboard
page beside trajectory verdicts, over the dashboard REST API, and through the operator-facing
admin MCP ([admin-mcp.md](admin-mcp.md)).

Alongside, a small named **model-roles** map lets an operator say "the advisor model" or "the
cheap reader" once in `hive.yaml` and reference it by name wherever hive asks for a model,
instead of pinning a model in each of the places that ask.

## Why

Three observations, in the order they were made.

**The behaviour exists, and it works, but only inside one harness.** OMP ships an `/advisor`
mode: a reviewer model paired to an `advisor` role reads every turn the main model takes and
injects a note — a quiet aside, a concern, or a blocker — into the transcript, with a bounded
catch-up wait so the main model does not run ahead of the review. Operators who run it report
that it catches real mistakes, and that it catches them at the turn boundary, before the main
model proceeds, rather than at pull-request review. An operator running a hive today can only
get that by choosing OMP as the backend, and even then hive sees nothing: no record, no cost, no
fleet view.

**Hive's closest thing is not turn-synchronous and never talks back.** The trajectory review
lane is a periodic, second-model check that reads each running agent's recent transcript on the
governor tick, scores whether the sequence of actions still serves the agent's assigned intent,
and pauses or alerts on divergence. It is the right defence against goal drift and it stays. But
it runs on a timer rather than at the turn, it emits a verdict rather than advice, and it has no
channel into the agent's session. An advisor is a different instrument: it speaks to the agent,
at the moment the agent is about to act on a mistake.

**Hive has grown four ways to say "use the cheap model".** Each agent pins its own backend,
model and reasoning effort. Beside that, `review_models` names a per-agent reviewer pool for
pull-request review, `governor.trajectory.model` names the trajectory reviewer,
`retro.analysis_model` names the retro analyser, and `intent.alignment_model` names the intent
checker. Four fields, four vocabularies, no way to change "the cheap model" in one place. The
advisor lane would be a fifth. A roles map is the point at which that stops.

## Shape: one evaluator, thin adapters

The advisor evaluator is hive code. Given a transcript reference and the agent's identity it
resolves the advisor model, builds the review prompt from the standing instructions and the
turn, calls the model, classifies the answer into a severity, records the review, and returns a
verdict. It is the same on every backend.

What differs per backend is only **where the turn-end hook is registered** and **the JSON
dialect the hook answers in**. Each backend already exposes a synchronous turn-end hook whose
handler can hold the turn and hand text back to the model. Verified 2026-09-29:

| Backend | Turn-end hook | What the hook may return | How hive supplies it at launch |
|---|---|---|---|
| Claude Code | `Stop` hook | block, with a reason the model sees | in the compact settings JSON hive already passes as a command-line argument |
| OMP | `session_stop` extension event, awaited before the agent settles | block with a reason, or continue with additional context | `--extension <file>` for the hook; `--config <overlay>` for roles, layered above the operator's own config |
| Copilot CLI | `agentStop` | `decision: block` with a reason that becomes the forced next prompt; the CLI force-ends the turn after 8 consecutive blocks | hooks directory under `COPILOT_HOME`, which hive points at a directory it owns; there is no launch flag |
| Codex CLI | `Stop` hook — experimental, off by default | `decision: block` with a reason as the continuation prompt; `continue: false` for a hard stop | `hooks.json` in the per-agent home hive provisions, plus `-c features.hooks=true` at launch |

The shape is the same everywhere: the hook receives a transcript reference, and answers with a
block decision and a reason. So hive ships **one small hook command** that every backend
invokes at its turn boundary. It reads the transcript reference the backend provides, asks the
hub for the verdict, and prints the answer in the calling backend's format. Keeping the
backend-specific part to formatting is what makes the fourth adapter cheap once the first works.

Claude Code and OMP are the reference adapters. Copilot CLI and Codex CLI follow the same shape
and carry the known fragilities recorded under *Risks*.

### Projection at launch, never a config-file edit

Hive never edits a backend's user-level configuration file to enable the advisor. Whatever a
backend needs is rendered by hive at launch, into something hive owns, the way hive already
passes Claude a settings argument and OMP an approval-mode flag. Two backend facts make this the
only sane choice: OMP rewrites its own settings file and strips comments when it does, so a file
the operator maintains is not a file hive may write; and Copilot accepts repository-level hooks
from `.github/hooks/`, which would mean committing hive's hook into the target repository, so
that path is ruled out and the hive-owned hooks directory is the one used.

Hub-launched agents already get a per-agent home directory
(`/data/home/agents/<name>`, `src/pkg/agent/interactive_home.go`), so the Copilot and Codex
hook files have a place to live that hive controls without touching anything the operator owns.

### Roles

A `model_roles` map in `hive.yaml`: a small set of names, each naming a backend, a model and a
reasoning effort. An agent's `model`, the advisor's model, and — in a follow-up — the four
existing second-model fields may name a role as `@<role>` instead of a literal model. Resolution
goes through the per-backend model-name normalisation `config/backends.conf` already performs,
so a role that says "Claude Haiku 4.5" renders correctly for whichever backend consumes it.

Roles are deliberately minimal here. The map exists, agents and the advisor lane may reference
it, and it is editable from the dashboard alongside the advisor settings. Migrating
`review_models`, the trajectory model, the retro model and the intent model onto it is a
separate change with its own compatibility argument, recorded below as deliberately absent.

Where a backend exposes a spawn-time model choice for its subagents — OMP's
`before_subagent_spawn` event returns a model — resolving that through roles is the natural
next use of the map. It is not part of this work.

## What it inherits, and what it does not

**Reviewer endpoint resolution and fail-open — inherited from the trajectory lane.** The
trajectory lane already resolves an OpenAI-compatible endpoint and key for a second model,
independent of the agents' own inference, falls back to the governor gateway, fails open when
the reviewer is unreachable, and carries a per-agent exemption list. The advisor lane reuses
that resolution and posture. It also reuses the lane's stateless one-request-per-review shape:
the advisor makes one call per turn and reads back a strict answer; it does not drive an
interactive CLI of its own.

**The guard invariant — inherited, and the thing this design must not bend.** The v6 line's
non-negotiable is that every non-dashboard surface routes through the same authorisation and
safety machinery. This design adds no surface an operator reaches, but it adds two things that
must sit inside existing gates:

- Advisor records, the API that lists them, the admin MCP tool over them, and the dashboard
  editing of advisor settings and roles all sit behind the dashboard's existing authentication
  and permission checks. No new way in, no new authority.
- The hook runs inside the agent's session and has to reach the hub for a verdict. Hub-launched
  agents already have a path back to the hub. The hook uses that path and **carries no new
  credential**: enabling the advisor places no secret, token or key in the agent's environment
  or home that was not there before. This is the same line [admin-mcp.md](admin-mcp.md) draws —
  "it adds no authority that does not already exist" — applied to an agent-side component.

**Backend confinement — untouched.** Each backend launches today with a sandbox or a deny-list,
recorded in [backend-support-tiers.md](../backend-support-tiers.md) and
[sandbox-isolation.md](../sandbox-isolation.md). Enabling the advisor changes none of it. The
hook is a process the backend spawns inside its existing confinement, and it needs nothing the
confinement forbids.

**Contributor-relay agents — not covered, by decision.** Contributors run their own CLIs on
their own machines and configure them as they see fit, including any advisor their harness
offers natively. The lane is a hub feature for hub-launched agents.

## Severity, delivery, and whether it was heeded

Every review yields one of three severities, borrowing OMP's vocabulary so an operator moving
between OMP's native advisor and hive's sees the same terms:

- **aside** — recorded, never delivered into the session. The agent's next turn proceeds
  untouched.
- **concern** — delivered into the session so the agent's next step is taken with the objection
  in front of it. The agent may still finish its turn.
- **blocker** — delivered, and the agent may not end its turn until it has responded to the
  objection.

Concerns and blockers are *interrupting* deliveries; an aside is *non-interrupting*. The
delivery mode is part of the record, so an operator can tell an advisor that talks a lot from one
that interrupts a lot.

A blocker can, in principle, hold an agent turn after turn. Two bounds prevent that. Hive's own
**consecutive-block limit** is operator-set, fleet-wide and per agent; once reached for an agent,
further blockers are downgraded to asides and a record says so. And the limit must stay below
the smallest bound a backend imposes on forced continuations — Copilot force-ends after eight —
so that hive's bound is the one that fires and the backend's is never relied on. A value at or
above a backend's bound is rejected at configuration time with a message naming it.

**Heeded.** Each interjection's record is later marked *yes*, *no* or *undetermined* for whether
the agent's following turn acted on it. This is a judgement, and the simplest workable judge is
the advisor itself on the following turn, answering "was my last note acted on". *Undetermined*
is an honest answer and will be common early. The point of the marker is the fleet-level
question it lets an operator ask — is this advisor earning its cost on this agent — not
per-turn precision.

## Records, and where they surface

Every review produces exactly one record, including a review that was skipped. A record carries
the agent, the turn it reviewed, the advisor model used, the severity, the text delivered,
whether an interjection was made, the delivery mode, the tokens and cost consumed, and — when
the review was skipped — why (timeout, unreachable, budget exhausted, unusable answer).

Records are shaped so that trajectory verdicts and advisor interjections can be listed together:
two second-opinion instruments, one timeline per agent. They surface in three places, all behind
the dashboard's existing checks:

- **The agent's dashboard page**, in the same area as trajectory verdicts, newest first, with
  severity and text visible without further clicks. Aggregated advisor spend appears beside the
  agent's own spend over the same time ranges the existing spend reporting offers.
- **The dashboard REST API**, filterable by agent and time range, so `hivectl`, the TUI and any
  client can read them.
- **The admin MCP**, as a read tool that answers "what did the advisor flag" for an agent or the
  fleet over a window. It is built on the REST surface above and adds no authority beyond it.

## Configuration

One fleet-wide `advisor` block in `hive.yaml` — enabled, model or `@role`, standing
instructions, daily budget, timeout, consecutive-block limit — and a per-agent override that may
change any part of it or opt the agent out, in the spirit of the trajectory lane's
`exempt_agents`. The roles map sits beside it.

All of it is editable from the dashboard's Settings area, which shows the effective value per
agent, and all of it is expressible in `hive.yaml`, so a fleet can be configured without the
dashboard. A change from either place takes effect for an agent by its next launch. Hive
projects the settings into the backend at launch; the operator never edits a backend's own
configuration.

Enabling the advisor for an agent on a backend outside the four is not an error and not a
silent no-op: the agent launches normally, and the dashboard and API report the advisor as *not
active on this backend* for it. The backend support documentation carries an advisor entry for
every backend hive knows, so the support set is readable from one page.

## Cost and failure

**Fail open, always.** If the advisor model cannot be reached, times out, or returns an
unusable answer, the agent proceeds unblocked and the review is recorded as skipped with the
reason. The advisor timeout is operator-set, fleet-wide and per agent, and bounds the delay a
turn can incur. An advisor outage degrades toward "catches less", never toward "agents stall".

**A daily budget per agent.** Operators cap the advisor's token spend per agent per day, on the
same day boundary hive's existing spend reporting uses. When the cap is reached the advisor stops
reviewing that agent until the boundary passes, and a record states that the budget was
exhausted.

**No runaway loops.** The consecutive-block limit above.

## Deliberately absent, and why

Each of these was considered and left out for the reason given, so that a later reader can judge
the reasoning rather than the bare exclusion.

- **Contributor-relay agents.** They run on contributors' machines under contributors'
  configuration. Hive projecting hook files and overlays into a contributor's environment is a
  different trust conversation, and contributors already have the native option.
- **Backends without a turn boundary** (goose, agy, bob, pi, aider, kilo, litellm, opencode,
  muse, gemini). Without a synchronous hook the only delivery is an asynchronous nudge that may
  land mid-turn, which throws away the "before the next step" property that makes the advisor
  worth having. An async mode is a possible follow-up; promising uniformity would be a lie.
- **Migrating the four existing second-model fields onto roles.** Each has its own
  compatibility story (the review pool's author-model exclusion in particular). The roles map is
  built here; the migration is its own change.
- **Multiple specialised advisors per agent**, as OMP's per-advisor watchdog configuration
  allows. One advisor per agent is enough to prove the lane; the record shape leaves room for an
  advisor identity.
- **The advisor acting.** It does not pause, restart, nudge through hive, run tools that change
  state, approve anything, or widen what the agent may do. Its outputs are text to the agent and
  a record. Pause-on-divergence remains the trajectory lane's job, and keeping the two
  instruments separate is what makes each one's records legible.
- **Replacing, disabling or harvesting OMP's native advisor.** An operator may keep using it on
  an OMP agent. Its own advisor transcripts (`__advisor*.jsonl` in the session directory) could
  be read into hive records later as a weaker-record mode; not here.
- **Subagent model routing through roles.** OMP's spawn event makes it easy there; Claude Code
  exposes a per-agent-definition model; Copilot and Codex have subagent events but no documented
  model override. It is the natural first follow-up for the roles map, not a v1 deliverable on
  any backend.
- **Feeding advisor records into the retro or intent lanes**, or acting automatically on a low
  heeded rate. The metrics are for operators to read first.

## Risks

- **Codex hooks are experimental** and off by default. Hive enables them per launch. If the
  hook shape changes upstream, the Codex adapter breaks first; it must not be load-bearing for
  the other three.
- **Copilot's eight-block cutoff** is a bound to stay under, not one to rely on. Hive's own
  limit fires first by construction.
- **The OMP extension is code hive ships.** It runs inside OMP's extension runtime and must be
  versioned with the OMP backend pin ([cli-pins.md](../cli-pins.md)), the way any other
  backend-specific wiring is.
- **Copilot has no launch flag for hooks.** The design leans on hive owning the agent's home
  directory, which it already does for the per-agent home layout and the Copilot token store.
  If that layout is ever made optional, the Copilot adapter needs a second look.
- **"Heeded" is a judgement.** Treating it as a fleet-level signal rather than a per-turn fact
  is the mitigation; the metric that depends on it (below) is deliberately coarse.

## Phases

1. **Roles, records, and the Claude Code adapter.** The `model_roles` map; the `advisor` block
   with fleet default and per-agent override; the evaluator on the trajectory lane's endpoint
   resolution; the record type and its REST listing; the hook command; the Claude Code `Stop`
   hook rendered into the settings argument. Dashboard agent page shows the records.
2. **OMP.** The `--config` overlay for roles and the `session_stop` extension for the hook.
3. **Copilot CLI and Codex CLI.** Hook files in the per-agent home; the Codex feature flag at
   launch; the consecutive-block limit validated against backend bounds.
4. **Operator surfaces.** Dashboard Settings editing of the advisor block and roles; aggregated
   advisor spend beside agent spend; the admin MCP read tool; the advisor entry in
   backend-support-tiers.

Each phase is independently useful: phase 1 alone gives an operator an advised Claude fleet with
records.

## Success, after delivery

- At least one in five interjections of concern severity or above is marked heeded over the
  first month. Lower, and the advisor model or instructions need revisiting; much higher, and
  the main model may be the wrong choice.
- Every advised agent on the four backends produces records, and no backend's review-skipped
  rate sits more than five percentage points above the lowest for the same advisor model.
- Advisor-attributable delay per turn stays under the configured timeout, and no agent is ever
  stalled by an advisor outage or budget exhaustion.
- Operators answer "what did the advisor flag today" from the dashboard or the admin MCP without
  opening a transcript.
- Pull requests from advised agents need fewer review-fix cycles than the same agents produced
  before the advisor was enabled, over the same month.

## Relationship to the trajectory lane and to OMP

The trajectory lane and the advisor lane are two instruments, not one. Trajectory answers "is
this agent still doing what it was asked", on a timer, with the power to stop it. The advisor
answers "was that last step a mistake", at the turn, with only the power to say so. They share
endpoint resolution, fail-open posture, and a record timeline, and nothing else.

OMP's native advisor is the proof that the idea works and the source of its vocabulary. Hive's
advisor is the same idea moved to where the fleet is, so that any backend with a turn boundary
gets it and the operator sees what it said.
