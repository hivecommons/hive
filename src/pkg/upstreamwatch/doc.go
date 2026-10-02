// Package upstreamwatch lists candidate changes from an upstream repo that a
// fork may want to port. This file is the first slice of hivecommons/hive#9967:
// only the read-only source over the GitHub API, modelled on
// pkg/releasesentinel's github_source.go. Classification, applicability, the
// durable watermark store, rendering and issue filing live in later slices.
package upstreamwatch
