# Portfolio search core

`search` is the storage and query core of the portfolio search RFC (#14491,
Part A, #14634). It defines the document model and the `Backend` contract, and
ships the default embedded backend. Nothing is wired to it yet: ingest, the
`/api/search` endpoint, the dashboard pane and the `search.*` configuration
keys build on this package in follow-up issues.

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
