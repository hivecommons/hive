# Inbox and voice sources

The **Inbox** (sidebar → Intelligence → 📥 Inbox) is where recordings and
dictations from voice sources land so you can turn them into work without
leaving the dashboard. It is source-agnostic: Plaud is the first source,
Wispr Flow (`type: wisprflow`) plugs in with the same model, and later sources
(file drop, webhook, Otter.ai, Fireflies, …) add nothing to the UI.

Tracker RFC: *Voice recorders as first-class input sources (Inbox)* —
see the issue linked from the changelog entry.

## Model

- **Source** — a knowledge connector whose facts are recordings (`plaud`,
  `wisprflow`). Connecting a source happens in **Knowledge → 🔌 Integrations**;
  that card shows only the connection state and an **Open Inbox** button.
- **Item** — one recording, keyed `(source, item_id)` (for Plaud, the
  `attr_plaud_file_id`). When the source is connected the Inbox lists
  recordings directly from it, so an item may appear before the sync has
  written its Knowledge fact; the Inbox only tracks what you did with it.
  "📚 Fact" opens the fact in Knowledge once that fact exists.
- **Destination** — where an item goes:
  - **→ RFC** (`spek_rfc`): the hive runs `spektacular voice --from stdin
    --title <title> --publish --repo <owner/repo>` on the transcript and
    files one RFC issue plus native child issues with the hive handshake
    marker. The hive's normal scanner then claims the children. If no
    `spektacular` binary is on the host, the hive hands the same job to a
    spoke agent as a kick prompt (state `delegated`): `inbox.spek.agent` if
    set, otherwise the first enabled, unpaused agent by name.
  - **→ dibs** (`dibs`): posts title, summary, transcript and the
    `plaud://recordings/<id>` provenance to `POST {inbox.dibs.url}/api/ideas`
    as a private draft idea (Bearer token from `inbox.dibs.token_env`).
  - **Knowledge only** (`dismiss`): leaves the fact where it is and drops the
    item from the unprocessed count.
- **State** per item per destination: `queued → running → filed | failed`
  (or `delegated` for the agent fallback), with the RFC/idea URL and child
  issue URLs. Stored in `connector-state/_inbox.json`.
- **Auto-routing rules** (⚙ Auto-routing in the Inbox, or `inbox.rules`):
  `source?`, `title_prefix` or `tag`, `destination`, `repo?`. Rules run once
  for each item that arrives after a connector sync; the first match wins;
  items already in the Inbox are never re-routed. The same editor, with the
  default RFC repository and dibs URL, is under Settings → Inbox.

## Reading and listening to an item

Each Inbox item shows what the source has for the recording:

- **Summary / memo** — Plaud's AI summary (`get_note`), collapsed after a
  few lines with a *more* toggle. Highlights (moments flagged with the
  device button, `mark_memo`) are listed as timestamps.
- **Transcript** — behind a *Transcript* disclosure, loaded on demand from
  `GET /api/inbox/{source}/{id}/content`. The connector reads the best
  transcript block the recording has: `transaction_polish` (AI-cleaned),
  then `transaction` (raw), then `outline`, following `next_cursor` pages.
- **Duration and recording time** — from the listing.
- **▶ Play recording** (owner or read-write) — asks the hive for a fresh
  presigned audio URL (`GET /api/inbox/{source}/{id}/audio`, Plaud:
  `get_file`, valid 24h) and plays it in an inline HTML5 player. The link is
  minted per click and never stored in facts or inbox state; the request is
  audited as `inbox_audio`. The Plaud card's *Include 24h audio link* option
  only decides whether a sync also writes a link into the fact.

A recording that was **never transcribed in the Plaud app** has no summary
and no transcript: the MCP's inventory (`get_file`) lists no `transaction`
block and `get_transcript` answers `Block "transaction" not available for
this recording. Available blocks: mark_memo.` The connector no longer stores
that reply as the fact body. Instead the fact (and the item) says *Plaud has
not transcribed this recording yet*, is marked `attr_plaud_content: pending`,
and is re-fetched on every sync (at most 25 pending recordings per sync, on
top of the listing) until Plaud has text; while Plaud is connected, the item's
content is read live so text shows up as soon as it exists. Run
transcription/summary in the Plaud app to fill it. Facts written before this
behaviour, whose body carries the "not available" line, are refreshed the
same way.

## Configuration

```yaml
inbox:
  sources: [plaud, wisprflow]     # default: every configured voice connector
  spek:
    repo: hivecommons/hive        # default RFC target
    binary: spektacular           # or an absolute path; empty = look on PATH
    agent: ""                     # fallback agent; default: first enabled agent
  dibs:
    url: https://dibs.example.org
    token_env: DIBS_TOKEN
  rules:
    - title_prefix: "RFC:"
      destination: spek_rfc
    - title_prefix: "Idea:"
      destination: dibs
```

`inbox.rules`, `inbox.spek.repo` and `inbox.dibs.url` are editable from the
dashboard (`POST /api/inbox/settings`); the token is environment-only.

## Hosted hives: signing in to Plaud

Plaud's OAuth client only redirects to `http://localhost:8199/auth/callback`,
so a hive running in a cluster never receives the browser callback. The
Plaud card therefore offers **Finish sign-in by pasting**:

1. Click **Connect Plaud**; open the authorization link and approve.
2. The browser lands on `localhost:8199/auth/callback?code=…` (a connection
   error page is expected). Copy that URL and paste it into the card within
   10 minutes; the hive forwards it to the login session running in the pod.
3. Alternative: on a laptop run `npx -y @plaud-ai/mcp@0.3.14 install`, sign
   in, and paste the contents of `~/.plaud/tokens-mcp.json`.

Both go through `POST /api/knowledge/connectors/{name}/auth/complete`.
A browser-only redirect to the hive's public URL is not possible until Plaud
allows additional redirect URIs for its client.

## Requirements on the hive image

- Node.js ≥ 20 for the Plaud MCP server (the image ships Node 26).
- `spektacular` for → RFC: not in the image yet (tracked as a child of the
  Inbox RFC); until then the agent fallback runs by default, or install it on
  the host (`go install github.com/hivecommons/spektacular/cmd/spektacular@latest`).
- A GitHub token with issue-write on the target repo (the hive's app token
  is injected as `GITHUB_TOKEN`).

## API

See [api-reference/knowledge.md](api-reference/knowledge.md) for
`GET /api/inbox`, `POST /api/inbox/route`, `POST /api/inbox/dismiss`,
`GET|POST /api/inbox/settings`, `GET /api/inbox/{source}/{id}/content`,
`GET /api/inbox/{source}/{id}/audio` and
`POST /api/knowledge/connectors/{name}/auth/complete`.

`GET /api/inbox` always answers `200`: on a hive with no voice connector
configured it returns an empty `items` list and every source as
`configured: false`, and the Inbox panel shows its empty state with a
"Connect sources" button. A connector added under Knowledge → Integrations
starts syncing after the next hive restart.
