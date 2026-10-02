package upstreamwatch

import (
	"context"
	"time"
)

// ItemKind labels an upstream candidate as either a merged pull request or a
// published release.
type ItemKind string

const (
	// KindPR is a pull request merged on the upstream after the watermark.
	KindPR ItemKind = "pr"
	// KindRelease is a non-draft release published on the upstream after the
	// watermark.
	KindRelease ItemKind = "release"
)

// RefPR is the stable upstream reference for a merged PR candidate; the
// number is substituted at use. It matches the dedupe key the hub-side
// pieces (hivecommons/hive#9999, #10000) will store.
func RefPR(number int) string { return formatPRRef(number) }

// RefRelease is the stable upstream reference for a release candidate.
func RefRelease(tag string) string { return "release:" + tag }

// Item is one upstream candidate: everything downstream classification and
// applicability need, with no judgement applied yet.
type Item struct {
	// Kind is KindPR or KindRelease.
	Kind ItemKind
	// Ref is the dedupe key: "upstream#<pr>" or "release:<tag>".
	Ref string
	// Title is the PR or release title as it appears upstream.
	Title string
	// Body is the PR or release body.
	Body string
	// HTMLURL points at the PR or release on GitHub.
	HTMLURL string
	// Timestamp is merged_at for PRs and published_at for releases. Results
	// are sorted oldest-first by this field so a caller can advance a
	// watermark incrementally.
	Timestamp time.Time
	// Labels are the labels carried by the upstream PR; always empty for
	// releases.
	Labels []string
	// Files are the files the upstream PR touched; always empty for
	// releases.
	Files []string
	// Additions and Deletions are the PR's totals across all files; always
	// zero for releases.
	Additions int
	Deletions int
}

// Source is the read-only view of a configured upstream the watch needs.
// A GitHub-backed implementation is in github_source.go; tests substitute a
// fake.
type Source interface {
	// List returns every upstream PR merged after since and every non-draft
	// release published after since, filtered by the configured sources and
	// pr_labels, sorted oldest-first by Timestamp.
	List(ctx context.Context, since time.Time) ([]Item, error)
}
