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
