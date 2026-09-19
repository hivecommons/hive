# v5 migration — operator announcement (draft)

> **Status: DRAFT — do not publish until Phase 0 of
> [#7721](https://github.com/hivecommons/hive/issues/7721) is complete.**
> This is the announcement the migration plan's Phase 2 requires
> ("Evidence: announcement link"). The migration marshal fills every
> `«fill-in»` field from Phase 0/1 evidence, deletes this banner, and posts
> the result to the announcement channels listed at the bottom. Filed from
> [#7736](https://github.com/hivecommons/hive/issues/7736).

---

## Hive is moving to v5 the week of 2026-09-22 — what changes for your hive

Starting the week of **2026-09-22**, the Hive release channels re-base from
the v4 line to v5, and the `edge` channel moves to the new v6 development
line. v4 enters its accepted feature freeze
([#6346](https://github.com/hivecommons/hive/issues/6346)) and serves
security and critical fixes only until EOL. The full plan and live tracker is
[#7721](https://github.com/hivecommons/hive/issues/7721).

### The channel table, before and after

| Channel | Today | After migration week |
|---|---|---|
| `stable` | v4 digest, soak-promoted | **v5** digest, soak-promoted (first v5 promotion: `«date from Phase 3»`) |
| `candidate` | v4, every green merge | **v5**, every green merge |
| `edge` | v5, every green merge | **v6**, every green merge |
| `latest` | follows v4 | follows **v5** |

`v4-latest` and immutable short-SHA tags continue to publish on the v4 line;
v4 simply stops feeding the moving channels.

### What to do, by how your hive selects its image

- **Tracking `stable`** — no action required. Your hive stays on the current
  v4 digest until the first v5 promotion passes the full soak gate
  ([stable-soak-policy](stable-soak-policy.md), 24h default, no emergency
  exception for the first v5 promotion), then rolls to v5 automatically.
  Read [UPGRADE.md §"v4 → v5"](../../UPGRADE.md#v4--v5) **before**
  `«date stable first resolves to v5»`: in particular the `/data` ownership
  notes (do **not** add an out-of-band `chown -R`) and the hosted-hub
  hostname change (set `hub.url`/`HIVE_HUB_URL` to
  `https://hive.hivecommons.dev` explicitly rather than relying on the
  compiled fallback).
- **Tracking `candidate`** — you move to v5 at Phase 1, at the start of the
  migration week, on your next roll. Same UPGRADE.md reading, sooner.
- **Tracking `edge`** — **your release line changes, not just your build.**
  `edge` has always meant "newest good build, no soak"; after Phase 1 that
  is the **v6** development line (dashboard-optional operation,
  [#7563](https://github.com/hivecommons/hive/issues/7563)), which has no
  GA bar yet ([#7683](https://github.com/hivecommons/hive/issues/7683)). If
  you run `edge` for early v5 visibility rather than to track the newest
  line, switch to `candidate` (v5) before migration week via the version
  pill or by retagging your deployment.
- **Pinned to `v4-latest` or a v4 SHA** — nothing moves without you. Plan an
  owner-initiated switch to `candidate` (or `v5-latest`) during the
  migration window; after the freeze, v4 receives security/critical fixes
  only, so staying pinned means a shrinking patch stream, not a stable
  plateau.

### Security fixes while `stable` lags

Between the channel re-base (Phase 1) and the first v5 `stable` promotion
(Phase 3), `stable` is a v4 digest that no longer advances. Security fixes
in that window reach `stable`-tracking hives by cherry-pick: v4 fix →
cherry-pick to v5 → `candidate` → soak → `stable`. The window is kept
deliberately short; if a critical fix lands during it, the promotion uses
the normal soak machinery, not an out-of-band retag.

### Rollback

Channels are moving tags. The digests of all three channels for all three
images are recorded before Phase 1 (`«link to Phase 0 digest record»`); a
rollback is the inverse retag and reaches every channel-tracking hive on its
next roll. Individual hives can roll back digest-verifiably per the
release-rollback runbook
([v5 branch](https://github.com/hivecommons/hive/blob/v5/src/docs/release-rollback.md)).
Keep the same `/data` volume across any rollback (UPGRADE.md).

### v4 support from here

v4 remains supported through v5 development: security and critical fixes
land on v4 and are cherry-picked forward
([v4-freeze-runbook](v4-freeze-runbook.md)). **No v4 EOL date is announced
in this message or before the v5 GA bar
([#6016](https://github.com/hivecommons/hive/issues/6016)) closes** — the
EOL announcement is a separate, later artifact
([#6140](https://github.com/hivecommons/hive/issues/6140)).

### Key dates

| Event | Date |
|---|---|
| Channel re-base (`candidate`←v5, `edge`←v6), v4 freeze flag set | `«Phase 1 date»` |
| Hosted hub on v5 | `«Phase 2 date»` |
| First v5 `stable` promotion | `«Phase 3 date»` |
| Default branch v4 → v5 | `«Phase 4 date»` |

---

## Publication checklist (marshal)

- [ ] All `«fill-in»` fields resolved from #7721 Phase 0/1 evidence; draft
      banner removed.
- [ ] UPGRADE.md §"v4 → v5" status line updated in the same Phase 1 doc
      sweep (it still reads "v5 not yet released").
- [ ] Posted to: #6016 (comment), hub dashboard notice, `«operator
      announcement channel(s): Discord/Slack, mailing list if any»`.
- [ ] Announcement link recorded on #7721's Phase 2 row as evidence.
