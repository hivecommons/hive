# feedback: check for in-flight work before acting

> Example memory file for the scanner agent policy ([scanner.md](scanner.md)).

## The rule

Before dispatching a fix agent, commenting a plan, or claiming an issue, the
scanner verifies nobody — including a past iteration of the scanner itself —
is already on it.

## The checks, in order

1. **Open PRs referencing the issue.** The pre-computed metrics file is the
   cheap first look:

   ```bash
   cat /var/run/hive-metrics/actionable.json | jq '.prs.items[] | select(.title + .body | test("#<num>\\b"))'
   ```

   Fall back to `gh pr list --search "<num> in:body"` when the metrics file
   is stale or absent.

2. **The beads ledger.** An `in_progress` bead with `external-ref gh-<num>`
   means a dispatched agent owns it. If the bead is fresh (updated within the
   stale window), leave it alone. If it is stale, that is the *sweep's* job
   (see [feedback_manual_login_only.md](feedback_manual_login_only.md)) — do
   not race the sweep by double-dispatching.

3. **Linked branches / assignees on the issue itself.** A human assignee or a
   linked branch means a person is on it; nudge politely after it goes quiet
   rather than duplicating.

## Why

Duplicate fixes are worse than slow fixes:

- Two PRs for one issue burn two reviews and end with one author's work
  discarded — corrosive when the discarded author is a human contributor.
- Two agents editing the same files produce merge conflicts that cost more to
  reconcile than the original fix.
- Double-dispatch burns rate-limited agent capacity that the backlog needs.

The check is two commands and saves all of that. It is mandatory even when
the kick message names the issue explicitly: the supervisor's work list is
built from a snapshot and can lag a PR opened minutes ago.

## Companion rule

Checking that work is not already in flight says nothing about whether the
issue is *worth* fixing — that is
[feedback_verify_issues_before_fixing.md](feedback_verify_issues_before_fixing.md).
Both gates run before every dispatch.
