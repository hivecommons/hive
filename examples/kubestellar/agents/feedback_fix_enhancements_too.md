# feedback: fix enhancements too — never filter the scan to bugs

> Example memory file for the scanner agent policy ([scanner.md](scanner.md)).

## The rule

The scan covers **every open issue kind**: bugs, enhancements, features,
documentation, help-wanted, questions that hide an actionable defect. Do not
add a `label:kind/bug` (or equivalent) filter to any scan query, and do not
skip an issue because it "is only an enhancement".

## Why this rule exists

It was added after the scanner was observed quietly narrowing its queries to
bug labels. The visible symptom was a healthy-looking dashboard over a rotting
backlog: bugs drained on SLA while enhancements and docs issues aged for
weeks, and reporters of non-bug issues correctly concluded nobody was reading
them.

- **Prioritization already handles the difference.** The supervisor's sort
  (older → critical → easy; see scanner.md) puts bugs ahead of features
  *within the queue*. Filtering them out of the queue entirely is not
  prioritization, it is abandonment.
- **Labels lie.** A large fraction of real defects arrive labelled
  `enhancement` ("it would be nice if the dashboard didn't crash when…") or
  unlabelled. A kind filter drops those on the floor.
- **Small enhancements are the cheapest wins.** The "easy over hard" tier
  exists precisely because a ten-line docs or UX fix merged today beats a
  perfect bug fix next week. Most of those live outside `kind/bug`.

## What "fix what you can" means per kind

| Kind | Scanner action |
|---|---|
| Bug | Dispatch a fix agent as normal. |
| Enhancement, small | Dispatch — same worktree/PR flow as a bug. |
| Enhancement, large / feature / architecture | Do not silently skip: leave a triage comment, label it, and file a bead with the right actor so it enters the right lane (these usually need an RFC before code). |
| Documentation | Dispatch — docs PRs are first-class work. |
| Help-wanted / question | Answer if the answer is known; convert to a concrete issue if a defect surfaces. |

The only issues the scanner does not touch are the explicit hard stops
(hold-labelled, ADOPTERS.md) — and those are excluded by rule, not by kind.
