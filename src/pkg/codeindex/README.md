# Local prior-art index

`codeindex` supplies the offline search component of #13774. It backs the
prior-art gate's `docs` and `code` sources (`pkg/github/prior_art_codeindex.go`,
#13878) in place of GitHub code search. It does not enforce a filing policy.

## Integration contract

- Construct `Repo` entries for **every registered repository** with its configured
  checkout root (`ProjectConfig.CheckoutRootFor`), not just the issue's repository.
  Missing checkouts are coverage errors, not evidence of no prior art.
- Call `Build` after checkout synchronization, off the filing request's hot path.
  Publish the returned immutable snapshot atomically. Keep the prior snapshot if
  the build was canceled. Log/display the returned per-repo coverage errors;
  partial coverage must never be presented as proof of absence.
- Search all extracted identifiers and title/body terms. Queries make no network
  calls, and have no GitHub code-search quota or two-identifier restriction.
  `Match.Doc` separates docs/README/CHANGELOG from code. Convert matches into
  prior-art candidates with `Path`, `Text` and `URL()`; the gate remains responsible
  for weighted confidence, final result limits and filing decisions.
- Rebuild to observe additions, edits and deletions. Search results do not expose
  mutable index storage, so concurrent readers can finish using the old snapshot.

The snapshot indexes tracked **working-tree** text; `Revision` identifies HEAD
at build time. Source links are revision-pinned, but local edits can differ from
those links. Production gate integration should build from clean synchronized
checkouts, not an agent's implementation worktree. This is a lexical index, not
semantic evidence that a feature is implemented or shipped.

## Bounds and exclusions

Only tracked regular files are indexed. Symlink leaves, paths resolving outside
the checkout, NUL-containing/non-UTF-8 files, files over 1 MiB, and dependency/build
directories (`vendor`, `node_modules`, `dist`, `build`, `.git`) are excluded.
Each repository has a 64 MiB text budget (exhaustion is a coverage error); source
lines are capped at 4096 bytes. Result ordering is deterministic by number of
shared exact terms, then repository and path. Query cancellation is honored.

## Remaining #13774 work

The gate's code/docs sources use this snapshot (#13878): it is built over every
`project.repos` entry at `checkouts_dir/<repo>`, rebuilt every 30 minutes, and
missing, dirty or partly indexed checkouts are listed as coverage gaps on the
gate report and its comment. Still remaining: enforce
`FilingVerdictFor` in the authorized relay filing path (including knowledge-bead
persistence and required "Gap vs existing" validation), and expose the remaining
prior-art configuration in Settings. No prompt-only filing precondition has been
strengthened by introducing this package alone.
