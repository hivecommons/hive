# Public snapshots

`/snapshot` is a read-only, public view of a hive's current dashboard state. It is intended for sharing status with communities, embedding in project pages, or linking from a hub without granting dashboard write access.

Snapshots are available when the spoke has auto-snapshots enabled:

```yaml
hub:
  auto_snapshot: true
```

The backing JSON is served from `/api/snapshot` with public cache headers. Both `/snapshot` and `/api/snapshot` are public paths, but they reject write methods and expose only the status payload, not mutating dashboard APIs.

## Run history

The snapshot payload carries a bounded run block, `runHistory`, alongside the
live `runs` list:

```json
"runHistory": {
  "active": [{"key": "org/repo#123", "title": "...", "stage": "plan", "gen": 7, "waiting_on": "human", "outcome": "active", "stage_started_at": "..."}],
  "recent": [{"key": "org/repo#98", "title": "...", "stage": "implement", "gen": 12, "waiting_on": "none", "outcome": "completed", "completed_at": "..."}],
  "limit": 20
}
```

- `active` mirrors the runs that hold a live stage lease; `recent` is the most
  recently completed stages without a live lease, newest first, capped at
  `limit` entries (`SnapshotRunHistoryLimit`). `outcome` is `active`,
  `completed`, or `merged` when the run's journey reached merge.
- Every title passes the status token redactor (GitHub tokens, API keys,
  device codes) before it is published, and the block never carries lease
  ids, task ids, or tokens.
- The block is omitted entirely when the spoke cannot project runs (for
  example a spoke on a release line without run leases). The snapshot page
  then renders "runs: unknown"; it never substitutes an empty history for
  unknown.

The field is additive and always on when snapshots are enabled; it needs no
extra configuration.

## Custom CSS

The snapshot page accepts the same sanitized stylesheet parameter as the dashboard:

```text
/snapshot?style=owner/repo/path/to/theme.css@ref
```

See [Custom stylesheets](custom-stylesheets.md) for allowed CSS and sanitizer reporting.

## Embedding and frame ancestors

By default Hive fails closed: snapshot pages send `X-Frame-Options: DENY` and `Content-Security-Policy: frame-ancestors 'none'`.

To allow selected HTTPS origins to embed `/snapshot`, configure:

```yaml
dashboard:
  snapshot_frame_ancestors:
    - https://docs.example.org
    - https://status.example.org:8443
```

Only exact HTTPS origins are accepted. Paths, wildcards, non-HTTPS origins, and malformed hosts are rejected. When an allow-list is present, `/snapshot` omits `X-Frame-Options` because that header cannot express multiple allowed origins; every other route keeps `DENY`.

The current allow-list is visible at `/api/snapshot/frame-ancestors` for operators debugging embed configuration.

