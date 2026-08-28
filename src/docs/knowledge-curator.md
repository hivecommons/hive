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
does the cloning, indexing, and periodic sync; `cmd/hive/main.go:2207-2249`
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

Config fields (`GitSourceConfigYAML`, `pkg/config/config.go:562-568`, mirrored
by the runtime type `GitSourceConfig`, `pkg/knowledge/gitsource.go:33-39`):

| YAML key | Required | Default | Notes |
|---|---|---|---|
| `name` | Yes | — | Display name; also the key used to look up/remove the source via the API. |
| `url` | Yes | — | Git remote URL. **`https://` only** — see Auth below. |
| `branch` | No | `main` (`gitsource.go:66-68`) | Branch to shallow-clone (`--depth 1 --branch <branch>`). |
| `subpath` | No | — (whole repo) | When set, only this subdirectory is checked out (git sparse-checkout, `gitsource.go:199-219,232-248`) and indexed. |
| `layer` | No | `project` when set via the API (`api.go:8115-8117`); **required, no code default, when set via `hive.yaml`** | One of `personal`, `project`, `org`, `community` — see Layer semantics below. |

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
`pkg/dashboard/api.go:8068,8090-8153,8155-8192`) manage sources at runtime
and are the *same* underlying list as `knowledge.git_sources` in
`hive.yaml` — not a separate system:

- `POST` connects a source immediately and, if it isn't already present
  (matched by `url`+`subpath`), appends it to `Config.Knowledge.GitSources`
  and persists the config (`api.go:8131-8149`). A `POST` for a source that
  is only in `hive.yaml` but not yet connected in the running process (e.g.
  right after editing the file without restarting) will add a duplicate
  config entry once reconnected, since the dedup check is by URL+subpath
  against what's already in `Config`, not against what main.go loaded at
  boot.
- `DELETE` disconnects the live source and removes matching entries from
  `Config.Knowledge.GitSources`, then persists (`api.go:8155-8192`).
- Editing `git_sources:` directly in `hive.yaml` takes effect on the next
  process restart (main.go's startup loop at `cmd/hive/main.go:2207-2249`);
  it does not hot-reload while the process is running. Use the API for a
  live change without a restart.

### Diagnosing a failed source

- **Never appears / `knowledge not enabled`**: if `knowledge.enabled` is
  `false` but `git_sources` is non-empty, startup auto-enables a minimal
  knowledge API (`engine: file`) just to host the git sources
  (`main.go:2208-2216`) — so a git source can work even with `knowledge.enabled: false`.
  If you still get "knowledge not enabled" from the API, no source has
  triggered that auto-enable yet (empty `git_sources` list).
- **Connect fails immediately**: check the hive log for `failed to connect
  git source` with the URL and error (`main.go:2225-2230`) — most often an
  SSRF-validation rejection, a bad branch name, or (for private repos) an
  authentication failure from git itself.
- **Connects but `subpath` errors**: `subpath "<x>" not found after clone` —
  the path is wrong or doesn't exist on the configured `branch`.
- **Facts never show up in kicks**: confirm the source reached `Ready: true`
  (`GET /api/knowledge/git-sources`) — the primer only registers a source's
  `FileStore` for priming after it reports ready
  (`main.go:2240-2249`).

## Open questions

- #4944 (the issue behind this section) suggested documenting auth for
  private repos as if it exists. It does not: see [Auth for private
  repos](#auth-for-private-repos) above. If private-repo support is added
  later, this section should be updated with the actual credential-config
  shape rather than assumed ahead of the code.
