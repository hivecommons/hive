# Ablation plan on consenting hives (2026-10)

**Status:** plan only — nothing is switched on. Consent for each hive is
recorded on [#12373](https://github.com/hivecommons/hive/issues/12373) before
its start date; the checkboxes below are copied from that issue and are the
source of truth.

**Parent:** RFC [#12062](https://github.com/hivecommons/hive/issues/12062),
proposal C (Study): "Ablations on consenting hives only (Bluefin, Console,
Hive), 30 days each: cross-family reviewer; shadow-gate accuracy vs actual
outcomes. Auto-merge unchanged everywhere."

**Pre-registration:** [`preregistration-2026-10.md`](preregistration-2026-10.md)
§Experimental arms. This plan adds no hypothesis, metric or exclusion rule
that is not already in the pre-registration. Where it needs a number it quotes
the pre-registration verbatim.

## Auto-merge is unchanged everywhere

> No new blocking gate ships by default. No change to merge = CI green + swarm
> eligibility. — RFC #12062, Non-goals

Neither arm touches merge eligibility on any hive, consenting or not:

- **Shadow gates are record-only.** `review.shadow` and `intent.shadow` evaluate
  a gate and write `shadow.review_gate.would_have_held` /
  `shadow.intent.would_have_blocked` on the PR's evidence bundle. They never
  hold, block or weaken an existing `require_approval` / `enforce`
  ([#12104](https://github.com/hivecommons/hive/issues/12104),
  [#12105](https://github.com/hivecommons/hive/issues/12105)).
- **The cross-family reviewer adds a perspective, not an approval
  requirement.** `review_models.exclude_author_family` changes *which model*
  reviews a PR; it adds no human step and no new gate. `review.require_approval`
  is not changed by this plan on any hive.

If anyone finds either statement untrue while an arm is running, the arm stops
(rollback below) and the finding is reported on #12373 as a result.

## Consenting hives

| Hive | Owner / maintainers | Consent (link to comment on #12373) | Hive id (as shown in the hub) |
|---|---|---|---|
| Bluefin | @mrbobbytables | - [ ] _pending_ | `<hive id>` |
| Console (KubeStellar) | Console owners | - [ ] _pending_ | `<hive id>` |
| Hive (hivecommons/hive) | hive maintainers | - [ ] _pending_ | `<hive id>` |

A hive whose consent is not recorded by its start date is left out of that
arm's period and the omission is reported. Hive-on-Hive results are reported
separately and never pooled, per the pre-registration.

## Arms

Verbatim from the pre-registration, §Experimental arms:

> 1. **Cross-family reviewer** — consenting hives (Bluefin, Console, hive) run
>    at least one review perspective on a different model family from the
>    author. Adds per-review cost; adds no human step; auto-merge unchanged.
> 2. **Shadow gates** — review-gated merge and blocking intent verification in
>    shadow mode on the same hives. Records only; blocks nothing.
>
> Assignment by hash of hive id where more than one consenting hive exists.

Each consenting hive runs **both arms, one at a time, 30 days each**, so the
shadow-gate accuracy read in arm 2 is not confounded by the reviewer change in
arm 1 (and vice versa). The hash decides **which arm a hive runs first**, which
balances calendar effects (model upgrades, load) across arms.

### Assignment rule

```bash
printf '%s' "<hive id>" | sha256sum | cut -c1
```

- First hex digit **even** (`0 2 4 6 8 a c e`): period A = cross-family reviewer, period B = shadow gates.
- First hex digit **odd** (`1 3 5 7 9 b d f`): period A = shadow gates, period B = cross-family reviewer.

| Hive | sha256 first digit | Period A (30 d) | Period B (30 d) |
|---|---|---|---|
| Bluefin | `<digit>` | `<arm>` | `<arm>` |
| Console | `<digit>` | `<arm>` | `<arm>` |
| Hive | `<digit>` | `<arm>` | `<arm>` |

Fill the table from the real hive ids before the start date and leave it
unchanged afterwards; a later change is an amendment and goes in a new dated
file.

## Schedule

Dates are placeholders until consent is in. Each period is 30 days; a 7-day
washout with both arms off separates them so links written in the last days of
one period (14-day revert window) do not straddle the switch.

| Hive | Period A start | Period A stop | Washout | Period B start | Period B stop | Who flips it |
|---|---|---|---|---|---|---|
| Bluefin | `TBD` | `TBD + 30 d` | 7 d | `TBD` | `TBD + 30 d` | @mrbobbytables or @clubanderson with written OK |
| Console | `TBD` | `TBD + 30 d` | 7 d | `TBD` | `TBD + 30 d` | Console owner on record |
| Hive | `TBD` | `TBD + 30 d` | 7 d | `TBD` | `TBD + 30 d` | @clubanderson |

The person who flips a hive posts the exact diff applied and the restart time
on #12373 at start and at stop.

## Exact config diff per arm

Keys are real `hive.yaml` leaves (`pkg/config`): `review.shadow`
(`ReviewConfig.Shadow`), `intent.shadow` (`IntentConfig.Shadow`),
`agents.<reviewer>.review_models.{pool, exclude_author_model, exclude_author_family, fallback}`
(`ReviewModelsConfig`). Nothing else changes. In particular
`review.require_approval` and `intent.enforce` are **not** in either diff.

### Arm 1 — cross-family reviewer

Pick the hive's existing review-queue reviewer agent(s) (the names in
`review.reviewer_agents`, or the pack's `reviewer` agent). For each, add
or extend `review_models` so the pool contains at least one model from a
family other than the hive's authoring agents' models, and turn on
`exclude_author_family`. Example for a hive whose coders run `claude-*`:

```diff
 agents:
   reviewer:
     backend: copilot
     model: claude-fable-5
+    review_models:
+      pool:
+        - {backend: copilot, model: gpt-5.6-terra}
+        - {backend: copilot, model: gemini-3.7-flash}
+        - {backend: copilot, model: claude-opus-4-6}
+      exclude_author_model: true
+      exclude_author_family: true
+      fallback: pinned
```

`fallback: pinned` keeps the reviewer's own model when no cross-family entry
is eligible, so no PR loses its review. Pool entries must be models the hive's
backend can actually run (check with the backend's model list first); record
the final pool on #12373. If the hive already has a `review_models` block, the
diff is only `exclude_author_family: false → true` plus any pool entries
needed to make a second family available.

Family is the normalized vendor prefix before the first dash (`claude`,
`gpt`, `gemini`, `grok`, …), the same rule the reviewer-accuracy family
metrics use.

### Arm 2 — shadow gates

```diff
 review:
+  shadow: true
 intent:
+  shadow: true
```

Both flags are ignored when the corresponding enforcing flag is already on
(`require_approval`, `enforce`); a hive that already enforces one gate records
only the other in shadow and says so on #12373.

### Rollback (either arm)

One config revert and a restart:

```bash
git revert <commit that applied the arm>   # or edit hive.yaml back by hand
# restart the spoke the usual way; settings are read at start
```

Anyone on the hive's owner list may roll back at any time without asking;
post the time and reason on #12373. A rolled-back period is reported as
truncated, with its actual stop date, not dropped.

## Metrics and where results are read

Copied verbatim from the pre-registration's hypothesis table; no new metric is
introduced after the fact.

| ID | Hypothesis | Metric | Expectation |
|---|---|---|---|
| H3 | A reviewer from a different model family than the PR author has a lower false-approve rate than a same-family reviewer. | false-approve rate per (author family, reviewer family) from `/api/reviewer/accuracy` and evidence records (#12065) | Expect to hold; today untestable (one reviewer family, author model not stamped). |
| H7 | Shadow review-gating would have held a material share of PRs that merged and did *not* produce a bad outcome (i.e. gating by default would cost throughput for little safety). | `would_have_held` vs actual outcome (#12068) | Expect to hold; informs whether gating ever becomes default. |

Supporting definitions, also verbatim: "**Reviewer accuracy and calibration**
— `/api/reviewer/accuracy?days=N`; false-approve = approved then bad outcome,
false-block = blocked then merged cleanly by humans." and "**Revert /
follow-up rate** — proxy for DORA change-failure rate. Source: revert /
follow-up recorder writing `revert_of` / `followup_of` into `pkg/evidence`
records. Labelled as a proxy everywhere it is shown."

Where each is read:

| Arm | Read from | Field |
|---|---|---|
| Cross-family reviewer (H3) | spoke `GET /api/evidence/summary?hours=0&repo=<owner/repo>` | `reviewer_accuracy_by_model` (`EvidenceModelAccuracy`, split by author kind) |
| Cross-family reviewer (H3) | hub `GET /api/hub/evidence` | `family_false_approve_rate` (`same` / `cross` / `mixed`, each `{n, rate, lo, hi}`, Wilson 95%, null when `n` < 10) |
| Shadow gates (H7) | spoke `GET /api/evidence/summary?hours=0&repo=<owner/repo>` | `shadow_gate` (`EvidenceShadowGate`: per gate `held` / `passed` × `merged` / `open` / `reverted` / `closed`) |
| Both — raw rows | `hivectl evidence export --since <period start> --format csv` ([#12367](https://github.com/hivecommons/hive/issues/12367)) | per-PR verdict model/backend, `shadow_review_gate.would_have_held`, `shadow_intent.would_have_blocked`, outcome, revert links |
| Both — baseline | `hivectl evidence backfill` ([#12371](https://github.com/hivecommons/hive/issues/12371); run plan `research/backfill.md` once it lands) | pre-arm bundles marked `backfilled: true`; excluded from verdict-based rates, used only for outcome baselines |

Analysis windows follow the pre-registration (30 / 90 days rolling); the
30-day period plus its 14-day revert window is the unit reported per arm per
hive. Bundles with `backfilled: true` carry no verdicts and never enter H3 or
H7 rates.

## Predictions we expect to lose (restated)

From the pre-registration, unchanged:

- **H2** — complexity rises at a rate indistinguishable from He & Miller's
  +41.6%: "Expect to hold — i.e. we expect to lose." Not an arm here, but the
  arms do nothing to change it and we say so.
- **H5** — higher ACMM level increases per-PR wait time: "Expect to hold — a
  negative result for Hive." Reported either way.
- **H7** is itself a result against default gating: if shadow review-gating
  would have held many PRs that merged cleanly, that is the finding, and the
  gate stays off by default.
- **H3** may come out null: with a 30-day window on three hives, `n` per
  (author family, reviewer family) cell may stay under 10 and every rate reads
  null. A null is reported as "underpowered", not as "no effect".

## Reporting

Results are posted on #12373 **win or lose**, per hive and per arm, with the
same prominence either way, and go into the paper as written. Each report
includes: period dates actually run, the config diff applied, the summary and
hub JSON used (attached), `n` per cell, and any rollback or instrumentation
change inside the window (footnoted with its date, as the pre-registration's
"instrumentation drift" rule requires).

## Consent checklist (mirrors #12373)

- [ ] Bluefin — consent comment from @mrbobbytables, linked above
- [ ] Console — consent comment from a Console owner, linked above
- [ ] Hive — consent comment from a hive maintainer, linked above
- [ ] Hive ids filled in and the assignment table frozen
- [ ] Period A start dates set and posted
- [ ] Arm-1 model pools verified runnable on each hive's backend and posted
