# Inbox transcription

Some recordings reach the [Inbox](inbox-voice-sources.md) without any text:
a Plaud recording that was never transcribed in the Plaud app, or a meeting
recording without captions. Inbox transcription turns the audio of such an
item into a timestamped transcript so it can be read, routed to an RFC or
dibs, and searched like any other item.

It is **off by default**.

## Providers

`inbox.transcription.provider` is one of:

| Provider | Account | Where the audio goes | Speakers | Speed |
| --- | --- | --- | --- | --- |
| `whisper` | none (built into the hive image) | nowhere: decoded and transcribed on the hive | no | a few minutes for a 5-minute recording on 2 to 4 vCPU |
| `gemini` | the hive's own Gemini credentials | Google (Gemini API) | yes | under a minute for a 5-minute recording |
| `deepgram` | the owner's Deepgram key | Deepgram (`mip_opt_out=true`) | yes | seconds, plus the upload |
| `auto` (default) | | whichever provider it picks | | |

`auto` resolves on every run, by what is available on this hive:

| Order | `auto` uses | when |
| --- | --- | --- |
| 1 | `whisper` | the whisper engine is in the hive image (its model is on disk or may be downloaded) |
| 2 | `gemini` | whisper is unavailable and the hive has Gemini credentials |
| 3 | `deepgram` | whisper and Gemini are unavailable and the Deepgram key is set |
| 4 | `deepgram`, not ready | nothing is available: items show "provider not ready" and nothing is sent |

An explicit `whisper`, `gemini` or `deepgram` is used as is, with no
fallback: Deepgram is used only when the owner selects it (or as the last
resort of `auto`). Existing configs with `provider: deepgram` keep their
behaviour; the ⚙ settings panel has always written `provider: deepgram`
explicitly. A config that never set `provider` now resolves through
`auto`. The ⚙ panel shows each provider's readiness (whisper engine and
model present, Gemini credentials present, Deepgram key present) without
ever showing a key, and which provider `auto` currently picks
(`settings.transcription.resolved_provider`, `providers[]`).

Hive versions without the whisper or Gemini engine report them as *not
ready* (`not_installed`), so `auto` behaves as `deepgram` there.

## Built-in whisper

The hive image bundles [whisper.cpp](https://github.com/ggml-org/whisper.cpp)
(MIT, v1.9.5) and a minimal FFmpeg (8.1.3), about 12 MB together, built in
the `whisper-builder` stage of `src/Dockerfile` for amd64 and arm64 from
tarballs pinned by version and SHA-256. whisper.cpp ships every CPU variant
of its ggml backend and picks the best at run time (AVX2/AVX-512 on amd64,
dotprod/fp16/SVE on arm64), so one image runs on old and new cores alike.

**Decoder.** Recordings arrive as mp3, m4a/mp4 (AAC), webm/ogg (Opus,
Vorbis), FLAC or WAV, and whisper.cpp needs 16 kHz mono WAV. A separate
FFmpeg binary does the conversion, rather than a whisper.cpp build that links
a decoder, so decoding is its own process with its own limits and timeout,
and the decoded length is known before transcription starts (the progress
bar's total). FFmpeg is built with `--disable-everything`, only the audio
demuxers, decoders, the WAV muxer and the `file`/`pipe` protocols, no
network protocols, no autodetected external libraries, and without
`--enable-gpl` or `--enable-nonfree`. It is therefore LGPL-2.1-or-later, which
the build asserts. The source is the official `ffmpeg.org` release tarball.

**Model.** The default is `base.en` (148 MB), or `base` when `language`
names another language. Set `inbox.transcription.whisper_model` to `tiny`,
`tiny.en`, `base`, `base.en`, `small` or `small.en`. No model ships in the
image. On the first whisper run the hive downloads the model over HTTPS from
the official ggml host (`huggingface.co/ggerganov/whisper.cpp`, pinned to
one repository revision, served from the `*.hf.co` CDN) into
`/data/models/whisper/ggml-<model>.bin`. The download is capped at the
pinned size and checked against the pinned SHA-256 before it is renamed
into place; a file with the wrong size or checksum is replaced. That first
run needs egress to those hosts, later runs none.

**Air-gapped hives.** Copy the model file into `/data/models/whisper/`
(`HIVE_WHISPER_MODEL_DIR`) and set `HIVE_WHISPER_MODEL_DOWNLOAD=false`. The
hive still verifies the size and SHA-256 before using it. The pinned values
are in `src/pkg/transcribe/whisper_model.go`; for `base.en` they are 147964211
bytes and `a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002`.

**Speed.** `base.en` was chosen because it handles a 5-minute recording in a
few minutes even with half of 2 vCPU. Measured with the image's own build in
a 2-CPU container (Apple M2 Pro, arm64), a 301-second recording took 56 s
with 1 thread and 31 s with 2 threads (0.19x and 0.10x real time). Cloud
vCPUs are typically two to three times slower per thread, so expect about 1
to 3 minutes for 5 minutes of audio on a 2 to 4 vCPU hive. `small` is about
three times slower again. The first estimate assumes 0.6x real time; after
that the hive uses its own measured speed.

**Threads and concurrency.** One transcription runs at a time per hive.
whisper uses `inbox.transcription.whisper_threads` threads, by default half
the CPUs the hive may use (at least one), so agents keep the rest. The
engines also run at a lower CPU priority (`nice 10`).

**Progress.** whisper reports measured progress: the sandbox reads
whisper.cpp's `-pp` percentages and the segment timestamps as they stream,
so the bar moves with the audio actually transcribed.

**Output.** whisper.cpp's JSON (`-oj`) segments become `[mm:ss] text` lines.
whisper has no speaker labels; non-speech markers such as `[BLANK_AUDIO]`
are dropped, and `--suppress-nst` keeps it from writing non-speech tokens.

## Gemini

`provider: gemini` (or `auto` when whisper is unavailable) transcribes with
the Gemini API under **the hive's own Gemini credentials**: the API key in
`GEMINI_API_KEY`, `GOOGLE_API_KEY` or `GOOGLE_GENAI_API_KEY`, the same
variables and order the dashboard's Gemini model discovery already reads.
No new key is added. A Gemini CLI login (OAuth in `~/.gemini`) used by agents
is not used for transcription; the hive needs the API key.

- The hive calls `POST /v1beta/models/<model>:generateContent` on
  `generativelanguage.googleapis.com` with the key in the `x-goog-api-key`
  header (never in a URL). Recordings up to 14 MB go inline; larger ones go
  through the Files API resumable upload and are deleted right after the run
  instead of waiting for the 48-hour expiry. Recordings over 256 MB or 9.5
  hours are not sent.
- The model (`inbox.transcription.gemini_model`, default `gemini-3.5-flash`)
  is asked for a verbatim transcript as JSON segments with `[mm:ss]` starts
  and `Speaker N` labels (none when `diarize: false`), and told not to invent
  speech during silence, music or noise. The reply is validated; anything
  that is not the expected JSON is kept as one plain-text segment.
- 429 and 5xx are retried (3 attempts, exponential backoff), honouring
  `Retry-After` up to 2 minutes. 401/403 fail with "check the hive's Gemini
  key", naming the variables, never the value. Error bodies are redacted.
- Audited as provider `gemini`, model `<model>`; the audit's cost estimate is
  0 (the Google invoice is authoritative).

**Caveats.** Gemini is a chat model, not a speech recogniser. Its timestamps
are approximate, its speaker labels are a best guess, and it may still
produce words where there was silence. That is why the transcript is always
labelled `Transcript (machine: gemini/<model>)`. The audio is sent to Google
under the hive's account, so the consent notice applies.

### Sandbox

Recordings are untrusted media, so neither FFmpeg nor whisper.cpp runs inside
the hive process or as the hive's uid:

- **Separate uid.** The hive (`dev`) starts `/usr/local/bin/hive-whisper`
  through su-exec as `hive-transcribe` (uid 1901): no home, no shell, no
  supplementary groups. It cannot read `/data`, the hive's credentials or any
  agent's files.
- **No environment, no files.** The sandbox gets an empty environment, the
  audio on stdin and the model as an inherited read-only file descriptor. It
  writes the JSON transcript to stdout and progress lines to stderr.
- **Private, temporary work dir.** The sandbox creates a 0700 directory
  (umask 077) for the input and the decoded WAV and removes it on exit,
  including after a timeout.
- **Limits.** Each engine runs under `prlimit` (address space 3 GiB by
  default, `HIVE_WHISPER_MEMORY_MB`; CPU seconds; file size; 64 open files;
  no core dumps) and `nice`, in its own process group, which is killed when
  its stage times out. The wall-clock limit is 3 minutes plus 4x the
  recording length, and the transcribe stage gets 1 minute plus 4x the
  decoded length.
- **No network.** FFmpeg is built without network protocols and whisper-cli
  has no network code. Where the kernel allows unprivileged user namespaces,
  both also run in an empty network namespace (`unshare --user
  --map-current-user --net`); the sandbox logs which isolation applied.
- **Fail closed.** Without su-exec or the `hive-transcribe` user, whisper
  reports *not ready* and `auto` moves on. `HIVE_WHISPER_RUN_AS=self` runs the
  sandbox as the hive's own uid, still with an empty environment and limits,
  for development hosts only.

## Each hive owns its transcriptions

Transcription is spoke-local. Nothing about it is shared or central:

- **Your hive, your account.** whisper runs on the hive's own CPU with no
  account. Gemini and Deepgram run under the hive's own credentials (the
  owner's Deepgram key, the hive's Gemini credentials) and bill that
  account. No hive pays for, or sees, another hive's transcriptions.
- **Your data volume.** The transcript is written only to this hive's data
  volume, under `connector-state/transcripts/<source>/<id>.json` (mode
  `0600`, written atomically), and into the item's Knowledge fact on the
  same volume. It is not sent to the hub, to other spokes, or back to the
  recording's source.
- **Your budget.** Monthly minutes, a daily item cap and size/duration limits
  are configured per hive and checked before any audio is uploaded. With
  whisper the minutes budget limits CPU time on the hive, not money.

## Security and privacy model

- **Deepgram: audio leaves the hive to Deepgram.** When an item is transcribed with Deepgram, the
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
  shows a notice for the selected provider before the checkbox can be
  ticked: with whisper the audio stays on the hive, with Gemini or Deepgram
  it is sent to Google or Deepgram under the hive's own account. A config with
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
    provider: auto              # auto | whisper | gemini | deepgram
    model: nova-3               # Deepgram pre-recorded model
    whisper_model: ""           # base.en by default (base for other languages)
    whisper_threads: 0          # 0 = half the CPUs
    gemini_model: ""            # gemini-3.5-flash by default
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

The form has two parts, **Routing** (default RFC repository, dibs URL,
rules) and **Transcription**
([#18701](https://github.com/hivecommons/hive/issues/18701)). Under
Transcription: the provider selector with one row per provider (name, what
it does, a ready or not ready pill, and what is missing on its own line);
the consent notice with the consent and enable boxes; then the options and
the limits. The fields of a provider (Whisper model and threads, Gemini
model, Deepgram key variable and model, speaker labels) show only while
that provider is selected, or, with `auto`, for every provider auto could
pick (the ready ones); **Auto-mode sources** shows in auto mode. A field
that is not shown keeps its value, and saving sends the same settings. Each
part lists its config keys under **Config keys**. In the dialog, Cancel and
Save stay in view while the form scrolls.

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
   job at a time per hive in the order they were queued: **running** →
   **done**, **failed** (with the reason), or **skipped** (`too large`, when
   the download is bigger than `max_bytes`; a limit reader also cuts a
   stream without a length).
4. HTTP 429, 5xx and network errors are retried up to 3 attempts with
   exponential backoff; each attempt downloads the audio afresh. A failed or
   skipped item keeps its **Retry transcription** button.

The item shows a pill for the state (with a spinner while queued or running,
and the failure reason with a **Retry transcription** button for owners and
read-write sessions). Once done, the item is no longer pending and its
*Transcript* disclosure shows the text under **Transcript (machine:
deepgram/nova-3)**, in the same `[mm:ss] Speaker N: text` line shape as Plaud
and meeting transcripts (`[mm:ss] text` when the provider has no speakers).

## Progress and time remaining

While an item is queued or running it shows a progress bar (the connector
sync styling) with the phase, the percentage, *about N min left* and the
elapsed time; a queued item shows how many transcriptions are ahead of it.
The item state carries:

| Field | |
| --- | --- |
| `phase` | `downloading`, `decoding` (whisper), `transcribing`, `finalizing` |
| `progress` | 0 to 1 |
| `estimated` | true when progress and ETA come from a speed estimate rather than measured output |
| `started_at` | when the run started |
| `audio_seconds` / `processed_seconds` | recording length / audio transcribed so far (measured providers) |
| `eta_seconds` | estimated seconds left |
| `queue_position` | jobs ahead while queued |

How progress is computed:

- **Measured (whisper).** Processed audio seconds over the recording
  length. The ETA is the transcribing time so far divided by progress, times
  what is left, smoothed so one fast or slow tick does not make the text
  jump. Below 2% it uses the estimate instead.
- **Estimated (Deepgram, Gemini, and whisper before its first tick).** One
  request has no streaming progress, so progress is elapsed time over the
  expected time, held at 95% until done. The expected time is this hive's
  measured speed for that provider and model, an exponential moving
  average of the real-time factor of its finished runs (stored with the
  usage ledger, `_usage.json` `speeds`), or until the first run a
  conservative typical speed: Deepgram 10 s + 0.05x, Gemini 15 s + 0.15x,
  whisper 5 s + 0.6x the recording length.

The Inbox polls `GET /api/inbox/transcriptions` every 3 seconds while a job
is queued or running and redraws only that item's progress; it stops when
nothing is busy and re-reads the Inbox once when a job finishes.

## Recordings are transcribed automatically

Audio the hive holds itself is transcribed as soon as it lands in the Inbox,
whatever `mode` says, once transcription is enabled with consent: dashboard
recordings (when they are finalized), dropped audio and video files, and
mailed-in audio (at once from a trusted sender, otherwise when the owner
approves it). Nobody clicks **Transcribe**; the item shows *Transcribing
soon* until its run is queued, then the progress bar.

- **Idempotent.** An item that is queued, running or done is never started
  again, whichever hook (creation, sweep, a second click) asks.
- **Restarts.** At boot, records the previous process left `queued` or
  `running` become `failed` with an `interrupted: ...` reason, and the boot
  sweep queues them again at once, together with every recording still
  pending. A run that had started counts toward the 3-run bound (so a
  recording that keeps crashing the hive stops being retried); one that was
  only waiting in the queue does not.
- **Failures** are kept on the item (`transcription.status: failed` with the
  `reason`) and logged, and retried as described below; **Retry
  transcription** stays available for them.
- **Concurrency.** Runs execute in queue order, one at a time by default;
  `HIVE_INBOX_TRANSCRIBE_CONCURRENCY` (max 8) allows more.

Meetings the hive attended through Quorum arrive with Quorum's live
transcript, so there is nothing to transcribe for them.

## Automatic transcription (auto mode)

With `mode: auto`, the hive also transcribes new pending items by itself,
but only from the sources listed in `inbox.transcription.sources` (an Inbox
source type such as `plaud`, or a `knowledge.connectors[]` entry name). An
empty list transcribes nothing automatically. The sweep runs after each
connector sync, right after the auto-routing rules:

- It queues only what today's `daily_max_items` and this month's
  `monthly_budget_minutes` leave room for; items that do not fit stay
  pending, without a skip record, and are picked up once budget frees up.
- Runs still execute in queue order (one at a time by default). An item that is queued, running or done
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

Egress depends on the provider (always plus the source's audio host, which
playback already needs; nothing while transcription is off):

- `whisper`: none, except `huggingface.co:443` and its `*.hf.co` CDN for the
  first-use model download (none at all with a pre-seeded model).
- `gemini`: the hive process sends the audio to
  `generativelanguage.googleapis.com:443`.
- `deepgram`: the hive process uploads the audio to `api.deepgram.com:443`.

`settings.transcription.egress_hosts` (for the provider `auto` resolves to)
and the boot log line `inbox transcription enabled` name the hosts. See
[network-requirements.md](network-requirements.md).

## Upstream text wins

The machine transcript is a fallback. When a run finishes, the hive also
writes the transcript into the item's Knowledge fact (marked
`attr_<x>_content: machine` with `attr_transcript_provider`), so knowledge
search, nous and agents see the text, not only the Inbox; see
[What knowledge an Inbox item becomes](inbox-voice-sources.md#what-knowledge-an-inbox-item-becomes).
Later syncs that still find no text upstream keep that fact as it is. If
the source later produces its own summary or transcript (for example the
owner runs transcription in the Plaud app), the next sync replaces the
machine text in the fact, the Inbox shows the source's text and the machine
transcript is no longer used. If the source is unreachable, the stored machine transcript
still serves the item's content.

## Cost

whisper costs CPU time on the hive only, so for whisper the monthly minutes
budget is a CPU-time budget. Deepgram bills the owner's account per audio minute. The audit entry carries
an estimate from Deepgram's published pay-as-you-go nova-3 price; the
Deepgram invoice is authoritative. The monthly budget counts the audio length
Deepgram reports for each finished run.

## Adapters

`src/pkg/transcribe` defines a small `Transcriber` interface (`Name`,
`Transcribe(ctx, Audio) (Result, error)`) with a Deepgram adapter and a fake
for tests. Adapters report phases and measured progress through
`Audio.Progress`; `transcribe.TypicalSpeed` holds each provider's default
speed for the estimate.

## API

| Method | Path | Access | |
| --- | --- | --- | --- |
| `POST` | `/api/inbox/{source}/{id}/transcribe` | Owner or read-write | Queue a transcription (202), or report an up-front skip (200); 409 busy/done/has text, 412 disabled or not consented, 404 unknown item |
| `GET` | `/api/inbox` | Dashboard auth | Items carry `transcription` (`status`, `reason`, `attempts`, `provider`, `model`, `updated_at`, and the progress fields above while busy) and `transcribable`; `settings.transcription` carries the effective config, `resolved_provider`, `resolved_model`, `providers[]` (`name`, `available`, `status`, `detail`, `audio_goes_to`, `model`, `egress`), `key_present`, `month_minutes_used` and `today_items` |
| `GET` | `/api/inbox/transcriptions` | Dashboard auth | Queued and running transcriptions in queue order with live progress (`active[]`: `source`, `id`, `state`); reads no source, for polling |
| `GET` | `/api/inbox/{source}/{id}/content` | Dashboard auth | Falls back to the machine transcript, with `transcript_label` |

See also [api-reference/knowledge.md](api-reference/knowledge.md).

## Transcribe first, then share

Choosing RFC… or Knowledge from 🔗 Share → Hive on an item that has audio but no text starts a transcription for it (the Transcribe path above, with the same consent, budget and daily caps) and keeps the choice as a pending share. The share runs when the transcript completes; the item's progress bar reads "Transcribing, then → RFC". The pending share holds only the dialog choices (destination, who chose it, repository), never item text, and is stored with the item state, so a restart resumes it. If the transcription fails or is skipped, nothing is sent and the item shows the reason with a Retry in the Share menu.
