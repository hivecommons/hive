# v4 feature-freeze execution runbook (draft)

> **Status: draft planning artifact — not yet maintainer-accepted.**
> The freeze *policy* is accepted
> ([#6346](https://github.com/hivecommons/hive/issues/6346), recorded in
> [v5-ga.md — v4 lifecycle policy](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md#v4-lifecycle-policy-accepted)).
> This runbook sequences its *execution* and becomes binding only when a
> maintainer records acceptance on
> [#7693](https://github.com/hivecommons/hive/issues/7693).

## Trigger

v4 feature-freezes when **every Release-train row on the
[v5 GA bar](https://github.com/hivecommons/hive/issues/6016) is green, or on
2026-10-15, whichever comes first**. From that point until EOL, v4 accepts
**security fixes and critical fixes only**, and v4→v5 movement is
**cherry-pick-only** — no batch syncs, each cherry-pick carrying its own
DCO-valid `Signed-off-by`.

The trigger can fire before the backstop date: the 7-day soak row was already
evidence-ready once
([#6808](https://github.com/hivecommons/hive/issues/6808)). Treat this
runbook as executable **on any day**, not as an October task.

## Roles

- **Freeze marshal** — the maintainer who observes the trigger and executes
  (or delegates) the steps below in order. Default: whoever records the final
  Release-train row on #6016, since they see the trigger first.
- **Merge-tier agent or human** — required for every step that touches
  `.github/workflows/`; contributor-tier agents cannot push those paths.

## Ordered checklist

Rows follow the GA bar convention: check a row only with a **linked evidence
artifact** (PR, workflow run, dashboard state), never on intent.

### 1. Declare

- [ ] Freeze marshal posts a freeze-declaration comment on #6016 naming the
      trigger (rows-green or backstop date) and the exact v4 SHA at the
      freeze line. Evidence: comment permalink.
- [ ] ROADMAP.md "v4 — Stable Line" section updated from *"v4
      feature-freezes when…"* (future tense) to a dated statement with the
      freeze SHA. Evidence: merged PR.

### 2. Gate v4 intake

- [ ] A `v4-freeze-exempt` label (or equivalent) is created, and
      CONTRIBUTING.md's base-branch table gains a row stating that post-freeze
      v4 PRs must be security or critical fixes. Evidence: merged PR.
- [ ] The merge path for v4 rejects (or escalates to `needs-human`) any v4 PR
      not labeled as a security/critical fix. Implementation choice —
      required-check, governor policy, or review-lane rule — is the
      maintainers' call; this runbook only requires that *some* enforcement
      exists, because unenforced freeze policy decays into fiction.
      Evidence: link to the gate (workflow run or config) plus one rejected
      or escalated test PR. **Workflow-file portion needs a human or
      merge-tier agent.**

### 3. Retarget the agent fleet

- [ ] Hive agent lanes (quality, guide, architect, strategist, scanner) stop
      opening non-critical PRs against v4 and target v5 instead. The last
      48 hours before this draft was written saw at least six non-critical
      agent commits land on v4 (`aafa26f`, `11a4ce6`, `2f88669`, `d1e4dd4`,
      `b1b677b`, `9a91315`) — at that cadence the fleet violates the freeze
      within hours unless retargeted the same day it is declared.
      Evidence: agent/hive configuration change or operator note, plus the
      first post-freeze week of v4 history showing only security/critical
      traffic.
- [ ] sec-check remains pointed at v4 (security fixes stay in scope).
      Evidence: same configuration artifact.

### 4. Dispose of the batch-sync automation

- [ ] `v5-topup.yml` is disabled or made manual-only. The workflow automates
      **batch** forward-merges — exactly what the accepted policy forbids
      post-freeze. Evidence: merged workflow change (**human or merge-tier
      agent required**) or repo Actions-settings screenshot.
      Open defects #7659 (`gh: command not found` on ARC runners) and #7660
      (recurring conflicts) become moot for v4→v5 once this lands; `v6-topup`
      (v5→v6) is unaffected and keeps running.
- [ ] The cherry-pick procedure replacing it is documented: one cherry-pick
      per security/critical v4 fix, opened against v5, carrying its own
      `Signed-off-by` per the accepted policy (see
      [v5-sync-policy.md](v5-sync-policy.md) for the pre-freeze contrast).
      Evidence: doc merged, first real cherry-pick PR linked.

### 5. Remap channels and default branch (at GA cut, after the freeze)

These rows execute at the **GA cut** (first stable v5 release), not at the
freeze line — listed here because the freeze marshal should know the whole
sequence before starting.

- [ ] `stable` promotion source moves from the v4 line to the v5 line:
      `docker.yml` on `v5` publishes `candidate`, the stable-promotion
      workflow soaks and promotes v5 digests, and
      [release-channels.md](release-channels.md) plus
      [stable-soak-policy.md](stable-soak-policy.md) are updated to match.
      Evidence: merged workflow + doc PRs (**human or merge-tier agent
      required for workflows**), first v5-digest `stable` promotion run.
- [ ] Default branch switches v4 → v5. Note this also moves OpenSSF
      Scorecard, which only evaluates the default branch
      ([#4393](https://github.com/hivecommons/hive/issues/4393)).
      Evidence: repo settings change reflected in the API, green Scorecard
      run on the new default.
- [ ] v4 EOL announcement is drafted only after #6016 closes, per the
      accepted dependency ([#6140](https://github.com/hivecommons/hive/issues/6140)).
      Evidence: announcement link.

## Tracker

Mirror this checklist into one issue titled **"v4 freeze execution"** when
the freeze is declared, and keep it in sync with this doc — the same
tracker convention the v5 GA bar (#6016) and v6 readiness bar
([#7683](https://github.com/hivecommons/hive/issues/7683)) use.

---

Related: [#7693](https://github.com/hivecommons/hive/issues/7693) (gap
finding), [#6346](https://github.com/hivecommons/hive/issues/6346) (accepted
policy), [#6016](https://github.com/hivecommons/hive/issues/6016) (GA bar),
[v5-ga.md](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md), [v5-sync-policy.md](v5-sync-policy.md),
[release-channels.md](release-channels.md).
