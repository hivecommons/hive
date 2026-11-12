# Knowledge connectors

External knowledge systems (Confluence, Notion, Google Drive, SharePoint/OneDrive, git repositories, documents, GitHub wikis, Plaud voice notes and Wispr Flow dictation) are synced into the file vault by connectors configured under `knowledge.connectors` in `hive.yaml`. Each upstream page becomes one markdown fact, and agents read those facts like any other vault content. Operators manage connectors from the Knowledge card's **🔌 Integrations** modal or from **Settings → Knowledge → Connectors** in the dashboard.

For the shared configuration fields, the fact format and how connectors relate to `git_sources` and `documents`, see [knowledge-curator.md → Connectors](knowledge-curator.md#connectors-knowledgeconnectors).

## Configuration entry

```yaml
knowledge:
  connectors:
    - name: team-wiki          # lowercase letters, digits, dashes
      type: github-wiki        # confluence | context7 | notion | git | document | github-wiki | google-drive | sharepoint | plaud | wisprflow
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

## Dashboard pane

The Knowledge card now has one **🔌 Integrations** button instead of a separate Obsidian-only setup button. The modal keeps the Obsidian setup guide as its Obsidian entry and adds connector entries for every registered connector type. Selecting a connector type shows configured entries, status, publish state when that connector is used for `knowledge.publish`, and owner actions to save config, validate credential references, enable/disable and start a sync when the syncer is running.

The same connectors remain available under **Settings → Knowledge → Connectors** for operators who prefer the governor settings dialog.

| Column | Meaning |
| --- | --- |
| Status | `ok` last sync succeeded, `syncing` a sync is running, `error` last sync failed, `disabled` not scheduled, `pending` no sync yet |
| Last sync | Completion time of the last successful sync |
| Pages / facts | Pages emitted by the last sync / active facts the connector owns |
| Last error | Truncated; hover for the full text |

Owners can **Sync now**, **Edit**, **Disable/Enable** and **Remove** a connector and **Add** new ones from Settings; the Knowledge-card Integrations modal provides the same save, validate, enable/disable and sync actions grouped by connector type. Non-owners see the table read-only. **Validate** inside the Add/Edit dialog is a conservative dry-run: it applies the config rules and the connector's own scope/auth checks and confirms the named environment variable is set (or the file is readable and non-empty). It performs no network access and never echoes the credential.

Status and **Sync now** need the connector syncer, which `hive` starts at boot whenever `knowledge.connectors` is non-empty. When it is not running, the table shows configuration only and Sync now answers `503`.

## API

| Endpoint | Access | Purpose |
| --- | --- | --- |
| `GET /api/config/knowledge/connectors` | dashboard session | Configured connectors and registered types |
| `PUT /api/config/knowledge/connectors` | owner | Replace the list (`{"connectors": [...]}`); the whole list is validated first |
| `GET /api/config/knowledge/connectors/status` | dashboard session | Status rows for the table |
| `POST /api/config/knowledge/connectors/validate` | owner | Dry-run validate one entry |
| `POST /api/config/knowledge/connectors/{name}/sync` | owner | Start a background sync (`202`; `409` if running) |
| `GET /api/knowledge/connectors` | dashboard session | `{"connectors": [...]}`: one status per configured connector, in config order |
| `POST /api/knowledge/connectors/{name}/sync` | owner | Start a background sync now, even if the connector is disabled (`202` with the status; `404` unknown name; `409` if running) |

Saved changes persist to the config overlay; the syncer picks up added, edited or removed connectors on the next restart.

Each status object returned by `GET /api/knowledge/connectors` contains:

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

## Context7 (`type: context7`)

The Context7 connector syncs documentation for a default set of Context7
libraries, one fact per library. Configure it from **Knowledge → 🔌
Integrations → Context7**; "Import from Context7" in Knowledge → Import keeps
working and links to that card.

```yaml
knowledge:
  connectors:
    - name: ctx7
      type: context7
      layer: community
      scope:
        library_ids: "/vercel/next.js, /golang/go"   # required
        topic: routing                                # optional
      auth:
        env: CONTEXT7_API_KEY   # a reference; with no auth block CONTEXT7_API_KEY is used if set
  context7:
    auto_import:               # legacy startup import, edited from the same card
      - id: /vercel/next.js
        query: routing
```

- The API key is only ever a reference (`auth.env` or `auth.file`); the card
  shows whether it is set or unset and never the value.
- **Validate** runs one live Context7 search with the resolved key. Status and
  last sync come from the same connector status table as other connectors.
- Each library's content is bounded before it is stored: whole snippets are
  kept by relevance to `topic` up to `HIVE_CONTEXT7_MAX_TOTAL_BYTES` (default
  16 KiB), and a `… truncated: N snippets / M bytes omitted (size cap)` line
  ends the content when any were dropped. See
  [Context7 content cap](knowledge-curator.md#context7-content-cap).

## Plaud Note Pro (voice notes, `type: plaud`)

The Plaud connector turns the recordings on a [Plaud Note / Note Pro](https://plaud.ai)
into knowledge facts: one fact per recording with the recording time,
duration, Plaud's AI summary and the full speaker-labelled transcript. Agents
search and cite them like any other fact, so a voice memo recorded on the way
to work can inform the issue or PR an agent picks up an hour later.

It talks to the **official, free Plaud MCP server** (`npx -y @plaud-ai/mcp`)
over stdio as an MCP client — there is no Zapier/Make/IFTTT hop and nothing to
pay beyond your existing Plaud plan. Plaud lists the MCP server as a Preview
feature on every tier including Starter; Plaud's docs note that preview
features may later move behind paid plans.

```yaml
knowledge:
  connectors:
    - name: plaud
      type: plaud
      layer: personal
      interval: 15m
      scope:
        command: npx -y @plaud-ai/mcp@latest   # default; any stdio MCP command
        since_days: "30"                       # how far back the first sync reaches
        max_files: "200"                       # recordings fetched per sync
        include_audio: "false"                 # add Plaud's 24h presigned MP3 link
        query: ""                              # optional name filter (list_files query)
        tags: "voice, ideas"                   # extra tags written as attr_tags
        # home: /data/knowledge/connector-state/plaud/home   # token store; default shown, or "inherit"
```

No `auth` block: Plaud signs you in with its own OAuth flow and keeps the token
in the MCP server's store (`$HOME/.plaud/tokens-mcp.json`). hive never sees,
stores or logs the token. By default the connector points the MCP server's
`HOME` at its own state directory under `/data/knowledge/connector-state/<name>/home`
so the token survives restarts; set `home: inherit` to use the hive process's
HOME instead.

The MCP server is third-party code fetched at run time, so it does not inherit
the hive process environment. It is started with an allowlist only — `PATH`,
`HOME`, locale, `TMPDIR`, `TERM`, the `HTTP(S)_PROXY`/`NO_PROXY` and CA-bundle
variables, `NODE_*`/`npm_config_*` and `PLAUD_*` — and never the hub-injected
credentials (`DASHBOARD_AUTH_TOKEN`, `HIVE_TERMINAL_KEY`, `HIVE_UPSTREAM_PROXY_URL`,
provider API keys). Pin `scope.command` to a specific `@plaud-ai/mcp@<version>`
if you do not want a sync to pull a new upstream release unreviewed.

### Setup

1. The hive host (or container image) needs **Node.js ≥ 20** so `npx` can run
   the Plaud MCP server.
2. Add the connector from **Knowledge → 🔌 Integrations → Plaud voice notes**
   (or the YAML above) and restart the hive so the syncer picks it up.
3. On the Plaud card click **Connect Plaud**. The card shows the Plaud
   authorization URL; open it, sign in and click **Authorize**. The OAuth
   callback lands on `localhost:8199` of the machine running hive, so for a
   remote hive forward that port first (`ssh -L 8199:localhost:8199 user@host`)
   and open the URL from the forwarding machine. The login session stays open
   for 10 minutes; click **Check** to confirm "connected as …".
   - Hosted hive (no port forward): open **Finish sign-in by pasting** on the
     card and paste either the `localhost:8199/auth/callback?…` URL the
     browser landed on after approving, or the contents of
     `~/.plaud/tokens-mcp.json` from a laptop where `npx -y
     @plaud-ai/mcp@latest install` signed in. See
     [Inbox and voice sources](inbox-voice-sources.md#hosted-hives-signing-in-to-plaud).
4. Click **Sync now**. New recordings are picked up every `interval` and
   appear in the **📥 Inbox**, where you file them as a spektacular RFC, send
   them to dibs, or keep them as knowledge only
   ([Inbox and voice sources](inbox-voice-sources.md)).

### Behaviour

- Each sync calls `list_files` (from the newest recording seen minus a 24 h
  overlap, or `since_days` back on the first run), then `get_note` and
  `get_transcript` for each recording. A recording whose transcript and
  summary are both still processing is skipped and retried next sync; the
  status reports it as a partial sync.
- Facts carry `source: plaud`, `attr_plaud_file_id`, `attr_recorded_at`,
  `attr_duration_ms` and `attr_device_serial`. Audio is never downloaded;
  with `include_audio: true` the fact links Plaud's 24-hour presigned URL.
- The listing is incremental, so recordings deleted in Plaud are not
  tombstoned automatically; remove the fact file or the connector.
- Sign-in state (`connected as`, pending authorization, last error) comes from
  `GET /api/knowledge/connectors/{name}/auth`; see [API](#api).

### Privacy and cost

- Transcripts and summaries are produced in **Plaud's cloud** and fetched from
  there; the hive stores them in the vault for the layer you choose (use
  `personal` unless the whole hive should see them). Do not record material
  that may not leave your organisation.
- **Local-only alternative:** Plaud's app can auto-sync notes to an Obsidian
  vault or Drive/Dropbox folder, and exports TXT/Markdown/DOCX by hand. Point
  the existing `git` connector at a Git-synced copy of that folder (see the
  Obsidian entry in 🔌 Integrations), or import single files with a
  `document` connector (`file_path`). No code, still $0, and no MCP sign-in.
- Everything in this section costs **$0** beyond your Plaud subscription.

## Wispr Flow (dictation, `type: wisprflow`)

[Wispr Flow](https://wisprflow.ai) is dictation into whatever field has focus:
hold the Flow key, speak, release, and polished text is pasted where the
cursor is. That gives two $0 paths into the hive.

**Direct dictation — no connector.** Put the cursor in **Knowledge → + New
Fact** (title or body), the Import box, an agent prompt, or any other text
field in the dashboard and dictate. Nothing to configure. This is the
recommended way to get *new* thoughts in.

**History connector.** The desktop app keeps every dictation in a local
SQLite file. The `wisprflow` connector reads it read-only and turns each
dictation (what you said, when, and into which app) into a fact, so ideas you
dictated into Slack or your editor last week are searchable by agents too.
There is no official export API for dictation history — Wispr's remote MCP
server covers Notetaker meetings only and states it cannot read dictation
history, and the Voice Interface API is invite-only and does speech-to-text,
not history — so the local file is the only free source. The schema is
community-documented (`History` table: `transcriptEntityId`, `asrText`,
`formattedText`, `editedText`, `timestamp`, `app`, `url`, `duration`,
`numWords`, `status`, `isArchived`) and can change with an app update.

```yaml
knowledge:
  connectors:
    - name: wispr
      type: wisprflow
      layer: personal
      interval: 15m
      scope:
        # path: /Users/me/Library/Application Support/Wispr Flow/flow.sqlite  # default on macOS; %APPDATA%\Wispr Flow\flow.sqlite on Windows
        # path: /data/knowledge/wispr-export        # or a folder of exported .json/.ndjson/.csv/.md files
        since_days: "30"        # how far back the first sync reaches
        max_items: "500"        # dictations read per sync
        min_words: "3"          # skip "ok", "yes", …
        group: dictation        # dictation = one fact each; day = one fact per calendar day
        apps: ""                # optional allow-list matched against the app bundle id, e.g. "slack, obsidian, vscode"
        include_raw: "false"    # also keep the raw ASR text under "Raw transcript"
        tags: "voice, ideas"    # extra tags written as attr_tags
        # sqlite3: sqlite3      # CLI used to read the file; ships with macOS and the hive image
```

No `auth` block and no sign-in: the data is already on disk. The connector
shells out to the `sqlite3` CLI with `-readonly -json` and selects text
columns only (the table also holds audio and screenshot BLOBs, which are never
read). Rows with `status = formatted` and `isArchived = 0` are used;
`editedText` wins over `formattedText` over `asrText`.

### Setup

1. **Same machine as the hive (laptop hive):** add the connector from
   **Knowledge → 🔌 Integrations → Wispr Flow dictation** and leave `path`
   empty. On macOS the hive process may need **Full Disk Access** (System
   Settings → Privacy & Security) to read `~/Library/Application Support`.
2. **Hive on a cluster or another host:** sync the file or an export to a
   path the hive can read and set `path` to it. Any of these work:
   - copy `flow.sqlite` (or one of the app's timestamped `backups/*.sqlite`)
     into the knowledge PVC, e.g. with a cron `kubectl cp`;
   - run a community exporter such as `wispr export -o <dir>`
     ([iamnikolie/wispr-cli](https://github.com/iamnikolie/wispr-cli), writes
     `dictations/*.md`) or
     [junxit/wispr-flow-exporter](https://github.com/junxit/wispr-flow-exporter)
     (Markdown/JSON/NDJSON) and point `path` at the folder, or commit that
     folder to a Git-synced vault and let the existing `git` connector pick
     it up.
3. Click **Sync now**. New dictations are picked up every `interval`.

Export folders are walked recursively; `.json` (array or
`{"dictations": [...]}`), `.ndjson`/`.jsonl`, `.csv` (header row) and `.md`
(one dictation per file; optional front matter `id:`, `timestamp:`, `app:`,
otherwise the date in the filename or the file mtime) are read, other files
are ignored. A file that fails to parse is reported as a partial sync and the
rest are still imported.

### Behaviour

- Incremental by `timestamp`: each sync re-reads from the newest dictation
  seen minus a 2 h overlap (Flow updates a row in place as it finalises), or
  `since_days` back on the first run. With `group: day` the current day's
  fact is rebuilt whole.
- Dictation facts carry `source: wisprflow`, `source_id`,
  `attr_recorded_at`, `attr_wispr_transcript_id`, `attr_dictated_at`,
  `attr_app` (bundle id), `attr_num_words`, `attr_language`, and the
  dictation date as the vault path (`Wispr Flow/2026-11-11`). The title is
  the first ten words, and the formatted text is under `## Transcript`.
  With `group: day`, the fact represents the entire day and its
  `attr_recorded_at` is the newest dictation's time.
- The listing is incremental, so dictations you archive or delete in Flow
  are not tombstoned automatically; remove the fact file or the connector.
- Everything here costs **$0** beyond your Wispr Flow plan; the free tier's
  weekly word cap applies to dictating, not to reading history. Tip: add
  your project vocabulary (`hive`, `dibs`, `spek`, `ACMM`, `crux`, repo
  names) under Flow → Settings → Dictionary so it is spelled right the first
  time.

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

Connector types that can publish (using any other type is rejected when the mirror is built):

| Type | `root` | What each fact becomes |
| --- | --- | --- |
| `sharepoint` | Folder path | `<root>/<page>.md`, uploaded to the first configured drive or site library. |
| `confluence` | Numeric id of the parent page | A child page titled `<page>` in the parent's space. The markdown is converted to storage format: headings, paragraphs, quotes, lists, rules, code blocks (code macro), bold, italic, inline code and links. The `hive_fact_id` marker is kept as a small visible line because Confluence drops HTML comments. The credential needs permission to add and edit pages in that space. |
| `notion` | Id of the parent page (shared with the integration) | A child page titled `<page>`. The body is sent as heading, paragraph, divider and code blocks, and inline markdown is kept as literal text. An update deletes the page's blocks and appends the new ones. Text is split to fit Notion's limits of 2000 characters per text item and 100 blocks per request. The integration needs the insert, update and read content capabilities. |
| `google-drive` | Folder path under the first `folder_ids` entry (or the first shared drive) | `<root>/<page>.md` (`text/markdown`). Missing folders are created. The token needs the `drive.file` or `drive` scope; `drive.readonly` can only sync. |

`<page>` is the page name `hive-<layer>-<slug>`. Each target finds the existing page or file by that name under `root` and updates it in place, so republishing never creates a duplicate.

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

`hive` starts the mirror at boot when `knowledge.publish.connector` is set. It looks up that `knowledge.connectors` entry and checks that its type can publish. If the entry is missing or its type cannot publish, the hive logs `knowledge publish mirror disabled` with the reason and boots without publishing. Facts are read from the local `path` of each published layer's `knowledge.layers` entry; a layer without a `path` has nothing to publish. Page hashes and the last status persist in `/data/knowledge/connector-state/_publish`, so a restart does not resend unchanged pages.

The mirror publishes once at startup and then on a daily sweep. When scheduled promotion is on (`knowledge.curator.enabled: true`), every promotion sweep that promotes at least one fact also triggers a publish. Each batch produces one audit record (`PublishReport`: created, updated, deprecated, unchanged, collisions, error) that is logged as `knowledge publish batch`.

**Status and Publish now.** The Knowledge-card **Integrations** modal shows publish state beside the selected connector, and **Settings → Knowledge → Connectors** shows a **Publish mirror** block under the connectors table: status, connector, layers, root, last run, published page count, the last batch's created/updated/deprecated/unchanged counts, collisions and last error. Owners get a **Publish now** button. The same data is the `publish` object of `GET /api/knowledge/connectors` and `GET /api/config/knowledge/connectors/status` (`null` when `knowledge.publish` is unset; `active: false` when it is set but the mirror is not running). `POST /api/knowledge/publish/sync` (owner only) runs a batch now: `202` with the publish status, `409` while a batch is running, `503` when the mirror is not running.

### Stopping publishing

- To preview without writing upstream, set `knowledge.publish.dry_run: true`.
- To stop publishing, remove the `knowledge.publish` block or clear `connector`. Pages that were already published stay in the external system. Delete them there if you no longer want them.
