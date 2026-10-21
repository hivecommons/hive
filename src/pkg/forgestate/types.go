package forgestate

import (
	"sort"
	"strings"
	"time"
)

// Kind distinguishes issues from pull requests; they share a number space on
// GitHub but are cached separately.
type Kind string

const (
	KindIssue Kind = "issue"
	KindPR    Kind = "pr"
)

// Source records which writer last set an item.
type Source string

const (
	SourceReconcile    Source = "reconcile"
	SourceWebhook      Source = "webhook"
	SourceWriteThrough Source = "writethrough"
	// SourceReadThrough marks an item ReadThrough re-fetched because its
	// cached copy was older than the caller's freshness bound.
	SourceReadThrough Source = "readthrough"
)

// Item is the cached state of one issue or pull request.
type Item struct {
	Kind      Kind      `json:"kind"`
	Number    int       `json:"number"`
	State     string    `json:"state"`
	Labels    []string  `json:"labels,omitempty"`
	Assignees []string  `json:"assignees,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Draft     bool      `json:"draft,omitempty"`
	Closed    bool      `json:"closed,omitempty"`
	// PR-only fields.
	HeadSHA        string `json:"head_sha,omitempty"`
	Merged         bool   `json:"merged,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`

	FetchedAt time.Time `json:"fetched_at"`
	Source    Source    `json:"source"`
	// Raw is the Fetcher's full payload for the item (for the GitHub
	// fetcher, the go-github issue or pull request plus the summaries the
	// list carried). Readers that classify items — the governor's work
	// source — take it from the cache instead of listing again. It is
	// treated as immutable once stored and shared between clones; the
	// typed fields above are authoritative where the two disagree (a
	// write-through updates Labels, not Raw). Never serialised.
	Raw any `json:"-"`
}

// Open reports whether the item belongs in the cache.
func (it Item) Open() bool {
	return !it.Closed && !it.Merged && !strings.EqualFold(it.State, "closed")
}

// HasLabel reports whether the item carries name (case-insensitive, as on GitHub).
func (it Item) HasLabel(name string) bool {
	for _, l := range it.Labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

func (it Item) clone() Item {
	it.Labels = append([]string(nil), it.Labels...)
	it.Assignees = append([]string(nil), it.Assignees...)
	return it
}

// sameContent reports whether a and b describe the same GitHub state,
// ignoring which writer set them and when.
func sameContent(a, b Item) bool {
	return a.State == b.State && a.UpdatedAt.Equal(b.UpdatedAt) && a.HeadSHA == b.HeadSHA &&
		a.Draft == b.Draft && a.Closed == b.Closed && a.Merged == b.Merged &&
		sameSet(a.Labels, b.Labels) && sameSet(a.Assignees, b.Assignees)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := lowerSorted(a), lowerSorted(b)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func lowerSorted(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.ToLower(v)
	}
	sort.Strings(out)
	return out
}

// Snapshot is a point-in-time copy of one repo's cache. Callers own it.
type Snapshot struct {
	Repo   string       `json:"repo"`
	Issues map[int]Item `json:"issues"`
	PRs    map[int]Item `json:"prs"`
	// Ready is true once the repo completed its first full reconcile.
	Ready bool `json:"ready"`
	// Stale is set when the most recent reconcile failed; the items are the
	// last good ones and StaleSince is when the failures began.
	Stale       bool      `json:"stale"`
	StaleSince  time.Time `json:"stale_since,omitempty"`
	StaleReason string    `json:"stale_reason,omitempty"`
	LastGoodAt  time.Time `json:"last_good_at,omitempty"`
	// LastDeliveryID is the most recent webhook delivery applied (audit).
	LastDeliveryID string `json:"last_delivery_id,omitempty"`
	// Drift is how many items the last reconcile found wrong in the cache;
	// DriftHigh is set when that exceeded the drift threshold.
	Drift     int  `json:"drift"`
	DriftHigh bool `json:"drift_high,omitempty"`
}

// IssueCount returns the number of cached open issues.
func (s Snapshot) IssueCount() int { return len(s.Issues) }

// PRCount returns the number of cached open pull requests.
func (s Snapshot) PRCount() int { return len(s.PRs) }

// canonicalRepo is the cache key for an owner/name slug.
func canonicalRepo(repo string) string {
	return strings.ToLower(strings.TrimSpace(repo))
}
