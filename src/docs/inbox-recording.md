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

## Retention

| What | Kept |
| --- | --- |
| Raw audio (`audio.webm` / `audio.mp4`) | `inbox.recording.retention_days` after the recording ends (default **5 days**), then deleted |
| Transcript (machine transcript on this hive) | kept |
| Knowledge fact (personal layer, never published to the hub) | kept |
| Recording metadata (title, who, when, duration, consent version) | kept |

After the audio is deleted the item shows **audio expired (kept 5 days)**
(with the configured number of days) and has no play control; the transcript
and the fact stay readable and shareable. The sweep that deletes audio runs at
boot and every 10 minutes, and logs and audits every deletion
(`inbox_recording_audio_expired`, actor `inbox-recording-sweep`).

## Configuration

```yaml
inbox:
  recording:
    enabled: false            # show ● Record (owner and read-write only)
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
