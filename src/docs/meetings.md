# Meetings

Hive can join a live meeting as a participant: it takes notes, asks clarifying questions in the meeting chat, and sends research to background agents that post results back while the meeting is still going. Google Meet, Microsoft Teams and Zoom go through [Vexa](https://github.com/Vexa-ai/vexa) meeting bots; Discord voice channels go through a per-hive Discord bot. The design is [hivecommons/quorum#1](https://github.com/hivecommons/quorum/issues/1); the interfaces between hive, Quorum and Spektacular are fixed in the [Quorum contract](https://github.com/hivecommons/quorum/blob/main/docs/contract.md) (`quorum.contract/v1`). Tracking issue: [#18284](https://github.com/hivecommons/hive/issues/18284).

The feature is off until `meetings.enabled` is set.

## How it works

- **Quorum** runs inside the hive container as a child process of hive, listening on loopback only (`127.0.0.1:8765` by default). Hive generates a bearer token for it on every boot. Hive is the MCP client: it calls Quorum's tools (`meeting.join`, `chat.post`, `say`, `meeting.leave`, `meeting.delete`) and follows the meeting's live events.
- **Spektacular live mode** (`spektacular meeting --from stdin --format jsonl`) runs as one child per live meeting. Hive feeds it final transcript segments and chat lines and reads its intents: chat posts, clarifying questions, notes, research requests and spoken replies.
- **Hive is the policy point.** Every intent passes hive's checks before it reaches the meeting: a chat rate limit, the voice rules, the research cap and the LLM budget.
- **Research** becomes a kick to the research agent pool (`research-1..N`, `meetings.research_pool_size`, at most 8). The agents are added to the runtime when meetings are on, not to `hive.yaml`, and reuse the scanner's backend and model (or the first configured agent's). Findings come back to Spek, and hive posts one summary per research item in the meeting chat and stores it as a knowledge fact. The Meetings panel shows each item's status from the live loop, whether or not the research agents are listed under Agents. The research agents are kept across config reloads, and a dispatch that finds none registers them again. Settings, **Meeting attendee**, **Research** lists the agents, how many can take a request now, and warns "No research agent available" when none can.
- **Notes** are one knowledge fact per meeting in the `meetings` vault (channel), updated in place as the meeting goes on. Meeting facts are marked sensitive, so repository publishing never copies them out of the hive.
- **After the meeting** the final transcript and notes appear in the [Inbox](inbox-voice-sources.md) as a `meeting` item, routable to a spek RFC or to dibs like any recording. Once it has been routed (or kept as knowledge only), hive asks Quorum to delete the transcript upstream.

## Consent and privacy

- The first thing hive posts in a meeting is the consent notice (`meetings.consent_text`), before any other message. The default names the persona: "Betty Bee from the hive has joined to take notes and help with research. Betty records this meeting's transcript. Type 'betty leave' or 'hive leave' to remove Betty." With the persona off it is "Hive has joined to take notes and help with research. It records this meeting's transcript. Type 'hive leave' to remove it."
- **Nothing from before the notice is kept.** Until the notice is delivered (`consent_posted_at` set: Quorum confirmed the chat line, or hive spoke it), every transcript segment, draft and chat line is discarded: not stored, and not handed to Betty, Spektacular, research, knowledge facts, the Inbox hand-off or drafts. A line that arrives after the notice but belongs before it is discarded too: one at or below Quorum's transcript cursor when the notice landed (`consent_seq`), a chat line stamped earlier, or, for two minutes, a segment that began before the last speech heard while the notice was pending. Hive keeps only the count, `pre_consent_dropped` on the meeting. It does not buffer and release: those lines are gone. A reconnect or restart while the notice is pending asks Quorum for no replay, so nothing buffered from that time comes back.
- A leave phrase is honoured before the notice too: the bot leaves, and the line itself is not kept.
- Vexa keeps its own copy of what the bot transcribes from the moment it is admitted, pre-consent lines included, and has no API to delete part of a live meeting's transcript. That copy goes when hive deletes the meeting upstream (`meeting.delete` at hand-off or purge), which erases the whole meeting. Quorum's in-memory replay buffer is cleared at the same time.
- Anyone in the meeting can type or say a leave phrase (`meetings.persona.leave_phrases`: "betty leave" or "hive leave", also "Betty, leave" or "hive, please leave") and the bot leaves at once.
- The bot joins as the persona's display name, `Betty Bee from the hive` (`meetings.persona.display_name`; `meetings.bot_name` overrides it). With the persona off it joins as `hive (notes)`.
- No audio is stored. Hive keeps the transcript under `/data/meetings` for `meetings.retention_days` (30 by default) after the meeting ends, then purges it. The transcript is deleted from Vexa at hand-off, and at the latest when hive purges it.
- Hive answers in the meeting chat by default and never talks over people. It speaks aloud only when the meeting has voice turned on (the dashboard toggle) and someone addresses it by name, and only while `meetings.voice_on_address` is true.
- Meeting speech is untrusted input. Research asked for in a meeting goes to read-only research agents; past `meetings.research_per_meeting` items it waits for the owner's approval in the dashboard.
- Research agents run in ADVISORY mode with no repository checkouts: the hive proxy lets them make only read requests to GitHub (GET, HEAD, OPTIONS and git fetch), so they cannot write to a repository or GitHub even if the prompt is ignored. Reading the web relies on the backend's own web tools (for Claude Code, its web fetch and search tools); this has not yet been verified end to end on a hosted hive, and a backend or egress policy without web access makes research answer with an error instead of findings.
- With the sidecar runtime (`CRUX_MODE=sidecar`) the research pool is not available: hive logs it at boot and research items show as failed in the panel.
- Recording consent law differs by place (two-party consent, GDPR). Review the notice text for where your meetings happen.

## Betty, the meeting attendee

Hive attends meetings as a persona: **Betty** ("Betty Bee from the hive") by default ([#18391](https://github.com/hivecommons/hive/issues/18391)). Spek still takes the notes and asks the clarifying questions; Betty is the voice in the chat.

- **Addressing her.** Start or end a line with one of her names (`meetings.persona.aliases`: "betty", "betty bee", "hive"), for example "Betty, when does the release ship?", "hey betty, any blockers on the deploy?" or "what do you think, Betty?". Common speech-to-text spellings ("Bettie", "Betty Bea") count. A sentence that merely mentions the name ("push it to the hive", "hive is slow today") does not. An alias that shares no word with her name, such as "hive", is an everyday word and is stricter: it addresses her only at the start of a line followed by a comma or colon ("hive, what's the status?", "Hive: any blockers?"), after "hey" or "ok" ("hey hive what's up"), or closing a question after a comma ("what do you think, hive?").
- **Intro.** "Betty, introduce yourself", "who are you, Betty?" or "who's the bot?" gets the intro (`meetings.persona.intro_template`), which says what she does and how to send her home. Asked in chat, the intro is posted in chat. Asked by voice in a meeting that can hear her, she says a short spoken intro (one or two sentences, at most 120 characters, so it starts quickly and fits the speech cache) and posts the full intro once in chat; it is never delivered in full twice.
- **What she answers.** When addressed she answers briefly from the hive's knowledge vaults, its beads (work items), its code maps (when `knowledge.code_maps.enabled` is on) and the web (when `meetings.persona.searxng_url` is set, see [Web answers](#web-answers)), plus the last three minutes of the meeting. If she does not know, she says so. A request that is not a question gets one clarifying question or one or two sentences on the recent discussion.
- **Voice.** She speaks only when the request was spoken (addressed in the meeting's audio) or asks for it ("say it", "out loud"). She answers directly, at most 2 sentences and 200 characters with no links or Markdown. An answer over 120 characters is said as its first sentence plus "The details are in chat.", and the whole answer is posted once in chat, so first audio and the say call stay short. A short acknowledgement ("Let me check on that.", rotating, in the persona's language, never with the speaker's name) is said only when the answer is still not ready 3 seconds after the request (6 seconds for a question about the hive's own state, which is normally answered well inside that), and one still waiting to be spoken when the answer arrives is skipped, so an acknowledgement never delays the answer behind it. The answer goes through the meeting's voice rules: it is spoken only when the meeting's voice is on and `meetings.voice_on_address` is true, and falls back to chat otherwise or when speaking fails; the acknowledgement is never posted to chat and is not charged to the budget. That gate is hive's, for every platform: Quorum v0.2.0 ignores `voice: false` on Discord, so hive never calls `say` for a meeting whose voice is off. A spoken "Betty?" with no question gets one clarifying question or one or two sentences on the last three minutes. When she heard her name in a spoken request and is about to answer, she first plays a short, soft chime (`meetings.listening_tone`, on by default; see Configuration).
- **Several requests at once.** Each request is worked on as soon as it is heard and answered as soon as its answer is ready, so answers can arrive out of order. Whenever that could confuse the room (another request is in flight, someone else spoke since, or the answer took more than 8 s) the answer opens with a short topic phrase: "On open PRs: ...". The topic is the model's 2 to 5 word label for the question, else the request's own words. She does not use the speaker's name in acknowledgements or answers; a name is added only when two people asked about the same topic within a minute ("Andy (re: open PRs): ..." in chat, "Andy, about open PRs: ..." spoken, first name only), written plain, never as `@name`, so nobody is pinged. A single quick answer has no prefix. With two or more requests in hand she posts one short chat note ("I have 3 questions; answering each as it's ready."), at most once a minute, through the chat rate limit and never charged;
- **Questions about the hive itself.** "How many open issues are there?", "what PRs are open in the hive repo?", "which agents are working?" are answered from the hive's live dashboard state (the same open, actionable, held and blocked counts, agents and open PRs the Overview and the dashboard chat use), read in-process, so the answer comes in seconds and never goes to research. Such a question names the hive (or "the repo") or asks for open issues, open PRs, actionable items or recent merges. The numbers are the dashboard's own: every count states its scope the same way ("across 13 repos", or the one repo asked about); a count taken while some repos are still being read after a restart is worded "at least N, counted in 5 of 13 repos so far" and never given as a total; a snapshot older than three minutes says how old it is. Agents are counted as the sidebar counts them (each replica is one agent), the ones working are named (up to five, then "and N more") and a paused agent is never named as running. Asked about one issue or PR she gives a short summary of its description, its state (open, draft, merged, closed), the review and CI status the hive holds and what is blocking it, in at most three short sentences, with no extra GitHub call.
- **About hive itself.** "What is hive?", "which voice memo providers does hive support?", "tell me all you know about hive" are answered from a short product brief built from the project's own docs (what hive is and who it is for, the Inbox voice sources, the meeting platforms, and how the docs position it beside adjacent tools), plus the knowledge base, which for her now includes the ready git sources as well as the vaults. A broad question gets a three sentence overview and an offer to go deeper. These questions are never sent to the public web or to research.
- **What she heard.** Speech-to-text mishears product words ("hive queue" as "high school", "Kubernetes" as "Cuba needs"). For a line addressed to her, a small table of known mishearings gives the most likely reading; the model sees both ("Heard: ... Most likely meant: ..."). When the reading is a guess she asks one short question ("Did you mean the hive queue?") rather than answer about the wrong thing. Other people's lines and the stored transcript are never rewritten.
- **Deep dives.** When an addressed question needs research (the model sets `needs_research`, or its confidence is below `min_confidence`) and the meeting has a research path, she says "I'll dig into that and report back", starts a research item through the same pool, cap and approval as Spek's research (shown in the panel as `betty-N`, dispatched with `betty=1` in its meta), and gives exactly one follow-up in her own voice when it completes (the summary as is if restyling fails), in the channel she was asked in: spoken for a spoken question when the meeting can hear her, else in chat. She always closes the loop: if the research cannot start (for example "no research agent is available on this hive"), the agent reports an error, or it is still running after 45 seconds, she says "I couldn't look that up: <reason>." and the reason is kept on the research item (`error`); one that runs past 20 seconds gets one "still looking" note first. When every research agent is busy the request waits its turn and she says "I'm busy with another question, yours is next."; its deadline starts when an agent picks it up. The research agents are warmed when a live meeting starts and kept warm while it runs. When an ack was already spoken, the "I'll dig into that" line goes to chat instead. A spoken request is acknowledged at most once, and only when its answer is late (see Voice).
- **Room questions.** Betty also answers questions people ask the room (a line ending in "?" or starting with "what", "how", "who" and the like, four words or more), but only if nobody answers within `answer_wait_s` (8 seconds), the knowledge sources had a hit, the model is at least `min_confidence` (0.75) sure, and the topic was not already answered. She never posts general commentary.
- **Caps.** Unprompted answers are capped at `per_10m` (2 per ten minutes), at least `min_gap_s` (90 seconds) apart and `per_meeting` (12) per meeting, with one reply per question, at most `queue_depth` (3) questions waiting and none older than `stale_s` (60 seconds). Addressed requests skip these caps but still go through the chat rate limit (`meetings.chat_posts_per_minute`) and the meeting budget; once that rate is spent, answers to addressed requests may take `meetings.chat_answer_burst` (3) more posts a minute, so answers worked on in parallel are not held for a minute.
- **Several spoken answers.** Quorum queues each spoken clip and plays them in order, so she never talks over herself; hive estimates the queued speech (about 15 characters a second). Spoken answers that are ready together are said back to back, shortest first. An answer that would wait behind more than 20 s of queued speech is posted to chat instead, and she says "I've put the rest in chat." (at most once every 30 s). An acknowledgement that would play only after more than 8 s of earlier speech is skipped. Chat answers are at most 3 sentences and 300 characters.
- **Sources and honesty.** A chat answer ends with the sources it used, from the context entries the model reports: a web hit's link, a work item's id, or the knowledge fact, code map or "hive live status" it came from, at most two, skipped when the answer already names them (`(Source: https://...; hive-42)`). The model is told to give issue and PR numbers as the context has them and never to guess numbers, names, dates or links. An addressed answer below `unprompted.min_confidence` that is not from the hive's live state, and that will not become a deep dive, opens with "I'm not sure, but ..." unless it already says so. Spoken answers carry no sources.
- **Speed.** Every line she delivers records its stages, the stock ones included (intro, acknowledgements, follow-ups, progress notes, the goodbye): when the request was heard, retrieval, the model call, answer ready, the wait behind earlier lines (`queue_ms`), delivery (chat post, or the say call, `say_ms`), and for speech the wait before the first audio (`first_audio_ms`, `synth_ms`, `tts_cached` as Quorum reports them, and `speech_start_ms` from heard to first audio). The meeting's live state (`GET /api/meetings/{id}/live`, under `live.persona_timings`, the last 40 lines) shows them with the source (`hive-status`, `knowledge`, `web`, `research`, `model`, `fixed` or `fallback`) and a budget: 4 s from heard to answer for a question about the hive's live state, 8 s otherwise; `live.persona_latency` sums answers, answers over budget, median and slowest. An answer over budget is logged with its stages. Retrieval stops waiting as soon as the hive's live state answers a question about the hive, so a slow web search never delays it.
- **Her own lines are in the record.** Every line she delivers (the consent notice, intro, acknowledgements, answers, research results, "Leaving now." and failures she reports) is appended to the meeting transcript as a `reply` line with her name, the channel (spoken or chat), the time, the source and the timing. A line that could not be delivered is recorded as not delivered with the reason. The meeting view, the Inbox item's transcript and the notes show them in time order, marked as hers ("Betty (assistant, spoken; source: hive-status): ..."), so a person or a model reading the transcript can tell her words from the participants'. Her own lines never reach the persona or Spek as input, follow the transcript's retention, and nothing from before the consent notice is kept.
- **When a reply cannot be delivered.** A reply goes out in the channel it was asked in. If that fails it goes out in the other one, saying so briefly: a chat post the platform did not show is spoken when the meeting has voice ("I couldn't post that in chat, so: ..."), and a line that cannot be spoken is posted. If neither works the reply stays visible in the meeting view as not delivered. After three minutes with at least two failed deliveries the meeting is marked "can't reach participants" with a pointer to Leave, and after ten minutes hive leaves by itself (state reason `unreachable`) rather than sit silent in the meeting. Calls into the meeting no longer block the meeting loop: she keeps hearing and answering while she speaks.
- **Leaving.** "betty leave", typed or spoken, is confirmed with "Leaving now." in the channel it was asked in, and hive leaves only once that line is known to be out (confirmed in chat, or spoken), waiting at most 8 seconds. Reading a typed leave does not depend on being able to post.
- **Spek.** With Betty on, Spek's spoken replies are dropped, Spek's chat posts go through Betty (posted unless she already covered the topic, so the meeting never gets two answers), and Spek's clarifying questions are reworded in Betty's voice with one quick model call (800 ms; on failure or timeout the original text is posted). The rewording runs beside the meeting loop, so the transcript, notes and other replies keep flowing; Spek's later questions and chat posts wait their turn so they still appear in Spek's order. Notes and research are unchanged. Lines addressed to Betty and leave phrases never count as answers to Spek's questions.
- **Manners.** Plain, brief language; at most one light touch of humor, never about a person and none when the conversation sounds tense. Set `meetings.persona.humor: off` to turn humor off. Meeting speech, knowledge and work items reach the model as data, never as instructions. She never claims to have done something she did not do.
- **Model.** Betty answers with the meetings model through the same path as `hive meeting-llm` (an OpenAI-compatible endpoint, charged to the meeting's usage ledger and budget). On a hive with only an Anthropic key (`HIVE_MEETINGS_ANTHROPIC_API_KEY` or `meetings.llm.anthropic_key_file`) she calls the Anthropic Messages API directly with `meetings.llm.model` (default `claude-haiku-5-5`), with the same timeouts, in-flight limit and usage ledger, so her tokens count toward the meeting's budget. With no model she still introduces herself but says she cannot answer questions. With `meetings.llm.mode: agent-backend` (or `claude-cli`) she answers through one of the hive's agent backends instead; see [Agent backends](#agent-backends).
- **Turning her off.** `meetings.persona.enabled: false` (or `HIVE_MEETINGS_PERSONA_ENABLED=false`) restores the plain `hive (notes)` attendee.

### Persona settings

A persona is a preset (`meetings.persona.preset`, default `betty`) plus any field set explicitly; every field also has an `HIVE_MEETINGS_PERSONA_*` override ([env-vars.md](env-vars.md#meetings-quorum)).

- `language` is a BCP-47 tag (default `en-US`). Betty replies in that language and its regional conventions (American spelling for `en-US`).
- `voice` is the text-to-speech voice id (default `af_heart`, a Kokoro voice) her spoken replies use: hive passes it to Quorum as `QUORUM_TTS_VOICE` for Discord and as `voice_id` on every `meeting.join`. Kokoro voice ids encode accent and gender: `af_*` is American female, `am_*` American male, `bf_*` and `bm_*` British female and male. A future persona picks a different voice id. A "bot-type" voice would be a different TTS voice or an effect, which is not built yet.
- `voice_type` (`female`, `male` or `bot`, default `female`) and `accent` (free text, default `American`) describe the persona's voice when asked; they do not change behaviour. `pronouns` is kept for compatibility but never stated: the persona does not announce pronouns in the intro, her self-description or any answer, and a pronoun parenthetical in a custom intro is dropped.
- `intro_template` placeholders: `{name}`, `{display_name}`, `{pronouns}` (always renders empty), `{leave}` (the first leave phrase), `{accent}`, `{voice_type}`, `{language}`.
- `searxng_url` is the SearXNG instance she searches the web with; see [Web answers](#web-answers).

### Web answers

With `meetings.persona.searxng_url` set (`HIVE_MEETINGS_PERSONA_SEARXNG_URL`), web search joins Betty's fast sources. For each question she queries `GET <url>/search?format=json&categories=general,it&q=<query>` (the `it` category brings in GitHub and Stack Overflow) with a 1.2 second timeout, in parallel with the knowledge, bead and code-map lookups, and keeps the top 3 results and infoboxes (title, URL and snippet, HTML stripped). The query is the cleaned request, not the raw utterance: her name, the framing ("can you tell me", "search the web for") and trailing punctuation are stripped. Questions about this hive (its issues, PRs, agents, the product, the configured org and repo names, "our", "this meeting") are never sent to the web; an explicit "search the web for ..." is. Three keyless public sources answer common live facts directly, each within 1.4 seconds and named in the answer: the Wikipedia summary for definitions and entities, Open-Meteo for "weather in <place>" (asked without a place she asks where), and the GitHub releases API for "latest release of <project>" (never with the hive's token). When an allowed question gets nothing she says the search returned nothing just now, or that web search is down, and offers to dig deeper. Each web retrieval logs one line with the source, result count and time. Results are untrusted data: they reach the model inside the data block and are never followed as instructions. A slow or failing search simply contributes nothing.

On hive-oke, SearXNG runs in the `meetings` namespace as a ClusterIP Service: set `searxng_url` to `http://searxng.meetings.svc.cluster.local:8080`. Its manifests (JSON output on, a small engine set, the limiter off for internal use, the secret key in a Secret) live in hivecommons/quorum `deploy/hive-oke/`. Any SearXNG with `json` in `search.formats` works.

### Meeting attendee settings

Settings, **Meeting attendee** edits all of this from the dashboard ([#18396](https://github.com/hivecommons/hive/issues/18396)). The owner and read-write users can open it; it saves `meetings.persona` (and `meetings.voice_on_address`) to `hive.yaml`. The persona is read when hive starts, so a saved change shows "restart the hive to apply" until the next restart. A field set by an `HIVE_MEETINGS_PERSONA_*` variable is marked **env**, and edits to it have no effect while the variable is set; a field that differs from the preset is marked **custom**.

- **Persona.** Preset cards show each persona's flag, language, accent, voice type and voice id. Picking one fills the form and plays that persona's introduction in its own voice (see [Hearing a preset](#hearing-a-preset)); Save stores it. A value equal to the preset's is stored as no override, so the persona keeps following the preset. The voice list shows the Kokoro voices for the chosen language, and changing the language picks a voice of the same type. The intro has a live preview. **Play sample** speaks a short line in the persona's language through the meetings TTS service (the browser plays it from `GET /api/meetings/persona/sample`, using the form's unsaved voice, language and name); until that service is configured (`meetings.tts_url` and its token) the button is disabled and the API answers "TTS not configured".
- **Behaviour.** The unprompted caps above (`per_10m`, `min_gap_s`, `per_meeting`, `answer_wait_s`, `min_confidence`, `queue_depth`, `stale_s`; 0 restores a default) and whether hive may speak aloud when addressed (`meetings.voice_on_address`).
- **Model.** The model Betty and Spek answer with: `meetings.llm.mode`, and on the agent-backend mode the backend and model (`meetings.llm.backend`, `meetings.llm.model`). The backend and model lists are the agents' own (the catalog the agent backend and model pickers use); a backend that cannot answer for meetings is listed but cannot be picked, and the section says why. **Default** means the backend most of the hive's agents run on and that backend's meetings default model. Saved on its own with **Save model** (owner, `PUT /api/meetings/model`); it applies after a restart. See [Agent backends](#agent-backends).
- **Email.** The attendee address, `hive@<dashboard host>`, with a copy button, the people with access to this hive (owner, read-write and read) with their registered calendar emails and proof status, the mailed-in files waiting for approval (invites waiting for approval are in the **Calendar**), **My calendar emails** where each user adds, proves and removes their own addresses (see [Proving calendar emails](#proving-calendar-emails)), and the owner's switches for calendar invites and mailed-in files (`meetings.email.*`, saved with **Save**; a read-write user sees them read-only). The section is live once the hive's calendar email allowlist is wired ([#18395](https://github.com/hivecommons/hive/issues/18395)); before that its controls are disabled.

#### Hearing a preset

Clicking a preset card selects it and plays that persona's introduction, in the preset's voice and language, so you can hear a persona before choosing it ([#18710](https://github.com/hivecommons/hive/issues/18710)). Each card also has a speaker button that plays or stops the introduction without selecting the preset.

- **What you hear.** The preset's own introduction: the text the persona says when asked to introduce itself, with the preset's name and leave phrase. The pronoun note is not spoken. The clip is made by the meetings TTS service (`meetings.tts_url`, the Kokoro service the attendee speaks with in a meeting), so it is the real voice.
- **Playing and stopping.** The card shows a dashed border while the clip loads and a green border while it plays. Clicking another card stops the current clip and plays the new one; clicking the playing card, or its speaker button, stops it. Nothing plays when the page loads: a clip only starts from a click. With the keyboard, Tab to a card or its speaker button and press Enter or Space.
- **When it cannot play.** The tab shows "Voice preview is unavailable:" followed by the reason: meetings are turned off, the TTS service is not configured or not reachable, or too many previews were asked for. It does not fall back to the browser's own speech synthesis, which would be a different voice.
- **Limits and privacy.** The hive keeps each clip in memory (per TTS service, preset, voice and text; at most 32 clips, gone at restart), so a repeated click does not call the TTS again. A user may ask for 30 previews a minute, an introduction is at most 600 characters, and one TTS call may take 20 seconds. No audio is written to disk, the browser is told not to keep a copy, and nothing is sent outside the hive and its own TTS service.

The API is `GET /api/meetings/persona/preview?preset=<id>` (owner or read-write).

The presets use only voices the self-hosted Kokoro TTS ([Kokoro-FastAPI](https://github.com/remsky/Kokoro-FastAPI) v0.10.0) ships. Leave phrases stay in Latin script because the leave matcher uses ASCII word boundaries, and every preset keeps "hive" as an alias and "hive leave" as a leave phrase.

| Preset | Language | Accent | Voice | Voice type |
|---|---|---|---|---|
| `betty`: Betty Bee from the hive (default) | `en-US` | American | `af_heart` | female |
| `barnaby`: Barnaby Bee from the hive | `en-US` | American | `am_michael` | male |
| `beatrice`: Beatrice Bee from the hive | `en-GB` | British | `bf_emma` | female |
| `bertie`: Bertie Bee from the hive | `en-GB` | British | `bm_george` | male |
| `melisa`: Melisa, la abeja de la colmena | `es-ES` | Spanish (Spain) | `ef_dora` | female |
| `melie`: Mélie, l'abeille de la ruche | `fr-FR` | French | `ff_siwis` | female |
| `melina`: Melina, l'ape dell'alveare | `it-IT` | Italian | `if_sara` | female |
| `bia`: Bia, a abelha da colmeia | `pt-BR` | Brazilian | `pf_dora` | female |
| `madhu`: मधु, छत्ते की मधुमक्खी | `hi-IN` | Indian | `hf_alpha` | female |
| `mitsu`: ミツバチのミツ | `ja-JP` | Standard Japanese | `jf_alpha` | female |
| `mimi`: 蜂巢的小蜜蜂蜜蜜 | `zh-CN` | Standard Mandarin | `zf_xiaobei` | female |

A bot-style voice is listed as coming later: Kokoro has no bot-style voice, and one would need a new service.

The API is `/api/meetings/persona` ([API reference](api-reference/knowledge.md)).

## The model Spek reasons with

Hive owns the model credential and the budget; Spektacular never receives an OpenAI-compatible key.

- **OpenAI-compatible endpoint (default).** `meetings.llm.endpoint`, `meetings.llm.model` and the key in `meetings.llm.api_key_file` fall back to `governor.litellm` (its endpoint, key and `default_model`), the same resolution the knowledge advisor uses, but with the meetings timeout (`meetings.llm.timeout_ms`, 2500 ms) and its own in-flight limit (`meetings.llm.max_in_flight`, 4 calls across all meetings). Spek runs with `SPEK_MEETING_LLM=command` and calls `hive meeting-llm` for every prompt. That subcommand reads the endpoint, model and key from the environment hive gives the Spek child (never from argv), makes one chat completion, prints the reply, and appends `{"at": ..., "tokens": N}` to the meeting's usage ledger `/data/meetings/<id>/llm-usage.jsonl`. The live loop charges that ledger to the per-meeting budget and the hive-wide hourly and daily ceilings, and `hive meeting-llm` refuses once the meeting's budget is spent.
- **Anthropic.** With an Anthropic key (`meetings.llm.anthropic_key_file` or `HIVE_MEETINGS_ANTHROPIC_API_KEY`) and `meetings.llm.mode` `auto` or `anthropic`, Spek calls Anthropic directly (`SPEK_MEETING_LLM=anthropic`). The key is put only into the Spek child's environment. Token use is then counted in intents, not tokens.
- **Agent backend.** With `meetings.llm.mode: agent-backend` Spek runs `SPEK_MEETING_LLM=command` against `hive meeting-llm` too, which then runs the backend's CLI once per prompt in its completion profile (see [Agent backends](#agent-backends)) with the cold-start timeout, and charges the CLI's reported usage to the same ledger.
- **No model.** Without an endpoint and model (or with `meetings.llm.mode: none`) Spek runs on its rules only, and the meeting panel shows "rules only, no model configured".

### Agent backends

`meetings.llm.mode: agent-backend` lets Betty and Spek use a backend and model the hive's agents are configured with ([#18517](https://github.com/hivecommons/hive/issues/18517), [#18549](https://github.com/hivecommons/hive/issues/18549)); `claude-cli` is the same mode with the `claude` backend. `meetings.llm.backend` names the backend (empty: the backend most of the hive's agents run on) and `meetings.llm.model` the model (empty: `claude-sonnet-5-5` on `claude`, otherwise the model most of that backend's agents use, then a gateway's `default_model`). The `openai`, `anthropic` and `none` modes are unchanged, and `auto` never picks an agent backend.

A CLI backend runs in a narrow completion profile: one request per call, every tool and MCP server off, no hooks, no settings files, no session written to the shared home, an empty temporary working directory, and a fixed system prompt that treats meeting text as untrusted data. The request's own instructions travel inside the turn, and the prompt goes in on stdin, never in argv. The CLI's environment is an allowlist (path, locale, proxy and CA settings, the CLI's own config directory and sign-in variables), so GitHub and hive tokens never reach it. Usage from the CLI's result is charged to the meeting's usage ledger, the per-meeting budget and the hive-wide ceilings, as with the API modes, and a call is refused once the budget is spent.

The CLI uses the sign-in the agents get, without hive logging it or writing it to disk:

- **claude:** the shared agent home (`/data/home`, override `HIVE_MEETINGS_CLI_HOME`) with its credentials file. On a hive that signs Claude in only with the operator's long-lived token (`HIVE_CLAUDE_OAUTH_TOKEN` or `HIVE_CLAUDE_OAUTH_TOKEN_FILE`), the token reaches the meetings CLI as `CLAUDE_CODE_OAUTH_TOKEN` in its environment, exactly as the agent launcher gives it to claude agents; a `CLAUDE_CODE_OAUTH_TOKEN` in hive's own environment is not passed on.
- **copilot:** `COPILOT_GITHUB_TOKEN`, else the dashboard Copilot sign-in (`/data/copilot-user-token`, read through the keystore), as the agents get it. Copilot runs with its home (`HOME`, `COPILOT_HOME`) in the call's own temporary directory, so no MCP config, plugin or hook of the agents' home loads and its session log is removed with the call. Without a Copilot token the call fails rather than fall back to the shared home.
- **omp:** the fleet's auth broker (`OMP_AUTH_BROKER_URL`, `OMP_AUTH_BROKER_TOKEN`), in an empty `hive-meetings` settings profile.
- **Model gateways** (`litellm`, `vllm`, `llm-d`, named gateways): the gateway's own key, sent as the bearer of the chat completion.

| Backend | How it answers | Latency | Status |
|---|---|---|---|
| `claude` (2.1.325) | Betty: one warm `claude -p --input-format stream-json --output-format stream-json` per meeting, started when the meeting starts; each call is one turn. Spek: one `claude -p --output-format json` per prompt. | Warm turn about 1 s with Sonnet 5.5 (measured 0.9 to 1.2 s on Claude Code 2.1.325); a cold `claude -p` adds the CLI start, several seconds. | Supported. |
| `copilot` (1.0.88) | One `copilot --output-format json --available-tools <none> --disable-builtin-mcps --no-custom-instructions --no-ask-user --no-auto-update --usage-output-file <tmp>` per call for Betty and Spek, the prompt piped on stdin. The reply is the last `assistant.message`; tokens are input plus output per model from the usage report. | No warm mode: about 4.5 s of CLI start and sign-in before the model answers (measured 4.4 to 4.8 s on Copilot CLI 1.0.109 from launch to the model's answer, there a quota refusal), so a reply takes about 5 to 8 s. | Supported. |
| `omp` (18.20.0) | One `omp -p --mode json --no-tools --no-lsp --no-extensions --no-skills --no-rules --no-session --no-title --no-context-files --profile hive-meetings --system-prompt <fixed>` per call, prompt on stdin. The reply is the last assistant `message_end`; tokens are its `usage.totalTokens`. Needs a model (the isolated profile has no default). | No warm mode: the CLI start plus one model call per reply, a few seconds; not yet measured against a signed-in CLI. | Supported. |
| `litellm` | The OpenAI-compatible path (`governor.litellm`), as `meetings.llm.mode: openai`. | One chat completion. | Supported. |
| `vllm`, `llm-d` | The OpenAI-compatible path at the first endpoint the agents use (`HIVE_VLLM_ENDPOINT`, `HIVE_LLMD_ENDPOINT`, default the in-cluster llm-d endpoint picker), no key. Needs a model: `meetings.llm.model` or the model of the hive's agents on that backend. | One chat completion. | Supported. |
| Named model gateways | Every gateway under Settings, Model Gateways with an endpoint answers through the OpenAI-compatible path with its endpoint, key, `default_model` and `ca_bundle`, and shows in the picker under its name. | One chat completion. | Supported, except watsonx gateways. |
| `codex` | | | Refused: `codex` has no flag that removes its shell tool. With a read-only sandbox, approval policy never and an empty workspace, `codex exec` still lets the model run read-only commands, which can read the CLI's sign-in and other files on the hive and repeat them into a meeting. |
| `gemini` | | | Refused: the gemini CLI is not installed in the hive image (no pinned version in `src/Dockerfile`), has no flag that removes every tool (its non-interactive mode still runs read-only shell and file tools), and stopped serving personal and Google AI Pro accounts on 2026-06-18. |
| `agy`, `bob` | | | Refused: neither has a flag that turns its tools off (the agent launcher cannot pass them tool rules either). |
| `goose`, `aider`, `cline`, `pi`, `opencode`, `kilo`, `muse`, `openhands`, `omnigent` | | | Refused: hive has no verified launch of the CLI that answers one prompt with every tool, MCP server and file write turned off. |
| `watsonx` | | | Refused: watsonx needs a short-lived IAM bearer minted from the IBM Cloud key and a project header, which the meetings OpenAI-compatible path does not send. |

A refused backend fails config validation and `PUT /api/meetings/model` with its reason, and the Settings picker lists it disabled, with the reason under "Why some backends are unavailable for meetings". A `meetings.llm.backend` that is neither a known backend nor a configured gateway is refused the same way.

The warm process is replaced after `HIVE_MEETINGS_CLI_RECYCLE_TURNS` turns (default 8, which bounds the history each turn re-reads and pays for), and at once after any error, timeout or crash, so a late reply can never answer the next request and the next call finds a fresh process. Calls from one meeting run in parallel, up to `meetings.llm.per_meeting_in_flight` (default 3) inside the shared in-flight limit: a warm CLI answers one turn at a time on one stream (its results carry no request id), so the meeting keeps a pool of up to that many warm CLIs. Only the first starts with the meeting; when two requests overlap and no idle warm CLI is left, the next one is warmed in the background, so a burst of questions pays at most one cold start (a few seconds on `claude`) and the rest of the meeting answers warm. A meeting that never overlaps requests runs one CLI. On a CLI backend Betty's timeouts are wider than the API modes' 4 s, 2.5 s and 800 ms: `HIVE_MEETINGS_CLI_ADDRESSED_TIMEOUT_MS`, `HIVE_MEETINGS_CLI_UNPROMPTED_TIMEOUT_MS` and `HIVE_MEETINGS_CLI_RESTYLE_TIMEOUT_MS`, default 8000, 6000 and 3000 on `claude` and 20000, 15000 and 10000 on `copilot` and `omp`, which start the CLI on every call. Spek's per-prompt call uses `HIVE_MEETINGS_CLI_ONESHOT_TIMEOUT_MS` (30000). `HIVE_MEETINGS_CLI_BINARY` overrides the CLI binary. The panel shows the model in use, for example "claude CLI claude-sonnet-5-5" or "vllm model llama-4".

Spektacular must be a version that includes the `meeting` command ([hivecommons/spektacular#126](https://github.com/hivecommons/spektacular/pull/126)). With an older binary the live mode exits at once; hive keeps the meeting going (transcript, consent, `hive leave`, research approvals, the panel) without Spek's questions and notes, and the panel says plainly that notes and clarifying questions are off for the meeting. While hive cannot reach a Spektacular child that is restarting, the panel says they are paused, and clears it when Spek writes again.

## The meeting budget

Every meeting has its own token budget (`meetings.budget.max_tokens_per_meeting`, 200000 by default), and the hive has hourly and daily ceilings across all meetings (`meetings.budget.max_tokens_per_hour`, `max_tokens_per_day`). When Spek reports no usage, `meetings.budget.max_intents_per_meeting` (500) caps intents instead.

**How it is counted.** The meeting's usage ledger charges fresh input tokens, output tokens and cache writes in full, and cache reads at a tenth (the weight set in #18911: a warm session re-reads its whole history every turn, and counting those reads in full spent a meeting budget in about fifteen answers). Each answer Betty gives also counts as one intent. Hive-wide ceilings use the same token count.

**Where the owner changes it.** In `hive.yaml` under `meetings.budget`, or with `HIVE_MEETINGS_MAX_TOKENS_PER_MEETING`, `HIVE_MEETINGS_MAX_TOKENS_PER_HOUR`, `HIVE_MEETINGS_MAX_TOKENS_PER_DAY` and `HIVE_MEETINGS_MAX_INTENTS_PER_MEETING`; Settings, Meetings shows the values in effect. The 80 and 90 percent steps below are fixed behaviour, not settings.

**The meter.** The meeting row and the live meeting view show a slim meter with the percent used, with the text alternative "62% of the meeting budget used". `GET /api/meetings/budget` returns it for every meeting plus the hourly and daily ceilings used, to any role, as numbers only; the owner-only `GET /api/meetings/{id}` also carries the questions that were skipped.

| Used | What happens |
|---|---|
| 80% | One warn log line per meeting (`[meetings] meeting budget at 80%`, ids and numbers only) and a "budget 80%" badge on the meeting. Nothing changes in the meeting. |
| 90% | Betty says once that answers will be shorter and no new long research runs will start. From then on she answers in one short sentence (at most 160 characters) and does not start deep dives; research Spek asks for is not started. Research already running finishes, and the owner can still approve a waiting item by hand. |
| 100% | Betty posts the existing notice, now saying what to do: ask the owner to raise the budget or start a new meeting. She keeps taking notes but stops asking, answering and researching. |

A question dropped because the budget ran out is never lost. It is counted, shown in the live meeting view, and listed under "Not answered: budget" in the meeting's inbox record. While the budget is out, Betty repeats the notice with the running count of skipped questions each time that count doubles (1, 2, 4, 8, and so on), so the room learns questions are being skipped without a post for each one. Logs and audit carry counts and ids only, never the question text.

## Using it

Only the hive owner can send hive to a meeting.

1. Open **Meetings** in the dashboard sidebar. The top of the section shows **Invite by email**: the hive's address, `hive@<dashboard host>`, as a link that opens a new mail, with a **Copy** button. Add it to a calendar invite and hive joins that meeting (see [Inviting Betty by email](#inviting-betty-by-email)), or go on with a link:
2. Paste a Google Meet, Teams or Zoom link (or open "Discord voice channel instead" and enter the guild and channel IDs), optionally a title, tick **Voice** if hive may speak, and press **Send hive to a meeting**.
3. Admit the bot if the meeting has a lobby. Hive gives up after `meetings.lobby_timeout_s` (600 seconds) in a lobby.
4. **Open** the meeting to follow the live transcript, notes, research and Spek's intents. Use **Approve research** for items past the cap, **Voice on/off**, and **Leave**.

Voice is fixed when the bot joins: Vexa only lets a bot speak if it was started with voice. Hive starts every bot able to speak while `meetings.voice_on_address` is true (and when you tick **Voice**), so the toggle can turn speaking on later; with `meetings.voice_on_address` false and **Voice** unticked, turning voice on later has no effect and hive keeps answering in chat (the panel says so).

**🗑 Delete** (owner only) removes a meeting for good: a meeting that ended, failed, was blocked, declined or left, or a join request still waiting (deleting it declines it). It deletes the meeting record, its transcript, notes and live state on this hive, erases the meeting in Vexa with Quorum's `meeting.delete`, and removes its Calendar entry when that entry existed only for this meeting. If Vexa cannot confirm the erase, the meeting is still removed here and the dashboard says so. A meeting hive is still joining, waiting in the lobby of or live in cannot be deleted: **Leave** it first. **Clear finished** deletes every finished meeting after one confirm; live meetings and pending join requests stay. Knowledge facts and drafts already made from a meeting are not touched. Each delete is audited as `meeting_delete`.

If the meeting platform's chat is unavailable, hive cannot post the consent notice. It retries a few times and posts nothing else into the meeting until the notice is out; with voice on it speaks the notice instead, and otherwise it leaves (`consent_undelivered`). Until the notice is delivered the panel shows no transcript, notes or research: those lines are discarded, not held back.

Zoom no longer lets a web-client bot join: it answers "automated bots aren't allowed" and points to its RTMS app platform ([#18596](https://github.com/hivecommons/hive/issues/18596)). The ways Betty can still follow a Zoom meeting without a Zoom app (phone dial-in, a desktop companion on a participant's computer, Zoom's live stream) and the one chosen are compared in [Betty on Zoom without a Zoom app](design/meetings-without-a-zoom-app.md); none of them is built yet.

The same operations are available over the [meetings API](api-reference/knowledge.md) (`/api/meetings`).

## Desktop companion

The desktop companion ([hivecommons/quorum-companion](https://github.com/hivecommons/quorum-companion), design in [#18596](https://github.com/hivecommons/hive/issues/18596)) lets Betty take part in a call the person is already in, through their **own** meeting app, without any bot joining the meeting. It is how Betty works on Zoom without a Zoom app: Zoom refuses automated web-client bots, and the companion does not try to get around that; it captures the person's own Zoom (or Teams, Meet, ...) audio on their own computer.

- **Consent.** Before every capture the companion shows the companion consent notice (`GET /api/companion/info`, versioned) and the person confirms the participants were told and agreed. Zoom does not tell the other participants, so that confirmation is the consent gate. The meeting records who confirmed it, when, and the notice version (`companion` on the meeting record). The companion keeps a "Betty is listening" indicator on while it captures and offers the notice on the clipboard to paste into the meeting chat.
- **Who can use it.** Owner or read-write users. A companion meeting can be used only by the person who started it (owners can use any). Read-only users cannot stream.
- **Signing in.** Each owner or read-write user opens `/companion/token` on their hive and creates a companion token (shown once; only its SHA-256 is stored, one per user, **Replace** or **Revoke** any time). The companion sends it as `X-Hive-Companion-Token`. It works only on the `/api/companion/info` and `/api/companion/meetings` routes, acts as that user at their current role (a user removed or downgraded below read-write stops working at once), and never in `Authorization`. On hosted hives the hub lets those routes through to the hive, which does the check; the token page stays behind the hub sign-in.
- **Audio.** Two channels of raw 16 kHz mono s16le PCM: `call` (the meeting app's audio, everyone else) and `mic` (the person). Quorum cuts each at silences and transcribes it with the shared transcription service (`meetings.transcribe_url` is required); `mic` segments are attributed to the person, `call` segments to "Call".
- **Betty's replies** go to the companion window (and the meeting panel) instead of a meeting chat: the consent notice first, then answers. Betty cannot speak in a companion meeting yet, but when she heard her name and will answer, the companion plays her soft listening chime on your own computer (never into the call), when `meetings.listening_tone` and `meetings.voice_on_address` are on; the meeting's own voice setting does not apply. Saying "betty leave" ends the meeting, as does stopping the capture. The lines still waiting when the meeting ends ("Leaving now." among them) are served by the replies route after the end, until the window's cursor has passed them or two minutes are over.
- **After the meeting** it is an ordinary meeting: notes, research and the Inbox hand-off are unchanged.

| Route | |
|---|---|
| `GET /api/companion/info` | Consent notice and version, sample rate, channels, `max_chunk_bytes`. |
| `POST /api/companion/meetings` | `{title, app, host_name, consent_version, consent_attested: true}`; 201 with the meeting record. |
| `POST /api/companion/meetings/{id}/audio?channel=call\|mic&seq=N` | One PCM chunk; per channel in order from 0; 409 with `expected_seq` on a gap; a resend answers `duplicate: true`. |
| `GET /api/companion/meetings/{id}/replies?after=N` | Betty's lines after the cursor, `next_seq`, the meeting `state`, and `tone` (true once when the window should play the listening chime now; Quorum's `say.tone`); after the meeting ended, its last unfetched lines once. |
| `POST /api/companion/meetings/{id}/end` | Ends the meeting after transcribing buffered speech. |

The dashboard's ● Record ([Recording into the Inbox](inbox-recording.md)) stays the no-install option: it records a Chromium tab's audio and is transcribed after the recording ends, so Betty cannot answer during the call, and it cannot hear a desktop meeting app.

## Calendar

The **Calendar** section, right after **Inbox** in the left menu, lists the meetings hive is scheduled to join. The scheduler checks every 15 seconds (`HIVE_MEETINGS_SCHEDULE_TICK_S`) and sends hive in 60 seconds before each start (`HIVE_MEETINGS_SCHEDULE_LEAD_S`), the same way **Send hive to a meeting** does. A meeting without an end time is still joined up to 15 minutes after it starts; after that it is marked skipped as missed.

- **Invite by email:** the top of the section shows the hive's address, `hive@<dashboard host>`, as a link that opens a new mail, with a **Copy** button: add it to a calendar invite and the meeting appears here ([#18693](https://github.com/hivecommons/hive/issues/18693)). Owner and read-write users see it. When mail cannot reach the hive, or `meetings.email.invites_enabled` is off, the row says so and links to [Is the address set up?](#is-the-address-set-up) instead of showing an address that would bounce.
- **Add meeting:** paste a Google Meet, Teams or Zoom link (the platform comes from the link), pick the start and optionally the end, a title and a repeat (daily, every weekday, weekly, monthly). Times are entered and shown in your browser's time zone; a repeating meeting keeps its local time across daylight saving changes. The owner's own meetings are scheduled at once. A read-write user can add one too, and it waits for the owner.
- **Each row** shows the title, organizer, start and end, platform, the next occurrence of a repeating meeting, the source (email invite, added here, or Discord) and the status: scheduled, awaiting approval, live, ended, cancelled, declined, failed, blocked or skipped.
- **Approve** or **Decline** a meeting awaiting approval (owner only). A declined invite stays declined when its organizer updates it.
- **Skip** skips the next occurrence: a single meeting becomes skipped, a repeating one moves on to the one after. **Cancel** stops hive joining it at all; a meeting already live is not left (use **Leave** in Meetings).
- **🗑 Delete** a past entry (ended, cancelled, declined, failed, blocked or skipped; owner only) to remove it together with the meetings it created, as **Delete** in Meetings does. Entries still to come keep **Skip** and **Cancel**.
- **Open live panel** while hive is in the meeting and **Notes** once it ended open that meeting in the Meetings section.

The schedule lives in `<meetings.data_dir>/schedule.json` (by default `/data/meetings/schedule.json`), rewritten atomically. Each occurrence is recorded before hive joins it, so a restart never joins the same occurrence twice; a join cut off by a restart is shown as failed rather than retried. Finished meetings stay listed for 14 days and are removed after 30.

Entries are keyed by their calendar UID and RECURRENCE-ID, as calendar invites carry them (see [Inviting Betty by email](#inviting-betty-by-email)). An update with a higher SEQUENCE replaces the entry, an equal or lower one is ignored, and a cancellation marks it cancelled. Everything not added by the owner or by an allowlisted organizer waits for approval.

While hive is in at least one meeting (joining, in the lobby or live), the top bar shows an indicator such as "🐝 Betty is in a meeting" or "🐝 Betty is in 2 meetings". The name comes from the bot display name (`meetings.bot_name`, "Betty Bee from the hive" shows as "Betty Bee"; the default `hive (notes)` shows as "Hive"). Hover it for each meeting's platform and title; click it to open the live panel, or the Calendar when there are several. The **Calendar** menu badge counts the meetings hive is in plus those awaiting approval. Both follow the dashboard's live event stream, so they update without polling.

The schedule API is `/api/meetings/schedule` ([API reference](api-reference/knowledge.md)).

## Inviting Betty by email

Invite Betty by adding `hive@<your hive's dashboard host>` (for example `hive@acme.hive.hivecommons.dev`) to a calendar invite. Mail for every hive on a cluster reaches one receive-only receiver, `hive-mx` (`src/cmd/hive-mx`), through a wildcard MX record. No spoke opens port 25.

What `hive-mx` does with a message:

- **Recipients:** it accepts `RCPT TO` only for `hive@<host>` where the host is a hive the hub knows (subdomain, vanity or dashboard host; the list is refreshed from the hub every 60 seconds). Everything else is refused at `RCPT`, so it is never an open relay. At most 5 recipients and 35 MiB per message (`SIZE 36700160`, so a 25 MB attachment fits after base64; a larger message gets `552` at `MAIL FROM` or `DATA`), rate limits per sender IP and per sender domain, a daily byte budget per hive (512 MiB by default, `452` past it), no AUTH, and it never sends mail. A message is spooled to disk while it is processed, never held in memory whole. STARTTLS is offered when a certificate is mounted (`HIVE_MX_TLS_CERT`, `HIVE_MX_TLS_KEY`); without one it accepts plain SMTP, which is what most senders fall back to.
- **Sender authentication:** after DATA it checks SPF, DKIM and DMARC and records whether SPF or DKIM passed for a domain aligned with the `From` address (`adkim`/`aspf` honoured). Only an aligned sender can ever be trusted by a hive; the receiver itself decides nothing.
- **Calendar invites:** only `text/calendar` and `application/ics` parts are read. It keeps UID, SEQUENCE, METHOD (REQUEST or CANCEL), RECURRENCE-ID, the summary (200 characters), organizer and attendees, start and end converted to UTC (Outlook's Windows time zone names such as "Eastern Standard Time" are mapped to IANA zones), RRULE and EXDATE, and the join details: the link from `X-GOOGLE-CONFERENCE`, `X-MICROSOFT-SKYPETEAMSMEETINGURL`, `LOCATION` or a link to `meet.google.com`, `teams.microsoft.com`, `teams.live.com` or `*.zoom.us` found in the description, the platform, and the meeting id and passcode where the platform uses one (Zoom `pwd`, the Teams "Meeting ID" and "Passcode" lines). Nothing else from the description is kept.
- **Mail-in files:** a message with no invite may carry Markdown (`.md`), office document (`.pdf`, `.docx`, `.pptx`, `.xlsx`), audio (`.m4a`, `.mp3`, `.wav`, `.ogg`, `.webm`), video (`.mp4`, `.mov`, `.mkv`) and image (`.png`, `.jpg`, `.gif`, `.webp`) attachments. The type is decided by the file's content, not its name, and must agree with the extension; a Word, PowerPoint or Excel file must be a readable container of that kind with no macro project. At most 10 files, 25 MB per file and 30 MB in all ([#18564](https://github.com/hivecommons/hive/issues/18564), [#18699](https://github.com/hivecommons/hive/issues/18699)). The subject becomes the title. Nothing in the message is executed or followed, and a document is stored, never opened.
- **Refusals say why:** a message that leaves nothing to deliver is refused after DATA with a reply the sender's provider returns in its bounce, naming each attachment's extension and what was wrong with it, for example `550 5.6.0 nothing to deliver: a .key attachment is not an accepted type. Accepted types: md, pdf, docx, pptx, xlsx, ...` or `552 5.3.4 nothing to deliver: a .mp4 attachment is over the 25 MB per-file limit (30 MB of attachments per message)`. An attachment left out of a message that is otherwise delivered is listed in the delivery (`files.skipped`: extension and reason) so the hive can show it. Every refusal that is about the message or the address ends with three more lines: how to try again, a link to the reason's entry in [Mailing your hive: what the replies mean](inbox-voice-sources.md#mailing-your-hive-what-the-replies-mean), and where to report a problem ([#18744](https://github.com/hivecommons/hive/issues/18744)). The lines are the same for every sender. This bounce is the only message a sender gets unless the hub's confirmation reply is turned on, and the receiver never sends mail: nothing is ever sent to an address that may be forged, and a confirmation reply goes only to a sender whose mail passed DMARC alignment ([Replies to the sender](#replies-to-the-sender-off-by-default), [#18746](https://github.com/hivecommons/hive/issues/18746)).
- **Text body:** the message's body becomes a Markdown note. `text/plain` is kept as sent; an HTML-only body is reduced to its text, headings, lists and link targets, with no scripts, styles, images or other remote content. A message with no attachment becomes one Markdown item (`message.md`); a message with attachments keeps its body as the note on each of its items. The same allowlist, DMARC and approval rules apply as to attachments, and the body is untrusted data, never instructions. At most 256 KiB of it is kept.
- **Proof mails:** a message whose subject carries a proof code (`hive-proof-` and 12 letters or digits) is reported as a proof of the sender address.

The result goes to the hub with a bearer token from a mounted Secret: as JSON when its attachments total at most 512 KiB, otherwise as `multipart/form-data` streamed from the spool, with the routing (`X-Hive-Mail-Delivery`, `X-Hive-Mail-Kind`, `X-Hive-Mail-Recipient`) in headers. If the hub cannot take it, the sender's mail server is told to retry later.

The hub maps the recipient host to the hive, signs the delivery with that hive's own relay key (the key it already uses for relayed GitHub webhooks) and forwards it to the spoke; it keeps the body on disk only while it relays it. A files delivery larger than one piece (768 KiB, under the 1 MiB request limit ingress proxies apply by default) goes to the spoke's `/api/inbox/email/chunk` in order, each piece signed with its own relay delivery id `<id>.<index>.<count>`; the spoke stages the pieces and stores the files when the last one arrives. The spoke refuses anything without a valid relay signature.

### Is the address set up?

The address is shown in three places besides **Settings, Meeting attendee**: at the top of the **Inbox** (send or forward a file or a note), the **Calendar** and **Meetings** (add it to a calendar invite), for owner and read-write users ([#18693](https://github.com/hivecommons/hive/issues/18693)). Each of those sections reads it from its own state: `email_in` in `GET /api/inbox`, `GET /api/meetings/schedule` and `GET /api/meetings`.

Mail reaches a hive only when all of this is true:

1. The cluster runs `hive-mx`, and a wildcard MX record points the hives' hosts at it (the operator of the hub does this once; see [Mail receiver](env-vars.md#mail-receiver-hive-mx)).
2. The hub routes mail: it has `HIVE_HUB_MAIL_TOKEN`, and it knows this hive's host.
3. The hive is registered with that hub and holds its relay key, which arrives with the heartbeat. Without it the hive refuses every relayed delivery.
4. The switch for what you send is on: `meetings.email.files_enabled` for files and notes to the Inbox, `meetings.email.invites_enabled` for invites, and for invites meetings are enabled.

The dashboard can only see the last two from inside the hive. When the hive holds no relay key the sections show **Not set up yet** with a link here, and when a switch is off they say which; in both cases no address is shown, because mail to it would bounce. A hive that is not registered with a hub that runs `hive-mx` (most self-hosted hives) has no working address: add meetings with a link, and files with **＋ Add** or by dropping them on the Inbox.

### What hive-mx logs

Every SMTP session leaves one INFO line with its outcome
([#18699](https://github.com/hivecommons/hive/issues/18699)), so a missing
mail can be told apart from a refused one. No line holds a subject, a file
name, a body or a full address: the sender is logged as a domain only.

| Line | When | Fields |
|---|---|---|
| `hive-mx: message accepted` / `message rejected` / `message deferred` | Once per DATA | `outcome`, `code` (the SMTP reply), `reason`, `rcpt_hosts`, `sender_domain`, `bytes`, `attachments`, `accepted_files`, `skipped` (`ext:reason`), `kind`, `dmarc`, `dmarc_aligned`, `spf`, `spf_domain`, `dkim` (`domain:result` per signature), `helo`, `deliveries` (the ids the hub and the hive log), `tls`, `remote_ip` |
| `hive-mx: command refused` | A refusal at MAIL FROM or RCPT TO | `outcome`, `code`, `reason`, `stage`, `rcpt_host`, `sender_domain`, `tls`, `remote_ip` |
| `hive-mx: session ended without a message` | The sender connected, greeted and left without sending DATA | `stage` (`greeted`, `mail_from`, `rcpt_to`), `tls`, `remote_ip` |

`reason` is one of: `empty_message`, `unsupported_attachment`,
`attachment_content_mismatch`, `attachment_too_large`,
`too_many_attachments`, `message_too_large`, `malformed_message`,
`hive_daily_volume`, `hive_refused` (the hub or the hive said no: the hub's
answer is on the `hub refused delivery` line), `delivery_failed` (the hub
or the hive could not be reached; the sender retries), `unknown_recipient`
(no hive has that host), `unknown_mailbox` (not `hive@`),
`host_list_not_loaded`, `rate_limited_ip`, `rate_limited_domain`,
`receiver_busy`, `receiver_storage` and `transfer_aborted`.

`remote_ip` is the sending server's own address: the receiver's Service keeps the client address (`externalTrafficPolicy: Local`, no proxy protocol), and SPF is evaluated against it. A `dmarc` of `fail` with `spf` not `pass` and no `dkim` entry for the `From` domain means the mail left through a server its domain does not authorize.

A sender that declares a size over the limit (`MAIL FROM:<...> SIZE=n`) is
answered `552` by the SMTP library before hive-mx sees the command; that
session shows as `session ended without a message` with `stage=greeted`.

A refusal that is about the message (the first eight reasons above) is also
reported to the hive: hive-mx posts a `report` delivery (the SMTP code, the
reason, the sender's domain, the size, the attachment count and each
skipped attachment's extension and reason, nothing else) to the hub, which
signs and forwards it to the spoke's `/api/inbox/email/report`. The hive
keeps the last 50 in the Inbox's **Recent mail** list
([inbox-voice-sources.md](inbox-voice-sources.md#when-a-mail-does-not-show-up)).
Reports are best effort: a hub or hive that does not take them yet changes
nothing for the sender.

### Replies to the sender (off by default)

[#18746](https://github.com/hivecommons/hive/issues/18746) designs a short
reply from the hive to whoever mailed it ("received", or "waiting for the
owner's approval"). Hive has never sent mail, and `hive-mx` still does not:
the reply is composed, decided and sent by the hub (option A of the design:
a hub-side sender with DKIM, one fixed `From`, the hive's own address as
`Reply-To`).

It is off unless `HIVE_HUB_MAIL_REPLY_DKIM_KEY_FILE` names a file that
exists and is not empty, and the `From` address, DKIM selector and HELO name
are set (the variables are in [env-vars.md](env-vars.md)). Removing the key
file stops replies at once; a key file that cannot be loaded sends nothing
and does not use up the mail's one reply. A hub with none of this set builds
no sender and opens no connection.

What decides a reply, all of it enforced in the hub:

- **Only authenticated senders.** The mail must have passed DMARC alignment
  (`wire.Auth.Aligned`). A mail whose `From` is not aligned gets what it gets
  today (the SMTP refusal, or nothing) and no reply, so a forged `From` never
  receives backscatter.
- **Only a person's mail.** No reply to `Auto-Submitted` other than `no`,
  `Precedence: bulk`, `list`, `junk` or `auto_reply`, any `List-*` header,
  `X-Auto-Response-Suppress`, a null return path (`MAIL FROM:<>`), a delivery
  status report, the local parts `mailer-daemon`, `postmaster`, `no-reply`,
  `noreply` and `hive`, any `hive@<known hive host>`, or the configured `From`
  address and domain. `hive-mx` passes these as `wire.Delivery.Facts`; a
  delivery without them (an older `hive-mx`) is never answered. Every reply
  carries `Auto-Submitted: auto-replied`, `X-Auto-Response-Suppress: All`,
  `Precedence: auto_reply` and a null envelope sender, so a reply is never
  answered, by a hive or anyone else.
- **Only after the hive took the mail.** The hub reads the spoke's answer
  (`items`, `awaiting_approval`): stored and allowlisted is "received", stored
  and awaiting approval is "held". Refusals keep their SMTP reply; no mail is
  sent for them. Only mail-in files are answered, never invites or proof
  mails.
- **At most one reply per `Message-ID` and hive**, remembered 8 days in the
  ledger, across retries and restarts. A message without a `Message-ID` gets
  none.
- **Limits** per sender address (10 an hour, 30 a day), per sender domain
  (100 a day) and per hive (200 a day). Past a limit the reply is dropped and
  counted; the mail is unaffected.
- **No content.** The reply is plain text. It repeats nothing of the mail but a
  sanitized subject (control and invisible characters removed, 120
  characters) in its `Subject`, and the `Message-ID` in `In-Reply-To` and
  `References`. The held text does not say why the sender is not trusted.

The ledger (`HIVE_HUB_MAIL_REPLY_LEDGER_FILE`) holds the delivery id, the
outcome and salted hashes of the message id, sender, sender domain and hive:
no address, subject or body. The hub logs a sent reply with the delivery id,
the hive id and the outcome only, never a key or any part of a mail.

#### How a reply is sent

The sender (`src/pkg/hub/mail_reply_smtp.go`) runs only after the decision
and the ledger above allowed the reply, so the limits are applied before
anything is sent.

- **Plain text, null envelope sender.** One `text/plain` message, 7-bit,
  with the headers of the design, and `MAIL FROM:<>`. A failed reply
  therefore never bounces into a hive.
- **DKIM.** The message is signed (relaxed/relaxed, `rsa-sha256` or
  `ed25519-sha256`) as the `From` domain with the configured selector, over
  `From`, `To`, `Subject`, `Date`, `Message-ID`, `Reply-To`, `In-Reply-To`,
  `References`, `Auto-Submitted`, `X-Auto-Response-Suppress`, `Precedence`
  and the MIME headers. The signature expires after 72 hours.
- **Direct delivery to one MX host.** The hub looks up the sender domain's
  MX records and uses only the best-preference host: one connection, one
  message, no second host, no retry, no redirect. A domain with no MX record
  or a null MX gets no reply. The hub connects to a public IPv4 address of
  that host (IPv6 only if it has no IPv4 address), on port 25. Addresses in
  private, loopback, link-local, shared (100.64.0.0/10), documentation
  (IPv6) and similar ranges are refused, because the sender domain is
  chosen by whoever sent the mail.
- **STARTTLS is required**, with a certificate that verifies for the MX host
  name (TLS 1.2 or later). A server without it, or with a certificate that
  does not verify, gets nothing.
- **Optional relay (smarthost).** With `HIVE_HUB_MAIL_REPLY_RELAY` set to
  `<relay host>` or `<relay host>:<port>` (port 587 when none is given), the
  one connection goes to that host instead of the sender's MX, so the mail
  leaves from one stable IP that carries the reverse DNS and SPF records. It
  is off while the variable is unset, and then everything above is unchanged.
  Nothing else changes with a relay: the message is DKIM-signed by the hub
  before the hand-off (the relay must not re-sign it), the envelope sender is
  still `MAIL FROM:<>`, STARTTLS is still required with a certificate that
  verifies for the relay's host name (a relay without it, or whose handshake
  fails, gets nothing, and the hub never falls back to direct delivery), and
  any failure is still final. The relay is chosen by the operator, so unlike
  an MX host it may have a private address. If the relay needs SMTP AUTH, set
  `HIVE_HUB_MAIL_REPLY_RELAY_AUTH_FILE` to a mounted file with the user name
  on the first line and the password on the second; AUTH PLAIN is sent only
  after STARTTLS, the credentials are read from the file for each send and
  never come from an inline setting, and they are never logged. A relay
  setting that is not a host name with an optional port, or a credentials
  file named without a relay, builds no sender (the log names the variable,
  not its value); credentials that cannot be read send nothing and use up
  nothing, like an unusable key. The hub has no YAML block for this sender,
  so the relay is configured through these environment variables only. A
  relay runbook is the owner's side of this option.
- **Bounded.** 10 seconds for each DNS lookup, 15 for the connection, 45
  for the whole SMTP conversation, 90 for the whole reply. At most 8 replies
  are in flight; past that a reply is dropped and counted.
- **Any failure is final.** A refused recipient, a DNS or TLS failure, a
  timeout or a server error ends the reply: it is not retried and the
  ledger keeps the claim, so the same message is never answered twice.
- **Refused before sending.** A header value with a control character or a
  line break, an address with a display name or outside ASCII, a
  `Message-ID` that is not a plain token, a body that is not 7-bit text, or
  a `From` other than the configured one is refused before any DNS lookup
  or connection. The sender's subject only reaches the `Subject` header,
  after the template removed control and invisible characters, and is sent
  as an RFC 2047 encoded word when it has letters outside ASCII.
- **Nothing private in the log.** The hub logs a failed reply with the
  delivery id, the hive id, a reason class (for example `mx_lookup`,
  `no_mx`, `timeout`, `starttls`, `recipient_refused`, `connect`,
  `signing_key_unusable`, and with a relay `relay_address_lookup`,
  `relay_auth_unavailable`, `relay_auth`, `relay_credentials_unusable`) and, when the server refused, its numeric code.
  Never a key, an address, a domain, a subject, a body or a server's text.

#### DNS records and network the hosted hub needs

For option A with one fixed `From` (shape 1), `<project domain>` being the
project's organizational domain and `<selector>` the DKIM selector:

| Record | Type | Value |
|---|---|---|
| `<selector>._domainkey.hive.<project domain>` | TXT | `v=DKIM1; k=rsa; p=<base64 public key>` (2048-bit; the private key stays in the hub's Secret) |
| `hive.<project domain>` | TXT | `v=spf1 ip4:<sending IP> -all` (recommended: DKIM alone satisfies DMARC, since the envelope sender is null) |
| `<sending host name>` | TXT | `v=spf1 a -all` (recommended: covers the HELO identity used with a null envelope sender); this is the name to set in `HIVE_HUB_MAIL_REPLY_HELO` |
| reverse DNS of `<sending IP>` | PTR | `<sending host name>`, set at the cloud provider, not in the zone |

No existing record changes: the organizational DMARC policy (`p=reject`,
strict alignment) already covers `hive.<project domain>`, and the DKIM
signing domain equals the `From` domain, so it aligns. The hub's network
also needs outbound TCP 25 (many clouds block it by default); if it cannot
be opened the fallback is a transactional mail provider (option C of the
design), or a relay with a fixed IP (the optional relay above), in which
case the records name the relay's IP and host name and the hub itself needs
no outbound port 25. Before turning replies on for everyone, send to a test mailbox at
two large providers and check `Authentication-Results` shows `dmarc=pass`.

### Who can invite Betty

An invite is scheduled at once only when all of these hold:

- the mail's `From` address passed DMARC alignment,
- that address is the invite's organizer, and
- it is a proven calendar email of a user who has owner, read-write or read access to the hive right now.

Everything else, including any invite from someone outside the hive, appears in the **Calendar** as awaiting approval, and the owner approves or declines it there. Attendees never approve anything: being on the invite next to `hive@` does not make a sender trusted. Removing a user's access in **Manage Access** revokes their calendar emails at once.

An update to a meeting already on the calendar (a higher SEQUENCE, or a cancellation) applies only when the mail is authenticated as that meeting's organizer; anything else is ignored, so nobody can move or cancel a meeting by copying its invite. Turn invites off with `meetings.email.invites_enabled: false`; senders then get a bounce.

### Proving calendar emails

Each user registers the addresses their calendar sends invites from, under **Settings, Meeting attendee** (API: `/api/meetings/calendar-emails`). Adding an address shows a proof code such as `hive-proof-k7f3qz2mwx4a` and the instruction: send an email from that address to `hive@<host>` with this code in the subject. When that mail arrives with DMARC alignment for exactly that address, the address is marked proven. A proof mail from another address, or one that fails DMARC, is recorded on the address so the user can see why. Codes are valid for 24 hours and can be reissued; each user can register 10 addresses. Hive never sends mail, so this is how it learns which addresses are yours.

When a proof mail does not count, the address stays pending and its row says why, in plain words, within a few seconds of the mail arriving ([#18579](https://github.com/hivecommons/hive/issues/18579)):

| Reason | What the row says |
| --- | --- |
| `not_aligned` | Your mail server did not authenticate this address: neither SPF nor DKIM passed for the domain. Turn on DKIM for the domain, add the sending server to SPF, or prove an address from a provider that signs mail. |
| `from_mismatch` | The proof came from a different address than the one registered, and names the From address hive-mx parsed, or "(none: the From header was missing, repeated or unparsable)" when there was no single readable From. |
| `expired` | The code expired. Remove and add the address again. |

The row also shows when the attempt arrived and what hive-mx saw: the SPF result and domain, each DKIM signature's result and domain, and whether either was aligned with the From domain (DMARC alignment: the domain that passed SPF or DKIM must match the From address's domain). Only these results and domains are kept, never the message. **Check again** reloads the list after you fix it and resend. In the API, an unproven address carries `last_failure` with `reason`, `at`, `from` (on `from_mismatch` only: the From address seen, shown in full because the address is the user's own entry), `from_domain`, `spf`, `spf_domain`, `spf_aligned`, `dkim`, `dkim_aligned`, `dmarc`, `aligned` and `message`.

A proof mail whose code matches no address (a typo, or a code already used) has no row to land on. The owner sees it under **Recent proof attempts** in the same section (API: `recent_proof_attempts`, owner only) with the sender masked (`a***@example.com`), the time and "code not recognized". Hive keeps the latest 20 in memory; a restart clears them.

## Asking Betty to do things

People in the meeting can ask Betty, spoken or typed in the meeting chat, to draft something in the hive ([#18406](https://github.com/hivecommons/hive/issues/18406)):

| Ask | Draft | Created on approval |
|---|---|---|
| "Betty, open an issue for the lobby timeout" | an issue | through the hive's GitHub issue path (the App, with the `origin/dashboard` provenance label) on the chosen monitored repo |
| "make that an RFC, Betty" | an RFC | the same path, titled `RFC: ...` with the `rfc` label (the convention RFC notifications read) |
| "Betty, start a Spek session on calendar sync" | a Spek session | handed to the Inbox `spek_rfc` destination: a hive agent drafts the spec with spektacular and publishes it to the repo |
| "Betty, save that as a knowledge note" | a knowledge note | written to the `meetings` knowledge vault, the same write meeting notes use |

- **Recognising a request.** The line must be addressed to her (her name opens or closes it, with or without a comma before a verb such as "open", "file", "make", "start" or "save") and ask to make an item ("issue", "bug", "ticket", "RFC", "Spek session", "knowledge note"). Plain "spec" and "design" (and their plurals) also count as a Spek session when they are the object of a request: "Betty, start a spec for this", "can you write a spec for the login flow", "draft a design for X" (verbs: start, write, draft, create, kick off, open, make; optionally led by "can you", "could you", "please"). Statements about a spec ("the spec says", "per the design", "the design review happened") are not requests. Questions about an item ("what's the issue with the lobby?") and negations ("don't open an issue") are not requests.
- **Drafting.** Betty drafts a short title and body from the request and the last three minutes of the meeting, with a link back to the meeting (the dashboard's Meetings section, when `dashboard.public_url` is set) and a transcript excerpt. The repo is a monitored repo the request names ("in spektacular", "hivecommons/spektacular"), else `meetings.persona.actions.default_repo`, else `inbox.spek.repo`, else `project.primary_repo`, else the first repo. A repo the model suggests is used only when it is a monitored repo. With no meetings model she drafts from the request itself.
- **Confirmation.** She replies in the meeting chat: "Drafted an issue: '<title>'. It's waiting for approval in the hive." If the request does not say what the item is about and nothing was said before it, she asks one clarifying question ("Happy to draft an issue. What should it be about?") and drafts from the asker's next line (or "never mind" drops it). The model may ask that one question too; it never asks twice.
- **Approval.** Speaker identity in a meeting is only a display name, so nothing is created from the meeting. Every draft lands in **Drafts from meetings** in the Calendar section, with the meeting, who asked (display name), the body and the transcript excerpt. The hive owner or a read-write user can **Edit** (type, title, body, repo), **Approve** or **Reject** it. Approval creates the item; a failed creation stays in the list with its error and can be approved again after a fix.
- **Result.** Once created, Betty posts the link in the meeting chat while the meeting is live ("Approved: an issue \"Lobby timeout\" is open: https://..."); after the meeting it is added to the meeting's notes instead.
- **Auto-approve.** Off for every type. Only knowledge notes can be auto-approved (`meetings.persona.actions.auto_approve.knowledge: true`); issues, RFCs and Spek sessions always wait for a person, and config validation refuses anything else.
- **Honesty and safety.** Betty never says an item exists before it was approved and created. Meeting speech reaches the model only as data, the type comes from the request and never from the model, `@` mentions in meeting text are removed before anything is filed, and the excerpt is quoted as a code block.
- **Settings.** `meetings.persona.actions` turns requests on or off, limits the allowed types and sets the default repo and auto-approve; Settings, **Meeting attendee**, **Action requests** shows and saves it (owner), through `GET`/`PUT /api/meetings/actions/settings`; `GET /api/meetings/persona` also carries it as `actions`. A saved change applies at once. The queue lives in `<meetings.data_dir>/actions.json`; decided drafts are kept for 30 days (at most 200).

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

Spoken replies on Discord (Quorum v0.2.0 and later) go through a text-to-speech service, Kokoro's `/v1/audio/speech`, which needs two settings of its own:

- the TTS URL, `meetings.tts_url` (or `HIVE_MEETINGS_TTS_URL`). On hive-oke the `vexa-tts` Service in the `meetings` namespace answers in-cluster at `http://vexa-tts.meetings.svc.cluster.local:8880`;
- the TTS token. Paste it in the **TTS token** field in **Settings, Meetings** (or set `HIVE_MEETINGS_TTS_TOKEN`). Like the transcription token it is write-only: hive stores it keystore-sealed at `meetings.tts_token_file` (`/data/secrets/meetings_tts_token`), the API reports only `tts_token_set`, and at boot hive writes the plaintext to an owner-only file in Quorum's private runtime directory and passes it as `QUORUM_TTS_TOKEN_FILE`. On hive-oke the operator copies the token from the Secret `vexa-tts` in the `meetings` namespace.

Hive also passes `QUORUM_TTS_VOICE` (the persona's voice, `meetings.persona.voice`, default `af_heart`) and sends the same voice as `voice_id` on every `meeting.join`, so Betty sounds the same on Discord and on the Vexa platforms. Without the TTS URL and token, Discord replies stay in chat. Both apply after a restart.

Quorum v0.2.0 ignores `voice: false` on Discord (a Discord bot can always speak), so hive's own policy is the gate: it never calls `say` for a meeting whose voice is off, on any platform, and answers in chat instead.

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
| `meetings.tts_url` | none | Text-to-speech service for Discord speech (`QUORUM_TTS_URL`). |
| `meetings.tts_token_file` | `/data/secrets/meetings_tts_token` | Keystore-sealed TTS service token file. |
| `meetings.delivery` | `chat` | Where meeting output goes. |
| `meetings.voice_on_address` | `true` | Allow speaking when addressed in a voice-enabled meeting. |
| `meetings.listening_tone` | `true` | Betty plays a short, soft two-note chime when she heard her name in a spoken request and is about to answer, so the room knows she is listening (`HIVE_MEETINGS_LISTENING_TONE`). The only setting: no volume or sound options. It plays only when voice is on (a companion meeting plays it in the companion on the host's computer instead), never over speech, and is dropped silently if the audio path is busy. |
| `meetings.bot_name` | the persona's display name | Bot display name; `hive (notes)` with the persona off. |
| `meetings.invite_policy` | `owner` | Who may send hive to a meeting. |
| `meetings.research_pool_size` | `2` | Research agents (`research-1..N`). |
| `meetings.research_per_meeting` | `5` | Research items per meeting before approval is needed. |
| `meetings.chat_posts_per_minute` | `6` | Chat post rate limit per meeting. |
| `meetings.chat_answer_burst` | `3` | Extra chat posts a minute for Betty's answers to requests addressed to her, once the rate limit is spent (`HIVE_MEETINGS_CHAT_ANSWER_BURST`). |
| `meetings.lobby_timeout_s` | `600` | Seconds to wait in a lobby. |
| `meetings.budget.max_tokens_per_meeting` | `200000` | LLM tokens per meeting (when Spek reports usage). |
| `meetings.budget.max_tokens_per_hour` | `500000` | Hive-wide hourly ceiling. |
| `meetings.budget.max_tokens_per_day` | `2000000` | Hive-wide daily ceiling. |
| `meetings.budget.max_intents_per_meeting` | `500` | Intents per meeting when Spek reports no usage. |
| `meetings.retention_days` | `30` | Days a transcript is kept after the meeting ends. |
| `meetings.consent_text` | see above | Consent notice, at most 2000 characters. |
| `meetings.data_dir` | `/data/meetings` | Meeting records and transcripts. |
| `meetings.llm.mode` | `auto` | `auto`, `openai`, `anthropic`, `agent-backend`, `claude-cli` or `none` (`HIVE_MEETINGS_LLM_MODE`). |
| `meetings.llm.backend` | the agents' most common backend | Agent backend for `agent-backend`: `claude` or `litellm` (`HIVE_MEETINGS_LLM_BACKEND`). |
| `meetings.llm.endpoint` | `governor.litellm` endpoint | OpenAI-compatible base URL for Spek's model. |
| `meetings.llm.model` | `governor.litellm.default_model`; `claude-sonnet-5-5` on the `claude` backend | Model name. |
| `meetings.llm.api_key_file` | `/data/secrets/meetings_llm_api_key` | Keystore-sealed key; falls back to the `governor.litellm` key. |
| `meetings.llm.anthropic_key_file` | `/data/secrets/meetings_anthropic_api_key` | Keystore-sealed Anthropic key. |
| `meetings.llm.anthropic_model` | Spektacular's default | Anthropic model. |
| `meetings.llm.timeout_ms` | `2500` | Per-call model timeout. |
| `meetings.llm.max_in_flight` | `4` | Concurrent model calls across meetings. |
| `meetings.llm.per_meeting_in_flight` | `3` | Concurrent model calls in one meeting, so Betty answers several requests at once; never above `max_in_flight` (`HIVE_MEETINGS_LLM_PER_MEETING_IN_FLIGHT`). Each call in flight reserves its expected cost against the meeting's token budget, so parallel answers cannot overspend it. |
| `meetings.email.invites_enabled` | `true` | Accept calendar invites sent to `hive@<host>` (`HIVE_MEETINGS_EMAIL_INVITES_ENABLED`). |
| `meetings.email.files_enabled` | `true` | Accept mail-in files as Inbox items (`HIVE_MEETINGS_EMAIL_FILES_ENABLED`). |
| `meetings.persona.enabled` | `true` | The meeting attendee persona. |
| `meetings.persona.preset` | `betty` | Built-in persona the fields below start from. |
| `meetings.persona.name` | `Betty` | Name she answers to. |
| `meetings.persona.display_name` | `Betty Bee from the hive` | Meeting display name, at most 64 characters. |
| `meetings.persona.aliases` | `[betty, betty bee, hive]` | Names that address her. |
| `meetings.persona.pronouns` | `she/her` | Kept for compatibility; never stated in any reply. |
| `meetings.persona.language` | `en-US` | BCP-47 reply language and locale. |
| `meetings.persona.voice` | `af_heart` | TTS voice id for spoken replies. |
| `meetings.persona.voice_type` | `female` | `female`, `male` or `bot` (descriptive). |
| `meetings.persona.accent` | `American` | Descriptive. |
| `meetings.persona.intro_template` | the preset's intro | Intro text. |
| `meetings.persona.leave_phrases` | `[betty leave, hive leave]` | Phrases that remove the bot. |
| `meetings.persona.humor` | `light` | `light` or `off`. |
| `meetings.persona.actions.enabled` | `true` | Let people ask Betty to draft issues, RFCs, Spek sessions and knowledge notes; each waits for approval. |
| `meetings.persona.actions.types` | all four | Allowed types: `issue`, `rfc`, `spek`, `knowledge`. |
| `meetings.persona.actions.default_repo` | `inbox.spek.repo`, then `project.primary_repo`, then the first repo | `owner/repo` a draft targets when the request names no monitored repo. |
| `meetings.persona.actions.auto_approve.knowledge` | `false` | Save knowledge notes without approval. No other type may be auto-approved. |
| `meetings.persona.unprompted.per_10m` | `2` | Unprompted answers per ten minutes. |
| `meetings.persona.unprompted.min_gap_s` | `90` | Seconds between unprompted answers. |
| `meetings.persona.unprompted.per_meeting` | `12` | Unprompted answers per meeting. |
| `meetings.persona.unprompted.answer_wait_s` | `8` | Seconds to wait for a person to answer first. |
| `meetings.persona.unprompted.min_confidence` | `0.75` | Model confidence an unprompted answer needs. |
| `meetings.persona.unprompted.queue_depth` | `3` | Room questions waiting at once. |
| `meetings.persona.unprompted.stale_s` | `60` | Seconds before a waiting question is dropped. |
| `meetings.persona.searxng_url` | none | SearXNG instance for web answers (for example `http://searxng.meetings.svc.cluster.local:8080`). |

When the budget runs out, hive posts one notice in the meeting and keeps taking notes, but stops asking questions, starting research and speaking. See [The meeting budget](#the-meeting-budget) for how it is counted and what happens at 80, 90 and 100 percent.
