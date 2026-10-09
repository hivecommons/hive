# Knowledge connectors

External knowledge systems (git repositories, documents, GitHub wikis, and later others) are synced into the file vault by connectors configured under `knowledge.connectors` in `hive.yaml`. Operators manage them from **Settings → Knowledge → Connectors** in the dashboard.

## Configuration entry

```yaml
knowledge:
  connectors:
    - name: team-wiki          # lowercase letters, digits, dashes
      type: github-wiki        # git | document | github-wiki
      enabled: true
      interval: 15m            # minimum 1m; blank = 15m
      layer: project           # personal | project | org | community
      scope:
        repos: org/repo-a, org/repo-b
      auth:
        env: WIKI_TOKEN        # an environment variable NAME, or
        # file: /run/secrets/wiki-token   # an absolute secret file path
```

Only one of `auth.env` / `auth.file` may be set. Inline secrets (an `auth.token`, or secret-looking scope keys such as `api_key`) are rejected everywhere, including by the dashboard.

## Dashboard pane

| Column | Meaning |
| --- | --- |
| Status | `ok` last sync succeeded, `syncing` a sync is running, `error` last sync failed, `disabled` not scheduled, `pending` no sync yet |
| Last sync | Completion time of the last successful sync |
| Pages / facts | Pages emitted by the last sync / active facts the connector owns |
| Last error | Truncated; hover for the full text |

Owners can **Sync now**, **Edit**, **Disable/Enable** and **Remove** a connector and **Add** new ones. Non-owners see the table read-only. **Validate** inside the Add/Edit dialog is a conservative dry-run: it applies the config rules and the connector's own scope/auth checks and confirms the named environment variable is set (or the file is readable and non-empty). It performs no network access and never echoes the credential.

Status and **Sync now** need the connector syncer to be running; when it is not, the table shows configuration only and Sync now answers `503`.

## API

| Endpoint | Access | Purpose |
| --- | --- | --- |
| `GET /api/config/knowledge/connectors` | dashboard session | Configured connectors and registered types |
| `PUT /api/config/knowledge/connectors` | owner | Replace the list (`{"connectors": [...]}`); the whole list is validated first |
| `GET /api/config/knowledge/connectors/status` | dashboard session | Status rows for the table |
| `POST /api/config/knowledge/connectors/validate` | owner | Dry-run validate one entry |
| `POST /api/config/knowledge/connectors/{name}/sync` | owner | Start a background sync (`202`; `409` if running) |

Saved changes persist to the config overlay; the syncer picks up added, edited or removed connectors on the next restart.

## Publish mirror

The publish mirror is the reverse direction: it renders curator-promoted facts from the vault as pages in an external system so people can read them where they already work. It is one-way. The vault stays the source of truth.

```yaml
knowledge:
  publish:
    connector: team-docs       # a knowledge.connectors entry whose type can publish
    layers: [org, project]     # project | org | community — personal is never published
    root: Hive/Knowledge       # folder (or parent page) to publish under
    include_types: [decision, gotcha]   # optional; empty publishes every type
    dry_run: false             # true: compute and audit the batch, write nothing
    propose_via: https://github.com/acme/knowledge   # shown in each page footer
```

Connector types that can publish: `sharepoint`, which uploads each fact as `<root>/<page>.md` to the first configured drive or site library. Using any other type is rejected when the mirror is built.

### What gets published

- Only **curator-promoted** facts are published. A fact counts as promoted when its `source` front-matter starts with `promoted from ` (the provenance that `knowledge.curator` promotion writes).
- Only facts in the configured `layers` are published, and only types in `include_types` when that list is set. The personal layer is never published, whether it's named in `layers` or marked `layer: personal` in a fact's front-matter.
- Every page begins with a `<!-- hive_fact_id: <layer>/<slug> -->` marker and ends with this footer:

  > _Maintained by Hive — edits here are overwritten; propose changes via <propose_via>._

  When `propose_via` is unset, the footer says "the Hive knowledge vault" instead.
- Page names come from the fact id (`hive-<layer>-<slug>`), so each publish upserts the same page instead of creating a duplicate. When two facts would map to the same page, the fact that already owns the page keeps it. The other fact is skipped and listed under `collisions` in the batch report.
- Publishing is diff-aware: the mirror stores a content hash for each page and resends only pages that changed.
- Deprecated or superseded facts are never deleted upstream. Their page gets a **Deprecated** banner instead. If a published fact leaves the vault, its page becomes a deprecated tombstone.

### When it runs

The mirror publishes once at startup and then on a daily sweep. It also exposes a trigger that the curator promotion scheduler can call after each sweep that promotes at least one fact (`PromotionScheduler.OnPromoted`). Each batch produces one audit record (`PublishReport`: created, updated, deprecated, unchanged, collisions, error) that is logged as `knowledge publish batch`. The mirror's status (last run, last success, page count, last error and last report) is available from `Mirror.Status()`.

### Stopping publishing

- To preview without writing upstream, set `knowledge.publish.dry_run: true`.
- To stop publishing, remove the `knowledge.publish` block or clear `connector`. Pages that were already published stay in the external system. Delete them there if you no longer want them.
