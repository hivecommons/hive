RFC: close issues automatically when the PR that fixes them merges

Today a maintainer has to go back and close each issue by hand after the pull request that fixes it is merged.

Proposal: when a PR that references an issue merges, the hive should close the issue on merge, maybe with a backfill sweep for older merges and an optional reporter confirmation step for bugs.
