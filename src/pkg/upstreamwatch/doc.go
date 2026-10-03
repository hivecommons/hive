// Package upstreamwatch lists candidate changes from an upstream repo that a
// fork may want to port. It carries the read-only source over the GitHub API
// (hivecommons/hive#9997), the durable state — per-repo watermark, filed-ref
// dedupe index and dismissal tracking (hivecommons/hive#10000) — and the
// classification, fork applicability and port-difficulty judgement
// (hivecommons/hive#9999), and the poll loop that renders and files fork
// issues with a hidden upstream-ref marker (hivecommons/hive#10002), all
// modelled on pkg/releasesentinel. It opens issues only, never PRs. Each run
// also reconciles previously filed refs against their fork issue, recording
// ported or dismissed once the fork has closed it (hivecommons/hive#9969).
package upstreamwatch
