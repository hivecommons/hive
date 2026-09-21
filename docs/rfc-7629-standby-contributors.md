# RFC #7629: Standby contributors

Status: proposal — open for maintainer discussion, not accepted  
Target: Hive v6  
Scope: Contributor relay (standby mode), scheduler budget-pause signalling, per-lane model floor, donated-PR marking and outcome tracking, dashboard  
Initially out of scope: Sharing credentials or tokens in any form, automatic dispatch (step 3 of the rollout), benchmark-based tiering beyond what #6825 already proposes  
Refs: [#7629](https://github.com/hivecommons/hive/issues/7629), [#8034](https://github.com/hivecommons/hive/issues/8034), [#6825](https://github.com/hivecommons/hive/issues/6825), [#5698](https://github.com/hivecommons/hive/issues/5698)  
Related design: [RFC #5698: Backend capacity, model inventory, pacing, and placement](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5698-backend-capacity-model-inventory-placement.md) · [RFC #6825: Capability-aware contributor task assignment](rfc-6825-capability-aware-contributor-task-assignment.md)

> **This page is the design under discussion, not an adopted one.** The text
> below is the proposal from
> [#7629](https://github.com/hivecommons/hive/issues/7629), committed verbatim
> so it can be reviewed as a diff and amended in place rather than edited in an
> issue body — the same treatment
> [RFC #6825](rfc-6825-capability-aware-contributor-task-assignment.md) got.
> Nothing here has shipped. Its four open questions are answered — as a
> recommendation for maintainers to accept or reject, not as a settlement — in
> [the S0 design document](../src/docs/design/standby-contributors.md).
>
> **The hold banner below is part of the committed text and still applies to the
> issue.** #7629 carries the `hold` label; S0 (this file plus the design
> document) is the design gate the RFC's own rollout asks for, and it is docs
> only. The hold comes off when S0 merges, at which point S1 to S4 can be filed
> from the design document's phase map.

---

> **Hold - design discussion, not a task.** This is a proposal for maintainer discussion. Please do not implement it, decompose it into sub-issues, or relay it to contributors until the design is settled.

**Status:** Proposal  
**Target:** Hive v6  
**Scope:** Contributor relay (standby mode), scheduler budget-pause signalling, per-lane model floor, donated-PR marking and outcome tracking, dashboard  
**Initially out of scope:** Sharing credentials or tokens in any form, automatic dispatch (step 3 of the rollout), benchmark-based tiering beyond what #6825 already proposes  
**Related design:** [RFC #5698: Backend capacity, model inventory, pacing, and placement](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5698-backend-capacity-model-inventory-placement.md) · [RFC #6825: Capability-aware contributor task assignment](https://github.com/hivecommons/hive/blob/v5/docs/rfc-6825-capability-aware-contributor-task-assignment.md)

---

This is a feature idea, not a bug. The outcome I want: **when a hive owner runs out of tokens, contributors who volunteered ahead of time can pick up that lane's queue on their own machines, and a cheap model can never quietly replace a good one.**

## What an operator sees today

I run out of subscription tokens. The quality lane pauses. The queue keeps growing. Nothing happens until I pay again, and for a hobby project that can be weeks.

At the same time, the contributor relay already lets someone else run an agent on their own machine, with their own model and their own credentials, and hand the result back as a normal PR. But they have to come by and pick a task. Nothing tells them a lane is stuck, and there is no way to say "use my machine whenever this hive is short."

## What I am proposing

The missing piece between the two: a lane that is paused for budget offers its queue to approved standby contributors, if their model clears the bar for that lane.

Two things it is not:

- **Not sharing tokens or keys.** Nobody's credentials move. A contributor donates a running agent, never a key. That is the relay's existing rule and it does not change.
- **Not a way around review.** Donated work is hold-gated no matter what the hive's ACMM level is, and it lands as a PR from the contributor's own account. It gets reviewed like any outside contribution.

## The risk: people will donate pennies, not quarters

This is the part I care most about. Donating a frontier model costs real money; donating a cheap one costs nearly nothing. So the standby pool will be mostly cheap models, and an owner staring at a stuck queue will be tempted to lower the bar until something qualifies. A lane that ran on a strong model for months and then quietly starts producing work from a weak one is worse than a paused lane: reviewers stop trusting the lane, not just the donated PRs. Reviewer time is the scarcest thing a small project has, and a cheap PR that needs a careful human review can cost more than it saves.

So the design has to assume the pool is pennies. What I think handles it:

- **Pennies buy penny work.** A cheap model is not useless; it is useless for hard work. Match the donation to the item, not just the lane. A T3 configuration takes T3 items (a docs fix, a changelog entry, a dependency bump, a test for a one-line function) and nothing else.
- **The floor starts high and nothing asks to lower it.** Every lane starts at T1. Lowering it is a deliberate config edit. The dashboard must not nudge with "0 contributors qualify — lower the floor?". "Nobody qualifies, the lane stays paused" is an acceptable answer.
- **Rejected work suspends the donor.** Track how each contributor configuration's donated PRs end: merged, closed unmerged, reworked by a human first. Closed unmerged N times in a row (default 2) suspends that configuration from standby for this hive until the owner clears it. That is the cheapest honest signal of model quality there is, and it needs no benchmark.

The tiers here are the model-capability tiers from RFC #6825 (T1/T2/T3 plus `unknown`), not the backend support tiers or the trust tiers. `unknown` never qualifies, same as #6825 says. And it is the whole configuration that is matched (model, harness, reasoning setting), reported by the relay, so nobody can offer Opus and run something else.

## What "fixed" looks like

Owner side, per lane in `hive.yaml` or the dashboard:

```yaml
quality:
  standby:
    enabled: true
    min_model_capability: T2   # default T1
    daily_cap_per_contributor: 3
```

plus an explicit list of approved standby contributors. Volunteering does not grant approval.

Contributor side: the relay gets a "stand by for `<hive>`" mode. It stays connected, reports the configuration it can run (the offered-configuration inventory from #6825), and enforces the contributor's own limits on their machine.

Hub side:

1. When a lane pauses for budget, the scheduler publishes the reason and the queue depth. It knows both today; it does not publish them.
2. For each queued item, find approved standby contributors whose configuration clears the lane floor and the item's tier and who have cap left.
3. Package the kick as a contributor task — the lane's policy text, the item, the usual task contract — and the relay launches it locally.
4. The PR arrives hold-gated, marked in the body with the lane it was donated to and the tier the configuration mapped to, from the contributor's account.

A donated agent never gets more than the owner's own agents had: same policy, same repos, same mode ceiling, minus merge. Fallback can only narrow.

Rollout, advisory first like #6825 recommends for itself:

1. **Show it.** "Quality lane paused (out of budget). 6 items waiting. 2 approved standby contributors qualify." Nothing dispatches.
2. **Owner dispatches by hand.** A button per item or per lane.
3. **Automatic dispatch, opt-in per lane**, once a few hives have used step 2 and reviewers say the donated work held up.

Each step is a small PR.

## Relation to existing work

- RFC #5698 (accepted): the hub knows it is out of capacity. This is what happens next.
- RFC #6825 (under discussion): contributors offer configurations with a capability estimate. This is one consumer of that, and probably the simplest, because the floor is a human setting per lane rather than something inferred per task. It only needs the tier vocabulary and the offered-configuration inventory, which I think are the least disputed parts.
- The relay and the `hive-contributor` image run work the same way; they gain a standby mode.

## Open questions

- Per lane or per repo for the floor? Per lane matches how policies are written. A repo that needs more sets a higher floor on every lane it cares about.
- Is hold-gated review of a T3 donated PR actually cheaper than doing the item by hand? For a changelog entry, probably not. The T3 match list should be narrow and owner-editable, not "everything `pkg/classify` calls simple".
- Does a donated PR count against the hive's own PR budget and cadence limits? I think yes, so a generous donor cannot flood a repo.
- Private repos: a standby contributor sees the task context. The answer is probably "only approve people you would give read access to", but the approval screen should say so.

