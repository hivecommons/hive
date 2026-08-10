# Public snapshots

`/snapshot` is a read-only, public view of a hive's current dashboard state. It is intended for sharing status with communities, embedding in project pages, or linking from a hub without granting dashboard write access.

Snapshots are available when the spoke has auto-snapshots enabled:

```yaml
hub:
  auto_snapshot: true
```

The backing JSON is served from `/api/snapshot` with public cache headers. Both `/snapshot` and `/api/snapshot` are public paths, but they reject write methods and expose only the status payload, not mutating dashboard APIs.

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

