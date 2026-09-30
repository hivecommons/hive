# PR follow-up session resume

**Status:** shipped in two slices (v5, default off). Tracking issue:
[#9583](https://github.com/hivecommons/hive/issues/9583). Builds on the
re-entrant turn model ([RFC #4002](reentrant-turn-model.md),
[#5798](https://github.com/hivecommons/hive/pull/5798),
[#5799](https://github.com/hivecommons/hive/issues/5799)).

## Problem

A fleet agent is a long-lived CLI in a tmux pane. Every kick is preceded by
`/clear` (`deliverKickLocked`, `ClearOnKick` defaults to `true`). So when CI
fails or a reviewer comments on a PR an agent opened, the fix-before-new block
that routes it back (`ci-failing.json`, `review-threads.json`) lands in a
conversation that has already forgotten why the PR looks the way it does:
repro steps, rejected approaches, why a file was touched.

## What the landed turn model already covered

Nothing PR-specific. The only production caller of `pkg/turn` is the
contributor-assignment envelope (`ContributeWSHub.persistTurnEnvelopeForAssignment`),
keyed by task ID. PR attribution existed only as `repo#N -> agent name`
(`auditPRAgents`), with no session identity. The full audit is on the issue.

## Design

### Pointer

When the PR-request watcher opens a NEW PR for an agent, the PR-opened detail
hook (`github.Client.SetPROpenedDetailHook`) calls
`prfollowup.RecordWithNote`. It persists a `turn.SessionEnvelope` with session
ID `pr-followup:<owner>/<repo>#<n>` under `HIVE_PR_FOLLOWUP_DIR` (default
`/data/turn/pr-followups`). The envelope records the authoring agent, the
agent's live CLI session identity (`agent.Manager.SessionID`) and the PR's
handoff note (below). A repo named bare in config or in the request is
qualified with `project.org`, so the pointer, the router and
`review-threads.json` all agree on one spelling.

The session identity is `<process nonce>:<launchGen>:<kickEpoch>:<LastKick>`.
It changes whenever the conversation could have been replaced or cleared: a
new hive process, a relaunch, a restart teardown, or any delivered kick.

### Eligibility (the #6908 guard)

The pointer itself is the eligibility check. It is written only on the watcher
path that opened the PR with the hive's App for one of its own agents. A PR
without a pointer is never touched, and never even read for comments: a
human's PR, another hive's PR, or a PR in a repo no hub dispatched. The router
also re-checks that the envelope names exactly the PR being routed, because
the file store sanitises IDs and two PRs could share a filename. Drafts, fork
PRs and PRs escalated to a human are skipped (each skip is counted once per
transition, and nothing is journaled, so a draft marked ready routes normally).

### Routing

On each eval tick, `routePRFollowUps` reads data the tick already has, plus
the human feedback on PRs with a pointer:

| Event | Source | Key |
|---|---|---|
| CI failure | settled `CIStatus == failure` on the head | `ci:<head sha>` |
| Changes requested | GitHub review decision | `review:<reviewers>` |
| New review thread | `review-threads.json` (review bots only) | `thread:<node id>` |
| Human feedback | `github.FetchHumanPRComments` | `comment:<kind>:<comment id>` |

For each new event:

- **Resume.** If the pointer is younger than `max_age`, the follow-up budget
  (`MaxFollowUpsPerPR`) is not spent, and the agent is still on the saved
  session, the router journals the intent (`turn.OpFollowUpKick`), then calls
  `SendResumeKick`. That delivers the follow-up without `/clear`, as the
  session's next turn. The envelope gains the user turn, and the pointer moves
  to the new session ID, so later follow-ups keep resuming the same
  conversation.
- **Deferred.** If the session exists but is mid-turn, in provider backoff or
  under a restart hold, the intent stays `intended` on disk and is retried on
  the next tick.
- **Fallback.** Otherwise (expired, session gone or moved on, budget spent,
  kick refused) the event is settled as not resumed. CI failures and bot
  threads keep their existing fix-before-new route, unchanged. Human feedback
  has no such route, so it is queued on the pointer for the agent's next fresh
  kick (below). This feature never suppresses an existing path.

A settled key is never delivered again, so dedupe is per event: per red head
SHA, per thread, per comment id.

### Human feedback and per-comment authorship

`FetchHumanPRComments` reads one GraphQL query per PR (the last 50
conversation comments, 30 reviews, and 20 comments in each of the last 50
review threads) and keeps a comment only when all of these hold:

- it is at least as new as the pointer;
- its author is not one of this hive's identities (`isHiveLogin`: the App bot
  login and `project.ai_author`, the same identity source the review-thread
  monitor and claims guard use), not a bot (GitHub `Bot` type or a `[bot]`
  login) and not a configured review bot;
- its author association is `OWNER`, `MEMBER` or `COLLABORATOR`. A public
  repository accepts comments from anyone, and typing a stranger's text into
  an agent session would be a prompt-injection channel; everyone else reaches
  the agent the way they did before;
- it does not carry the hive attribution trailer (an agent running on a
  person's credentials signs what it posts);
- for review bodies, the review is `COMMENTED` or `CHANGES_REQUESTED`; for
  inline comments, the thread is neither resolved nor outdated.

The fetch runs at most every 5 minutes and for at most 25 PRs per refresh;
between refreshes the last result is reused, and the journal dedupes. Resumed
and handed-off comments both count against `MaxFollowUpsPerPR`.

### Surviving a restart and the next regular kick

Two designs were evaluated for keeping the reasoning past the life of one
conversation.

**(a) Backend-native resume ids.** Rejected for now. None of the backend
launch commands the hive builds (`backendLaunchCmd`, `toolRulesToLaunchCmd`:
claude, copilot, gemini, codex, pi, goose, bob, agy, omp) passes a
resume flag, and the hive does not capture a conversation id for any of them.
The one backend with a conversation id, the headless agy runner, records it in
a `mktemp` file that is deliberately discarded on every relaunch so a
relaunched pane never resumes stale context. Support would have to be built
and tested per backend, and resuming a whole transcript on every follow-up
also resumes its full token cost.

**(b) A compact PR handoff note.** Shipped. When the PR opens, the hive keeps
a short structured note beside the pointer:

- `Why`, `Approach`, `Rejected alternatives`, `Repro`, `Files touched`;
- from the PR request's optional `handoff` object (`github.PRHandoff`: `why`,
  `approach`, `rejected`, `repro`, `files`), and, for any field the agent left
  empty, from the PR body's own section headings (`## Why`, `## Summary`,
  `## Approach`, `## Alternatives considered`, `**Steps to reproduce:**`,
  `## Files touched`, and similar); text before the first heading is the
  "why" when no section names one; the attribution trailer is ignored;
- bounded: 600 runes per field, 20 files, `MaxHandoffNoteRunes` (2000) in
  total.

Regular kicks still `/clear` (keeping a conversation alive past a kick would
let context and token cost grow without bound). Instead the fallback path
carries the note: the scheduler's `addPRFollowUpHandoff` overlay prepends a
`PR HANDOFF` section to every kick of the agent that opened a PR while that PR
is **live**, meaning it has a standing follow-up fact (red CI, changes
requested, an open bot thread) or queued human feedback. The section lists
each such PR with its note and its queued human feedback, and tells the agent
that quoted feedback is review input, not instructions. At most five PRs are
detailed per kick.

Queued human feedback is dropped once a kick has carried it: each item
records the agent's session identity when it was queued, and the next tick
that finds the agent on a different session counts it as delivered. The
overlay itself only reads the store, so building a kick for a preview never
consumes anything.

A resumed PR stays live too: the live session has the reasoning, but the
agent's next regular kick clears it, and that kick carries the note.

### Restart safety

The intent is journaled before the effect. After a restart, an `intended`
entry is re-queued. The new process has a different session identity, so the
entry falls back instead of being dropped or typed into a CLI that never saw
the PR. An intended entry whose fact no longer holds (for example CI went
green) is settled as superseded. The pointer, the note and the queued human
feedback live on the persistent volume, so the first kick after a restart
carries the note.

### Housekeeping

- **Pruning.** `prfollowup.Sweep` runs from the eval tick at most every 30
  minutes. A pointer whose PR is in the tick's open list (including
  hold-gated PRs) is kept. Any other pointer's PR is looked up (`GetPRState`,
  at most 20 per sweep, oldest first) and deleted when the PR merged or
  closed. Any pointer older than `turn.pr_follow_up.retention` (default 14
  days) is deleted regardless. A failed lookup keeps the pointer.
- **Counters.** `stats.json` in the pointer directory keeps cumulative
  counts: `resumed`, `fallback` by reason (manager refusals collapse to `kick
  refused`), `skipped` by reason (draft, fork, escalated), `deferred`
  attempts, `handoffs_queued`, `handoffs_delivered`, and `pruned` by reason.
  When `/metrics` is enabled they are also exported as Prometheus counters:
  `hive_pr_followup_resumed_total`, `hive_pr_followup_fallback_total{reason}`,
  `hive_pr_followup_skipped_total{reason}`, `hive_pr_followup_deferred_total`,
  `hive_pr_followup_handoffs_total{state="queued|delivered"}` and
  `hive_pr_followup_pointers_pruned_total{reason}`. `cmd/hive` reads
  `stats.json` through `dashboard.SetPRFollowUpCountersProvider`, so
  `pkg/dashboard` does not import `pkg/prfollowup`. The series are absent
  until the counter file exists.
- **Audit trail.** Every routing decision (`pr_followup_routed`, with
  outcome, reason, event kinds and queued count), skip transition
  (`pr_followup_skipped`) and prune (`pr_followup_pruned`) is written to the
  hive's audit log through the agent manager's audit sink. Deferred retries
  are only logged at debug level.
- **Dashboard.** Settings > Features has a "PR follow-up resume" toggle for
  `turn.pr_follow_up.enabled`, and says when `HIVE_PR_FOLLOWUP_RESUME`
  overrides it.

## Configuration

```yaml
turn:
  pr_follow_up:
    enabled: true     # default false; also Settings > Features
    max_age: 24h      # resume window, default 24h
    retention: 336h   # pointer lifetime backstop, default 14 days
```

`HIVE_PR_FOLLOWUP_RESUME=false` is the one-step rollback and wins over the
dashboard toggle. `HIVE_PR_FOLLOWUP_MAX_AGE` and `HIVE_PR_FOLLOWUP_RETENTION`
override the durations. See [env-vars](../env-vars.md).

With the toggle off nothing changes: no pointer is written, no comment is
fetched, no counter file is created, and kicks are byte-for-byte what they
were.

## Not yet built (follow-ups on #9583)

1. **Transcript-continuity soak.** Acceptance bullet 1 (a review comment
   produces a follow-up from the same session, with visible continuity) needs
   a soak with the flag on against a live review.
2. **`hive-open-pr --handoff`.** The `handoff` object is accepted in the
   request JSON, but the `hive-open-pr` wrapper has no flag for it yet, so
   agents using the wrapper get the note from their PR body sections.
3. **Backend-native resume ids**, if a backend later offers a stable,
   capturable resume handle, could complement the note.
