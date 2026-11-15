# Inbox transcription (Deepgram)

Some recordings reach the [Inbox](inbox-voice-sources.md) without any text:
a Plaud recording that was never transcribed in the Plaud app, or a meeting
recording without captions. Inbox transcription turns the audio of such an
item into a speaker-labelled transcript with [Deepgram](https://deepgram.com)
so it can be read, routed to an RFC or dibs, and searched like any other item.

It is **off by default**.

## Each hive owns its transcriptions

Transcription is spoke-local. Nothing about it is shared or central:

- **Your key, your account.** The hive owner supplies their own Deepgram API
  key through an environment variable on their hive. Audio is billed to that
  account. No hive pays for, or sees, another hive's transcriptions.
- **Your data volume.** The transcript is written only to this hive's data
  volume, under `connector-state/transcripts/<source>/<id>.json` (mode
  `0600`, written atomically). It is not sent to the hub, to other spokes, or
  back to the recording's source.
- **Your budget.** Monthly minutes, a daily item cap and size/duration limits
  are configured per hive and checked before any audio is uploaded.

## Security and privacy model

- **Audio leaves the hive to Deepgram.** When an item is transcribed, the
  hive downloads the recording from its source (for Plaud, a fresh presigned
  link minted for this run, as the ▶ Play button does) and streams the bytes
  to `https://api.deepgram.com/v1/listen` under the owner's key. Deepgram
  never receives the presigned link; it receives the audio itself. The link
  is never stored or logged: logs name the audio host only.
- **No training.** Every request sets Deepgram's `mip_opt_out=true`, which
  opts the audio out of the Deepgram Model Improvement Program.
- **Nothing else is kept.** The hive stores the transcript text and the run
  state (status, attempts, provider, model, language, audio length, the
  download's size and ETag). It does not keep the audio.
- **The key is never echoed.** The key is read from the environment at run
  time. Settings and the UI show only whether the named variable is set
  (*key present* / *key missing*). A rejected key (HTTP 401/403) fails the run
  with "check the Deepgram key in `$DEEPGRAM_API_KEY`", naming the variable,
  never its value.
- **Consent.** Recordings contain other people's voices. Enabling
  transcription requires `consent_acknowledged: true`; the ⚙ settings panel
  shows a notice that the audio is sent to Deepgram under the hive owner's
  own account before the checkbox can be ticked. A config with
  `enabled: true` and no consent is rejected at load and on save.
- **Untrusted text.** A machine transcript is third-party speech like any
  other Inbox text. It reaches spektacular and agent prompts through the same
  untrusted-data envelope as source transcripts (see
  [inbox-voice-sources.md](inbox-voice-sources.md)).
- **Audit.** Every run writes one `inbox_transcribe` audit entry: actor,
  source, item id, provider, model, audio seconds, estimated cost and
  outcome (with the reason for a failure or skip). Never the link or key.

## Configuration

```yaml
inbox:
  transcription:
    enabled: false              # off by default
    mode: manual                # manual | auto
    provider: deepgram          # the only provider today
    model: nova-3               # Deepgram pre-recorded model
    api_key_env: DEEPGRAM_API_KEY   # env var holding the owner's key
    sources: []                 # auto mode only transcribes these source names
    diarize: true               # label speakers: Speaker 1, Speaker 2, ...
    language: auto              # or a language code: en, de, pt-BR
    max_duration_minutes: 120   # longer recordings are skipped
    max_bytes: 524288000        # uploads are cut (and skipped) past 500 MiB
    monthly_budget_minutes: 600 # audio minutes per UTC month
    daily_max_items: 20         # runs per UTC day
    consent_acknowledged: false # required to enable
```

Set the key in the hive's environment (for example a Kubernetes Secret
mapped to `DEEPGRAM_API_KEY`); it never goes into `hive.yaml`. The limits can
also be overridden by environment variables, which win over `hive.yaml`:
`HIVE_INBOX_TRANSCRIPTION_MAX_DURATION_MINUTES`,
`HIVE_INBOX_TRANSCRIPTION_MAX_BYTES`,
`HIVE_INBOX_TRANSCRIPTION_MONTHLY_BUDGET_MINUTES` and
`HIVE_INBOX_TRANSCRIPTION_DAILY_MAX_ITEMS`.

Everything above is editable in the Inbox under **⚙ Auto-routing →
Transcription** and in **Settings → Inbox** (`POST /api/inbox/settings` with
a `transcription` object).

## Transcribing an item

Items that are pending (no summary, no transcript) and whose source can mint
audio show a **🎧 Transcribe** button for owners and read-write sessions. It
calls `POST /api/inbox/{source}/{id}/transcribe`:

1. Gates: transcription must be enabled and consented (`412` otherwise); an
   item that is queued, running or already transcribed, or that has text
   from its source, answers `409`.
2. Checks that need no upload: a source without audio (`unsupported`), a
   recording longer than `max_duration_minutes` (`too long`), the daily item
   cap or the monthly minutes budget (`budget`). These are recorded as
   **skipped** with the reason and answered `200`.
3. Otherwise the item is **queued** (`202`) and runs in the background, one
   job at a time per hive: **running** → **done**, **failed** (with the
   reason), or **skipped** (`too large`, when the download is bigger than
   `max_bytes`; a limit reader also cuts a stream without a length).
4. HTTP 429, 5xx and network errors are retried up to 3 attempts with
   exponential backoff; each attempt downloads the audio afresh. A failed or
   skipped item keeps its **Retry transcription** button.

The item shows a pill for the state (with a spinner while queued or running,
and the failure reason). Once done, the item is no longer pending and its
*Transcript* disclosure shows the text under **Transcript (machine:
deepgram/nova-3)**, in the same `[mm:ss] Speaker N: text` line shape as Plaud
and meeting transcripts.

## Automatic transcription (auto mode)

With `mode: auto`, the hive also transcribes new pending items by itself,
but only from the sources listed in `inbox.transcription.sources` (an Inbox
source type such as `plaud`, or a `knowledge.connectors[]` entry name). An
empty list transcribes nothing automatically. The sweep runs after each
connector sync, right after the auto-routing rules:

- It queues only what today's `daily_max_items` and this month's
  `monthly_budget_minutes` leave room for; items that do not fit stay
  pending, without a skip record, and are picked up once budget frees up.
- Runs still execute one at a time. An item that is queued, running or done
  is never queued again, and a recording listed twice is queued once.
- A **failed** item is retried after 1 hour, then 2 hours, up to 3 runs in
  total. An item **skipped for budget** is retried once the UTC day turns.
  Items skipped as too long, too large or unsupported are not retried
  automatically. The owner can always click **Retry transcription**.
- Sweep-started runs are audited as `inbox_transcribe` with the actor
  `inbox-auto-transcribe`.

The ⚙ settings panel shows this month's minutes used against the budget,
today's runs against the daily limit, and the egress the hive needs.

## Egress

The hive process itself uploads the audio, so a spoke behind an egress
allowlist must allow `api.deepgram.com:443` while transcription is enabled
(and the source's audio host, which playback already needs). Nothing is
needed while transcription is off. `settings.transcription.egress_hosts` and
the boot log line `inbox transcription enabled` name the host. See
[network-requirements.md](network-requirements.md).

## Upstream text wins

The machine transcript is a fallback. If the source later produces its own
summary or transcript (for example the owner runs transcription in the Plaud
app), the Inbox shows the source's text and the machine transcript is no
longer used. If the source is unreachable, the stored machine transcript
still serves the item's content.

## Cost

Deepgram bills the owner's account per audio minute. The audit entry carries
an estimate from Deepgram's published pay-as-you-go nova-3 price; the
Deepgram invoice is authoritative. The monthly budget counts the audio length
Deepgram reports for each finished run.

## Providers

`src/pkg/transcribe` defines a small `Transcriber` interface (`Name`,
`Transcribe(ctx, Audio) (Result, error)`) with a Deepgram adapter and a fake
for tests. Another provider (for example an OpenAI-compatible
`/audio/transcriptions` endpoint) can be added behind the same interface.

## API

| Method | Path | Access | |
| --- | --- | --- | --- |
| `POST` | `/api/inbox/{source}/{id}/transcribe` | Owner or read-write | Queue a transcription (202), or report an up-front skip (200); 409 busy/done/has text, 412 disabled or not consented, 404 unknown item |
| `GET` | `/api/inbox` | Dashboard auth | Items carry `transcription` (`status`, `reason`, `attempts`, `provider`, `model`, `updated_at`) and `transcribable`; `settings.transcription` carries the effective config, `key_present`, `month_minutes_used` and `today_items` |
| `GET` | `/api/inbox/{source}/{id}/content` | Dashboard auth | Falls back to the machine transcript, with `transcript_label` |

See also [api-reference/knowledge.md](api-reference/knowledge.md).
