# Recording into the Inbox

**● Record** in the [Inbox](inbox-voice-sources.md) records a meeting or a
note straight from the dashboard, without a recorder device or a meeting
connector. The audio is uploaded to this hive while you record, the finished
recording appears in the Inbox like any other item, and it can be
transcribed, read and shared (Share → Hive: RFC, spek, Knowledge, DIBS).

## Who can record

Owners and read-write members. Read-only members do not see ● Record and the
API answers them 403. Each member may have one recording in progress at a
time. An owner can abort anyone's recording; everyone else can only stop or
abort their own.

## Recording a meeting tab

Choose **Microphone and a meeting tab's audio** in the ● Record dialog to
record a video meeting running in another browser tab together with your own
voice. The browser asks which tab to share: pick the meeting tab and tick
**Share tab audio**. The hive keeps only the audio (the video track is
dropped at once) and mixes it with the microphone in the browser. The
recorder panel shows a level meter for each.

If the shared tab has no audio track (Share tab audio was not ticked, or a
window or screen was shared instead), the recorder warns and records the
microphone only. Stopping the share in the browser bar keeps the recording
going with the microphone. The dashboard's `Permissions-Policy` allows
display capture for its own origin only (`display-capture=(self)`); the
terminal and API responses keep it closed.

## Browser support

| Browser | Microphone | Microphone + tab audio | Format |
| --- | --- | --- | --- |
| Chrome, Edge, Brave (desktop) | yes | yes | WebM/Opus |
| Firefox (desktop) | yes | no (Firefox shares no tab audio) | WebM/Opus |
| Safari (macOS) | yes | no (Safari shares no tab audio) | MP4/AAC (fragmented) |
| Safari, Chrome (iPhone, iPad) | yes, while the screen stays on | no (no tab sharing) | MP4/AAC |
| Chrome (Android) | yes, while the page stays in front | no (no tab sharing) | WebM/Opus |

Where tab audio is not available the option is hidden and the dialog says
why. ● Record itself is hidden where the browser has no `MediaRecorder` or no
microphone access (the dashboard must be served over https).

### Phones and tablets

- **Keep the screen open.** iOS stops capture when the screen locks or
  another app takes the microphone; the recorder then ends the recording with
  what it has. The recorder asks for a Wake Lock so the screen stays on, and
  warns when the page is hidden.
- Android stops capture when the browser goes to the background on many
  devices; keep the dashboard in front.
- A network drop does not lose audio: chunks wait in the browser (IndexedDB)
  and upload when the connection is back. If the page is reloaded or closed,
  the queued chunks upload and the recording is finished on the next visit
  (what had not been recorded yet is, of course, not there).

## Consent

The dashboard shows a consent notice before **every** recording, and a red
**● Recording** indicator stays on screen for the whole recording. Starting a
recording stores the notice version you acknowledged and your user with the
recording (`consent_version`, `recorded_by`, `consent_at` in its metadata, and
in the Knowledge fact).

Recording laws differ between countries, states and workplaces: some allow a
recording when one participant agrees, others need everyone's agreement, and
many require that people are told before the recording starts. **Making sure
a recording is lawful, and that everyone recorded knows about it, is the
responsibility of the person who records.** The hive does not decide this for
you. Good practice:

- say at the start that you are recording and why, and offer to stop;
- when you include a browser tab (a video meeting), the remote participants
  are recorded too: tell them, or use the meeting platform's own recording
  notice;
- do not record people who have not been told.

## Transcription

When you stop, the hive transcribes the recording automatically with its
[transcription provider](inbox-transcription.md) (`provider: auto` picks the
built-in whisper first, so the audio stays on the hive), with the same
progress bar and time-left estimate as any other Inbox transcription. The
transcriber reads the file from the hive's own disk; nothing is fetched over
HTTP. The transcript is written into the recording's Knowledge fact under the
line `_Transcript made by this hive with whisper/base.en on 2026-11-16._`
(provider, model and day); the
Inbox preview shows the transcript text only.

The **Saved to the Inbox** panel above the Inbox follows the item: it says
**Transcribed. See the item below.** once the transcript lands, or
**Transcription failed:** with the reason (or that transcription is off), and then
closes itself with the same countdown bar and duration as the dashboard's
toasts (hovering or focusing it pauses the countdown). **Close** dismisses it
at once.

- Recording does **not** replace the hive-wide transcription consent: nothing
  is transcribed until an owner enables `inbox.transcription` with
  `consent_acknowledged: true`. Once that is done, every dashboard recording
  counts as opted in, whatever `inbox.transcription.mode` says.
- While transcription is off you can still record; the consent notice says
  the recording will not be transcribed yet, and the item shows why. The
  transcription sweep (every 10 minutes and after connector syncs) picks up
  such recordings once transcription is enabled, as long as their audio has
  not expired.
- Transcription limits (`max_bytes`, `max_duration_minutes`, daily and
  monthly budgets) apply as for every other source.

## Deleting a recording

**🗑 Delete** on a recording's Inbox item deletes it for good: the audio
stored on this hive (and any chunk left over), the recording's metadata, its
machine transcript record and the Inbox item. The recording's Knowledge fact
with its transcript is kept unless you tick **Also remove the Knowledge
copy** in the dialog. Owners can delete any recording; read-write members
only the ones they recorded; read members cannot. The dashboard asks first,
in the page; when the item was shared, the question says that what was
already shared (RFCs, issues, DIBS ideas, spek seeds) stays where it is,
because delete never touches those copies. A recording that is being
transcribed can be deleted once the transcription finishes. Every delete is
audited as `inbox_recording_delete` and `inbox_delete`. Deleting works the
same for every other Inbox item; see
[Deleting an item](inbox-voice-sources.md#deleting-an-item).

## Retention

| What | Kept |
| --- | --- |
| Raw audio (`audio.webm` / `audio.mp4`) | `inbox.recording.retention_days` after the recording ends (default **5 days**), then deleted |
| Transcript (machine transcript on this hive) | kept |
| Knowledge fact (personal layer, never published to the hub) | kept |
| Recording metadata (title, who, when, duration, consent version) | kept |

While the audio is kept, the note under the player says **The audio is kept
on this hive until** the day the recording ended plus `retention_days`,
**then deleted; the transcript stays.** Dropped audio files say the same with
the upload retention.

After the audio is deleted the item shows **audio expired (kept 5 days)**
(with the configured number of days) and has no play control; the transcript
and the fact stay readable and shareable. The sweep that deletes audio runs at
boot and every 10 minutes, and logs and audits every deletion
(`inbox_recording_audio_expired`, actor `inbox-recording-sweep`).

## Configuration

```yaml
inbox:
  recording:
    enabled: true             # show ● Record (owner and read-write only)
    retention_days: 5         # raw audio is deleted this long after the recording ends
    max_seconds: 10800        # 3 hours per recording (wall clock, pauses included)
    max_bytes: 209715200      # 200 MiB per recording
    quota_bytes: 2147483648   # 2 GiB of recording audio on this hive at once
    idle_finalize_minutes: 120 # a recording whose browser went silent this long is finished automatically
```

`max_bytes` must not exceed `quota_bytes`. At the dashboard's 32 kbit/s a
3-hour recording is about 45 MB, so the defaults leave room for long meetings
and many recordings at once.

## How a recording is stored

- The browser records with `MediaRecorder` and uploads one chunk every 10
  seconds (`POST /api/inbox/recordings/{id}/chunks?seq=N`), each at most
  512 KiB, so it fits the default 1 MiB ingress body limit and the
  dashboard's 30 second request timeout.
- Chunks are accepted in order only. Each one is written and fsynced as its
  own part file before the hive acknowledges it; an out-of-order chunk gets
  409 with the expected `seq`, and a chunk re-sent with the same bytes is
  acknowledged again without being stored twice. That makes a resumed
  upload safe after a network drop or a reload.
- Stopping (finalize) concatenates the parts in order into one file under
  `<knowledge data>/connector-state/recordings/<id>/` (0700 directory, 0600
  files) and writes a pending Knowledge fact.
- The hive only checks the container signature of the first chunk (WebM or
  MP4) and never decodes audio itself; the transcription provider does (the
  sandboxed whisper subprocess by default).
- A recording whose browser stops sending chunks for
  `idle_finalize_minutes` is finished automatically with what arrived; one
  with no audio at all is removed.

Playback is served from this hive's disk on the dashboard origin
(`GET /api/inbox/recordings/{id}/audio`, with Range for seeking and
`Cache-Control: no-store`). See the [API reference](api-reference/knowledge.md).

### Safari and MP4

Safari records fragmented MP4: the first chunk carries the `ftyp` and `moov`
boxes, every later chunk a `moof` + `mdat` fragment. Concatenating the chunks
of one recorder in order gives a valid fragmented MP4, so the hive uses one
recorder for the whole recording (pause and resume included) and stores one
file. On finalize the hive walks the top-level MP4 boxes of the result and
logs a warning if they do not chain to the end of the file; the file is kept
either way.
