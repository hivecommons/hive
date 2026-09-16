Fixed: the PR re-engagement budget is no longer spent at governor-tick pace. A
red PR whose head SHA never moves reads as stale on every tick, so all six
re-engagements were consumed in ~13 minutes and the PR was escalated to
`needs-human` before the owning agent's next kick could deliver a repair.
Re-engagements are now spaced by `ReEngageCooldown`, so the budget spans at
least an hour of real agent cadence.
