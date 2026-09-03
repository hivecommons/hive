# Governor mode thresholds and repo-count scaling

The governor picks a mode — **idle → quiet → busy → surge** — by comparing queue depth (actionable issues + open PRs, summed across every repo the hive watches) against three thresholds. The mode then selects each agent's kick cadence, so the thresholds are what decide how hard the hive works.

## The problem the scaling solves

Queue depth scales with how many repos a hive watches; the thresholds did not. A fixed `surge: 20` therefore encoded an implicit repo count:

- A 39-repo hive naturally carries a queue in the low hundreds. Against absolute thresholds it sat in **SURGE permanently**, running the most aggressive cadences even though the per-repo backlog was small. Observed live: queue ~210 against a threshold of 70.
- A 3-repo hive with the same thresholds would idle through a backlog that is genuinely deep *per repo*.

Every hive with a different repo count had to hand-tune, and re-tune whenever repos were added or archived.

## What happens by default

The **default** thresholds are now per-repo base values, multiplied by the number of repos in `project.repos`:

| | base (1 repo) | 3 repos | 39 repos |
|---|---:|---:|---:|
| `surge` | 20 | 60 | 780 |
| `busy` | 10 | 30 | 390 |
| `quiet` | 2 | 6 | 78 |

A hive watching one repo (or none — `primary_repo` still counts as one) gets exactly the numbers it got before, so single-repo hives see no change at all.

The effect is that the mode ladder means the same thing at any hive size: **surge is "20+ items per repo"** rather than "20 items, however many repos you watch".

## Choosing a curve

```yaml
governor:
  threshold_scaling: linear   # linear (default) | sqrt | none
```

| Value | Factor | When to use |
|---|---|---|
| `linear` (default) | `× repo_count` | The general case. Exactly equivalent to comparing per-repo queue pressure against the base thresholds. |
| `sqrt` | `× ceil(√repo_count)` | Hives whose queue depth does not grow linearly with repo count — many small, quiet repos alongside a few busy ones. Reaches surge sooner than linear. |
| `none` | `× 1` | Treat the thresholds as absolute queue depths. This is the behavior from before scaling existed. |

For 39 repos, `linear` surges at 780 and `sqrt` at 140 (`ceil(√39) = 7`). Note that `sqrt` and `linear` are identical at 1 and 2 repos and only separate from 3 onward, because the ceiling cannot produce a factor below 2 until 5 repos.

An unrecognized value is rejected at config load.

## Operator-set thresholds always win

A threshold **you** set yourself is used **verbatim and never scaled**:

```yaml
governor:
  modes:
    surge:
      threshold: 300     # used as-is on a 39-repo hive, not 300 × 39
```

So a hive that already hand-tuned its way around this problem keeps working unchanged, and dragging a handle in the dashboard's **Governor → Thresholds** tab pins that threshold to an absolute value.

A threshold of `0` counts as **unset**, not as an explicit zero — mode entries often exist only to carry cadences, and a literal zero would put every non-empty queue in that mode.

### Mixing explicit and scaled thresholds

Because explicit values are not scaled, mixing them with scaled defaults can put the ladder out of order. On a 39-repo hive:

```yaml
governor:
  modes:
    surge:
      threshold: 70      # explicit → stays 70
    # busy and quiet unset → scaled to 390 and 78
```

`busy` (390) now sits **above** `surge` (70). The governor tests surge first and returns on the first threshold the queue exceeds, so BUSY becomes unreachable. The governor logs a warning naming all three effective values when it detects this:

```
governor mode thresholds are not in descending order — a mode is unreachable
  surge=70 busy=390 quiet=78 repo_count=39 threshold_scaling=linear
```

It warns rather than silently reordering, because the inversion comes from a number you set. Either set all three explicitly, or remove the explicit one so all three scale together.

## ACMM packs seed thresholds, and those DO scale

ACMM level packs (`src/pkg/config/packs/level-*.yaml`) write `surge`/`busy`/`quiet` thresholds into `governor.modes` when a level is applied — for example L4–L6 seed `surge: 10, busy: 5, quiet: 2`, and L3 seeds `15/10/3`.

Applying a pack is the normal path, so if those counted as hand-tuned the scaling above would almost never engage. Config therefore records **where the thresholds came from** ([#4037](https://github.com/hivecommons/hive/issues/4037)):

```yaml
governor:
  thresholds_source: pack     # written by the pack-apply path
  modes:
    surge:
      threshold: 10           # a per-repo BASE, not an absolute
```

With that marker present the seeded numbers are treated as **per-repo bases** and scaled exactly like the built-in defaults — so a 39-repo hive at L4 surges at `10 × 39 = 390`, and L3's `15` stays distinct from L4's `10` rather than collapsing onto one default.

**Editing any threshold clears the marker.** The moment you set one in the dashboard's **Governor → Thresholds** tab (or by hand), the whole set becomes yours and is used verbatim. That is deliberate: clearing only the mode you touched would leave the others scaling, and a scaled `busy` of 195 sitting above a hand-set `surge` of 30 inverts the ladder described above. All three move together, so they cannot invert.

### On upgrade

A hive that applied a pack **before** this change has seeded thresholds and no `thresholds_source` marker. Those read as operator-set and keep their exact current values — upgrading never silently multiplies your thresholds. **Re-apply the level** (or change it) to stamp the marker and turn scaling on.

To go the other way — pack-seeded values you want as absolutes — set them explicitly once; that clears the marker.

## Where the numbers are shown

The dashboard governor gauge and the **Governor → Thresholds** settings tab both display the **effective** thresholds — the same values the governor laddered on, resolved through the same code path — along with the watched repo count. The settings sliders edit the base values.

## Related

- [Agent configuration → Cadences and the governor](agent-configuration.md#cadences-and-the-governor)
- Issue [#3498](https://github.com/hivecommons/hive/issues/3498)
