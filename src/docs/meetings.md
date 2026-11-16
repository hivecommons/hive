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

- The first thing hive posts in a meeting is the consent notice (`meetings.consent_text`), before any other message. The default is: "Hive has joined to take notes and help with research. It records this meeting's transcript. Type 'hive leave' to remove it."
- Anyone in the meeting can type or say "hive leave" (also "hive, leave" or "hive, please leave") and hive leaves at once.
- The bot joins as `hive (notes)` (`meetings.bot_name`), so its name says it takes notes.
- No audio is stored. Hive keeps the transcript under `/data/meetings` for `meetings.retention_days` (30 by default) after the meeting ends, then purges it. The transcript is deleted from Vexa at hand-off, and at the latest when hive purges it.
- Hive answers in the meeting chat by default and never talks over people. It speaks aloud only when the meeting has voice turned on (the dashboard toggle) and someone addresses it by name, and only while `meetings.voice_on_address` is true.
- Meeting speech is untrusted input. Research asked for in a meeting goes to read-only research agents; past `meetings.research_per_meeting` items it waits for the owner's approval in the dashboard.
- Research agents run in ADVISORY mode with no repository checkouts: the hive proxy lets them make only read requests to GitHub (GET, HEAD, OPTIONS and git fetch), so they cannot write to a repository or GitHub even if the prompt is ignored. Reading the web relies on the backend's own web tools (for Claude Code, its web fetch and search tools); this has not yet been verified end to end on a hosted hive, and a backend or egress policy without web access makes research answer with an error instead of findings.
- With the sidecar runtime (`CRUX_MODE=sidecar`) the research pool is not available: hive logs it at boot and research items show as failed in the panel.
- Recording consent law differs by place (two-party consent, GDPR). Review the notice text for where your meetings happen.

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
| `meetings.bot_name` | `hive (notes)` | Bot display name. |
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

When the budget runs out, hive posts one notice in the meeting and keeps taking notes, but stops asking questions, starting research and speaking.
