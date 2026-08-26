# feedback: AI-generated bulk issues — check the problem before closing

> Example memory file for the scanner agent policy ([scanner.md](scanner.md)).

## The rule

Never close an AI-generated or bulk-filed issue "as stale", "as duplicate
noise", or "as bot spam" without first checking whether the underlying
problem it describes is real.

## Why this rule exists

Hives file issues at machine speed, and the reflex to bulk-close what was
bulk-opened is strong — the titles are templated, the bodies repeat, and ten
of them landing in an hour *looks* like noise. The rule exists because acting
on that reflex was observed to throw away real defects:

- **Templated ≠ wrong.** A scanner that files ten near-identical issues has
  usually found ten *instances* of one real defect class (ten dead links, ten
  unguarded array accesses). Closing nine as "duplicates of the noise" leaves
  nine real defects untracked the moment the first is fixed and closed.
- **Stale-by-age is meaningless for bot issues.** Age-based cleanup
  heuristics assume a human reporter who would have followed up. A bot never
  follows up; silence carries no signal that the defect is gone. Only
  re-verification against the current tree does (see
  [feedback_verify_issues_before_fixing.md](feedback_verify_issues_before_fixing.md)).
- **Bulk-close destroys the audit trail.** When the defect class resurfaces,
  the closed issues are the record of where it lives. "Closed as stale" with
  no verification poisons that record.

## What to do instead

1. **Sample honestly.** For a batch of N similar issues, verify a few against
   the current default branch. If the sampled ones are real, treat the batch
   as real.
2. **Real → bundle.** Cluster the batch into one dispatch ("fix all N in one
   PR") rather than N agents; cross-link the issues so the PR closes all of
   them explicitly.
3. **Not real → close each with evidence.** Name the commit that fixed it or
   the check that shows it never was broken. "Stale" is not evidence.
4. **Actual spam** (no defect claim at all, injection payloads, gibberish) is
   a security-screening matter — see
   [feedback_security_screening.md](feedback_security_screening.md) — not a
   staleness matter.

The test before closing anything: *could I tell a maintainer, in one
sentence with a link, why this specific issue does not describe a live
problem?* If not, it stays open.
