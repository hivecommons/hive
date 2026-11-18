---
title: cncf-techdocs-assessment reviewer rules for evidence and scoring
type: gotcha
layer: project
state: approved
tags: [repo:cncf/techdocs, techdocs, assessment, reviewer-rule]
source: https://github.com/cncf/techdocs/pull/424#discussion_r4618113440
scope:
  lanes: [cncf-techdocs-assessment]
---

For TechDocs assessments, apply the published criteria to observed behavior on
the requested documentation domain, not a different site CLOMonitor scans.
Record the target URL and results for each readiness criterion. An `llms.txt`
file or URL-presence test alone does not prove Markdown availability, content
parity or synchronization. Do not inflate the grade or call readiness a
“standout area” without criterion-level evidence.

Additional reviewer corrections on the same assessment:

- Inspect existing adoption/testimonial materials before reporting their absence
  ([source](https://github.com/cncf/techdocs/pull/424#discussion_r4617291386)).
- Distinguish installed localization/analytics infrastructure from functionality
  actually delivered on documentation pages
  ([localization](https://github.com/cncf/techdocs/pull/424#discussion_r4617357700),
  [analytics](https://github.com/cncf/techdocs/pull/424#discussion_r4617591526)).
- Assess the applicable website checklist before assigning an exemplary Sandbox
  grade; documentation volume alone is insufficient
  ([source](https://github.com/cncf/techdocs/pull/424#discussion_r4618143283)).
- Mark usability/accessibility “not assessed” until supporting interaction checks
  are recorded. Say “No scan was performed or supplied for this assessment”,
  not that no scan has ever happened
  ([source](https://github.com/cncf/techdocs/pull/424#discussion_r4618183780)).
