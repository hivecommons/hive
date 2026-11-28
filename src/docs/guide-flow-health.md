# Guide flow-health diagnosis

Guide's **surge-coach lens** prioritizes a documentation finding when existing
queue signals suggest sustained congestion. It is part of the existing Guide
kick and ACMM policies, not a new agent, timer, or permission grant.

The scheduler appends a single `HIVE_FLOW:` line to Guide kicks, including
custom templates and manual kicks. Other agents' kicks are unchanged. Existing
cadence, pause, budget, and write gates still apply: a paused Guide is not woken
by congestion.

## Signal contract

| Field | Meaning |
| --- | --- |
| `normal`, `clogged`, `unknown` | Any observed threshold breach is clogged; otherwise missing measurements yield unknown; otherwise normal. Normal means no sampled threshold breach, not proof of healthy flow. |
| `clogged_by` | Each tripped threshold, comma-separated: `surge`, `oldest`, `mttm`; `none` when nothing tripped. `clogged` without `mttm` and with `merged_sample=0` carries no merge evidence ([#20273](https://github.com/hivecommons/hive/issues/20273)). |
| `surge` | Continuous observed hive-wide SURGE dwell, in minutes, from the governor's latest mode transition. Missing or inconsistent history is unknown; outside SURGE it is zero. Restart observation gaps are not reconstructed. |
| `mttm_sample` | Arithmetic mean creation-to-merge duration for cached attributed PRs merged in the 14 days ending at the snapshot timestamp. Unmerged PRs and invalid dates are excluded; duplicate repo/number pairs count once. No qualifying merges means unknown. |
| `merged_sample` | Number of qualifying merged PRs in that mean. This is a bounded enumeration sample, not an exhaustive count of all watched-repo merges. |
| `window` | Merge lookback, currently `14d`. |
| `oldest_actionable` | Maximum issue or PR age in the actionable snapshot, in minutes; held work, draft PRs and PRs whose review decision is `CHANGES_REQUESTED` are excluded, since they wait on their author, not on review or merge flow. Zero means no positive actionable age was observed. |
| `scope` | Item measurements use authorized, active repos, further narrowed for repo-scoped cadence kicks. |
| `surge_scope` | Always `hive`; a hive-wide trigger does not authorize inspection of other repos. |
| `source`, `snapshot` | Cached attributed-PR enumeration and the actionable snapshot timestamp (UTC), or unknown when unavailable. |

The initial thresholds are SURGE **at least 3 days**, sampled mean time to merge
**over 7 days**, or oldest actionable age **at least 14 days**. These are advisory
constants in `pkg/scheduler/guide_flow.go`, not governor mode thresholds or new
`hive.yaml` keys. No extra GitHub API requests are made to calculate the signal.
The existing attributed closed-PR scan has a page cap, configurable lookback,
and cached fallback on errors. Its mean can therefore be incomplete or stale;
Guide must disclose that limitation and verify evidence before recommending a
change. Held-only congestion with no observed SURGE or slow merged sample is
not detected by the actionable-age trigger. Before Guide names a long-lived
item as a blocker, it reads the operator and repo knowledge in its kick: a PR
that knowledge says is held on purpose, such as a release PR waiting for a
release decision, is not congestion.

## Finding and follow-up

When clogged, Guide buckets observed blockers by repo (holds, CI, reviews,
grants, agent availability, and threshold/cadence settings), ranks the top three
supported causes, and gives org-level and repo-level recommendations with
counts, denominators, links, confidence, and a measurable follow-up criterion.
A snapshot's blocker share is not a causal share of elapsed SURGE time.

Guide records findings within the current mode's permissions: advisory beads,
issues where allowed, and documentation PRs where allowed, retaining hold gates.
It reuses existing findings and closes only its own beads after re-verification.
Unknown telemetry is not evidence that a finding is resolved. Normal or unknown
signals keep the usual documentation audit; they do not trigger more scanning.

Guide never applies governor changes, edits grants, assigns reviewers, removes
holds, writes configuration/code PRs, or merges as part of this lens. Recommended
changes are text for a human to evaluate and apply. This implements the revised
Guide-based shape of [RFC #9593](https://github.com/hivecommons/hive/issues/9593),
without the original standalone agent or dashboard tile.
