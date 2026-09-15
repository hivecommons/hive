# v5 GA candidate week — sequencing plan

This page sequences the remaining **human-gated** steps of the
[v5 GA bar](v5-ga.md) ([tracker #6016](https://github.com/hivecommons/hive/issues/6016)).
As of 2026-09-14 no GA-bar row is waiting on measurement or produced work:
every remaining item is a maintainer action (see
[#7015](https://github.com/hivecommons/hive/issues/7015)). This plan exists so
those actions run as one dependency-ordered block instead of ad-hoc, and so the
perishable evidence windows (twice invalidated within hours on 2026-09-14 —
[#6971](https://github.com/hivecommons/hive/issues/6971),
[#6984](https://github.com/hivecommons/hive/issues/6984)) are consumed rather
than re-derived.

## Step 0 — designate the GA candidate SHA (unblocks everything below)

A maintainer designates the GA candidate on
[#6016](https://github.com/hivecommons/hive/issues/6016): the newest `v5` SHA
that is **dual-green** — a green required `gate` run (docker.yml) *and* a green
v2 Tests run *and* a green post-merge DCO monitor run. As of 2026-09-14 that
SHA is `bab972ac` (v2 Tests
[run 34905587101](https://github.com/hivecommons/hive/actions/runs/34905587101),
DCO monitor
[run 34905587102](https://github.com/hivecommons/hive/actions/runs/34905587102));
use whatever the newest dual-green SHA is at decision time.

Designation is the single highest-leverage action on the board: four GA-bar
rows and the [#6346](https://github.com/hivecommons/hive/issues/6346) v4
feature-freeze trigger are pinned to it.

## Day 1 — candidate-pinned measurements (no exercise required)

Both are read-only checks against the candidate SHA, recordable the same day:

1. **v5 channel publication** — confirm the candidate's short-SHA image tags
   exist on GHCR for all three images and that `edge` points at the candidate
   digest while `stable`/`candidate` remain v4.
2. **Digest-verifiable rollback** — perform the GHCR manifest inspection from
   [`release-rollback.md`](release-rollback.md) for the candidate's short-SHA
   tag and record the digests.

## During the week — the two bound exercises (parallel, independent)

3. **Reviewer-lane production exercise**
   ([#6130](https://github.com/hivecommons/hive/issues/6130)) — defined as
   running "during the GA candidate week": a maintainer kicks the
   review-capable agent against a candidate-week PR; evidence per
   [v5-ga.md](v5-ga.md#reviewer-lane-production-exercise-bound).
4. **Dual-version validation**
   ([#6112](https://github.com/hivecommons/hive/issues/6112), owner
   @clubanderson) — v4 hub / v5 spoke (or the supported pairing) with
   heartbeat, one channel switch, one rollback; evidence bullet per
   [v5-ga.md](v5-ga.md#dual-version-validation-exercise-bound). This does not
   depend on the candidate SHA and may start before Step 0 if scheduling
   allows.

## Any time — candidate-independent recordings

5. **Merge-hygiene row** — record the citation on #6016 and have an
   independent maintainer ratify the `01bd2469` waiver
   ([#6939](https://github.com/hivecommons/hive/issues/6939)).
6. **Fresh-fork CI run** ([#6090](https://github.com/hivecommons/hive/issues/6090))
   — link one green fork CI run URL with zero required edits.

## Close of week — freeze evaluation

7. With the candidate-pinned Release-train rows recorded, evaluate the
   [#6346](https://github.com/hivecommons/hive/issues/6346) v4 feature-freeze
   trigger ("all Release-train rows green"). If any row failed, the calendar
   fallback remains 2026-10-15.

## Known risk going in

Current `v5` HEAD `d4c3660f` is red on v2 Tests with a **new** failure shape —
`pkg/dashboard` shuffled-order tests
([run 34915061785](https://github.com/hivecommons/hive/actions/runs/34915061785))
— distinct from the [#6935](https://github.com/hivecommons/hive/issues/6935)
apt-mirror egress class. Until triaged, later SHAs may not be dual-green;
this strengthens, not weakens, the case for designating the candidate from the
existing dual-green SHA now.
