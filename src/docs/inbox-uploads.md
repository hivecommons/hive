# Add files and links to the Inbox

Besides the recordings that [voice and meeting connectors](inbox-voice-sources.md)
bring in, the owner can add material to the Inbox directly: drop a file onto
the Inbox, choose it with **＋ Add**, paste it with Ctrl/Cmd+V while the
Inbox has focus, or add a link (type it into ＋ Add, or paste a URL onto the
Inbox). Each file becomes an Inbox item like any other: it gets a
Knowledge fact, the 🔗 Share ▾ menu (RFC, Knowledge, DIBS) and the route
history.

Adding files is for the **owner and read-write** sessions. Read sessions do
not see the drop zone, the ＋ Add button or the paste handling, and the API
refuses them (403).

## Supported types

The hive decides the type from the **content**, never from the file name or
the browser's MIME type alone: it checks magic bytes first (an executable,
archive, PDF, Office or media file renamed to `.txt` is refused), then that
the file is UTF-8 text, then that it parses as the format its extension asks
for. A file without an extension is read as JSON when it parses as JSON and
as plain text otherwise. A zip archive is refused unless it is named `.docx`
and opens as a Word document.

| Type | Extensions | What the item shows |
| --- | --- | --- |
| Markdown | `.md`, `.markdown` | the text as is; the first `# ` heading becomes the title |
| Plain text | `.txt`, `.text`, `.log` | the text as is |
| JSON | `.json` | a one-line summary (top-level keys or array length) and the pretty-printed document in a fenced `json` block |
| JSON Lines | `.jsonl`, `.ndjson` | the record count and the first 20 records in a fenced block; every line is validated |
| YAML | `.yaml`, `.yml` | a one-line summary and the document in a fenced `yaml` block (parsed with `gopkg.in/yaml.v3`) |
| CSV / TSV | `.csv`, `.tsv`, `.tab` | row and column counts and a markdown table of the first 20 rows (20 columns at most) |
| Word | `.docx` | headings (Title and Heading 1 to 6 styles) as `#`, paragraphs, bulleted and numbered lists (by level) as `-` items, and tables as markdown tables; deleted text and field codes are left out; the title comes from the document properties, else the first heading. Read in Go with `archive/zip` and `encoding/xml` (standard library): at most 1000 zip entries, 200 MiB declared uncompressed in all, 50 MiB per part (checked while inflating, not only from the header) and a 100:1 compression ratio per entry; a part with a DOCTYPE or an entity the decoder does not know is refused (XXE), and nothing is fetched |
| PDF | `.pdf` (or a file without an extension that starts with `%PDF-`) | one `## Page N` section per page with text, a page-count summary, and the document title from its Info dictionary. Parsed **out of process** in the `hive-pdftext` sandbox with `github.com/ledongthuc/pdf` (BSD-3-Clause, pure Go): at most 200 pages and 1 MiB of text; a PDF without a text layer is refused with "no text layer (scanned PDF); OCR not supported yet" |
| Audio | `.mp3`, `.m4a`, `.aac`, `.wav`, `.ogg`, `.opus`, `.webm` | an audio item with ▶ Play through the same-origin stream (served from this hive's disk) and 🎧 Transcribe; when transcription is enabled and consented, the hive queues the transcription as soon as the file is added. The content must carry the container's signature (ID3 or MPEG frame sync, ADTS, `ftyp`, RIFF/WAVE, Ogg, EBML) |
| Video | `.mp4`, `.mov` | the same as audio: only the audio track is used (for transcription and playback); there is no video playback |
| HTML | `.html`, `.htm`, and web pages behind a link | readable markdown: the `<title>` names the item; the `<article>` or `<main>` (else `<body>`) is kept with headings, paragraphs, lists, quotes, code, tables, link targets and image alt text; scripts, styles, forms, navigation, footers, asides and hidden elements are dropped. Parsed with `golang.org/x/net/html` (BSD-3-Clause); nothing is executed and nothing the page references is loaded |

Text is validated as UTF-8 (a UTF-8 BOM is dropped; UTF-16 is refused with
"save the file as UTF-8") and newlines are normalised to `\n`. The fenced
previews are bounded at 256 KiB and the item text at 1 MiB; the whole file is
always stored.

Not supported (the item is refused with a message saying what to do):

| File | Message |
| --- | --- |
| Legacy Word `.doc` (and `.xls`, `.ppt`) | save it as .docx or PDF and try again |
| RTF, OpenDocument `.odt` | save it as .docx, PDF or plain text |
| PowerPoint `.pptx` | export the slides as PDF |
| Excel `.xlsx` | export the sheet as CSV |
| Executables, archives, images, anything else | unsupported file type |

## What happens to a file

1. The browser checks the drop against the limits below (too many files, too
   large, empty, an extension the hive does not read) and sends each accepted
   file on its own request, so its row shows upload progress and its own
   error.
2. The hive sniffs the content, extracts markdown in-process with a 30-second
   timeout, and stores the upload under
   `<data>/connector-state/uploads/<id>/` (directory 0700, files 0600):
   `original` (the file as dropped), `extracted.md` and `meta.json` (who added
   it, when, its size and SHA-256).
3. It writes a Knowledge fact to `<data>/connectors/personal/upload-<id>.md`
   and reindexes that vault, so knowledge search finds the text at once. The
   extracted text sits in the fact inside a nonce-tagged
   `<untrusted-inbox-data>` envelope.
4. The item appears as **upload · notes.md**, with who added it and when.
   **▸ Text** shows the extracted markdown.

Every file, accepted or refused, is audited as `inbox_upload` with the user,
the item id, the detected kind, the size and the outcome or refusal code; the
file's content and name are not written to the audit log.

## Audio and video

An audio or video file is streamed straight to `connector-state/uploads/<id>/original`
(never held in memory), capped by `inbox.recording.max_bytes` and the uploads
quota, and its item starts **pending**: it has no text until it is
transcribed. ▶ Play streams it from this hive's disk through the same
same-origin endpoint recordings use (`GET /api/inbox/upload/<id>/audio/stream`,
with Range for seeking), and transcription reads the same file locally, so
with whisper the audio never leaves the hive. When `inbox.transcription` is
enabled and consented, the transcription is queued as soon as the file is
added (audited as `inbox_transcribe`, actor = who added it); otherwise the
item shows the usual reason 🎧 Transcribe is unavailable. The machine
transcript is written into the item's Knowledge fact like any recording's.
The original is deleted after `inbox.uploads.media_retention_days`, which
defaults to `inbox.recording.retention_days` (5 days); the transcript, the
item and the fact stay. Video files keep only their audio track: there is
no video playback. The browser request may take up to 20 minutes to arrive
(the dashboard's 30-second read timeout is lifted for uploads).

## PDF sandbox

PDF parsers have a long history of bugs, so the hive never parses an
uploaded PDF in its own process. It runs `/usr/local/bin/hive-pdftext` the
same way it runs the whisper transcription sandbox:

- as the separate `hive-transcribe` uid (no home, shell or supplementary
  groups), reached through `su-exec`, with an environment of only `PATH`
  and `LC_ALL`;
- under `prlimit` (1 GiB address space, CPU seconds = timeout + 10, file
  size = the PDF's size, 32 open files, no core dumps) and `nice -n 10`;
- in an empty network namespace (`unshare --user --net`) where the kernel
  allows unprivileged user namespaces;
- with the PDF on stdin, copied into a private 0700 work dir (file 0600)
  that is removed on exit, and only JSON on stdout (capped);
- with a 60-second wall clock, after which the whole process group is
  killed.

`HIVE_PDF_SANDBOX`, `HIVE_PDF_RUN_AS` and `HIVE_PDF_MEMORY_MB` override the
path, the uid and the memory cap. When the sandbox is not in the image, PDF
uploads are refused with "PDF reading is not available on this hive".

## Links

A link is fetched by the hive itself, once, when it is added, and its body
goes through the same type pipeline as a dropped file: an HTML page becomes
text, a link to a `.json`, `.csv` or `.md` file becomes that kind (the file
name in the URL or `Content-Disposition`, else the response `Content-Type`,
picks the format, and the content must still pass the sniff and the parse).
YouTube and other streaming pages are read as HTML pages, never as video;
links to audio or video files are refused for now.

The item is titled by the page `<title>`, else the file name, else the URL,
and shows **link · host**. The URL is kept as the item's provenance (and the
fact's `source_url`) only.

Fetch rules (SSRF protection):

- `http` and `https` only; a link with a user name or password is refused.
- The hive resolves the host itself and connects to the address it checked;
  if **any** answer is blocked the link is refused, so a second
  (rebinding) answer is never used. Blocked: loopback, RFC 1918 private,
  link-local (including the cloud metadata address `169.254.169.254`),
  CGNAT `100.64.0.0/10`, multicast, reserved and documentation ranges,
  `0.0.0.0/8`, IPv6 unspecified, loopback, unique-local `fc00::/7`
  (including `fd00:ec2::254`), link-local and site-local, Teredo, and IPv4
  addresses embedded in IPv4-mapped, NAT64 and 6to4 addresses; host names
  `localhost`, `*.localhost`, `*.local`, `*.internal` and `metadata`.
- Every redirect hop is checked the same way, at most 3 redirects, and a
  redirect never carries `Authorization` or `Cookie`.
- The body is streamed through a limit (`inbox.uploads.max_file_bytes`);
  connect 10 s, TLS 10 s, response headers 15 s, whole fetch 60 s.
- Requests send `User-Agent: hive-inbox/<version>`.
- `HTTP(S)_PROXY` from the environment is not used (a proxy would resolve
  the name again). When `network.upstream_proxy` matches the host, the hive
  asks the proxy to `CONNECT` to the checked IP and port.

## Quick note

**＋ Add** also has a **Quick note** box: type or dictate text and choose
**Add note**. It is the way to put a thought into the Inbox without a file,
and it works with any input method that enters text (a keyboard, dictation,
gaze typing, switch access; see [Input methods and the Inbox](inbox-input-methods.md)).

- The note is sent as a markdown file named `note-<date>-<time>.md`
  (`note-2026-11-17-143005.md`, the browser's local time) through the same
  route as a dropped file, `POST /api/inbox/uploads`. There is no separate
  route and no separate storage: everything on this page applies to it. The
  hive stores it under `connector-state/uploads/<id>/`, writes the Knowledge
  fact, records who added it, audits it as `inbox_upload`, applies the
  auto-routing rules and deletes it with the item.
- A first line that is a `# ` heading becomes the item's title; otherwise
  the item is titled after the file name.
- The size cap is the document cap, `inbox.uploads.max_file_bytes`. The
  browser checks it before sending and the hive enforces it; a note the hive
  refuses stays in the box with the reason, so the text is not lost.
- Text of **several lines** pasted onto the Inbox (outside a text field)
  lands in the Quick note box for review instead of being dropped; nothing
  is created until you choose **Add note**. A single pasted link is still
  added as a link, and other single-line text is still ignored.
- The text in the box survives the Inbox's background refreshes, and the
  caret stays where it was.
- Owner and read-write sessions only, like every other way of adding.

## Limits

All limits have defaults; `inbox.uploads` in `hive.yaml` overrides them and
the environment overrides both (a non-positive or malformed value falls
through).

| Setting | Environment | Default | |
| --- | --- | --- | --- |
| `inbox.uploads.max_file_bytes` | `HIVE_INBOX_UPLOADS_MAX_FILE_BYTES` | `26214400` (25 MiB) | largest text document |
| `inbox.uploads.max_files_per_drop` | `HIVE_INBOX_UPLOADS_MAX_FILES_PER_DROP` | `20` | files in one drop or one request |
| `inbox.uploads.quota_bytes` | `HIVE_INBOX_UPLOADS_QUOTA_BYTES` | `2147483648` (2 GiB) | everything stored under `connector-state/uploads` |
| `inbox.uploads.document_retention_days` | `HIVE_INBOX_UPLOADS_DOCUMENT_RETENTION_DAYS` | `0` (keep) | delete a document's original after this many days |
| `inbox.uploads.media_retention_days` | `HIVE_INBOX_UPLOADS_MEDIA_RETENTION_DAYS` | `inbox.recording.retention_days` (5) | delete an audio or video original after this many days |
| `inbox.recording.max_bytes` | | `209715200` (200 MiB) | largest audio or video file (the recording cap) |

```yaml
inbox:
  uploads:
    max_file_bytes: 10485760
    quota_bytes: 1073741824
    document_retention_days: 365
```

## Retention

A retention sweep runs at boot and then hourly. It deletes only the
**original** file: the extracted text, the meta, the item, its Knowledge fact
and its share history stay. Text documents are kept unless
`document_retention_days` is set; audio and video originals go after
`media_retention_days` (5 days), the same as recordings made in the hive.

## Privacy

- Files stay on this hive's data volume. Nothing is sent anywhere when a file
  is added.
- The extracted text reaches an LLM only when someone shares the item (for
  example → RFC hands it to a hive agent), and then only inside the
  `<untrusted-inbox-data>` envelope that tells the model the text is data, not
  instructions. The same envelope fences it in the Knowledge fact.
- The browser request carries the session cookie (SameSite=Lax), and the hive
  also refuses a request the browser marks `Sec-Fetch-Site: cross-site`.

## API

`POST /api/inbox/uploads/url` (owner or read-write) takes `{"url": "..."}`
and answers like a single-file upload (201, or 400 `link_refused`, 413, 415,
422, 502 `fetch_failed`, 507); refusals and additions are audited as
`inbox_upload` with `origin=link` and the host, never the full URL.

`POST /api/inbox/uploads` (owner or read-write) takes `multipart/form-data`
with one or more `file` fields. One file answers `201` with
`{results: [{filename, ok: true, source: "upload", item_id, title, kind}]}`,
or with the status of its refusal: `400` (`empty`, `too_many_files`), `413`
(`too_large`), `415` (`unsupported_type`), `422` (`parse_failed`), `507`
(`quota_exceeded`); each refused result carries `code` and `error`. Several
files answer `200` with one result per file unless all of them failed. See
[api-reference/knowledge.md](api-reference/knowledge.md).

`GET /api/inbox` reports the limits and current usage under
`settings.uploads` (`enabled`, `max_file_bytes`, `max_files_per_drop`,
`quota_bytes`, `used_bytes`, `media_retention_days`,
`document_retention_days`, `extensions`, `links`).
