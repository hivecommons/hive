# Hive issue labels

Hive creates and maintains a small set of repository labels so operators can see why an issue is or is not being offered to agents.

| Label | Meaning | Queue effect |
| --- | --- | --- |
| `hive/already-done` | A verified already-done path or operator confirmation says the issue is resolved. This label remains in the default contributor skip set. | Withheld until a maintainer removes the label. |
| `hive/covered-by-pr` | Hive verified an open pull request that references or claims the issue. The PR is real API evidence, but not a resolution by itself. | Still actionable; shown on the repo card with a linked-PR badge. Hive removes the label when no qualifying open PR remains. |
| `hive/likely-done` | Hive verified a merged PR that references or claims the still-open issue, but GitHub has not closed the issue and no operator has confirmed it. | Still actionable; shown in the likely-done band with a merged linked-PR badge. |
| `hive-pause/<hive-id>` | A dashboard operator manually held this issue or PR for this hive. | Held until an operator resumes it. |

`hive/covered-by-pr` and `hive/likely-done` are pending-state labels. A comment that merely names a PR does not create them: Hive first verifies the PR or merged PR through GitHub. They also do not close, hide, or mark an issue already done. Resolution still requires the issue closing, GitHub's `closingIssuesReferences` relationship, or an operator confirmation.
