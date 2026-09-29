# PR follow-up session resume

**Status:** partly shipped (v5, default off). Tracking issue:
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

When the PR-request watcher opens a NEW PR for an agent, the PR-opened hook
calls `prfollowup.Record`. It persists a `turn.SessionEnvelope` with session ID
`pr-followup:<repo>#<n>` under `HIVE_PR_FOLLOWUP_DIR`
(default `/data/turn/pr-followups`). The envelope records the authoring agent
and the agent's live CLI session identity (`agent.Manager.SessionID`).

The session identity is `<process nonce>:<launchGen>:<kickEpoch>:<LastKick>`.
It changes whenever the conversation could have been replaced or cleared: a
new hive process, a relaunch, a restart teardown, or any delivered kick.

### Eligibility (the #6908 guard)

The pointer itself is the eligibility check. It is written only on the watcher
path that opened the PR with the hive's App for one of its own agents. A PR
without a pointer is never touched: a human's PR, another hive's PR, or a PR
in a repo no hub dispatched. The router also re-checks that the envelope names
exactly the PR being routed, because the file store sanitises IDs and two PRs
could share a filename. Drafts, fork PRs and PRs escalated to a human are
skipped.

### Routing

On each eval tick, `routePRFollowUps` reads only data the tick already has:

| Event | Source | Key |
|---|---|---|
| CI failure | settled `CIStatus == failure` on the head | `ci:<head sha>` |
| Changes requested | GitHub review decision | `review:<reviewers>` |
| New review thread | `review-threads.json` (non-hive authors only) | `thread:<node id>` |

For each new event:

- **Resume.** If the pointer is younger than `max_age`, the follow-up cap
  (`MaxFollowUpsPerPR`) is not reached, and the agent is still on the saved
  session, the router journals the intent (`turn.OpFollowUpKick`), then calls
  `SendResumeKick`. That delivers the follow-up without `/clear`, as the
  session's next turn. The envelope gains the user turn, and the pointer moves
  to the new session ID, so later follow-ups keep resuming the same
  conversation.
- **Deferred.** If the session exists but is mid-turn, in provider backoff or
  under a restart hold, the intent stays `intended` on disk and is retried on
  the next tick.
- **Fallback.** Otherwise (expired, session gone or moved on, cap reached,
  kick refused) the event is settled as not resumed. The existing
  fix-before-new path handles it exactly as before. This feature never
  suppresses that path.

A settled key is never delivered again.

### Restart safety

The intent is journaled before the effect. After a restart, an `intended`
entry is re-queued. The new process has a different session identity, so the
entry falls back instead of being dropped or typed into a CLI that never saw
the PR. An intended entry whose fact no longer holds (for example CI went
green) is settled as superseded.

## Configuration

```yaml
turn:
  pr_follow_up:
    enabled: true   # default false
    max_age: 24h    # default 24h
```

`HIVE_PR_FOLLOWUP_RESUME=false` is the one-step rollback.
`HIVE_PR_FOLLOWUP_MAX_AGE` overrides the window. See [env-vars](../env-vars.md).

## Not yet built (follow-ups on #9583)

1. **Resume across a pod restart.** This needs a backend-native resume handle
   (for example a CLI `--resume <id>`) captured at PR-open time. Today a
   restart falls back.
2. **Human review comments and PR conversation comments.** They need
   per-comment authorship to exclude the hive's own replies. Today only
   review-bot threads and changes-requested reviews are routed.
3. **Keeping the session across the next cadence kick.** The resume window
   ends when the agent's next kick `/clear`s the conversation. A later slice
   could skip that clear while the agent has open follow-ups.
4. **Pointer pruning** for merged or closed PRs, and a dashboard toggle.
