# v5 migration — operator notice (post-hoc)

> **Status: DRAFT (post-hoc rewrite)** — the original pre-migration
> announcement (a Phase 0 prerequisite of
> [#7721](https://github.com/hivecommons/hive/issues/7721), amended per
> [#7958](https://github.com/hivecommons/hive/issues/7958)) was never
> posted: on 2026-09-21 the channel moves it was meant to precede happened
> ahead of it by Hub Admin decision
> ([#8058](https://github.com/hivecommons/hive/pull/8058) /
> [#8059](https://github.com/hivecommons/hive/pull/8059) /
> [#8060](https://github.com/hivecommons/hive/pull/8060), emergency
> `stable` promotion evidence
> [#8062](https://github.com/hivecommons/hive/issues/8062)). This rewrite
> tells operators what **already changed** and what to check now. The
> marshal resolves the remaining `«fill-in»` fields, deletes this banner,
> and posts the result to the announcement channels listed at the bottom.
> Rewrite tracked by
> [#8101](https://github.com/hivecommons/hive/issues/8101).

---

## Hive moved to v5 on 2026-09-21 — what changed for your hive

On **2026-09-21** the Hive release channels re-based from the v4 line to
v5, and the `edge` channel moved to the new v6 development line. v4 is now
in its feature freeze
([#6346](https://github.com/hivecommons/hive/issues/6346);
`V4_FEATURE_FREEZE="c354a5008 2026-09-21"`) and serves security and
critical fixes only until EOL. This happened ahead of the announced
schedule and the normal 24h `stable` soak, by Hub Admin decision — the
deviation and its follow-up evidence are recorded on
[#8062](https://github.com/hivecommons/hive/issues/8062) and the
freeze-declaration comment on
[#6016](https://github.com/hivecommons/hive/issues/6016). The migration
plan and tracker is
[#7721](https://github.com/hivecommons/hive/issues/7721).

### The channel table, before and after

| Channel | Before 2026-09-21 | Now |
|---|---|---|
| `stable` | v4 digest, soak-promoted | **v5** digest (promoted 2026-09-21 by emergency exception, `soak-hours=0`; next promotions use the normal soak) |
| `candidate` | v4, every green merge | **v5**, every green merge |
| `edge` | v5, every green merge | **v6**, every green merge |
| `latest` | follows v4 | follows **v5** |

`v4-latest` and immutable short-SHA tags continue to publish on the v4
line; v4 has stopped feeding the moving channels.

### What to check, by how your hive selects its image

- **Tracking `stable`** — your hive rolls to a v5 digest on its next roll
  (or already has). This promotion did **not** get the normal 24h soak;
  the post-hoc soak conditions are tracked on
  [#8062](https://github.com/hivecommons/hive/issues/8062), and the
  pre-move v4 rollback digests are recorded there. After rolling, verify a
  healthy heartbeat and that the version pill shows `stable (v5)`. Read
  [UPGRADE.md §"v4 → v5"](../../UPGRADE.md#v4--v5) — in particular the
  `/data` ownership notes (do **not** add an out-of-band `chown -R`) and
  the hosted-hub hostname change (set `hub.url`/`HIVE_HUB_URL` to
  `https://hive.hivecommons.dev` explicitly rather than relying on the
  compiled fallback).
- **Tracking `candidate`** — you moved to v5 on your next roll after
  2026-09-21. Same UPGRADE.md reading.
- **Tracking `edge`** — **your release line changed, not just your
  build.** `edge` is now the **v6** development line (dashboard-optional
  operation, [#7563](https://github.com/hivecommons/hive/issues/7563)),
  whose readiness bar now lives in the #7563 epic
  ([#7563](https://github.com/hivecommons/hive/issues/7563) §v6 readiness bar). If you ran
  `edge` for early v5 visibility rather than to track the newest line,
  switch to `candidate` (v5) now via the version pill or by retagging your
  deployment — the pre-migration opt-out window described in earlier
  drafts of this notice has passed.
- **Pinned to `v4-latest` or a v4 SHA** — nothing moved without you. Plan
  an owner-initiated switch to `candidate` (or `v5-latest`); v4 now
  receives security/critical fixes only, so staying pinned means a
  shrinking patch stream, not a stable plateau.

### Security fixes on v4 from here

v4 remains supported: security and critical fixes land on v4 (gated by the
required `freeze-gate` check) and are cherry-picked forward
([v4-freeze-runbook](v4-freeze-runbook.md)). Because `stable` is already
v5, there is no lag window where `stable`-tracking hives wait on a frozen
v4 digest for fixes.

### Rollback

Channels are moving tags. The pre-move digest anchors are recorded in two
places: all three channels for all three images as of 2026-09-20T01:26Z
(comment on [#7721](https://github.com/hivecommons/hive/issues/7721)) and
the pre-promotion v4 `stable` digests (v4 `896140f`) on
[#8062](https://github.com/hivecommons/hive/issues/8062). A rollback is
the inverse retag (`docker buildx imagetools create -t <image>:<channel>
<image>@<digest>`) and reaches every channel-tracking hive on its next
roll. Individual hives can roll back digest-verifiably per the
release-rollback runbook
([v5 branch](https://github.com/hivecommons/hive/blob/v5/src/docs/release-rollback.md)).
Keep the same `/data` volume across any rollback (UPGRADE.md).

### v4 support from here

v4 serves security and critical fixes only until EOL. **No v4 EOL date is
announced in this message or before the v5 GA bar
([#6016](https://github.com/hivecommons/hive/issues/6016)) closes** — the
EOL announcement is a separate, later artifact
([#6140](https://github.com/hivecommons/hive/issues/6140)).

### Key dates

| Event | Date |
|---|---|
| Channel re-base (`candidate`←v5, `edge`←v6, `latest`←v5), v4 freeze flag set | 2026-09-21 (actual) |
| First v5 `stable` promotion (emergency exception, post-hoc soak per #8062) | 2026-09-21 (actual) |
| First **normal** soak-satisfied v5 `stable` promotion | `«date (estimate)»` |
| Hosted hub on v5 | `«Phase 2 date (estimate)»` |
| docs.hivecommons.dev → v5 (hivecommons/docs#13) | `«date (estimate)»` |
| Default branch v4 → v5 | `«Phase 4 date (estimate)»` |

---

## Publication checklist (marshal)

- [ ] Remaining `«fill-in»` dates resolved or marked `(estimate)`; draft
      banner removed.
- [ ] UPGRADE.md §"v4 → v5" status line updated in the doc sweep (it may
      still read "v5 not yet released").
- [ ] Posted to:
  - [ ] #6016 (comment)
  - [x] hub dashboard notice — now possible via `hub.contribute_announcement`
  - [ ] `«operator announcement channel(s): Discord/Slack, mailing list if any»`
- [ ] Notice link recorded on #7721's Phase 0 announcement row (marked as
      posted **post-hoc**, per the deviation record) and on #8062.
