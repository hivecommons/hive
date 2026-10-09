# Knowledge promotion configuration

The `knowledge.curator` block now controls only promotion of existing verified facts between llm-wiki layers. The earlier merged-PR extraction pipeline was never wired into any binary and has been removed rather than leaving a config-promised feature that did not run.

```yaml
knowledge:
  enabled: true
  engine: llm-wiki
  curator:
    auto_promote_threshold: 0.9
```

| Field | Current behavior |
| --- | --- |
| `auto_promote_threshold` | Defaults to `0.9` when knowledge is enabled. `Promoter.AutoPromoteCandidates` selects facts whose llm-wiki page has `status == "verified"` and `confidence >= threshold`. |

Merged-PR extraction and its old scheduling/source-list knobs are intentionally not documented as supported configuration because no scheduler, CLI command, or HTTP endpoint triggered extraction.

## Remote git sources (`knowledge.git_sources`)

`knowledge.git_sources` indexes markdown from a remote git repository (or a
subdirectory of one) as a knowledge source, so agents get facts from an
external repo — a runbook repo, an upstream docs repo, a shared pattern
library — primed into their kicks the same way wiki-layer facts are. This is
implemented and live, unlike curator scheduling above: `pkg/knowledge/gitsource.go`
does the cloning, indexing, and periodic sync; `cmd/hive/main.go:2303-2347`
wires configured entries at startup.

```yaml
knowledge:
  enabled: true
  git_sources:
    - name: dakota-skills            # required — display name and dedup/lookup key
      url: https://github.com/projectbluefin/dakota   # required — https:// only
      branch: main                   # optional — default "main"
      subpath: docs/skills           # optional — index only this subdirectory
      layer: project                 # optional — default "project"
```

Config fields (`GitSourceConfigYAML`, `pkg/config/knowledge_config.go:58-65`, mirrored
by the runtime type `GitSourceConfig`, `pkg/knowledge/gitsource.go:33-39`):

| YAML key | Required | Default | Notes |
|---|---|---|---|
| `name` | Yes | — | Display name; also the key used to look up/remove the source via the API. |
| `url` | Yes | — | Git remote URL. **`https://` only** — see Auth below. |
| `branch` | No | `main` (`gitsource.go:66-68`) | Branch to shallow-clone (`--depth 1 --branch <branch>`). |
| `subpath` | No | — (whole repo) | When set, only this subdirectory is checked out (git sparse-checkout, `gitsource.go:199-219,232-248`) and indexed. |
| `layer` | No | `project` when set via the API (`api_knowledge.go:1032-1034`); **required, no code default, when set via `hive.yaml`** | One of `personal`, `project`, `org`, `community` — see Layer semantics below. |

### What "indexed" means

On connect, the source is shallow-cloned (depth 1) into
`<knowledge-data-dir>/git-sources/<slug>` and the clone (or its `subpath`) is
handed to a `FileStore`, which indexes markdown files under it
(`gitsource.go:78-114`). If `subpath` doesn't exist after cloning, connection
fails with `subpath "<x>" not found after clone` — check the path relative
to the *repository root*, not the branch's top-level display in GitHub's UI.

### Layer semantics

`layer` places the source's facts in the same 4-layer precedence used
everywhere else in the knowledge system (`pkg/knowledge/types.go:8-30`,
lower number = higher precedence, i.e. overrides on conflict):

| Layer | Precedence | Typical use for a git source |
|---|---|---|
| `personal` | 1 (highest) | A private runbook repo only this operator's hive should see. |
| `project` | 2 | A repo scoped to the project this hive manages — the default. |
| `org` | 3 | A shared org-wide reference repo. |
| `community` | 4 (lowest) | A public upstream docs repo, e.g. a framework's own documentation. |

### Auth for private repos

**There is no auth configuration for git sources.** `ensureCloned` invokes a
plain `git clone` with `GIT_TERMINAL_PROMPT=0` (so a credential prompt fails
instead of hanging) and no token, SSH key, or credential-helper wiring
(`gitsource.go:183-227,503-512`). In practice this means:

- A **public HTTPS repo** works with just `url:`.
- A **private repo** will fail to clone — there is no field to supply a
  token or deploy key, and the code does not fall back to any host git
  credential store. Do not attempt to embed a token in the `url` (e.g.
  `https://TOKEN@github.com/...`); nothing in this codebase does that
  pattern for git sources, and hardcoding a token in `hive.yaml` is
  explicitly against the fail-closed secret-handling used elsewhere in this
  repo.
- Only `https://` (and, if `HIVE_ALLOW_PRIVATE_GIT_SOURCE=true`, `http://`)
  URLs validate at all; `git@`-style SCP syntax is explicitly rejected
  (`gitsource.go:267-268,275-281`).

If your knowledge source is private, treat this as unsupported today rather
than assuming a missing config knob — see Open questions.

### SSRF / URL hardening (operator-relevant)

`ValidateGitSourceURLContext` rejects URLs whose host resolves to a
loopback, private, or link-local address (including the cloud metadata IP)
before cloning, and fails closed on a DNS lookup error
(`gitsource.go:259-351`). git itself is invoked with
`-c http.followRedirects=false` so a remote can't 302 the clone to an
internal address after validation passes (`gitsource.go:479-501`). An
in-cluster git server (e.g. an internal GitLab) needs
`HIVE_ALLOW_PRIVATE_GIT_SOURCE=true` set as an explicit opt-in
(`gitsource.go:293-297,368-370`).

### Refresh / staleness

Once connected, a background loop pulls and reindexes every 5 minutes
(`gitSourceSyncInterval`, `gitsource.go:25`, `StartSyncLoop`,
`gitsource.go:161-180`). A failed sync is logged at `warn` and the loop
keeps retrying on the next tick — it does not surface as a dashboard alert,
so staleness is only visible in the hive's logs
(`git source sync failed`). There is no operator-facing "last synced"
timestamp in `GitSourceInfo` (`gitsource.go:531-540`) — only whether the
source is `Ready` and its current page count.

### Static config vs. the runtime API

`GET/POST/DELETE /api/knowledge/git-sources` (owner-role only,
`pkg/dashboard/api_knowledge.go:980-986,988-1070,1072-1114`) manage sources at runtime
and are the *same* underlying list as `knowledge.git_sources` in
`hive.yaml` — not a separate system:

- `POST` connects a source immediately and, if it isn't already present
  (matched by `url`+`subpath`), appends it to `Config.Knowledge.GitSources`
  and persists the config (`api_knowledge.go:1048-1066`). A `POST` for a source that
  is only in `hive.yaml` but not yet connected in the running process (e.g.
  right after editing the file without restarting) will add a duplicate
  config entry once reconnected, since the dedup check is by URL+subpath
  against what's already in `Config`, not against what main.go loaded at
  boot.
- `DELETE` disconnects the live source and removes matching entries from
  `Config.Knowledge.GitSources`, then persists (`api_knowledge.go:1100-1110`).
- Editing `git_sources:` directly in `hive.yaml` takes effect on the next
  process restart (main.go's startup loop at `cmd/hive/main.go:2420-2466`);
  it does not hot-reload while the process is running. Use the API for a
  live change without a restart.

### Diagnosing a failed source

- **Never appears / `knowledge not enabled`**: if `knowledge.enabled` is
  `false` but `git_sources` is non-empty, startup auto-enables a minimal
  knowledge API (`engine: file`) just to host the git sources
  (`main.go:2421-2427`) — so a git source can work even with `knowledge.enabled: false`.
  If you still get "knowledge not enabled" from the API, no source has
  triggered that auto-enable yet (empty `git_sources` list).
- **Connect fails immediately**: check the hive log for `failed to connect
  git source` with the URL and error (`main.go:2437`) — most often an
  SSRF-validation rejection, a bad branch name, or (for private repos) an
  authentication failure from git itself.
- **Connects but `subpath` errors**: `subpath "<x>" not found after clone` —
  the path is wrong or doesn't exist on the configured `branch`.
- **Facts never show up in kicks**: confirm the source reached `Ready: true`
  (`GET /api/knowledge/git-sources`) — the primer only registers a source's
  `FileStore` for priming after it reports ready
  (`main.go:2450-2461`).

## Local vaults (`knowledge.vaults`)

`knowledge.vaults` lists local file-based (Obsidian-style) vaults that main.go
auto-connects at boot (`cmd/hive/main.go:2381-2417`):

```yaml
knowledge:
  vaults:
    - name: team-notes
      path: /data/vaults/team-notes
      auto_index: true
      git_sync: true
```

For each entry, boot git-inits the directory if needed
(`InitVaultRepo`, `pkg/knowledge/gitsync.go:146`), seeds it from
`/opt/hive/seed-data/wiki` (`SeedVaultContent`, `gitsync.go:162`), connects it
to the knowledge API, and registers its store with the kick primer at the
**personal** layer.

- `auto_index` is parsed-but-unactioned: connecting a vault always indexes it
  (`ConnectVault` → `NewFileStore`, `pkg/knowledge/api.go:588-606`); the flag
  is only echoed in the `vault auto-connected` log line. Setting it `false`
  does not skip indexing.
- `git_sync: true` adds the vault to a background syncer that runs
  `git pull` and reindexes every 60 seconds
  (`gitSyncInterval`, `gitsync.go:14`) — the Obsidian Git integration path.

## Document sources (`knowledge.documents`)

`knowledge.documents` imports standalone documents (PDF, HTML, markdown,
plain text) as knowledge facts at boot (`cmd/hive/main.go:2467-2494`):

```yaml
knowledge:
  documents:
    - name: style-guide
      url: https://example.com/style-guide.pdf   # or file_path: /data/docs/guide.pdf
      layer: project
```

Each entry names either a `url` to fetch or a local `file_path`. Imports run
once per boot via `KnowledgeAPI.ImportDocument`; the parsed facts land in the
vault with a `doc-` slug prefix (`pkg/knowledge/docsource.go`). Fetches are
capped at 50 MB with a 30-second timeout (`docsource.go:19-24`) and go through
the same SSRF validation as git sources. Like `git_sources`, a non-empty
`documents` list auto-enables a minimal file-engine knowledge API even when
`knowledge.enabled: false` (`main.go:2468-2475`). Documents can also be
imported at runtime via `POST /api/knowledge/documents` — see
[api-reference.md](api-reference.md).

## Connectors (`knowledge.connectors`)

`knowledge.connectors` is the shared seam every external knowledge system
plugs into (`pkg/knowledge/connector`). A connector lists and fetches
upstream pages; the connector syncer runs each enabled connector on its
`interval`, converts the pages to markdown facts and writes them into the
vault for the connector's `layer`. Incremental sync, auth, SSRF hardening,
size/time caps and per-connector status are shared, so individual connectors
only implement listing and fetching.

```yaml
knowledge:
  connectors:
    - name: eng-handbook          # lowercase letters, digits, dashes; unique
      type: git                   # connector type (see below)
      layer: org                  # personal | project | org | community
      interval: 30m               # Go duration, minimum 1m; default 15m
      enabled: true               # default true
      scope:                      # type-specific keys
        url: https://github.com/acme/handbook.git
        branch: main
        subpath: docs
    - name: style-guide
      type: document
      layer: project
      scope:
        url: https://example.com/style-guide.pdf
    - name: next-docs
      type: document
      layer: project
      scope:
        context7_id: /vercel/next.js
      auth:
        env: CONTEXT7_API_KEY     # or file: /secrets/context7-key
```

| Field | Required | Meaning |
|-------|----------|---------|
| `name` | yes | Unique id; part of every fact slug. |
| `type` | yes | Connector type registered in `pkg/knowledge/connector`. |
| `layer` | yes | Knowledge layer the facts are written to. |
| `interval` | no | Sync period (Go duration, at least `1m`); default `15m`. |
| `enabled` | no | `false` keeps the entry configured but unscheduled. |
| `scope` | per type | Type-specific settings (`map[string]string`). |
| `auth.env` / `auth.file` | per type | Where the credential lives: an environment variable name or an absolute file path. At most one. |

Credentials are never accepted inline: any other key under `auth` (for
example `auth.token`) and any `scope` key that looks like a secret (`token`,
`password`, `secret`, `api_key`, `private_key`, `credential`) fail config
validation with an error naming the field.

### Built-in connector types

| Type | Scope keys | Notes |
|------|------------|-------|
| `git` | `url` (required), `branch` (default `main`), `subpath` | Runs the existing `git_sources` clone/pull code path (same SSRF validation, redirect suppression and sparse checkout) in a private per-connector directory, then emits every indexed markdown page. Cursor: checked-out commit SHA. |
| `document` | exactly one of `url`, `file_path`, `context7_id` | Runs the existing `documents` fetch/parse/chunk pipeline; one fact per extracted chunk. `url` is SSRF-checked before the fetch. `auth` optionally supplies the Context7 API key. Cursor: content hash. |
| `confluence` | `base_url` (required), `spaces` and/or `root_page_ids`, `email` (Cloud), `deployment`, `include_labels`, `exclude_labels`, `include_attachments`, `max_pages`, `full_sync_every` | Confluence Cloud and Data Center via the REST content search API (CQL). Storage-format bodies are converted to markdown with Confluence macros (code, panels, expand, links, images) rendered; archived/trashed pages are deprecated. Cursor: newest `version.when`. See [knowledge-connectors.md](knowledge-connectors.md#confluence-type-confluence). |
| `notion` | `root_page_ids`, `database_ids`, `include_archived`, `max_pages`, `full_sync_every` | Notion API v1 with an internal integration token; pages must be shared with the integration. Blocks are converted to markdown; database rows carry their properties as attributes; archived/trashed pages are deprecated. Cursor: newest `last_edited_time`. See [knowledge-connectors.md](knowledge-connectors.md#notion-type-notion). |
| `github-wiki` | `repos` (required, comma-separated `owner/repo`), `branch` (default `master`) | Clones `https://github.com/<owner>/<repo>.wiki.git` through the same git clone/SSRF path as `git`. Emits `Home` first (marked `root: "true"`), then pages in `_Sidebar.md` link order (standard Markdown links to a page name, or `[[text\|page]]` wiki links; headings and nested items become the page path), then the remaining pages alphabetically. `_Sidebar` and `_Footer` are not emitted. `updated_at` comes from the last commit touching each page. A repository without a wiki (404) or one that needs credentials reports a clear "not found" status error. Only public wikis are supported for now. Cursor: per-repo commit SHAs. |
| `google-drive` | `folder_ids` and/or `shared_drive_ids` (comma-separated Drive IDs, at least one), `include_mime` (comma-separated `docs`, `sheets`, `md`, `txt`; default `docs,md,txt`) | Lists each folder recursively (`'<folder>' in parents`) and each shared drive through Drive v3 `files.list` with pagination. Google Docs are exported as `text/markdown` (falling back to `text/html`), Sheets as CSV rendered to a markdown table capped at 200 rows, and `.md`/plain-text files are downloaded as-is. PDFs are not supported yet. Trashed files are emitted as archived on incremental syncs. `auth` must supply an OAuth2 bearer token with the `drive.readonly` scope (for a service account, mint the token externally and point `auth.env`/`auth.file` at it) and the folders or drives must be shared with that identity. Cursor: newest `modifiedTime` seen; later syncs emit only files modified after it. |
| `sharepoint` | `drive_ids` and/or `site_ids` (comma-separated, at least one), `folder_path`, `include_files` (default `.md,.txt,.html,.htm`) | Reads document libraries and OneDrive drives through Microsoft Graph. A site contributes its default library. The first sync enumerates the drive with the `delta` endpoint and later syncs fetch only changes; files are downloaded via `/content` (HTML is converted to markdown) and items deleted upstream are marked deprecated. `auth` (required) supplies a Graph bearer token, for example from a client-credentials app registration with the `Sites.Read.All` / `Files.Read.All` application permission, or `Sites.Selected` granted per site for least privilege. docx/pdf files and modern site pages are not read yet. Cursor: JSON map of per-drive delta links. |

```yaml
knowledge:
  connectors:
    - name: project-wikis
      type: github-wiki
      layer: project
      scope:
        repos: acme/app, acme/docs
    - name: team-drive
      type: google-drive
      layer: org
      scope:
        folder_ids: 1AbCdEfGhIjKlMnOp
        include_mime: docs, sheets
      auth:
        env: GOOGLE_DRIVE_TOKEN
    - name: team-sharepoint
      type: sharepoint
      layer: org
      scope:
        site_ids: contoso.sharepoint.com,<site-collection-id>,<site-id>
        folder_path: Handbook
        include_files: .md,.txt
      auth:
        env: GRAPH_TOKEN
```

### Facts written by connectors

Each page becomes one fact with the deterministic slug
`<type>-<name>-<source_id>` (lower-cased, reduced to `[a-z0-9-]`, with a short
hash appended when that reduction loses information). Front-matter:

```yaml
---
title: Deploy runbook
type: reference
layer: org
status: active            # or deprecated
tags: [connector, git, eng-handbook]
source: git               # connector type
connector: eng-handbook   # connector name
source_id: docs-deploy
source_url: https://github.com/acme/handbook.git
synthesized: 2026-10-08T12:00:00Z   # upstream last-modified when known
synced_at: 2026-10-08T12:00:00Z
---
```

A fact is marked `status: deprecated` (a tombstone; the file stays so links
resolve) when the upstream page is archived, or when a page disappears from a
full listing. Incremental syncs never tombstone pages they did not list.
Unchanged pages are not rewritten. Page bodies are capped at 1 MiB, each sync
run at 10 minutes, and connector HTTP fetches at 30 seconds and 20 MiB with
`Retry-After`-aware backoff on 429/5xx.

### Status

The syncer records, per connector: last successful sync, last attempt, pages
emitted by the last sync, active and deprecated fact counts, last error, the
incremental cursor and whether the last sync was truncated at its page cap.
`GET /api/knowledge/connectors` returns it and owners can trigger a sync with
`POST /api/knowledge/connectors/{name}/sync` — see
[knowledge-connectors.md](knowledge-connectors.md#api).

### Relationship to `git_sources` and `documents`

`knowledge.git_sources` and `knowledge.documents` keep working exactly as
before: same YAML keys, same boot-time behaviour, same
`/api/knowledge/sources` output. The `git` and `document` connector types
reuse their code paths for operators who want the connector lifecycle
(interval, layer-targeted facts, tombstones, status). `hive` starts the
connector syncer at boot: facts land in `/data/knowledge/connectors/<layer>`
(connected as vault `connectors-<layer>`), and cursors/status persist in
`/data/knowledge/connector-state/status.json`. Operators manage connectors
from the **Settings → Knowledge → Connectors** pane (see
[knowledge-connectors.md](knowledge-connectors.md#dashboard-pane)).

## Bead synthesizer (`knowledge.bead_synthesizer`)

The bead synthesizer periodically scans every agent's **closed** beads,
classifies them into wiki fact types, and writes the results into a dedicated
vault so future kicks are primed with past findings
(`pkg/knowledge/bead_synthesizer.go`, wired at `cmd/hive/main.go:2508-2581`).

**It is on by default.** `enabled` is a tri-state pointer that defaults to
true when absent (`IsEnabled`, `pkg/config/knowledge_config.go:51-56`), and
main.go auto-enables a file-based knowledge API for it even when
`knowledge.enabled: false` (`main.go:2498-2505`). A hive that never mentions
`knowledge:` in its config still runs the hourly synthesis loop and the
retention manager below. `bead_synthesizer.enabled: false` is the only
opt-out.

**Synthesized facts are only primed when `knowledge.enabled: true`.**
`knowledge.enabled` gates exactly one thing: the kick primer. With it
`false` the synthesizer still writes facts into its vault and they stay
browsable in the dashboard and API, but no primer exists, so no kick
receives them. The dashboard's knowledge panel reports this state as
**not primed** (`enabled` on `GET /api/knowledge/stats` and
`GET /api/knowledge/health` is primer registration, with the registered
sources under `primer`), not as enabled.

Enabling knowledge from the dashboard (`PUT /api/knowledge/enabled`,
`handleKnowledgeToggle`) builds the primer live and registers every store
already connected — the bead-synth vault, ready git sources, and any
configured vault connected at boot (`registerKnowledgeStores`,
`cmd/hive/boot_knowledge_primer.go`) — so kicks are primed immediately.
When the hive booted with `knowledge.enabled: false`, configured
`vaults:` were never connected and the dashboard's knowledge API is the
file-only fallback; the response's `primer.restart_required` and
`primer.restart_reason` (also shown as a panel banner) name what still
needs a restart: those vaults, browsing configured wiki `layers:` (kicks
query them already), a non-`file` `engine`, and `knowledge.curator`
settings.

```yaml
knowledge:
  bead_synthesizer:
    # enabled: false                # opt out (defaults to true)
    schedule: hourly                # hourly (default) or daily
    min_confidence: 0.0             # drop facts classified below this
    target_layer: personal          # primer layer for the synth vault
    max_facts_per_cycle: 0          # 0 = unlimited
    vault_path: /data/vaults/bead-synth-wiki
    retention_policy:
      max_beads: 5000
      archive_after_synth_days: 7
      high_priority_retain_days: 30
      preserve_with_deps: true
```

| Field | Current behavior |
| --- | --- |
| `schedule` | `hourly` (also the default and the fallback for unrecognized values) or `daily` (`ParseSynthSchedule`, `bead_synthesizer.go:929-937`). |
| `min_confidence` | Facts whose heuristic classification confidence (`ClassifyBead`, `bead_synthesizer.go:254`, emits 0.5–0.8) falls below this are skipped. Default `0.0` — everything classifiable is ingested. |
| `target_layer` | Layer the synth vault is registered at with the primer; empty defaults to `personal` (`main.go:2525-2528`). |
| `max_facts_per_cycle` | Cap per synthesis cycle after dedup; `0` means unlimited (`bead_synthesizer.go:223-226`). |
| `vault_path` | Defaults to `/data/vaults/bead-synth-wiki`; the directory is created and auto-connected as vault `bead-synth-wiki` (`main.go:2510-2522`). |

`retention_policy` drives a bead lifecycle manager that archives old beads —
it is always constructed, whether or not the block is present
(`main.go:2541-2553`). Zero-valued fields fill in as `max_beads: 5000`,
`archive_after_synth_days: 7`, `high_priority_retain_days: 30`
(`pkg/knowledge/bead_lifecycle.go:29-31`); archiving stops once counts drop to
80% of `max_beads`. One asymmetry to watch: `preserve_with_deps` (never
archive a bead with open dependents) defaults to **true when the whole
`retention_policy` block is absent**, but to **false when the block is present
without the key** — set it explicitly whenever you write the block.

Runtime status and toggle: `GET /api/knowledge/bead-synthesizer` and
`PUT /api/knowledge/bead-synthesizer/enabled` (owner-only) — see
[api-reference.md](api-reference.md).

## Repository code maps (`knowledge.code_maps`)

A code map is a **generated** knowledge page — never hand-authored — that
summarizes a checked-out repository so agents get a bounded orientation
primer for it. The generator lives in `pkg/knowledge/codemap.go` and the
refresh loop in `pkg/knowledge/codemap_schedule.go`.

```yaml
knowledge:
  code_maps:
    enabled: true                 # opt-in; absent or false does nothing
    schedule: hourly              # how often HEAD is checked (hourly or daily)
    vault_path: /data/vaults/code-maps
    layer: project
    max_bytes: 16384
    repos:
      - name: hivecommons/hive    # used as the repo:<name> scope tag
        path: /workspace/hive     # an existing checkout...
      - url: https://github.com/org/other   # ...or an https URL to clone
        branch: main
```

Each map has these sections, every one sorted deterministically:

| Section | Contents |
| --- | --- |
| Packages | Directories with source files; Go directories show the package name and file/test counts, other languages a per-language file count. |
| Entry points | Go `main` packages, `cmd/` directories, scripts (`*.sh`, `scripts/`, `bin/`) and build/entry files (`Makefile`, `Dockerfile`, `go.mod`, `package.json`, …). |
| Ownership hotspots | `CODEOWNERS` rules (repo root, `.github/` or `docs/`) and, for git checkouts, directories ranked by file changes in the last 200 commits. |
| Public APIs | Exported top-level Go identifiers per non-`main` package (first 15, then `+N more`). Other ecosystems get only the generic directory summary. |
| Test layout | Test-file counts per directory and test directories (`test/`, `tests/`, `e2e/`, `testdata/`, …). |
| Extension points | Exported Go interfaces, `Register*` functions, and plugin-style directories (`plugins/`, `hooks/`, `connectors/`, `providers/`, …). |

Dependency and build directories (`vendor/`, `node_modules/`, `dist/`,
`target/`, dot-directories other than `.github/`) are skipped.

**Size caps.** Each section keeps at most 40 entries and 4 KiB; the whole
body is capped at `max_bytes` (default 16 KiB, minimum 1 KiB); individual
entries are clipped to 240 bytes; the walk stops after 20,000 files and the
map says it is partial. Dropped entries or sections are replaced by a
`… truncated:` marker line, always at the same point for the same input.

**Storage and scoping.** Maps are written to
`<vault_path>/codemaps/codemap-<repo>.md` as `type: reference`,
`state: approved` facts tagged `codemap`, `generated` and `repo:<name>`, so the
existing repo scoping (for example the knowledge table of contents `repos`
filter) includes a map only for its own repository. The vault is connected as
`code-maps` and primed like any other vault.

**Freshness metadata.** Frontmatter records `generator: hive-codemap`,
`generator_version`, `source_sha` (the checkout's `HEAD`), `content_hash`,
`generated_at` and `synced` (which feeds the existing freshness signal).

**Regeneration.** Every tick the refresher reads `HEAD` for each repo (URL
repos are pulled first). A map is regenerated when it is missing, the generator
version changed, `HEAD` moved, or the revision is unknown (not a git
checkout). The file is rewritten only when something changed: a new `HEAD`
whose summary is identical updates `source_sha` but keeps `generated_at`, and
an identical regeneration leaves the file untouched. Edits made by hand are
overwritten on the next change, so do not edit these pages.

## Open questions

- #4944 (the issue behind this section) suggested documenting auth for
  private repos as if it exists. It does not: see [Auth for private
  repos](#auth-for-private-repos) above. If private-repo support is added
  later, this section should be updated with the actual credential-config
  shape rather than assumed ahead of the code.
