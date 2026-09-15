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
v2 Tests run *and* a green post-merge DCO monitor run. As of 2026-09-15 that
SHA is `973425c6` (v2 Tests
[run 34924306954](https://github.com/hivecommons/hive/actions/runs/34924306954),
DCO monitor
[run 34924306975](https://github.com/hivecommons/hive/actions/runs/34924306975),
gate
[run 34924306933](https://github.com/hivecommons/hive/actions/runs/34924306933),
all 2026-09-15T03:15Z; prior dual-green SHAs: `a7c1cf6b`, `bab972ac`);
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

*(Resolved 2026-09-15.)* The `d4c3660f` v2 Tests red with the `pkg/dashboard`
shuffled-order shape
([run 34915061785](https://github.com/hivecommons/hive/actions/runs/34915061785))
was fixed by [#7018](https://github.com/hivecommons/hive/pull/7018) (flaky SSE
gap test racing the heartbeat ping), and the
[#6935](https://github.com/hivecommons/hive/issues/6935) apt-mirror egress
class was mitigated by [#7009](https://github.com/hivecommons/hive/pull/7009):
the offline cgo apt cache is now seeded under `v5` (68 `.deb` files, recorded
in [#7023](https://github.com/hivecommons/hive/pull/7023)). Since then `v5`
has produced four consecutive green v2 Tests runs (`4ecd8be0` → `973425c6`,
2026-09-15T01:44Z–03:15Z). The evidence window is the healthiest it has been
during the entire GA measurement period — which strengthens the case for
designating the candidate now, while it holds.
