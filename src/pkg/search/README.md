# Portfolio search core

`search` is the storage and query core of the portfolio search RFC (#14491,
Part A, #14634). It defines the document model and the `Backend` contract, and
ships the default embedded backend and the `Indexer` that feeds it (#14635).
The dashboard's `GET /api/search` (#14637) queries the indexer's backend; the
dashboard pane, forge metadata and metrics ingest build on this package in
follow-up issues.

## Document model

Every `Document` carries `Repo` (`owner/name`), `ID` (unique within the repo),
`Kind`, `At`, `URL` and `Visibility` (`public` or `private`). `Title` and `Text`
are tokenised for full-text search; `Fields` holds keyword values such as
`verdict` or `sentinel.rule` that are tokenised, filterable and facetable.

## Embedded backend

Modelled on `pkg/codeindex` (decision 1 of #14491: extend codeindex-style
postings rather than adopt bleve; no new module):

- Tokenisation follows `codeindex`: identifiers, dotted keys and `--flags` are
  whole terms and their components are indexed too. Unlike `codeindex`, terms
  may start with a digit, so issue and PR numbers are searchable. Terms match
  exactly (case-insensitively); query text that yields no terms matches
  nothing. Results rank by distinct shared terms, then newest
  `At`, then repo and ID.
- Each repo has an immutable snapshot (postings plus per-field facet postings)
  that is swapped in atomically, so concurrent queries never see a partial
  update.
- Snapshots persist to `<Dir>/<owner>/<name>.idx` (default `Dir` is
  `/data/search`) via a sibling temp file, fsync and atomic rename.
- `MaxIndexBytesPerRepo` (default 32 MiB, the future
  `search.max_index_bytes_per_repo`): an update whose serialised image exceeds
  it is rejected with a `*CoverageError` wrapping `ErrIndexBudget`; the previous
  snapshot stays published and the coverage error is reported by `Query` and
  `Stats` until the next successful update.
- `MaxMemoryBytes` (default 256 MiB, the future `search.max_memory_bytes`):
  loaded snapshots are accounted by serialised size; least-recently-queried
  repos are dropped from memory and reload from disk on demand.
- An unreadable or corrupt index file is a coverage error (`ErrCorruptIndex`)
  for queries and is treated as empty by updates, so the next `Index` or
  `Delete` for the repo (even a no-op one) rewrites it and clears the error.

As with `codeindex`, a repo listed in `Result.Coverage` may be incomplete;
missing hits there are not proof that no matching document exists.

## Indexer

`Indexer` (#14635) is started by `cmd/hive` only when `search.enabled: true`;
otherwise there is no goroutine and no `/data/search` directory. Every 30
minutes (`DefaultRebuildInterval`, the prior-art code index cadence) it runs
one incremental rebuild off the hot path:

- One walk of the evidence root: each review bundle
  (`<owner>/<repo>/<n>/<head>.json`) becomes an `evidence` document plus one
  `finding` document per verdict finding and one `sentinel` document per
  sentinel hit; each complexity snapshot becomes a `complexity` document.
  Facets include `verdict` and `sentinel.rule`.
- Beads (`bead`, under the repo of an `owner/repo#N` external ref, else the
  primary repo) and knowledge facts (`fact`, under the primary repo).
- Sources are read through their package APIs and never written. A source is
  re-indexed only when its stamp changes: bundle `updated_at`, snapshot
  `captured_at`, bead `updated_at`/status, or a hash of the fact content.
  Documents a source no longer yields, or whose source disappeared, are
  deleted.
- Documents older than `search.retention` months (by `At`) are not indexed
  and age out; the first rebuild of a process also removes leftovers of the
  indexer's kinds that no current source produces.
- Every title, text and field value passes through `crux/logscrub` first.
- No ingest runs during the hosted startup budget (#14389).
- Documents are `private` (never federated) unless the `Visibility` hook
  reports the repo `public`.

## Prior-art gate source

`PriorArtSource` (#14639) adds a lexical `search` source to the Governor
prior-art gate, wired by `cmd/hive` only when `search.enabled: true`. It
queries `evidence`, `finding`, `sentinel` and `bead` documents for the
issue's identifiers and top terms (knowledge facts stay with the gate's own
knowledge source) and hands at most 10 hits to the gate's ranker as `review`
candidates (`#<pr>` for evidence, findings and sentinel hits, the bead ID for
beads); the gate keeps its thresholds and parking. Beads and review
records of the gated issue itself (by `external_ref` or `#<n>`) are dropped
before ranking. Titles and
text pass through `ioscan.EnforceInput`, and documents it blocks are dropped.
One query is bounded by `PriorArtTimeout` (5s); an error or timeout only
counts as a failed source, so the gate answers from its other sources.

## Repo health score

At the end of every completed rebuild the indexer recomputes a report-only
health score per repo with review evidence (`health.go`, served by
`GET /api/repos/{owner}/{repo}/health`). Components: CI pass rate, median
time-to-merge (`evidence.MedianTimeToMerge`), revert/follow-up rate
(`evidence.PR.ChangeFailure`), review verdict mix (all over 90 days of
evidence), stale-issue ratio, sign-off rate, dependency freshness and
security-doc rot. The last four come from `Indexer.HealthSignals`; a nil
signal means no local data and the component is `not_reported`. A component
with fewer than `HealthMinN` samples (the hub's `minEvidenceN`) is also
`not_reported`, and the weighted 0–100 sum is renormalised over the reported
components, so missing data never counts as zero. The score is never an ACMM
input and nothing that decides a merge or Governor priority reads it.
