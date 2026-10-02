// Package upstreamwatch lists candidate changes from an upstream repo that a
// fork may want to port. It carries the read-only source over the GitHub API
// (hivecommons/hive#9997) and the durable state — per-repo watermark, filed-ref
// dedupe index and dismissal tracking (hivecommons/hive#10000) — both modelled
// on pkg/releasesentinel. Classification, applicability, rendering and issue
// filing live in later slices of hivecommons/hive#9967.
package upstreamwatch
