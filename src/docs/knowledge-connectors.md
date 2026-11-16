# Knowledge connectors

External knowledge systems (Confluence, Notion, Google Drive, SharePoint/OneDrive, git repositories, documents, GitHub wikis, Plaud voice notes and Wispr Flow dictation) are synced into the file vault by connectors configured under `knowledge.connectors` in `hive.yaml`. Each upstream page becomes one markdown fact, and agents read those facts like any other vault content. Operators manage connectors from the Knowledge card's **🔌 Integrations** modal or from **Settings → Knowledge → Connectors** in the dashboard.

For the shared configuration fields, the fact format and how connectors relate to `git_sources` and `documents`, see [knowledge-curator.md → Connectors](knowledge-curator.md#connectors-knowledgeconnectors).

## Configuration entry

```yaml
knowledge:
  connectors:
    - name: team-wiki          # lowercase letters, digits, dashes
      type: github-wiki        # confluence | context7 | notion | git | document | github-wiki | google-drive | sharepoint | plaud | wisprflow | zoom | teams | discord | google-meet | webex | slack-huddles | fireflies | fathom | limitless | granola | otter
      enabled: true
      interval: 15m            # minimum 1m; blank = 15m
      layer: project           # personal | project | org | community
      scope:
        repos: org/repo-a, org/repo-b
      auth:
        env: WIKI_TOKEN        # an environment variable NAME, or
        # file: /run/secrets/wiki-token   # an absolute secret file path
```

Only one of `auth.env` / `auth.file` may be set. Inline secrets (an `auth.token`, or secret-looking scope keys such as `api_key`) are rejected everywhere, including by the dashboard. Connectors that talk to an OAuth app the operator registered (`zoom`, `teams`, `google-meet`, `webex`) name that registration the same way, through `auth.client_id_env`, `auth.client_secret_env` and `auth.account_id_env` / `auth.tenant_id_env`: environment variable **names**, never values, plus `auth.mode` (`user_oauth` or `server_to_server`).

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

While a sync runs, the connector's card (and the status cell of the Settings table) shows live progress instead of waiting for the sync to finish: a progress bar, determinate once the connector knows how many items it will process (Plaud reports the recording count right after `list_files`; other connectors count pages as they are written, so their bar is indeterminate), a line such as `fetching 12 of 200 · Standup ideas` naming the current item, and the elapsed time. The `syncing` pill follows the same live state. The dashboard polls the status endpoint every 2.5 s while any connector is syncing and stops when the last one finishes; a sync that fails shows its error inline under the bar. The Knowledge section header carries a compact `syncing: <name> 12/200` indicator so a running sync is visible without opening Integrations.

Owners can **Sync now**, **Edit**, **Disable/Enable** and **Remove** a connector and **Add** new ones from Settings; the Knowledge-card Integrations modal provides the same save, validate, enable/disable and sync actions grouped by connector type. Non-owners see the table read-only. **Validate** inside the Add/Edit dialog is a conservative dry-run: it applies the config rules and the connector's own scope/auth checks and confirms the named environment variable is set (or the file is readable and non-empty). It performs no network access and never echoes the credential.

Status and **Sync now** need the connector syncer, which `hive` starts at boot whenever `knowledge.connectors` is non-empty. When it is not running, the table shows configuration only and Sync now answers `503`.

## API

| Endpoint | Access | Purpose |
| --- | --- | --- |
| `GET /api/config/knowledge/connectors` | dashboard session | Configured connectors and registered types |
| `PUT /api/config/knowledge/connectors` | owner | Replace the list (`{"connectors": [...]}`); the whole list is validated first |
| `GET /api/config/knowledge/connectors/status` | dashboard session | Status rows for the table, each with `running` and the live `progress` block below |
| `POST /api/config/knowledge/connectors/validate` | owner | Dry-run validate one entry |
| `POST /api/config/knowledge/connectors/{name}/sync` | owner | Start a background sync (`202`; `409` if running) |
| `GET /api/knowledge/connectors` | dashboard session | `{"connectors": [...]}`: one status per configured connector, in config order |
| `POST /api/knowledge/connectors/{name}/sync` | owner | Start a background sync now, even if the connector is disabled (`202` with the status; `404` unknown name; `409` if running) |

Saved changes persist to the config overlay and apply at once, without a restart: a hive booted with no connectors starts the syncer on the first save, and on a running syncer an added connector starts syncing, an edited one is rebuilt with its cursor and counts kept, and a disabled or removed one stops. Unchanged connectors keep syncing undisturbed. The `knowledge.publish` mirror is not rebuilt this way; a change to its connector still needs a restart.

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
| `progress` | Live state of the running sync, or of the last one. Not persisted across restarts (starts as `idle`). |

The `progress` object (also on each row of `GET /api/config/knowledge/connectors/status`) has:

| Field | Meaning |
|-------|---------|
| `phase` | `idle` (no sync yet in this process), `listing`, `fetching`, `writing`, `done` or `failed`. The dashboard treats `listing`/`fetching`/`writing` as syncing. |
| `items_total` | Items this sync will process, when known (Plaud: recordings listed); omitted when unknown. |
| `items_done` | Items processed so far: pages written plus items the connector skipped. |
| `current` | Title of the item being fetched or written, truncated to 80 characters; never a credential. |
| `started_at`, `finished_at` | When the sync started and, once over, ended. |
| `last_error` | Error that ended a `failed` sync, or the partial-sync error of a `done` one. |
| `bytes_written`, `facts_written` | Page markdown bytes and pages written to the vault so far. |

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

It talks to the **official, free Plaud MCP server** (`npx -y @plaud-ai/mcp@0.3.14`)
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
        command: npx -y @plaud-ai/mcp@0.3.14   # default (pinned); any stdio MCP command
        since_days: "30"                       # how far back the first sync reaches
        max_files: "200"                       # recordings fetched per sync
        include_audio: "false"                 # also store Plaud's 24h presigned MP3 link in the fact (Inbox playback never needs it)
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
provider API keys). The default command is **pinned** to `@plaud-ai/mcp@0.3.14`
(the `plaudMCPVersion` constant in `src/pkg/knowledge/connector/plaud.go`), so a
sync never pulls a new upstream release unreviewed: the floating `@latest` it
replaced is how 0.3.14's reply envelope broke every sync until
[#18212](https://github.com/hivecommons/hive/pull/18212). The pin is bumped by
hand, as the CLI pins in [cli-pins.md](cli-pins.md) are; a `scope.command` you
set yourself is never rewritten, so an explicit `@latest` still floats.

### Setup

1. The hive host (or container image) needs **Node.js ≥ 20** so `npx` can run
   the Plaud MCP server.
2. Add the connector from **Knowledge → 🔌 Integrations → Plaud voice notes**
   (or the YAML above). A dashboard save loads it at once; a YAML edit
   needs a restart.
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
     @plaud-ai/mcp@0.3.14 install` signed in. See
     [Inbox and voice sources](inbox-voice-sources.md#hosted-hives-signing-in-to-plaud).
4. Click **Sync now**. New recordings are picked up every `interval` and
   appear in the **📥 Inbox**, where you file them as a spektacular RFC or
   keep them as knowledge only
   ([Inbox and voice sources](inbox-voice-sources.md)).

### Behaviour

- Each sync calls `list_files` (from the newest recording seen minus a 24 h
  overlap, or `since_days` back on the first run), then `get_note` and
  `get_transcript` for each recording. A recording whose transcript and
  summary are both still processing is skipped and retried next sync; the
  status reports it as a partial sync. The card shows the recording count
  as soon as the listing is in and the recording being fetched after that
  (`progress.items_total`, `progress.current`); a first sync of 200
  recordings is two or three MCP calls each, so the bar is what tells you
  it is moving.
- Facts carry `source: plaud`, `attr_plaud_file_id`, `attr_recorded_at`,
  `attr_duration_ms`, `attr_device_serial` and `attr_plaud_content`
  (`ready`, or `pending` while Plaud has produced neither a summary nor a
  transcript; pending facts are re-fetched on every sync, 25 at most). The
  body has `## Summary (Plaud)`, `## Transcript` (the best block the
  recording has: `transaction_polish`, `transaction`, `outline`) and
  `## Highlights` (device-button marks) sections. A sync never downloads
  audio; with `include_audio: true` the fact also links Plaud's 24-hour
  presigned URL. The Inbox plays audio by streaming it through the hive on
  demand regardless, nothing is stored (see
  [inbox-voice-sources.md](inbox-voice-sources.md)). Recordings Plaud never
  transcribed can be transcribed on the hive with the owner's own Deepgram
  key ([inbox-transcription.md](inbox-transcription.md)).
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

## Zoom cloud recordings (`type: zoom`)

The Zoom connector turns cloud recordings into knowledge facts: one fact per
recorded meeting with the topic, start time, duration, host, participants,
Zoom's AI meeting summary (when the account produces one) and the transcript
rendered as `[mm:ss] Speaker: text` lines from Zoom's VTT file. Recordings
land in the **📥 Inbox** with inline playback, next to Plaud recordings. The
design record is [design/meeting-recordings.md](design/meeting-recordings.md).

```yaml
knowledge:
  connectors:
    - name: calls
      type: zoom
      layer: personal
      interval: 15m
      auth:
        mode: user_oauth                 # my own recordings; or server_to_server for the account
        client_id_env: ZOOM_CLIENT_ID    # environment variable NAMES, never values
        client_secret_env: ZOOM_CLIENT_SECRET
        # account_id_env: ZOOM_ACCOUNT_ID   # server_to_server only
      scope:
        users: me                        # server_to_server: user ids or emails, comma-separated
        since_days: "30"
        max_recordings: "200"
        include_summary: "true"          # GET /meetings/{uuid}/meeting_summary (meeting_summary:read)
        include_participants: "true"     # GET /past_meetings/{uuid}/participants
        include_video_link: "false"      # also store the Zoom play link in the fact (playback never needs it)
        redirect_uri: http://localhost:8199/zoom/callback   # user_oauth: the redirect registered on the app
        tags: "meetings"
```

### Setup

1. On the [Zoom App Marketplace](https://marketplace.zoom.us) create an OAuth
   app: **General app** (user-managed) for your own recordings, or
   **Server-to-Server OAuth** for every user of the account. Add the scopes
   `cloud_recording:read:list_user_recordings`,
   `cloud_recording:read:list_recording_files` and `user:read:user` (classic
   names: `recording:read`, `user:read`), and optionally
   `meeting_summary:read` and `meeting:read:list_past_participants` for the AI
   summary and the participant list; Server-to-Server apps use the `:admin`
   variants. For a General app add the redirect URL (default
   `http://localhost:8199/zoom/callback`) to the OAuth allow list.
2. Put the client id and secret (and the account id for Server-to-Server) in
   the hive's environment, and add the connector from **Knowledge → 🔌
   Integrations → Zoom recordings**, naming those variables in the form.
3. Click **Connect Zoom**. User-level: the card shows the authorization URL;
   approve it, then paste the `…/zoom/callback?code=…&state=…` URL the
   browser lands on (a connection error page is expected) into **Finish Zoom
   sign-in by pasting** within 10 minutes. The hive exchanges the code and
   keeps the refresh token at
   `/data/knowledge/connector-state/<name>/home/.zoom/tokens.json` (mode
   0600); refresh tokens rotate on every refresh and the new one is written
   back. Server-to-Server: Connect requests an account token and reports
   "connected" at once. Click **Check** any time to re-probe.
4. Click **Sync now**. Recordings appear in the Inbox; ▶ Play fetches the
   audio-only file (else the video) with a one-hour download token appended
   as `access_token`, minted per click and never stored.

### Behaviour

- Each sync lists `GET /users/{user}/recordings` in month-sized `from`/`to`
  windows from the newest recording seen (minus 48 h) back, following
  `next_page_token`, de-duplicated by meeting instance UUID; the first sync
  reaches `since_days` back and stops at `max_recordings` (reported as
  truncated). Per recording it downloads the `TRANSCRIPT` file with the
  bearer token and renders it as VTT, asks for the meeting summary and the
  participants when enabled (a missing scope only drops that section), and
  writes the fact. Recordings without transcript or summary are marked
  `attr_meeting_content: pending` and re-read on later syncs (25 at most).
- Facts carry `source: zoom`, `attr_zoom_recording_id` (meeting UUID, the
  Inbox item id), `attr_zoom_meeting_id`, `attr_recorded_at`,
  `attr_duration_ms`, `attr_host`, `attr_participants` and `attr_tags`; the
  body has `## Summary (Zoom)` and `## Transcript` sections.
- A `401` from Zoom reads as *not connected* with a pointer to the card; the
  download link host must be a `zoom.us` host, so the token never leaves
  Zoom.

## Microsoft Teams recordings (`type: teams`)

The Teams connector reads meeting recordings and transcripts for the
organizers you list through Microsoft Graph **application permissions**
(client credentials, no signed-in user): one fact per recorded meeting with
the subject, start time, duration, organizer, attendees and the transcript
rendered from Graph's VTT. It is complete except where the tenant
administrator has to act; the Integrations card says so when Graph refuses.

```yaml
knowledge:
  connectors:
    - name: meetings
      type: teams
      layer: project
      auth:
        tenant_id_env: TEAMS_TENANT_ID   # environment variable NAMES, never values
        client_id_env: TEAMS_CLIENT_ID
        client_secret_env: TEAMS_CLIENT_SECRET
      scope:
        users: 8b081ef6-4792-4def-b2c9-c363a1bf41d5, someone@contoso.example   # meeting organizers
        # group: <group object id>       # or every member of a group (GroupMember.Read.All)
        since_days: "30"
        max_recordings: "200"
        include_video_link: "false"      # also store the Teams meeting link in the fact
```

### Setup (tenant administrator)

1. In Microsoft Entra → App registrations, register an app and create a
   client secret. Under API permissions add the **application** permissions
   `OnlineMeetingRecording.Read.All` and `OnlineMeetingTranscript.Read.All`;
   optionally `OnlineMeetings.Read.All` (meeting subject, times, attendees),
   `User.Read.All` (list organizers by UPN instead of object id) and
   `GroupMember.Read.All` (`scope.group`). Grant **admin consent**.
2. Create an application access policy for the app and grant it to the
   organizers (or the tenant): `New-CsApplicationAccessPolicy -Identity
   hive-recordings -AppIds "<client id>"` then
   `Grant-CsApplicationAccessPolicy -PolicyName hive-recordings -Identity
   "<user object id>"` (or `-Global`). Changes take up to 30 minutes.
3. Make sure the tenant allows Graph API access to meeting transcripts
   (Teams admin setting); otherwise Graph answers `403
   GraphAccessToTranscriptsDisabled`.
4. Put the tenant id, client id and secret in the hive's environment and add
   the connector from **Knowledge → 🔌 Integrations → Microsoft Teams
   recordings**. **Connect** fetches a token and probes the first organizer's
   recordings; when consent or the policy is missing the card shows
   *Requires Entra admin consent for OnlineMeetingRecording.Read.All and
   OnlineMeetingTranscript.Read.All* with the admin-consent link.

### Behaviour

- Each sync calls, per organizer,
  `GET /users/{id}/onlineMeetings/getAllRecordings(meetingOrganizerUserId='{id}',startDateTime=…,endDateTime=…)`
  and `getAllTranscripts(...)` following `@odata.nextLink`, groups the
  artifacts by `meetingId`, reads the meeting (`GET
  /users/{id}/onlineMeetings/{meetingId}`, best effort) and the newest
  transcript's `/content` as `text/vtt`, falling back to the unattributed
  `application/vnd.microsoft.graph.transcript+text` when the tenant forbids
  speaker attribution.
- Facts carry `source: teams`, `attr_teams_recording_id` (the callRecording
  id, or `transcript:<id>` for a transcript-only meeting),
  `attr_teams_meeting_id`, `attr_recorded_at`, `attr_duration_ms`,
  `attr_host`, `attr_participants`; the body has a `## Transcript` section
  (Graph exposes no meeting summary).
- Playback: Graph serves recording content behind a bearer token, which a
  browser media element cannot send, so ▶ Play answers *no playable media*
  for Teams items until a streaming proxy exists (tracked in the design
  record). Transcripts and metadata sync regardless.

## Discord meetings (`type: discord`)

Discord has no cloud recordings and no transcripts, so the Discord connector
reads what a meeting leaves behind, with a bot token through the Discord REST
API, and writes one fact per item:

- **Recordings posted to a channel.** List the channels where finished
  recordings land (`scope.channels`). A message counts as a recording when it
  carries an audio attachment (`.ogg`, `.flac`, `.mp3`, `.m4a`, `.wav`,
  `.opus`, ...) or a recording-bot download link (by default Craig's
  `craig.horse` / `craig.chat` links; `scope.link_hosts` replaces the list).
  Craig sends its download link to the person who started the recording, so
  forward or paste it into the recordings channel, or upload the exported
  audio there. A `.vtt` / `.srt` / `.txt` attachment on the same message is
  read as the transcript.
- **Scheduled events.** With `scope.guild`, every active or completed
  scheduled event becomes a meeting item: name, start/end, creator and
  interested users as the envelope, the event description as the summary and
  the event channel's text chat between start and end as the transcript, so
  an Inbox item exists even when nothing was recorded.

```yaml
knowledge:
  connectors:
    - name: community
      type: discord
      layer: project
      auth:
        env: DISCORD_BOT_TOKEN          # environment variable NAME, never the value
      scope:
        guild: "111111111111111111"     # server id: scheduled events
        channels: "222222222222222222"  # recording channel ids (comma-separated)
        # recorder_bots: "<craig user id>"  # only messages from these user ids count
        since_days: "30"
        max_recordings: "200"
        include_events: "true"          # scheduled events as meeting items (needs guild)
        include_chat: "true"            # the event channel chat as the transcript
        include_participants: "true"    # interested users
        include_video_link: "false"     # also store the recording-bot link (it carries the recording's key) in the fact
```

### Setup

1. In the Discord Developer Portal create an application, add a **bot**, and
   turn on the **Message Content** privileged intent (without it the API
   returns messages with empty text, attachments and embeds).
2. Invite the bot to the server with **View Channel** and **Read Message
   History** only (permissions integer `66560`). **Connect** on the card
   shows the invite link when the bot is not in the server or cannot read a
   configured channel.
3. Put the bot token in the hive's environment and add the connector from
   **Knowledge → 🔌 Integrations → Discord meetings**, with the credential
   source set to *Environment variable* and the variable name. **Connect**
   reads the bot user (`GET /users/@me`), the server and each channel.

### Behaviour

- Recording channels are walked back from the newest message with
  `GET /channels/{id}/messages?before=…` (100 per page) until `since`; event
  chat is read forward from the event start with `after=…` until its end (or
  now while it is active). A `429` is retried after the `retry_after` the
  reply carries (capped at a minute). A `401` reads as *not connected*; a
  `403`, or a `404 Unknown Guild/Channel`, as *missing access* with the
  invite link.
- Facts carry `source: discord`, `attr_discord_recording_id` (`m:<channel
  id>:<message id>` or `e:<event id>`), `attr_discord_channel_id`,
  `attr_discord_message_id` or `attr_discord_meeting_id` (the event id),
  `attr_discord_event_status`, `attr_recorded_at`, `attr_duration_ms`,
  `attr_host`, `attr_participants`.
- Chat messages and event descriptions are written by server members, so they
  are wrapped in an untrusted-data envelope (a preamble plus an
  `<untrusted-user-data-…>` block with a random tag name, the shape the Plaud
  MCP server uses) before they reach the fact, the Inbox and spek.
- Items without text (an audio upload or bot link without a transcript file)
  are marked `attr_meeting_content: pending`, shown as *not transcribed yet*
  in the Inbox and re-read on every sync. The hive does not transcribe audio
  itself. An event that was still active is re-read once it ends, while
  Discord still returns it.
- Playback: ▶ Play re-reads the message for a freshly signed CDN URL of the
  audio attachment (it expires at the URL's own `ex` time); recording-bot
  links and events have no playable file. The bot token is never sent to the
  CDN.
- Not in v1: a hive bot that joins voice channels and records itself. That
  needs a long-lived gateway voice connection and per-speaker consent
  handling, so it is left as future work.

## Google Meet recordings (`type: google-meet`)

The Google Meet connector reads [conference records](https://developers.google.com/workspace/meet/api/guides/conferences)
through the [Google Meet REST API v2](https://developers.google.com/workspace/meet/api/reference/rest/v2):
one fact per meeting that produced a recording, a transcript or Gemini notes,
with the event title, start time, duration, participants, the Gemini *Take
notes for me* document as the summary and the transcript rendered as
`[mm:ss] Speaker: text` lines from the structured
[transcript entries](https://developers.google.com/workspace/meet/api/reference/rest/v2/conferenceRecords.transcripts.entries).
Meetings land in the **📥 Inbox** next to Zoom and Teams items.

```yaml
knowledge:
  connectors:
    - name: meet
      type: google-meet
      layer: personal
      interval: 15m
      auth:
        mode: user_oauth                        # my own meetings; or server_to_server (service account)
        client_id_env: GOOGLE_MEET_CLIENT_ID    # environment variable NAMES, never values
        client_secret_env: GOOGLE_MEET_CLIENT_SECRET
        # server_to_server instead: the service-account key JSON
        # env: GOOGLE_MEET_SA_KEY               # or file: /run/secrets/meet-sa.json
      scope:
        # users: alice@example.com, bob@example.com   # server_to_server: Workspace users to impersonate
        since_days: "30"
        max_recordings: "200"
        include_summary: "true"                 # smartNotes (Gemini notes) exported from Docs
        include_participants: "true"
        include_video_link: "false"             # also store the Drive recording link in the fact
        redirect_uri: http://localhost:8199/google-meet/callback   # user_oauth: the redirect registered on the client
        tags: "meetings"
```

### Setup

1. In the Google Cloud console enable the **Google Meet REST API** and the
   **Google Drive API** for a project, and add the scopes
   `https://www.googleapis.com/auth/meetings.space.readonly` and
   `https://www.googleapis.com/auth/drive.meet.readonly` to its OAuth consent
   screen (both are restricted/sensitive scopes: an *Internal* consent screen
   in a Workspace organization avoids Google verification).
2. **User-level OAuth** (your own meetings): create an OAuth client of type
   *Web application* with the redirect URL (default
   `http://localhost:8199/google-meet/callback`) as an authorized redirect
   URI, put its id and secret in the hive's environment, and add the
   connector from **Knowledge → 🔌 Integrations → Google Meet recordings**.
   Click **Connect Google Meet**, approve, and paste the
   `…/google-meet/callback?state=…&code=…` URL the browser lands on (a
   connection error page is expected) into **Finish Google sign-in by
   pasting** within 10 minutes. The hive asks for offline access, exchanges
   the code and keeps the refresh token at
   `/data/knowledge/connector-state/<name>/home/.google-meet/tokens.json`
   (mode 0600).
3. **Service account** (Workspace users): create a service account and a JSON
   key, have a Workspace super admin authorize its client id for the two
   scopes above under *Admin console → Security → API controls → Domain-wide
   delegation*, set `auth.mode: server_to_server`, point `auth.env` or
   `auth.file` at the key, and list the users in `scope.users`. Connect signs
   an RS256 assertion per user (`sub` = the user) and reports the outcome.
4. Click **Check** any time to re-probe, **Sync now** to sync.

### Behaviour

- Each sync pages `GET /v2/conferenceRecords?filter=start_time>="…"`
  (`pageSize` 100, `nextPageToken`) from the newest meeting seen (minus 48 h)
  back to `since_days`, skips meetings still in progress, and for each
  meeting lists `recordings`, `transcripts` and `smartNotes`; meetings with
  none of the three are not facts. `conferenceRecords.list` only returns
  meetings the user **organized**.
- Title: the Meet-created Drive file name (the calendar event title, with
  the date and the " - Transcript" / " - Notes by Gemini" suffix removed),
  else `Google Meet <meeting code>`. Transcript: `transcripts.entries` with
  each `participant` resolved through `participants`; when the entries are
  empty the Docs transcript is exported as text. Summary: the newest
  `FILE_GENERATED` smart note exported as text (`smartNotes` needs Gemini in
  Meet; without it only the summary is dropped).
- Meetings with a recording but no transcript or notes are marked
  `attr_meeting_content: pending` and re-read on later syncs (25 at most).
- Facts carry `source: google-meet`, `attr_google_meet_recording_id` (the
  conference record id, the Inbox item id), `attr_google_meet_meeting_id`
  (the meeting code), `attr_recorded_at`, `attr_duration_ms`,
  `attr_participants`, `attr_host` (service account: the impersonated user).
- Playback: Meet stores the MP4 as a Drive file that Drive serves only
  behind a bearer token, so ▶ Play answers *no playable media* and names the
  Drive page (`driveDestination.exportUri`) instead. Text syncs regardless.
- A `401` reads as *not connected* with a pointer to the card; a `403` keeps
  Google's reason (API not enabled, scope not granted). `429` and `5xx` are
  retried honouring `Retry-After`. The bearer token is only sent to the Meet
  and Drive API hosts.

## Webex recordings (`type: webex`)

The Webex connector reads Webex Meetings through a Webex **integration**
(OAuth as the signed-in user): one fact per ended meeting instance that has
a recording or a transcript, with the topic, start time, duration, host,
participants, the Webex AI Assistant summary (notes and action items, when
the site has AI Assistant) and the transcript rendered as `[mm:ss] Speaker:
text` lines from Webex's VTT. Meetings land in the **📥 Inbox** with inline
playback.

```yaml
knowledge:
  connectors:
    - name: webex
      type: webex
      layer: personal
      interval: 15m
      auth:
        client_id_env: WEBEX_CLIENT_ID          # environment variable NAMES, never values
        client_secret_env: WEBEX_CLIENT_SECRET
      scope:
        since_days: "30"
        max_recordings: "200"
        include_summary: "true"                 # GET /meetingSummaries (meeting:summaries_read, AI Assistant)
        include_participants: "true"            # GET /meetingParticipants (host only)
        include_video_link: "false"             # also store the Webex playback link in the fact
        # site_url: acme.webex.com              # default: every site of the user
        redirect_uri: http://localhost:8199/webex/callback
        # oauth_scopes: meeting:recordings_read meeting:transcripts_read   # narrow the request
        tags: "meetings"
```

### Setup

1. On [developer.webex.com](https://developer.webex.com/my-apps) create an
   **Integration** with the redirect URI (default
   `http://localhost:8199/webex/callback`) and the scopes
   `meeting:recordings_read`, `meeting:transcripts_read`,
   `meeting:summaries_read` (Webex AI Assistant), `meeting:participants_read`
   and `spark:people_read`. If the integration has fewer scopes, list the ones
   it has in `scope.oauth_scopes`: Webex rejects an authorization request for
   scopes the integration was not registered with.
2. Put the client id and secret in the hive's environment and add the
   connector from **Knowledge → 🔌 Integrations → Webex recordings**.
3. Click **Connect Webex**, approve, and paste the
   `…/webex/callback?code=…&state=…` URL the browser lands on into **Finish
   Webex sign-in by pasting** within 10 minutes. The hive exchanges the code
   and keeps the tokens at
   `/data/knowledge/connector-state/<name>/home/.webex/tokens.json` (mode
   0600); access tokens last 14 days and refresh tokens 90 days, and a
   rotated refresh token is written back on every refresh.

### Behaviour

- Each sync lists [`GET /recordings`](https://developer.webex.com/docs/api/v1/recordings/list-recordings)
  and [`GET /meetingTranscripts`](https://developer.webex.com/docs/api/v1/meeting-transcripts/list-meeting-transcripts)
  in explicit 30-day `from`/`to` windows (the API defaults a missing bound to
  7 days), `max=100`, following the `Link: rel="next"` header, and groups
  both by meeting instance id (`meetingId`, the Inbox item id). Deleted
  recordings and transcripts are skipped. Per meeting it downloads the newest
  transcript as VTT (`/meetingTranscripts/{id}/download?format=vtt`), asks
  [`/meetingSummaries`](https://developer.webex.com/docs/api/v1/meeting-summaries)
  for the AI Assistant notes (HTML, converted) and action items, and lists
  the participants; a missing scope or feature only drops that section.
- Meetings with a recording but no transcript or summary are
  `attr_meeting_content: pending` and re-read on later syncs (25 at most).
- Facts carry `source: webex`, `attr_webex_recording_id` and
  `attr_webex_meeting_id` (both the meeting instance id), `attr_recorded_at`,
  `attr_duration_ms`, `attr_host`, `attr_participants`.
- Playback: ▶ Play asks [`GET /recordings/{id}`](https://developer.webex.com/docs/api/v1/recordings/get-recording-details)
  for its `temporaryDirectDownloadLinks` and returns the audio link (else the
  MP4), which needs no token and expires 3 hours after the request; nothing
  is stored. With *Prevent downloading* on, Webex returns no link and the
  error names the playback page.
- A `401` or a dead refresh token reads as *not connected* with a pointer to
  the card; a `403` keeps Webex's reason. `429` and `5xx` are retried
  honouring `Retry-After`. The bearer token is only sent to `webexapis.com`.

## Slack huddle notes (`type: slack-huddles`)

Slack does not record huddle audio or video and has no huddle-recording API.
What it keeps is the **AI notes canvas** it posts in the huddle's thread when
someone turns on AI notes (paid plans; not in huddles with external people),
which embeds the transcript when the workspace allows transcripts. The
connector reads those canvases: one fact per ended huddle that has notes,
with the canvas title (else *Huddle in #channel*), start time, duration,
host, participants, the notes as the summary and the transcript section as
the transcript. Items land in the **📥 Inbox** as text only.

```yaml
knowledge:
  connectors:
    - name: huddles
      type: slack-huddles
      layer: project
      interval: 15m
      auth:
        env: SLACK_HUDDLES_TOKEN       # variable NAME holding an xoxb- (bot) or xoxp- (user) token
      scope:
        channels: C0123ABC, G0456DEF   # conversation ids whose huddles to read
        since_days: "30"
        max_recordings: "200"
        include_participants: "true"   # users.info (users:read)
        tags: "huddles"
```

### Setup

1. Create a Slack app (api.slack.com/apps), add the scopes
   `channels:history`, `groups:history` (private channels), `canvases:read`,
   `files:read`, `users:read` and `channels:read` / `groups:read`, install it
   in the workspace and `/invite` it to each channel. For huddles in DMs use
   a user token with `im:history` / `mpim:history` instead.
2. Put the token in the hive's environment and add the connector from
   **Knowledge → 🔌 Integrations → Slack huddle notes** with the credential
   source *Environment variable* naming that variable. **Connect** / **Check**
   call `auth.test` and read one message of the first channel, so a missing
   scope (`needs scope …`) or a missing channel membership shows on the card.

### Behaviour

- Each sync pages [`conversations.history`](https://docs.slack.dev/reference/methods/conversations.history)
  from the newest huddle seen (minus 48 h, which also catches notes Slack
  writes a little after the huddle ends) back to `since_days`, following
  `response_metadata.next_cursor`, and keeps the `huddle_thread` messages
  whose [`room`](https://docs.slack.dev/reference/objects/conversation-object)
  has ended (`has_ended` / `date_end`). Per huddle it reads the thread
  (`conversations.replies`) and the room's `attached_file_ids`
  (`files.info`) for canvases, prefers the one titled as huddle notes, and
  reads it as markdown with [`canvases.getContent`](https://docs.slack.dev/reference/methods/canvases.getContent);
  without `canvases:read` it falls back to the canvas file's private
  download (`files:read`, HTML converted). A `Transcript` heading in the
  canvas splits notes from transcript. Huddles without a notes canvas are
  skipped: nothing else exists to file.
- Facts carry `source: slack-huddles`, `attr_slack_huddle_recording_id`
  (`<channel>:<message ts>`, the Inbox item id), `attr_slack_huddle_meeting_id`
  (the room id), `attr_recorded_at`, `attr_duration_ms`, `attr_host`,
  `attr_participants`; the recording page is the huddle message permalink.
- Playback: none (▶ Play answers 404, Slack keeps no audio). Slack answers
  `ok: false` with HTTP 200: `invalid_auth`, `token_revoked` and the like
  read as *not connected*; `missing_scope` names the scope needed;
  `not_in_channel` asks for `/invite`. `429` is retried honouring
  `Retry-After`. The token is only sent to `slack.com` hosts.

## Fireflies.ai meetings (`type: fireflies`)

[Fireflies.ai](https://fireflies.ai) sends a notetaker to Zoom, Meet, Teams
and other calls and keeps a transcript, speakers and an AI summary per
meeting. The connector reads them through the
[Fireflies GraphQL API](https://docs.fireflies.ai/graphql-api/query/transcripts)
with an API key: one fact per meeting with title, date, duration, organizer,
attendees, the summary and the transcript. Items land in the **📥 Inbox**
with playback.

```yaml
knowledge:
  connectors:
    - name: fireflies
      type: fireflies
      layer: project
      interval: 15m
      auth:
        env: FIREFLIES_API_KEY         # variable NAME holding the Fireflies API key
      scope:
        since_days: "30"
        max_recordings: "200"
        mine: "false"                  # true: only meetings the key's owner organized
        include_summary: "true"        # summary { overview action_items ... }
        include_participants: "true"   # meeting_attendees / participants
        tags: "meetings"
```

### Setup

1. In Fireflies open **Integrations → Fireflies API** and copy the API key
   ([authorization](https://docs.fireflies.ai/fundamentals/authorization)).
2. Put it in the hive's environment and add the connector from
   **Knowledge → 🔌 Integrations → Fireflies.ai meetings** with the
   credential source *Environment variable* naming that variable.
   **Connect** / **Check** read the key's owner (`user { email name }`), so a
   dead key shows on the card before the first sync.

### Behaviour

- Each sync pages [`transcripts`](https://docs.fireflies.ai/graphql-api/query/transcripts)
  (`limit: 50`, `skip`, `fromDate`, optional `mine`) from the newest meeting
  seen (minus 48 h, which also catches summaries that finish later) back to
  `since_days`, and asks for `sentences` and `summary` in the same query, so a
  sync costs one request per 50 meetings. The Inbox listing asks for the
  metadata only; opening an item reads
  [`transcript(id)`](https://docs.fireflies.ai/graphql-api/query/transcript).
- Facts carry `source: fireflies`, `attr_fireflies_recording_id` (the
  transcript id, the Inbox item id), `attr_recorded_at` (`date`, epoch ms),
  `attr_duration_ms` (Fireflies reports `duration` in minutes), `attr_host`
  (`organizer_email`), `attr_participants`; the recording page is the
  `transcript_url`. A meeting with neither sentences nor summary yet is
  `attr_meeting_content: pending` and re-read on later syncs.
- Playback: `audio_url` (else `video_url`), hashed links that
  [expire after 24 hours](https://docs.fireflies.ai/schema/transcript) and
  need a Pro plan or higher (`paid_required` answers 404 with that reason).
  The hive mints one per click and never stores it.
- Errors: Fireflies answers GraphQL `errors` with a `code`
  ([error codes](https://docs.fireflies.ai/miscellaneous/error-codes));
  `auth_failed` or HTTP 401 reads as *not connected*. `429`
  (`too_many_requests`) is retried honouring `Retry-After`; the
  [limits](https://docs.fireflies.ai/fundamentals/limits) are 30 requests a
  minute and 500 a day on Free, 60 a minute on Pro. The key is only sent to
  `api.fireflies.ai`.

## Fathom meetings (`type: fathom`)

[Fathom](https://fathom.video) records Zoom, Meet and Teams calls and keeps
a transcript, an AI summary and action items per call. The connector reads
them through the [Fathom API](https://developers.fathom.ai/api-overview)
with an API key: one fact per recording with title, recording time and
duration, the recorder as host, the calendar invitees, the summary and
action items, and the transcript. Items land in the **📥 Inbox** with
playback.

```yaml
knowledge:
  connectors:
    - name: fathom
      type: fathom
      layer: project
      interval: 15m
      auth:
        env: FATHOM_API_KEY            # variable NAME holding the Fathom API key (sent as X-Api-Key)
      scope:
        since_days: "30"
        max_recordings: "200"
        recorded_by: ""                # optional: only calls these users recorded (emails)
        teams: ""                      # optional: only these Fathom teams
        include_summary: "true"        # default_summary + action_items
        include_participants: "true"   # calendar_invitees
        tags: "meetings"
```

### Setup

1. In Fathom open **User Settings → API Access** and generate an API key
   ([quickstart](https://developers.fathom.ai/quickstart)). The key belongs
   to its user: it sees the meetings that user recorded and the ones shared
   with them or their team, never other users' private meetings.
2. Put it in the hive's environment and add the connector from
   **Knowledge → 🔌 Integrations → Fathom meetings** with the credential
   source *Environment variable* naming that variable. **Connect** /
   **Check** list one page of meetings, so a dead key shows on the card.

### Behaviour

- Each sync pages [`GET /meetings`](https://developers.fathom.ai/api-reference/meetings/list-meetings)
  (`created_after`, `next_cursor`, optional `recorded_by[]` / `teams[]`)
  from the newest call seen (minus 48 h, which also catches summaries Fathom
  writes after the call) back to `since_days`, with `include_transcript`,
  `include_summary` and `include_action_items`, so the text comes in the
  same pages. The Inbox listing asks for the metadata only; opening an item
  reads [`/recordings/{id}/transcript`](https://developers.fathom.ai/api-reference/recordings/get-transcript)
  and [`/recordings/{id}/summary`](https://developers.fathom.ai/api-reference/recordings/get-summary).
- Facts carry `source: fathom`, `attr_fathom_recording_id` (the recording
  id, the Inbox item id), `attr_recorded_at` (`recording_start_time`),
  `attr_duration_ms` (to `recording_end_time`), `attr_host` (`recorded_by`),
  `attr_participants`; the recording page is the `share_url`. A call with
  neither transcript nor summary yet is `attr_meeting_content: pending` and
  re-read on later syncs.
- Playback: [`POST /recordings/{id}/download`](https://developers.fathom.ai/api-reference/recordings/request-a-download)
  starts a download and `GET /recordings/{id}/downloads/{download_id}`
  polls it; a completed download's `audio` (else `video`) `url` is a signed
  link valid until its `expires_at` (about 24 hours). `422` (no
  downloadable media) answers 404. The hive never stores the link.
- Errors: `401` reads as *not connected*; `403` names limited-access
  sharing. `429` is retried honouring `Retry-After`
  ([rate limits](https://developers.fathom.ai/api-overview): 60 calls a
  minute, 30 a minute for transcript and summary reads, 30 download
  requests a minute). The key is only sent to `api.fathom.ai`.

## Limitless Pendant lifelogs (`type: limitless`)

The [Limitless](https://www.limitless.ai) Pendant records conversations and
Limitless turns them into transcribed **lifelogs**. The connector reads them
through the [Limitless Developer API](https://www.limitless.ai/developers)
with an API key: one fact per lifelog with title, start time, duration, the
speakers, the topic headings as summary and the spoken lines as transcript.
Items land in the **📥 Inbox** as text.

The Developer API is in beta and covers Pendant data only. Limitless was
acquired by Meta in December 2025 and has said it keeps serving existing
Pendant owners for at least a year; service has already ended in some
regions (EU, UK), where the API answers *not connected*.

```yaml
knowledge:
  connectors:
    - name: pendant
      type: limitless
      layer: personal
      interval: 15m
      auth:
        env: LIMITLESS_API_KEY         # variable NAME holding the Limitless API key (sent as X-API-Key)
      scope:
        since_days: "30"
        max_recordings: "200"
        starred_only: "false"          # true: only starred lifelogs (isStarred)
        include_participants: "true"   # speakerName of the transcript lines
        tags: "pendant"
```

### Setup

1. In the Limitless desktop or web app open the **Developer** settings and
   create an API key.
2. Put it in the hive's environment and add the connector from
   **Knowledge → 🔌 Integrations → Limitless Pendant** with the credential
   source *Environment variable* naming that variable. **Connect** /
   **Check** list one lifelog, so a dead key shows on the card.

### Behaviour

- Each sync pages `GET /v1/lifelogs` (`start` in `YYYY-MM-DD HH:mm:SS`
  with `timezone=UTC`, `direction=desc`, `limit=10`, the API maximum,
  following `meta.lifelogs.nextCursor`) from the newest lifelog seen (minus
  48 h) back to `since_days`, with `includeContents`, so the transcript
  comes in the same pages. The Inbox listing asks for metadata only;
  opening an item reads `GET /v1/lifelogs/{id}`. Without contents the
  lifelog `markdown` is the transcript.
- Facts carry `source: limitless`, `attr_limitless_recording_id` (the
  lifelog id, the Inbox item id), `attr_recorded_at` (`startTime`),
  `attr_duration_ms` (to `endTime`) and `attr_participants`.
- Playback: none. Audio comes only from `GET /v1/download-audio` (Ogg Opus,
  two hours per request) with the `X-API-Key` header, which the hive never
  hands to a browser; ▶ Play answers 404 and the Deepgram fallback does not
  apply (Limitless transcribes every lifelog itself).
- Errors: `401` / `403` read as *not connected*. `429` is retried
  honouring `Retry-After` (180 requests a minute per key; the body's
  `retryAfter` is named in the error). The key is only sent to
  `api.limitless.ai`.

## Granola meeting notes (`type: granola`)

[Granola](https://www.granola.ai) transcribes meetings on the device and
writes AI notes next to your own. The connector reads them through the
[Granola API](https://docs.granola.ai/introduction) with an API key: one
fact per meeting with title (else the calendar event title), scheduled
time, duration, owner, attendees (else the calendar invitees), the notes
and the transcript. Items land in the **📥 Inbox** as text.

```yaml
knowledge:
  connectors:
    - name: granola
      type: granola
      layer: project
      interval: 15m
      auth:
        env: GRANOLA_API_KEY           # variable NAME holding the grn_… API key
      scope:
        since_days: "30"
        max_recordings: "200"
        folder: ""                     # optional folder_id
        space: ""                      # optional space_id
        include_private_notes: "true"  # your own notes under "My notes"
        include_participants: "true"
        tags: "meetings"
```

### Setup

1. Create an API key (`grn_…`) in Granola's API settings
   ([Granola API](https://docs.granola.ai/help-center/sharing/integrations/granola-api)).
   A personal key reads your notes; a workspace key reads public notes and
   the spaces with API access turned on.
2. Put it in the hive's environment and add the connector from
   **Knowledge → 🔌 Integrations → Granola meeting notes** with the
   credential source *Environment variable* naming that variable.
   **Connect** / **Check** list one note, so a dead key shows on the card.

### Behaviour

- Each sync pages [`GET /v1/notes`](https://docs.granola.ai/api-reference/list-notes)
  (`updated_after`, `page_size=30`, the maximum, `cursor` while `hasMore`,
  optional `folder_id` / `space_id`) from the newest update seen (minus
  48 h) back to `since_days`, skips deleted notes and reads each with
  [`GET /v1/notes/{id}?include=transcript`](https://docs.granola.ai/api-reference/get-note);
  a `413` (transcript too large to inline) falls back to the paged
  [`/v1/notes/{id}/transcript`](https://docs.granola.ai/api-reference/get-transcript).
  A note without generated notes answers `NOTE_NOT_FOUND` and is picked up
  by a later sync.
- Facts carry `source: granola`, `attr_granola_recording_id` (the note id,
  the Inbox item id), `attr_recorded_at` (the calendar event's
  `scheduled_start_time`, else `created_at`), `attr_duration_ms` (the
  transcript span, else the scheduled length), `attr_host` (`owner`) and
  `attr_participants`; the recording page is the `web_url`.
- Playback: none (▶ Play answers 404, Granola keeps no audio).
- Errors: `401` / `403` read as *not connected*. `429` is retried
  honouring `Retry-After`. The key is only sent to `public-api.granola.ai`.

## Otter.ai conversations (`type: otter`)

[Otter.ai](https://otter.ai) records and transcribes meetings and writes an
AI summary, an outline and action items per conversation. The connector
reads them through the
[Otter.ai Public API](https://help.otter.ai/hc/en-us/articles/36130822688279-Otter-ai-Public-API),
which Otter offers to **Enterprise workspaces**, with an API key: one fact
per conversation with title, creation time, duration (the last transcript
offset), owner, calendar guests, the summary and the transcript. Items
land in the **📥 Inbox** with playback.

On other Otter plans there is no Public API (the Developer tab is missing).
Otter's own exports (txt, docx, srt) can still be filed by hand through
**+ New Fact**; the hive never uses Otter's private web API.

```yaml
knowledge:
  connectors:
    - name: otter
      type: otter
      layer: project
      interval: 15m
      auth:
        env: OTTER_API_KEY             # variable NAME holding the Otter API key
      scope:
        since_days: "30"
        max_recordings: "200"
        include_shared: "false"        # true: also conversations shared with the key's user
        channel: ""                    # optional channel id (overrides include_shared)
        include_summary: "true"        # abstract_summary, outline, action_items
        include_participants: "true"   # calendar_guests
        tags: "meetings"
```

### Setup

1. In Otter open **Integrations → Developer**, click **Create key** and copy
   it (it is shown once; two keys per user). If the tab is missing on an
   Enterprise workspace, Otter's account manager turns it on.
2. Put it in the hive's environment and add the connector from
   **Knowledge → 🔌 Integrations → Otter.ai conversations** with the
   credential source *Environment variable* naming that variable.
   **Connect** / **Check** read `GET /workspace`, so a dead key or a
   workspace without the Public API shows on the card.

### Behaviour

- Each sync pages `GET /conversations` (newest first, `limit=20`, the
  maximum, `cursor` while `meta.has_more`; `include_shared` or
  `channel_id`) until a conversation older than the newest one seen (minus
  48 h) or `since_days`, and reads each with
  `GET /conversations/{id}?include=action_items,outline,transcript`. The
  `txt` transcript (`Speaker  00:07` heads) becomes `[mm:ss] Speaker: text`
  lines.
- Facts carry `source: otter`, `attr_otter_recording_id` (the conversation
  id, the Inbox item id), `attr_recorded_at` (`created_at`),
  `attr_duration_ms`, `attr_host` (`owner`) and `attr_participants`; the
  recording page is the conversation `url`.
- Playback: `GET /conversations/{id}/audio` returns the MP3 download link;
  Otter does not state its lifetime, so the hive mints it per click with a
  one-hour expiry and never stores it.
- Errors: `401` reads as *not connected* (a wrong or deleted key, or a
  workspace without the Public API); `403` keeps Otter's refusal. `429` is
  retried honouring `Retry-After` (10 requests a second). The key is only
  sent to `api.otter.ai`.

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
