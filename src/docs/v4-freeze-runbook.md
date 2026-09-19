# v4 feature-freeze execution runbook

> **Status: mechanics wired, awaiting the trigger.** The freeze *policy* is
> accepted ([#6346](https://github.com/hivecommons/hive/issues/6346), recorded
> in [v5-ga.md — v4 lifecycle policy](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md#v4-lifecycle-policy-accepted)).
> The enforcement below is already in the tree behind one switch — the
> repository variable **`V4_FEATURE_FREEZE`** — so declaring the freeze is a
> single command, not a workflow change under time pressure. The gap this
> closes was filed as [#7693](https://github.com/hivecommons/hive/issues/7693).

## The switch

```bash
# At the freeze line (freeze marshal, maintainer permissions):
unset GITHUB_TOKEN
gh variable set V4_FEATURE_FREEZE --repo hivecommons/hive \
  --body "$(git rev-parse --short origin/v4) $(date -u +%F)"
```

Setting the variable does two things on its own, with no further merges:

| Effect | Where | Behaviour while frozen |
| --- | --- | --- |
| **v4 intake gate** | `.github/workflows/v4-freeze-gate.yml` | Every PR targeting `v4` must carry `security`, `agent/security`, `priority/critical-urgent`, or `v4-freeze-exempt`; otherwise the check fails and the PR is labelled `needs-human`, which the automerge path refuses to land. |
| **Batch-sync shutdown** | `.github/workflows/v5-topup.yml` | The `top-up` job is skipped on every trigger (release, schedule, manual) and a `frozen` job records why in the run summary. `v6-topup` (v5→v6) is unaffected. |

While the variable is unset both workflows are no-ops that pass. Unsetting it
(`gh variable delete V4_FEATURE_FREEZE`) re-arms the cadence and disarms the
gate — that is the roll-back if the freeze is declared in error.

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

- [x] A `v4-freeze-exempt` label exists and CONTRIBUTING.md's base-branch
      table states that post-freeze v4 PRs must be security or critical fixes
      and names the gate. Evidence: label
      [`v4-freeze-exempt`](https://github.com/hivecommons/hive/labels/v4-freeze-exempt),
      CONTRIBUTING.md row (this runbook's PR).
- [x] The merge path for v4 rejects **and** escalates to `needs-human` any
      v4 PR not labelled as a security/critical fix:
      `.github/workflows/v4-freeze-gate.yml`, armed by `V4_FEATURE_FREEZE`.
      Unenforced freeze policy decays into fiction, so this is a failing
      check plus a label the automerge path refuses, not a warning.
      Evidence: the workflow (this runbook's PR).
- [ ] At the freeze line, after setting the variable: mark `freeze-gate` a
      **required** status check on `v4` branch protection, and open one
      deliberately unlabelled test PR to confirm it is rejected and labelled
      `needs-human`. Evidence: protection setting via API, the test PR.

### 3. Retarget the agent fleet

- [ ] Hive agent lanes (quality, guide, architect, strategist, scanner) stop
      opening non-critical PRs against v4 and target v5 instead. Until they
      are retargeted the intake gate is the backstop: every non-critical
      agent PR fails `freeze-gate` and is parked `needs-human`, so the fleet
      cannot land a freeze violation unattended — but it will pile up parked
      PRs, which is why this row is same-day. The last
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

- [x] `v5-topup.yml` disables itself behind `V4_FEATURE_FREEZE`: the
      `top-up` job is skipped on release, schedule **and** manual dispatch,
      and a `frozen` job writes the reason into the run summary. The
      workflow automated **batch** forward-merges — exactly what the accepted
      policy forbids post-freeze. `v6-topup` (v5→v6) is unaffected and keeps
      running. Evidence: the workflow (this runbook's PR); after the flag is
      set, the first run showing `frozen` and a skipped `top-up`.
- [x] The cherry-pick procedure replacing it is documented below
      (§ Cherry-pick procedure). Evidence: this doc.
- [ ] First real post-freeze cherry-pick PR linked here.

#### Cherry-pick procedure (post-freeze v4 → v5)

One PR per security/critical v4 fix, never a range, never a merge commit. The
cherry-pick carries the original author and adds the picker's own
`Signed-off-by` (`-s`), so `dco-push-delta` / `dco-post-merge` on v5 see a
valid trailer for the person who actually pushed it (the squash-attribution
guard skips nothing here — this is a single-parent commit).

```bash
unset GITHUB_TOKEN
git fetch origin v4 v5
git switch -c pick/v5-<v4-pr-number> origin/v5
git cherry-pick -x -s <v4-merge-sha>      # -x records "(cherry picked from commit …)"
# resolve conflicts if any; keep the v4 fix's intent, adapt to v5 APIs
cd src && go build ./... && go test -race -short ./pkg/<touched>/... && cd ..
git push -u origin HEAD
gh pr create --repo hivecommons/hive --base v5 \
  --title "🐛 <original title> (cherry-pick of #<v4-pr>)" \
  --body "Cherry-pick of #<v4-pr> onto v5 per the post-freeze policy (#6346). Refs #<issue>."
```

Rules:

- Pick the v4 **merge/rebase result SHA** (what `git log origin/v4` shows), not
  the PR's pre-rebase head, so `-x` points at history v5 readers can find.
- A pick that touches `changelog.d/` keeps its fragment (the compiler on v5
  owns the headings); a pick that needs adaptation gets a new fragment
  describing the v5 behaviour, not the v4 one.
- Do **not** label a pick `no-changelog` to dodge the guard — that label is
  for batch syncs, which no longer exist.
- The pre-freeze contrast (batch forward-merges, `no-changelog`, merge
  commits) is in [v5-sync-policy.md](v5-sync-policy.md); it stops applying at
  the freeze line.

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
