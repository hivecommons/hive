# Knowledge curator configuration

The knowledge curator extracts candidate facts from merged PR activity and can prepare high-confidence facts for promotion between llm-wiki layers. The live code is in `pkg/knowledge/curator.go` and `pkg/knowledge/promote.go`.

Enable the knowledge system before tuning the curator:

```yaml
knowledge:
  enabled: true
  engine: llm-wiki
  curator:
    schedule: daily
    extract_from:
      - pr_comments
      - review_comments
    auto_promote_threshold: 0.9
```

## Fields

| Field | Current behavior |
| --- | --- |
| `schedule` | Defaults to `daily` when knowledge is enabled. The value is stored as `CuratorConfig.Schedule`. No scheduler parses or actions it. Extraction runs only when Go code invokes `Curator.RunExtraction` explicitly; see [Scheduling extraction](#scheduling-extraction). |
| `extract_from` | Sources the curator should inspect. Implemented sources are `pr_comments` and `review_comments`. `ci_failures` appears in the example config/design but is not currently extracted by `Curator.extractFromPR`. |
| `auto_promote_threshold` | Defaults to `0.9` when knowledge is enabled. `Promoter.AutoPromoteCandidates` selects facts whose llm-wiki page has `status == "verified"` and `confidence >= threshold`. |

## Extraction behavior

`RunExtraction(ctx, since)` lists up to 50 recently updated closed PRs per configured repo, keeps merged PRs newer than `since`, and examines configured comment sources. Comments shorter than 20 characters are ignored.

The classifier is heuristic. It emits candidates for comments containing signals such as:

- `always`, `never`, `must`, `do not` → gotchas,
- `regression`, `broke`, `reverted` → regressions,
- `pattern`, `convention`, `prefer`, `best practice` → patterns,
- `test`, `coverage`, `mock`, `fixture`, `assert` → test scaffolding,
- `decided`, `agreed`, `going forward`, `from now on` → decisions.

Extracted facts are sent to the wiki `/api/ingest` endpoint. Promotion only flows upward (for example project → org), preserving provenance in the promoted fact.

## Scheduling extraction

Extraction runs only when Go code invokes `Curator.RunExtraction` explicitly. No scheduler, CLI command, or HTTP endpoint triggers extraction.

A Go caller builds a `Curator` with `NewCurator(ghClient, wikiURL, org, repos, config, logger)`. The caller calls `RunExtraction(ctx, since)` with a `since` time. The caller then calls `Ingest(ctx, facts)`. The call POSTs the facts to the wiki `/api/ingest` endpoint:

```go
c := NewCurator(ghClient, wikiURL, org, repos, config, logger)
facts, err := c.RunExtraction(ctx, since)
err = c.Ingest(ctx, facts)
```

The design doc `src/docs/design/knowledge-system.md` plans a `scheduler.RunCurator(schedule)` wiring with a daily or on-merge trigger. This wiring is not implemented. The programmatic path above is the only way to run extraction today.

See [Knowledge system design](design/knowledge-system.md) for architecture context; this page documents the implemented operator-facing knobs.

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
| `layer` | No | `project` when set via the API (`api_knowledge.go:937-939`); **required, no code default, when set via `hive.yaml`** | One of `personal`, `project`, `org`, `community` — see Layer semantics below. |

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
`pkg/dashboard/api_knowledge.go:885-891,893-975,977-1019`) manage sources at runtime
and are the *same* underlying list as `knowledge.git_sources` in
`hive.yaml` — not a separate system:

- `POST` connects a source immediately and, if it isn't already present
  (matched by `url`+`subpath`), appends it to `Config.Knowledge.GitSources`
  and persists the config (`api_knowledge.go:954-971`). A `POST` for a source that
  is only in `hive.yaml` but not yet connected in the running process (e.g.
  right after editing the file without restarting) will add a duplicate
  config entry once reconnected, since the dedup check is by URL+subpath
  against what's already in `Config`, not against what main.go loaded at
  boot.
- `DELETE` disconnects the live source and removes matching entries from
  `Config.Knowledge.GitSources`, then persists (`api_knowledge.go:1005-1015`).
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

## Open questions

- #4944 (the issue behind this section) suggested documenting auth for
  private repos as if it exists. It does not: see [Auth for private
  repos](#auth-for-private-repos) above. If private-repo support is added
  later, this section should be updated with the actual credential-config
  shape rather than assumed ahead of the code.
