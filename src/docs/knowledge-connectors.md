# Knowledge connectors

Knowledge connectors sync external knowledge systems (Confluence, Notion, git
repositories, GitHub wikis, documents) into the hive knowledge base on a
schedule. Each upstream page becomes one markdown fact, and agents read those
facts like any other vault content.

This page covers how to operate connectors. For the shared configuration
fields, the fact format and how connectors relate to `git_sources` and
`documents`, see
[knowledge-curator.md → Connectors](knowledge-curator.md#connectors-knowledgeconnectors).

## How it works

- **Config:** `knowledge.connectors` in `hive.yaml` lists the connectors. Each
  entry has a `name`, `type`, `layer`, optional `interval` (default `15m`,
  minimum `1m`), `enabled`, a type-specific `scope` map and an optional
  `auth.env` / `auth.file` credential reference. Inline credentials are
  rejected by config validation.
- **Scheduling:** at startup `hive` builds every entry from that config. A
  connector whose config is invalid is kept but disabled, and its status shows
  the error. Every enabled connector syncs once immediately and then on its
  `interval`.
- **Storage:**
  - Facts for layer `<layer>` are written to `/data/knowledge/connectors/<layer>`.
    That directory is connected as the vault `connectors-<layer>` and
    reindexed after every sync.
  - Cursors and status persist in `/data/knowledge/connector-state/status.json`,
    so incremental syncs survive restarts.
- **Incremental vs full syncs:**
  - Most syncs are incremental: they emit only pages changed since the stored
    cursor.
  - Connectors that page through large spaces (Confluence, Notion) run a full
    listing every `full_sync_every` syncs (default 10). On a full listing, a
    page that no longer appears is tombstoned: its fact is set to
    `status: deprecated` and the file stays, so links still resolve.
  - A sync that hits the connector's `max_pages` cap is reported as
    `truncated` and never tombstones anything.
- **Network:**
  - Every connector HTTP request goes through the shared SSRF-hardened client.
    Private, loopback and link-local addresses are refused, including after
    redirects.
  - Each request has a 30 s timeout and a 20 MiB body cap.
  - `429` and `502`/`503`/`504` responses are retried with `Retry-After`-aware
    backoff.

## API

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `GET` | `/api/knowledge/connectors` | Dashboard auth/session | `{"connectors": [...]}`: one status per configured connector, in config order. |
| `POST` | `/api/knowledge/connectors/{name}/sync` | Owner only | Starts one sync now, even if the connector is disabled. Returns `202` with the status, `404` for an unknown name, and `409` while a sync is already running. |

Each status object contains:

| Field | Meaning |
|-------|---------|
| `name`, `type`, `layer` | From the config entry. |
| `enabled`, `interval` | Whether the connector is scheduled, and how often. |
| `last_sync` | Time of the last successful sync. |
| `last_attempt` | Time of the last sync attempt, successful or not. |
| `pages` | Pages emitted by the last successful sync. |
| `facts` | Active facts written by this connector. |
| `deprecated` | Tombstoned facts written by this connector. |
| `last_error` | Error from the last attempt, if any. |
| `cursor` | The incremental cursor. |
| `running` | `true` while a sync is in progress. |
| `truncated` | `true` when the last sync stopped at `max_pages`. |

```sh
curl -H "Authorization: Bearer $HIVE_TOKEN" https://hive.example/api/knowledge/connectors
curl -X POST -H "Authorization: Bearer $HIVE_TOKEN" https://hive.example/api/knowledge/connectors/eng-wiki/sync
```

## Confluence (`type: confluence`)

The Confluence connector supports both Confluence Cloud and Confluence Data
Center / Server. It uses the REST content search API (`/rest/api/content/search`
with CQL), which both editions share.

### Setup

1. Create a dedicated account for hive and give it **read-only** (view)
   permission on the spaces you want to sync.
2. Create a credential for that account:
   - **Cloud:** create an API token at
     <https://id.atlassian.com/manage-profile/security/api-tokens>. Put the
     account email in `scope.email`. The connector authenticates with HTTP
     Basic auth (`email:token`).
   - **Data Center / Server:** create a personal access token (Profile →
     Personal Access Tokens). The connector sends it as `Authorization: Bearer`.
3. Store the token in a Kubernetes secret and mount it as an environment
   variable (`auth.env`) or a file (`auth.file`). Never put it in `hive.yaml`.

```yaml
knowledge:
  connectors:
    - name: eng-wiki
      type: confluence
      layer: org
      interval: 1h
      scope:
        base_url: https://acme.atlassian.net/wiki   # Data Center: https://confluence.acme.com
        email: hive-bot@acme.com                    # Cloud only
        spaces: ENG, OPS
        exclude_labels: draft
      auth:
        env: CONFLUENCE_API_TOKEN
```

| Scope key | Default | Meaning |
|-----------|---------|---------|
| `base_url` | required | Site URL. Cloud sites include the `/wiki` context path. Must be `https` (or `http`) and is SSRF-checked. |
| `deployment` | `cloud` for `*.atlassian.net`, otherwise `datacenter` | Selects the auth scheme. |
| `email` | — | Atlassian account email. Required for Cloud. |
| `spaces` | — | Comma-separated space keys. |
| `root_page_ids` | — | Comma-separated page ids. Each page and all of its descendants are synced. |
| `include_labels` / `exclude_labels` | — | Comma-separated label filters. |
| `include_attachments` | `false` | `true` adds an "Attachments" section linking each page's attachments. |
| `max_pages` | `2000` | Maximum pages per sync. A sync that hits the cap is reported as `truncated`. |
| `full_sync_every` | `10` | Every Nth sync is a full listing that tombstones deleted or moved pages. |

You must set at least one of `spaces` or `root_page_ids`.

### Behaviour

- **Rendering:** page bodies (`body.storage`, falling back to `body.view`) are
  converted to markdown:
  - Headings, lists, tables, code and links are preserved.
  - Code and noformat macros become fenced code blocks.
  - Info, note, warning and tip panels become labelled blockquotes.
  - Expand macros are unwrapped.
  - Page, attachment and user links are resolved against `base_url`.
  - Any other macro keeps its body, or becomes a `[Confluence macro: name]`
    placeholder.
- **Fact metadata:**
  - The fact path is the space name followed by the page's ancestor titles.
  - Attributes record the space key, labels and version number.
- **Incremental sync:** the cursor is the newest `version.when` the connector
  has seen. Incremental syncs query `lastmodified >= <cursor date − 1 day>`.
  The one-day overlap covers CQL's day granularity and server time zones;
  unchanged pages are not rewritten.
- **Archived and deleted pages:** pages in the `archived` or `trashed` state
  are written as deprecated. Pages that disappear from the search (deleted,
  archived out of CQL, moved out of scope, or no longer readable by the account)
  are tombstoned on the next full listing.
- **Paging:** results are paged through `_links.next`. Next links are only
  followed relative to `base_url`, never to another host.

## Notion (`type: notion`)

The Notion connector syncs pages and database rows through the public Notion
API (v1, `Notion-Version: 2022-06-28`). It authenticates with an internal
integration token.

### Setup

1. Go to <https://www.notion.so/profile/integrations> and create an
   **internal** integration for your workspace. Give it the **Read content**
   capability only.
2. **Share the pages with the integration.** Notion integrations only see
   pages that are explicitly shared with them. Open each top-level page or
   database you want to sync, click **•••** → **Connections** → **Connect to**,
   and choose the integration. Child pages inherit access. A page the
   integration cannot see is skipped silently: it never appears in the search.
3. Store the integration secret (`ntn_…` / `secret_…`) in a Kubernetes secret
   and reference it with `auth.env` or `auth.file`.

```yaml
knowledge:
  connectors:
    - name: product-notes
      type: notion
      layer: project
      interval: 30m
      scope:
        root_page_ids: 1a2b3c4d5e6f47a8b9c0d1e2f3a4b5c6
        database_ids: 0f1e2d3c-4b5a-4968-8776-655443322110
      auth:
        env: NOTION_TOKEN
```

| Scope key | Default | Meaning |
|-----------|---------|---------|
| `root_page_ids` | — | Comma-separated page ids (32 hex characters, dashes optional). Each page and every page below it are synced. |
| `database_ids` | — | Comma-separated database ids. Every row is synced as a fact, with the row's properties as attributes. |
| `include_archived` | `false` | `true` also fetches the body of archived or trashed pages. They are always written as deprecated. |
| `max_pages` | `2000` | Maximum pages per sync. A sync that hits the cap is reported as `truncated`. |
| `full_sync_every` | `10` | Every Nth sync is a full listing that tombstones pages that are no longer shared or no longer exist. |

Without `root_page_ids` and `database_ids`, every page shared with the
integration is synced.

### Behaviour

- **Listing:** pages come from `POST /v1/search` (oldest edit first). Rows of
  the configured databases come from `POST /v1/databases/{id}/query`. Scope is
  checked by walking each page's parent chain. Ancestors the integration cannot
  see end the chain instead of failing the sync.
- **Rendering:** blocks are fetched recursively from
  `/v1/blocks/{id}/children` and converted to markdown:
  - Headings, lists, to-dos, toggles (`<details>`), quotes, callouts, code,
    equations, tables, images, files, bookmarks, embeds, columns and synced
    blocks are converted.
  - Child pages and databases become links.
  - Unsupported blocks become an `[Unsupported Notion block: type]`
    placeholder.
- **Incremental sync:** the cursor is the newest `last_edited_time` emitted.
  Database queries use an `on_or_after` filter on that time.
- **Archived pages:** pages that are `archived` or `in_trash` are written as
  deprecated. Pages that stop being shared, or are deleted, are tombstoned on
  the next full listing.
- **Rate limit:** requests are spaced 350 ms apart to stay under Notion's
  average of three requests per second. `429` responses are retried with
  `Retry-After`.
