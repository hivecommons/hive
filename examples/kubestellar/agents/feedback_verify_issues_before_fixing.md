# feedback: verify an issue is real before fixing it

> Example memory file for the scanner agent policy ([scanner.md](scanner.md)).

## The rule

Before dispatching a fix agent, the scanner confirms the defect still exists
on the current default branch. "The issue says so" is not confirmation.

## Why

The backlog is full of issues that were true when filed and are not true now:

- **Already fixed incidentally.** A refactor or a related fix removed the
  defect, and nobody closed the issue. Oldest-first ordering makes this the
  *common* case, not the edge case — the oldest issues are the most likely to
  have been overtaken.
- **Environment-specific.** The reporter's setup (old version, local config,
  broken install) produced a symptom the codebase never had.
- **Misdiagnosed.** The symptom is real but the issue's claimed cause is
  wrong; a fix agent pointed at the claimed cause will "fix" healthy code.

Dispatching on a stale issue wastes an agent slot, produces a PR that changes
behaviour nobody asked to change, and — worst — can *introduce* a defect
while "fixing" one that no longer exists.

## What verification looks like

Proportional to the claim, on a fresh checkout of the default branch:

| Claim | Minimum verification |
|---|---|
| "X crashes / errors" | Reproduce the error, or find the faulty code path and confirm it is still present |
| "Docs say Y but code does Z" | Read the current doc *and* the current code; confirm both halves |
| "Link/file is missing" | Check the current tree, not the tree at filing time |
| "CI job fails" | Look at the latest runs, not the run linked in the issue |

If the defect no longer reproduces, close the issue with a comment naming the
commit or PR that resolved it (subject to the hold-label gate — never close a
held issue), instead of dispatching.

The verification result goes **into the dispatch prompt** when the issue is
real: a fix agent that starts from "confirmed on <sha>, failing at
<file:line>" converges far faster than one starting from the issue text.

## Companion rule

This gate is about whether the work is *needed*;
[feedback_scanner_check_existing.md](feedback_scanner_check_existing.md) is
about whether it is *already happening*. Run both before every dispatch.
