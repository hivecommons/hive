// Package forgestate is the per-repo GitHub state cache of
// hivecommons/hive#13373: one in-process snapshot of every open issue and
// pull request per repository, kept current by three writers and read by
// everything else.
//
//  1. Reconcile (authoritative) lists a repo's open issues and PRs on a slow
//     interval, ETag-conditional when the Fetcher supports it. A failed
//     reconcile keeps the last good snapshot and marks it Stale; it never
//     replaces it with an empty one. Each reconcile diffs the cache against
//     the API and records the difference as the forgestate_drift metric;
//     drift above the threshold shortens that repo's interval.
//  2. ApplyWebhook treats a delivery as a hint: it re-fetches only the named
//     item and upserts it monotonically (an older updated_at never replaces
//     a newer one). Closed and merged items leave the cache immediately and
//     leave a tombstone so a late, older "open" read cannot resurrect them;
//     a genuine reopen carries a newer updated_at and re-adds the item.
//  3. WriteThrough applies the hive's own mutations (stage labels, comments,
//     merges, closes, claims) to the cache as soon as GitHub accepted them,
//     so the next read sees them without another API call.
//
// Every GitHub call made here passes the Gate (core and search buckets)
// before it reaches the Fetcher; the production Fetcher wraps the hive's
// go-github client, whose transport already carries slow-start pacing and
// the shared ETag cache.
//
// Readers use Snapshot, Issue and PR for cached reads, ReadThrough for
// freshness-bounded reads before irreversible actions, Fresh for a
// repo-level check, and WaitReady as the startup gate that holds the first
// governor pass until every repo has completed one full reconcile.
//
// The package imports only pkg/config and go-github. Nothing constructs a
// Store unless forgestate.enabled is set and a caller wires it in.
package forgestate
