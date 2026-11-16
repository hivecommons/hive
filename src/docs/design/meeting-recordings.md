# Meeting recordings in the Inbox: Zoom and Microsoft Teams

**Status: Zoom shipped; Teams scaffold shipped, blocked on tenant consent for
a live verification.** The owner's ask: "we want to add teams and zoom support
to get their recordings into the inbox." This page records how the two
platforms map onto the Inbox model the Plaud connector established, the auth
model each platform forces, what becomes a fact and an Inbox item, and what
the operator has to register. Read [`inbox-voice-sources.md`](../inbox-voice-sources.md)
first for the shipped Inbox and
[`knowledge-connectors.md`](../knowledge-connectors.md#zoom-cloud-recordings-type-zoom)
for the operator steps.

## 1. Goals and non-goals

Goals:

- A recorded Zoom or Teams meeting lands as an Inbox item on the hive the
  same way a Plaud recording does: title = meeting topic, start time,
  duration, participants when the platform reports them, the platform's AI
  summary when there is one, the transcript, and (where the platform allows a
  browser to fetch it) inline playback through the existing audio endpoint.
- One fact per recorded meeting, de-duplicated by the platform's recording
  id, re-read while the platform has no text for it yet.
- Credentials referenced by environment variable name only, like every other
  connector; refresh tokens kept in the connector's private token store.
- The same Integrations card controls as Plaud: Connect / Check / Sync now /
  Edit / Disable, the hosted "paste the redirect URL" fallback, and the
  registration steps in the card help.

Non-goals (this round):

- Webhooks. Both platforms can push "recording completed" events, but a
  hosted hive has no public callback for a Zoom app and no Graph change
  notification endpoint; polling on the connector interval (15 minutes by
  default) is what Plaud does and is enough for triage.
- Downloading media into the vault. Audio and video stay on the platform;
  the hive only mints short-lived links.
- Teams recordings served to the browser. Graph serves `/content` behind a
  bearer token; see section 6.

## 2. Data flow

Both connectors sit behind the `connector.Connector` seam and reuse the
Plaud sibling shape:

1. **Sync** (`Syncer` on the interval, or *Sync now*): list recordings since
   the cursor minus a 48 hour overlap (first run: `since_days`), capped at
   `max_recordings` (reported as truncated); fetch the text for each; emit
   one `Page` per recording through the `#18221` progress reporter
   (`listing` with the count, then `fetching` with the meeting topic as the
   current item). `FactWriter` turns pages into facts under
   `connectors/<layer>/`.
2. **Inbox listing** (`GET /api/inbox`): the `inbox.MeetingSource`
   (`src/pkg/inbox/meetingsource.go`) implements the same `Source` and
   `AudioSource` interfaces as `PlaudSource`; `PreferDirect` lists live
   through it while the connector is signed in (cached, refreshed in the
   background, never blocking the poll) and merges with the facts of the
   same id, falling back to facts otherwise. `PreferDirect.Direct` became an
   interface (`DirectSource`) so both sources fit.
3. **Content** (`GET /api/inbox/{source}/{id}/content`): fact text, or a
   live read while the fact is still pending.
4. **Playback** (`GET /api/inbox/{source}/{id}/audio`, from #18247): the
   source's `AudioLink` calls the connector's `MediaLink`; the link is
   handed back once with `Cache-Control: no-store` and never persisted.

Shared code lives in `src/pkg/knowledge/connector/meeting.go`: the
`MeetingRecording` / `MeetingContent` / `MeetingReader` / `MeetingMediaReader`
types, the WebVTT renderer (`renderVTT`, producing the same `[mm:ss] Speaker:
text` lines as `renderPlaudTranscript`, understanding Zoom's `Name: text`
cues and Teams' `<v Name>` voice tags), the fact renderer (`meetingPage`) and
the 0600 token store.

## 3. Auth model per platform

### Zoom

The operator registers an OAuth app on the Zoom App Marketplace. Two grants:

- **User-level OAuth** (`auth.mode: user_oauth`, General app): authorization
  code flow. Connect builds
  `https://zoom.us/oauth/authorize?response_type=code&client_id=…&redirect_uri=…&state=…`
  and shows it on the card. The redirect URL registered on the app defaults
  to `http://localhost:8199/zoom/callback`, which the hosted hive never
  receives, so the operator pastes the URL the browser landed on into the
  card (the Plaud flow); the hive checks the `state`, exchanges the code at
  `POST https://zoom.us/oauth/token` with the client id and secret as Basic
  auth, and stores the tokens at
  `/data/knowledge/connector-state/<name>/home/.zoom/tokens.json` (0600).
  Access tokens live one hour; refresh tokens 90 days and **rotate** on
  every refresh, so the new one is written back at once. Disconnect revokes
  the token upstream and deletes the file.
- **Server-to-Server OAuth** (`auth.mode: server_to_server`): the
  `account_credentials` grant with `account_id`; one-hour tokens cached in
  memory, nothing on disk. `scope.users` must list the recording owners
  (`me` has no meaning for an account token).

Scopes: `cloud_recording:read:list_user_recordings`,
`cloud_recording:read:list_recording_files`, `user:read:user` (classic
`recording:read`, `user:read`); optional `meeting_summary:read` (AI summary)
and `meeting:read:list_past_participants` (participants). Server-to-Server
apps use the `:admin` variants.

### Teams

The operator registers an app in Microsoft Entra and a tenant administrator
grants **application** permissions `OnlineMeetingRecording.Read.All` and
`OnlineMeetingTranscript.Read.All` (admin consent), then an **application
access policy** granting the app the organizers' online meetings
(`New-CsApplicationAccessPolicy` / `Grant-CsApplicationAccessPolicy`). The
connector uses the client-credentials grant against
`https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token` with scope
`https://graph.microsoft.com/.default`; tokens are cached in memory only.
There is no browser step: Connect fetches a token, probes
`getAllRecordings` for the first organizer, and when Graph answers 403 the
card states *Requires Entra admin consent for OnlineMeetingRecording.Read.All
and OnlineMeetingTranscript.Read.All* with the admin-consent page
(`https://login.microsoftonline.com/{tenant}/adminconsent?client_id=…`) as
the link; the "No application access policy" variant names the PowerShell
cmdlets.

Optional permissions widen the fact: `OnlineMeetings.Read.All` for the
meeting subject, times, organizer and attendees (without it the fact is
titled "Teams meeting <time>"), `User.Read.All` to list organizers by UPN,
`GroupMember.Read.All` for `scope.group`.

## 4. Polling, Graph and Zoom endpoints

Zoom: `GET /users/{user}/recordings?from=&to=&page_size=300&next_page_token=`
in month-sized windows (the API caps one listing at a month), then per
recording the `TRANSCRIPT` recording file (`download_url`, bearer token,
WebVTT), `GET /meetings/{uuid}/meeting_summary` and
`GET /past_meetings/{uuid}/participants` (both optional; a missing scope or
plan only drops the section). UUIDs starting with `/` or containing `//` are
double URL-encoded as the Zoom API requires. Playback:
`GET /meetings/{uuid}/recordings?include_fields=download_access_token&ttl=3600`
and the audio-only (`M4A`) else video (`MP4`) `download_url` with the token
appended as `access_token`, since a media element cannot send a header; the
OAuth access token is the fallback when Zoom grants no download token.
Download hosts must be `zoom.us` hosts.

Teams (Graph v1.0, verified against the published reference):
`GET /users/{id}/onlineMeetings/getAllRecordings(meetingOrganizerUserId='{id}',startDateTime=…,endDateTime=…)?$top=50`
and `getAllTranscripts(...)`, following `@odata.nextLink`; artifacts are
grouped by `meetingId`; `GET /users/{id}/onlineMeetings/{meetingId}` for
details; the newest transcript's `transcriptContentUrl` with
`Accept: text/vtt`, falling back to
`application/vnd.microsoft.graph.transcript+text` on
`SpeakerAttributionNotAllowed`. Link hosts must equal the Graph host.

## 5. Facts and Inbox items

| Field | Zoom | Teams |
|---|---|---|
| `source_id` / item id | meeting instance UUID (`attr_zoom_recording_id`) | callRecording id, or `transcript:<id>` for a transcript-only meeting (`attr_teams_recording_id`) |
| meeting id | `attr_zoom_meeting_id` (numeric) | `attr_teams_meeting_id` (onlineMeeting id) |
| title | `topic` | `subject` (needs `OnlineMeetings.Read.All`), else "Teams meeting <time>" |
| `attr_recorded_at` / `attr_duration_ms` | `start_time`; recording file span, else `duration` minutes | meeting `startDateTime`; recording created→end span |
| `attr_host`, `attr_participants` | `host_email`; past-meeting participants | organizer; attendees (display name, else UPN) |
| `## Summary (Zoom)` | `meeting_summary` overview, detail sections, next steps | none (Graph exposes no recap) |
| `## Transcript` | VTT → `[mm:ss] Speaker: text` | same |
| `attr_meeting_content` | `ready` / `pending` | same |
| `source_url` | `share_url`; `include_video_link` adds the play link | `joinWebUrl`; `include_video_link` adds it to the body |

Dedupe: one fact per platform recording id; a meeting recorded twice is two
items. Pending recordings (no transcript, no summary yet) are re-read on
later syncs: Zoom by id (`GET /meetings/{uuid}/recordings`, 25 per sync on
top of the listing); Teams relies on the 48 hour overlap because Graph has no
lookup of a callRecording by id alone.

## 6. Limits and open items

- Zoom lists at most 300 recordings per page and a month per window; the
  connector walks windows back to `since_days` and stops at
  `max_recordings`. Zoom's AI summary and participant endpoints depend on
  the plan and the scopes; both are optional and failures are logged at
  debug level. The `meeting_summary` field shape (overview, details, next
  steps, edited summary) follows the published reference and is **to be
  verified against a live account**; an unexpected shape yields an empty
  summary, not a failed sync.
- Zoom playback appends the token as a query parameter because browsers
  cannot add an `Authorization` header to a media element. The token is
  one-hour and minted per click; the link is never written anywhere.
- Teams playback: Graph serves `recordingContentUrl` only with a bearer
  token, so `MediaLink` answers *no playable media*. The follow-up is a
  hive-side streaming proxy (`GET /api/inbox/{source}/{id}/audio` returning
  the bytes, or a one-time signed hive URL), which needs the same role gate
  and an audit line; not built here.
- Teams details to verify against a tenant: whether
  `meetingOrganizerUserId` accepts a UPN (the connector resolves UPNs to
  object ids when `User.Read.All` is granted and otherwise passes them as
  given), the 30 minute policy propagation, and the transcript access
  tenant setting (`GraphAccessToTranscriptsDisabled`).
- Settings → Knowledge → Connectors (the table editor) does not yet render
  the `auth.*_env` fields; use the Integrations card for Zoom and Teams.

## 7. Privacy

Recordings and transcripts contain other people's words. The connectors
write them into the vault for the layer the operator chooses (`personal`
unless the whole hive should see them) and never download media. Transcripts
and summaries are **untrusted data**: they are stored and shown as text,
never executed or treated as instructions, with the same posture as Plaud's
`<untrusted-user-data>` envelope. Participant lists are limited to 50 names.
Tokens are referenced by environment variable name in config, stored 0600
under the connector state directory (Zoom user-level) or kept in memory
only (Zoom Server-to-Server, Teams), never logged, never returned by the
API. Download links and tokens minted for playback are returned once with
`Cache-Control: no-store` and audited as `inbox_audio`; the Inbox player
itself streams through the hive (`…/audio/stream`, audited as
`inbox_audio_stream`), so the link never reaches the browser. Operators should
record only what may leave the meeting; a hive that syncs an account's
recordings (Zoom Server-to-Server, Teams organizers) is reading other
people's meetings and should say so to them.

## 8. What the operator registers

Zoom: an OAuth app (General for own recordings, Server-to-Server for the
account) with the scopes in section 3, the redirect URL for the General app,
and the client id / secret (/ account id) in the hive environment under the
names given in `auth.client_id_env`, `auth.client_secret_env`,
`auth.account_id_env`.

Teams: an Entra app registration with a client secret, the application
permissions `OnlineMeetingRecording.Read.All` and
`OnlineMeetingTranscript.Read.All` (plus the optional ones above) with admin
consent, an application access policy granted to the organizers, and the
tenant id / client id / secret in the hive environment under the names in
`auth.tenant_id_env`, `auth.client_id_env`, `auth.client_secret_env`.
