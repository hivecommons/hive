# Add files and links to the Inbox

Besides the recordings that [voice and meeting connectors](inbox-voice-sources.md)
bring in, the owner can add material to the Inbox directly: drop a file onto
the Inbox, choose it with **＋ Add**, or paste it with Ctrl/Cmd+V while the
Inbox has focus. Each file becomes an Inbox item like any other: it gets a
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
as plain text otherwise.

| Type | Extensions | What the item shows |
| --- | --- | --- |
| Markdown | `.md`, `.markdown` | the text as is; the first `# ` heading becomes the title |
| Plain text | `.txt`, `.text`, `.log` | the text as is |
| JSON | `.json` | a one-line summary (top-level keys or array length) and the pretty-printed document in a fenced `json` block |
| JSON Lines | `.jsonl`, `.ndjson` | the record count and the first 20 records in a fenced block; every line is validated |
| YAML | `.yaml`, `.yml` | a one-line summary and the document in a fenced `yaml` block (parsed with `gopkg.in/yaml.v3`) |
| CSV / TSV | `.csv`, `.tsv`, `.tab` | row and column counts and a markdown table of the first 20 rows (20 columns at most) |

Text is validated as UTF-8 (a UTF-8 BOM is dropped; UTF-16 is refused with
"save the file as UTF-8") and newlines are normalised to `\n`. The fenced
previews are bounded at 256 KiB and the item text at 1 MiB; the whole file is
always stored.

Not supported (the item is refused with a message saying what to do):

| File | Message |
| --- | --- |
| PDF, Word `.docx` | not supported yet |
| Legacy Word `.doc` (and `.xls`, `.ppt`) | save it as .docx or PDF and try again |
| RTF, OpenDocument `.odt` | save it as .docx, PDF or plain text |
| PowerPoint `.pptx` | export the slides as PDF |
| Excel `.xlsx` | export the sheet as CSV |
| Audio and video | not supported yet |
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
| `inbox.uploads.media_retention_days` | `HIVE_INBOX_UPLOADS_MEDIA_RETENTION_DAYS` | `5` | delete an audio or video original after this many days |

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
`document_retention_days`, `extensions`).
