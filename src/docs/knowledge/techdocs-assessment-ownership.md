---
title: cncf-techdocs-assessment ownership uses the project's authoritative roster
type: gotcha
layer: project
state: approved
tags: [repo:cncf/techdocs, techdocs, assessment, reviewer-rule, ownership]
source: https://github.com/cncf/techdocs/pull/424#discussion_r4617524979
scope:
  lanes: [cncf-techdocs-assessment]
---

Identify the project's authoritative maintainers roster before recommending
changes to documentation ownership. Distinguish project maintainers, docs
reviewers and CNCF technical writers; do not claim somebody reviewed an
assessment without evidence.

For Hive Commons, the authoritative roster is `hivecommons/.project`'s
`maintainers.yaml`, not docs `OWNERS` or `CODEOWNERS`. Reconcile those files with
the current official roster and intended docs-review responsibilities instead
of merely recommending a second approver. Check the current roster and GitHub
CODEOWNERS validation rather than freezing the reviewer comment's counts or
membership as permanent facts. This project's roster is not the authority for
other projects.

Reviewer evidence:
https://github.com/hivecommons/.project/blob/5eba57f04b9228427aef2b5c7af6d1232fd4a5bd/maintainers.yaml#L15-L25
