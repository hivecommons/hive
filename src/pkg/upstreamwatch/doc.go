// Package upstreamwatch lists candidate changes from an upstream repo that a
// fork may want to port. It carries the read-only source over the GitHub API
// (hivecommons/hive#9997), the durable state — per-repo watermark, filed-ref
// dedupe index and dismissal tracking (hivecommons/hive#10000) — and the
// classification, fork applicability and port-difficulty judgement
// (hivecommons/hive#9999), all modelled on pkg/releasesentinel. Rendering and
// issue filing live in the final slice of hivecommons/hive#9967.
package upstreamwatch
