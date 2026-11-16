# Meetings

Hive can join a live meeting as a participant: it takes notes, asks clarifying questions in the meeting chat, and sends research to background agents that post results back while the meeting is still going. Google Meet, Microsoft Teams and Zoom go through [Vexa](https://github.com/Vexa-ai/vexa) meeting bots; Discord voice channels go through a per-hive Discord bot. The design is [hivecommons/quorum#1](https://github.com/hivecommons/quorum/issues/1); the interfaces between hive, Quorum and Spektacular are fixed in the [Quorum contract](https://github.com/hivecommons/quorum/blob/main/docs/contract.md) (`quorum.contract/v1`). Tracking issue: [#18284](https://github.com/hivecommons/hive/issues/18284).

The feature is off until `meetings.enabled` is set.

## How it works

- **Quorum** runs inside the hive container as a child process of hive, listening on loopback only (`127.0.0.1:8765` by default). Hive generates a bearer token for it on every boot. Hive is the MCP client: it calls Quorum's tools (`meeting.join`, `chat.post`, `say`, `meeting.leave`, `meeting.delete`) and follows the meeting's live events.
- **Spektacular live mode** (`spektacular meeting --from stdin --format jsonl`) runs as one child per live meeting. Hive feeds it final transcript segments and chat lines and reads its intents: chat posts, clarifying questions, notes, research requests and spoken replies.
- **Hive is the policy point.** Every intent passes hive's checks before it reaches the meeting: a chat rate limit, the voice rules, the research cap and the LLM budget.
- **Research** becomes a kick to the research agent pool (`research-1..N`, `meetings.research_pool_size`, at most 8). The agents are added to the runtime when meetings are on, not to `hive.yaml`, and reuse the scanner's backend and model (or the first configured agent's). Findings come back to Spek, and hive posts one summary per research item in the meeting chat and stores it as a knowledge fact. The Meetings panel shows each item's status from the live loop, whether or not the research agents are listed under Agents.
- **Notes** are one knowledge fact per meeting in the `meetings` vault (channel), updated in place as the meeting goes on. Meeting facts are marked sensitive, so repository publishing never copies them out of the hive.
- **After the meeting** the final transcript and notes appear in the [Inbox](inbox-voice-sources.md) as a `meeting` item, routable to a spek RFC or to dibs like any recording. Once it has been routed (or kept as knowledge only), hive asks Quorum to delete the transcript upstream.

## Consent and privacy

- The first thing hive posts in a meeting is the consent notice (`meetings.consent_text`), before any other message. The default names the persona: "Betty Bee from the hive has joined to take notes and help with research. Betty records this meeting's transcript. Type 'betty leave' or 'hive leave' to remove Betty." With the persona off it is "Hive has joined to take notes and help with research. It records this meeting's transcript. Type 'hive leave' to remove it."
- Anyone in the meeting can type or say a leave phrase (`meetings.persona.leave_phrases`: "betty leave" or "hive leave", also "Betty, leave" or "hive, please leave") and the bot leaves at once.
- The bot joins as the persona's display name, `Betty Bee from the hive` (`meetings.persona.display_name`; `meetings.bot_name` overrides it). With the persona off it joins as `hive (notes)`.
- No audio is stored. Hive keeps the transcript under `/data/meetings` for `meetings.retention_days` (30 by default) after the meeting ends, then purges it. The transcript is deleted from Vexa at hand-off, and at the latest when hive purges it.
- Hive answers in the meeting chat by default and never talks over people. It speaks aloud only when the meeting has voice turned on (the dashboard toggle) and someone addresses it by name, and only while `meetings.voice_on_address` is true.
- Meeting speech is untrusted input. Research asked for in a meeting goes to read-only research agents; past `meetings.research_per_meeting` items it waits for the owner's approval in the dashboard.
- Research agents run in ADVISORY mode with no repository checkouts: the hive proxy lets them make only read requests to GitHub (GET, HEAD, OPTIONS and git fetch), so they cannot write to a repository or GitHub even if the prompt is ignored. Reading the web relies on the backend's own web tools (for Claude Code, its web fetch and search tools); this has not yet been verified end to end on a hosted hive, and a backend or egress policy without web access makes research answer with an error instead of findings.
- With the sidecar runtime (`CRUX_MODE=sidecar`) the research pool is not available: hive logs it at boot and research items show as failed in the panel.
- Recording consent law differs by place (two-party consent, GDPR). Review the notice text for where your meetings happen.

## Betty, the meeting attendee

Hive attends meetings as a persona: **Betty** ("Betty Bee from the hive", she/her) by default ([#18391](https://github.com/hivecommons/hive/issues/18391)). Spek still takes the notes and asks the clarifying questions; Betty is the voice in the chat.

- **Addressing her.** Start or end a line with one of her names (`meetings.persona.aliases`: "betty", "betty bee", "hive"), for example "Betty, when does the release ship?", "hey betty, any blockers on the deploy?" or "what do you think, Betty?". Common speech-to-text spellings ("Bettie", "Betty Bea") count. A sentence that merely mentions the name ("push it to the hive", "hive is slow today") does not. An alias that shares no word with her name, such as "hive", is an everyday word and is stricter: it addresses her only at the start of a line followed by a comma or colon ("hive, what's the status?", "Hive: any blockers?"), after "hey" or "ok" ("hey hive what's up"), or closing a question after a comma ("what do you think, hive?").
- **Intro.** "Betty, introduce yourself", "who are you, Betty?" or "who's the bot?" gets the intro (`meetings.persona.intro_template`), which says what she does and how to send her home.
- **What she answers.** When addressed she answers briefly from the hive's knowledge vaults, its beads (work items), its code maps (when `knowledge.code_maps.enabled` is on) and the web (when `meetings.persona.searxng_url` is set, see [Web answers](#web-answers)), plus the last three minutes of the meeting. If she does not know, she says so. A request that is not a question gets one clarifying question or one or two sentences on the recent discussion.
- **Voice.** She speaks only when the request was spoken (addressed in the meeting's audio) or asks for it ("say it", "out loud"). She first says a short acknowledgement at once ("Let me check on that.", rotating, in the persona's language), then the answer, at most 2 sentences and 200 characters with no links or Markdown. The answer goes through the meeting's voice rules: it is spoken only when the meeting's voice is on and `meetings.voice_on_address` is true, and falls back to chat otherwise or when speaking fails; the acknowledgement is never posted to chat and is not charged to the budget. That gate is hive's, for every platform: Quorum v0.2.0 ignores `voice: false` on Discord, so hive never calls `say` for a meeting whose voice is off. A spoken "Betty?" with no question gets one clarifying question or one or two sentences on the last three minutes.
- **Deep dives.** When an addressed question needs research (the model sets `needs_research`, or its confidence is below `min_confidence`) and the meeting has a research path, she says "I'll dig into that and report back", starts a research item through the same pool, cap and approval as Spek's research (shown in the panel as `betty-N`, dispatched with `betty=1` in its meta), and posts exactly one follow-up in her own voice when it completes (the summary as is if restyling fails).
- **Room questions.** Betty also answers questions people ask the room (a line ending in "?" or starting with "what", "how", "who" and the like, four words or more), but only if nobody answers within `answer_wait_s` (8 seconds), the knowledge sources had a hit, the model is at least `min_confidence` (0.75) sure, and the topic was not already answered. She never posts general commentary.
- **Caps.** Unprompted answers are capped at `per_10m` (2 per ten minutes), at least `min_gap_s` (90 seconds) apart and `per_meeting` (12) per meeting, with one reply per question, at most `queue_depth` (3) questions waiting and none older than `stale_s` (60 seconds). Addressed requests skip these caps but still go through the chat rate limit (`meetings.chat_posts_per_minute`) and the meeting budget. Chat answers are at most 3 sentences and 300 characters.
- **Spek.** With Betty on, Spek's spoken replies are dropped, Spek's chat posts go through Betty (posted unless she already covered the topic, so the meeting never gets two answers), and Spek's clarifying questions are reworded in Betty's voice with one quick model call (800 ms; on failure or timeout the original text is posted). The rewording runs beside the meeting loop, so the transcript, notes and other replies keep flowing; Spek's later questions and chat posts wait their turn so they still appear in Spek's order. Notes and research are unchanged. Lines addressed to Betty and leave phrases never count as answers to Spek's questions.
- **Manners.** Plain, brief language; at most one light touch of humor, never about a person and none when the conversation sounds tense. Set `meetings.persona.humor: off` to turn humor off. Meeting speech, knowledge and work items reach the model as data, never as instructions. She never claims to have done something she did not do.
- **Model.** Betty answers with the meetings model through the same path as `hive meeting-llm` (an OpenAI-compatible endpoint, charged to the meeting's usage ledger and budget). On a hive with only an Anthropic key (`HIVE_MEETINGS_ANTHROPIC_API_KEY` or `meetings.llm.anthropic_key_file`) she calls the Anthropic Messages API directly with `meetings.llm.model` (default `claude-haiku-5-5`), with the same timeouts, in-flight limit and usage ledger, so her tokens count toward the meeting's budget. With no model she still introduces herself but says she cannot answer questions.
- **Turning her off.** `meetings.persona.enabled: false` (or `HIVE_MEETINGS_PERSONA_ENABLED=false`) restores the plain `hive (notes)` attendee.

### Persona settings

A persona is a preset (`meetings.persona.preset`, default `betty`) plus any field set explicitly; every field also has an `HIVE_MEETINGS_PERSONA_*` override ([env-vars.md](env-vars.md#meetings-quorum)).

- `language` is a BCP-47 tag (default `en-US`). Betty replies in that language and its regional conventions (American spelling for `en-US`).
- `voice` is the text-to-speech voice id (default `af_heart`, a Kokoro voice) kept for spoken replies, which are not built yet. Kokoro voice ids encode accent and gender: `af_*` is American female, `am_*` American male, `bf_*` and `bm_*` British female and male. A future persona picks a different voice id. A "bot-type" voice would be a different TTS voice or an effect, which is not built yet.
- `voice_type` (`female`, `male` or `bot`, default `female`), `accent` (free text, default `American`) and `pronouns` (default `she/her`) describe the persona in the intro and her self-description only; they do not change behaviour.
- `intro_template` placeholders: `{name}`, `{display_name}`, `{pronouns}`, `{leave}` (the first leave phrase), `{accent}`, `{voice_type}`, `{language}`.
- `searxng_url` is the SearXNG instance she searches the web with; see [Web answers](#web-answers).

### Web answers

With `meetings.persona.searxng_url` set (`HIVE_MEETINGS_PERSONA_SEARXNG_URL`), web search joins Betty's fast sources. For each question she queries `GET <url>/search?format=json&categories=general,it&q=<question>` (the `it` category brings in GitHub and Stack Overflow) with a 1.2 second timeout, in parallel with the knowledge, bead and code-map lookups, and keeps the top 3 results (title, URL and snippet, HTML stripped). Results are untrusted data: they reach the model inside the data block and are never followed as instructions. A slow or failing search simply contributes nothing.

On hive-oke, SearXNG runs in the `meetings` namespace as a ClusterIP Service: set `searxng_url` to `http://searxng.meetings.svc.cluster.local:8080`. Its manifests (JSON output on, a small engine set, the limiter off for internal use, the secret key in a Secret) live in hivecommons/quorum `deploy/hive-oke/`. Any SearXNG with `json` in `search.formats` works.

## The model Spek reasons with

Hive owns the model credential and the budget; Spektacular never receives an OpenAI-compatible key.

- **OpenAI-compatible endpoint (default).** `meetings.llm.endpoint`, `meetings.llm.model` and the key in `meetings.llm.api_key_file` fall back to `governor.litellm` (its endpoint, key and `default_model`), the same resolution the knowledge advisor uses, but with the meetings timeout (`meetings.llm.timeout_ms`, 2500 ms) and its own in-flight limit (`meetings.llm.max_in_flight`, 4 calls across all meetings). Spek runs with `SPEK_MEETING_LLM=command` and calls `hive meeting-llm` for every prompt. That subcommand reads the endpoint, model and key from the environment hive gives the Spek child (never from argv), makes one chat completion, prints the reply, and appends `{"at": ..., "tokens": N}` to the meeting's usage ledger `/data/meetings/<id>/llm-usage.jsonl`. The live loop charges that ledger to the per-meeting budget and the hive-wide hourly and daily ceilings, and `hive meeting-llm` refuses once the meeting's budget is spent.
- **Anthropic.** With an Anthropic key (`meetings.llm.anthropic_key_file` or `HIVE_MEETINGS_ANTHROPIC_API_KEY`) and `meetings.llm.mode` `auto` or `anthropic`, Spek calls Anthropic directly (`SPEK_MEETING_LLM=anthropic`). The key is put only into the Spek child's environment. Token use is then counted in intents, not tokens.
- **No model.** Without an endpoint and model (or with `meetings.llm.mode: none`) Spek runs on its rules only, and the meeting panel shows "rules only, no model configured".

Spektacular must be a version that includes the `meeting` command ([hivecommons/spektacular#126](https://github.com/hivecommons/spektacular/pull/126)). With an older binary the live mode exits at once; hive keeps the meeting going (transcript, consent, `hive leave`, research approvals, the panel) without Spek's questions and notes, and the panel says Spektacular live mode is not running.

## Using it

Only the hive owner can send hive to a meeting.

1. Open **Meetings** in the dashboard sidebar.
2. Paste a Google Meet, Teams or Zoom link (or open "Discord voice channel instead" and enter the guild and channel IDs), optionally a title, tick **Voice** if hive may speak, and press **Send hive to a meeting**.
3. Admit the bot if the meeting has a lobby. Hive gives up after `meetings.lobby_timeout_s` (600 seconds) in a lobby.
4. **Open** the meeting to follow the live transcript, notes, research and Spek's intents. Use **Approve research** for items past the cap, **Voice on/off**, and **Leave**.

Voice is fixed when the bot joins: Vexa only lets a bot speak if it was started with voice. Hive starts every bot able to speak while `meetings.voice_on_address` is true (and when you tick **Voice**), so the toggle can turn speaking on later; with `meetings.voice_on_address` false and **Voice** unticked, turning voice on later has no effect and hive keeps answering in chat (the panel says so).

If the meeting platform's chat is unavailable, hive cannot post the consent notice. It retries about once a minute and posts nothing else into the meeting until the notice is out; the transcript, notes and research still show in the panel.

The same operations are available over the [meetings API](api-reference/knowledge.md) (`/api/meetings`).

## Calendar

The **Calendar** section, right after **Inbox** in the left menu, lists the meetings hive is scheduled to join. The scheduler checks every 15 seconds (`HIVE_MEETINGS_SCHEDULE_TICK_S`) and sends hive in 60 seconds before each start (`HIVE_MEETINGS_SCHEDULE_LEAD_S`), the same way **Send hive to a meeting** does. A meeting without an end time is still joined up to 15 minutes after it starts; after that it is marked skipped as missed.

- **Add meeting:** paste a Google Meet, Teams or Zoom link (the platform comes from the link), pick the start and optionally the end, a title and a repeat (daily, every weekday, weekly, monthly). Times are entered and shown in your browser's time zone; a repeating meeting keeps its local time across daylight saving changes. The owner's own meetings are scheduled at once. A read-write user can add one too, and it waits for the owner.
- **Each row** shows the title, organizer, start and end, platform, the next occurrence of a repeating meeting, the source (email invite, added here, or Discord) and the status: scheduled, awaiting approval, live, ended, cancelled, declined, failed, blocked or skipped.
- **Approve** or **Decline** a meeting awaiting approval (owner only). A declined invite stays declined when its organizer updates it.
- **Skip** skips the next occurrence: a single meeting becomes skipped, a repeating one moves on to the one after. **Cancel** stops hive joining it at all; a meeting already live is not left (use **Leave** in Meetings).
- **Open live panel** while hive is in the meeting and **Notes** once it ended open that meeting in the Meetings section.

The schedule lives in `<meetings.data_dir>/schedule.json` (by default `/data/meetings/schedule.json`), rewritten atomically. Each occurrence is recorded before hive joins it, so a restart never joins the same occurrence twice; a join cut off by a restart is shown as failed rather than retried. Finished meetings stay listed for 14 days and are removed after 30.

Entries are keyed by their calendar UID and RECURRENCE-ID, ready for calendar invites (planned: inviting hive by email). An update with a higher SEQUENCE replaces the entry, an equal or lower one is ignored, and a cancellation marks it cancelled. Until invites arrive, everything not added by the owner waits for approval.

While hive is in at least one meeting (joining, in the lobby or live), the top bar shows an indicator such as "🐝 Betty is in a meeting" or "🐝 Betty is in 2 meetings". The name comes from the bot display name (`meetings.bot_name`, "Betty Bee from the hive" shows as "Betty Bee"; the default `hive (notes)` shows as "Hive"). Hover it for each meeting's platform and title; click it to open the live panel, or the Calendar when there are several. The **Calendar** menu badge counts the meetings hive is in plus those awaiting approval. Both follow the dashboard's live event stream, so they update without polling.

The schedule API is `/api/meetings/schedule` ([API reference](api-reference/knowledge.md)).

## Setup

### Hosted hives

The operator who provisions hosted hives:

1. Runs a shared Vexa install and mints one Vexa tenant API key per hive.
2. Sets on the hive deployment:
   - `HIVE_MEETINGS_ENABLED=true`
   - `HIVE_MEETINGS_VEXA_URL=<Vexa gateway base URL>`
   - the tenant key, either as `HIVE_MEETINGS_VEXA_API_KEY` from a Kubernetes Secret, or written once from **Settings, Meetings** (stored keystore-sealed at `/data/secrets/meetings_vexa_api_key`)
   - for Discord, `HIVE_MEETINGS_TRANSCRIBE_URL` (the shared transcription service) and its token, either as `HIVE_MEETINGS_TRANSCRIBE_TOKEN` from a Kubernetes Secret, or written once from **Settings, Meetings** (stored keystore-sealed at `/data/secrets/meetings_transcribe_token`)
3. Restarts the hive.

The hive image carries the pinned Quorum bundle at `/usr/local/lib/quorum/quorum.mjs` and the `spektacular` binary, so nothing else is installed.

### Self-hosted hives

1. Run Vexa somewhere the hive can reach and create an API key for this hive.
2. In the dashboard, open **Settings, Meetings**: turn meetings on, enter the Vexa gateway URL and paste the tenant key (write-only; the dashboard only shows whether it is set). Or set the `meetings:` block in `hive.yaml`:

   ```yaml
   meetings:
     enabled: true
     vexa:
       url: https://vexa.example.org
   ```

3. Restart the hive. Quorum and its secrets are read at boot.

An image built without the Quorum bundle (the `QUORUM_VERSION` build argument empty) logs that the bundle is missing and leaves meetings unavailable; set `meetings.quorum.binary` to a bundle you installed yourself.

### Discord

Create a Discord application with a bot, invite it to your server with permission to connect and speak in voice channels, and paste the bot token in **Settings, Meetings** (or set `HIVE_MEETINGS_DISCORD_TOKEN`). Without a token Discord meetings are unavailable.

Discord audio is transcribed by the shared transcription service, which needs two more settings:

- the transcribe URL, `meetings.transcribe_url` (or `HIVE_MEETINGS_TRANSCRIBE_URL`). Where the service runs in the same cluster as the hive (the `vexa-transcription` Service in the `meetings` namespace), use the in-cluster address `http://vexa-transcription.meetings.svc.cluster.local:8000`;
- the transcription token, the service's API token. Paste it in the **Transcription token** field in **Settings, Meetings** (or set `HIVE_MEETINGS_TRANSCRIBE_TOKEN`). Hive stores it keystore-sealed at `meetings.transcribe_token_file` (`/data/secrets/meetings_transcribe_token`) and never returns it; the panel only shows whether it is set. At boot hive writes the plaintext to an owner-only file in Quorum's private runtime directory and passes it as `QUORUM_TRANSCRIBE_TOKEN_FILE`.

Without the URL and token Discord meetings join but nothing is transcribed. All three (bot token, transcribe URL, transcription token) apply after a restart.

Discord voice also needs the native `@snazzah/davey` module next to the Quorum bundle; the hive image installs it.

Anyone in the Discord server can ask for hive with `/hive join`. The request shows in **Meetings** as a join request with **Approve** and **Decline**; hive joins only when the owner approves it. (No Discord account is mapped to the hive owner yet, so no request is approved automatically.)

## Configuration

Every key has an environment override that wins over `hive.yaml`. The full list with defaults is in [env-vars.md](env-vars.md#meetings-quorum).

| Key | Default | Meaning |
|---|---|---|
| `meetings.enabled` | `false` | Turn meetings on. |
| `meetings.quorum.binary` | `/usr/local/lib/quorum/quorum.mjs` | Quorum bundle (run through `node`) or executable. |
| `meetings.quorum.listen` | `127.0.0.1:8765` | Loopback address for Quorum. |
| `meetings.vexa.url` | none | Vexa gateway base URL. |
| `meetings.vexa.api_key_file` | `/data/secrets/meetings_vexa_api_key` | Keystore-sealed tenant key file. |
| `meetings.discord.token_file` | `/data/secrets/meetings_discord_token` | Keystore-sealed Discord bot token file. |
| `meetings.transcribe_url` | none | Transcription service for Discord audio. |
| `meetings.transcribe_token_file` | `/data/secrets/meetings_transcribe_token` | Keystore-sealed transcription service token file. |
| `meetings.delivery` | `chat` | Where meeting output goes. |
| `meetings.voice_on_address` | `true` | Allow speaking when addressed in a voice-enabled meeting. |
| `meetings.bot_name` | the persona's display name | Bot display name; `hive (notes)` with the persona off. |
| `meetings.invite_policy` | `owner` | Who may send hive to a meeting. |
| `meetings.research_pool_size` | `2` | Research agents (`research-1..N`). |
| `meetings.research_per_meeting` | `5` | Research items per meeting before approval is needed. |
| `meetings.chat_posts_per_minute` | `6` | Chat post rate limit per meeting. |
| `meetings.lobby_timeout_s` | `600` | Seconds to wait in a lobby. |
| `meetings.budget.max_tokens_per_meeting` | `200000` | LLM tokens per meeting (when Spek reports usage). |
| `meetings.budget.max_tokens_per_hour` | `500000` | Hive-wide hourly ceiling. |
| `meetings.budget.max_tokens_per_day` | `2000000` | Hive-wide daily ceiling. |
| `meetings.budget.max_intents_per_meeting` | `500` | Intents per meeting when Spek reports no usage. |
| `meetings.retention_days` | `30` | Days a transcript is kept after the meeting ends. |
| `meetings.consent_text` | see above | Consent notice, at most 2000 characters. |
| `meetings.data_dir` | `/data/meetings` | Meeting records and transcripts. |
| `meetings.llm.mode` | `auto` | `auto`, `openai`, `anthropic` or `none`. |
| `meetings.llm.endpoint` | `governor.litellm` endpoint | OpenAI-compatible base URL for Spek's model. |
| `meetings.llm.model` | `governor.litellm.default_model` | Model name. |
| `meetings.llm.api_key_file` | `/data/secrets/meetings_llm_api_key` | Keystore-sealed key; falls back to the `governor.litellm` key. |
| `meetings.llm.anthropic_key_file` | `/data/secrets/meetings_anthropic_api_key` | Keystore-sealed Anthropic key. |
| `meetings.llm.anthropic_model` | Spektacular's default | Anthropic model. |
| `meetings.llm.timeout_ms` | `2500` | Per-call model timeout. |
| `meetings.llm.max_in_flight` | `4` | Concurrent model calls across meetings. |
| `meetings.persona.enabled` | `true` | The meeting attendee persona. |
| `meetings.persona.preset` | `betty` | Built-in persona the fields below start from. |
| `meetings.persona.name` | `Betty` | Name she answers to. |
| `meetings.persona.display_name` | `Betty Bee from the hive` | Meeting display name, at most 64 characters. |
| `meetings.persona.aliases` | `[betty, betty bee, hive]` | Names that address her. |
| `meetings.persona.pronouns` | `she/her` | Used in the intro and self-description. |
| `meetings.persona.language` | `en-US` | BCP-47 reply language and locale. |
| `meetings.persona.voice` | `af_heart` | TTS voice id for spoken replies. |
| `meetings.persona.voice_type` | `female` | `female`, `male` or `bot` (descriptive). |
| `meetings.persona.accent` | `American` | Descriptive. |
| `meetings.persona.intro_template` | the preset's intro | Intro text. |
| `meetings.persona.leave_phrases` | `[betty leave, hive leave]` | Phrases that remove the bot. |
| `meetings.persona.humor` | `light` | `light` or `off`. |
| `meetings.persona.unprompted.per_10m` | `2` | Unprompted answers per ten minutes. |
| `meetings.persona.unprompted.min_gap_s` | `90` | Seconds between unprompted answers. |
| `meetings.persona.unprompted.per_meeting` | `12` | Unprompted answers per meeting. |
| `meetings.persona.unprompted.answer_wait_s` | `8` | Seconds to wait for a person to answer first. |
| `meetings.persona.unprompted.min_confidence` | `0.75` | Model confidence an unprompted answer needs. |
| `meetings.persona.unprompted.queue_depth` | `3` | Room questions waiting at once. |
| `meetings.persona.unprompted.stale_s` | `60` | Seconds before a waiting question is dropped. |
| `meetings.persona.searxng_url` | none | SearXNG instance for web answers (for example `http://searxng.meetings.svc.cluster.local:8080`). |

When the budget runs out, hive posts one notice in the meeting and keeps taking notes, but stops asking questions, starting research and speaking.
