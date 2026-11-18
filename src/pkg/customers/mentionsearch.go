package customers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Step E of the Customers RFC (hivecommons/hive#18742, #18884): when a
// person accepts a suggested GitHub login, one budgeted GitHub search for
// mentions and comments gives the new profile history beyond what the hive
// already holds. It is reached from the accept path only. The discovery
// sweep (Sweep) is handed no searcher and makes no network call.
//
// Privacy: only the hit's repository, number and kind are read, never a
// title, body or comment text; an edge is a reference. Nothing here logs a
// login, a query or a token: the caller logs the person id and the counts in
// MentionSearchResult.

// Per-accept budget. Hard limits, not defaults: a configuration value can
// lower the hourly budget but never raises these.
const (
	// MentionSearchMaxRequests is the most search requests one accept makes:
	// one for mentions and one for comments, about two calls per person as
	// the RFC says.
	MentionSearchMaxRequests = 2
	// MentionSearchPageSize is the results one request asks for (one page).
	MentionSearchPageSize = 30
	// MentionSearchMaxResults is the most hits one accept keeps.
	MentionSearchMaxResults = MentionSearchMaxRequests * MentionSearchPageSize
	// MentionSearchSource names the edges this search writes.
	MentionSearchSource = "github_search"
	// MentionSearchWindow is the rolling window of the per-hour budget.
	MentionSearchWindow = time.Hour
)

// Search kinds, one request each, and the edge each one produces.
const (
	MentionKindMentions  = "mentions"
	MentionKindCommenter = "commenter"
)

// Skip reasons a search reports instead of running. They are fixed words,
// safe to log.
const (
	SkipDisabled     = "disabled"
	SkipNoRepos      = "no_repos"
	SkipNotLogin     = "not_a_github_login"
	SkipHourlyBudget = "hourly_budget_spent"
	SkipQuotaLow     = "quota_low"
	SkipNoSearcher   = "no_searcher"
)

// MentionHit is one issue or pull request GitHub returned for a search. It
// carries no text.
type MentionHit struct {
	// Repo is "owner/name".
	Repo   string
	Number int
	IsPR   bool
	// At is when the item last changed, the time of the edge.
	At time.Time
}

// MentionSearcher runs one GitHub search request. kind is MentionKindMentions
// or MentionKindCommenter; repos scopes the query. It returns at most limit
// hits from one page.
type MentionSearcher interface {
	SearchMentions(ctx context.Context, kind, login string, repos []string, limit int) ([]MentionHit, error)
}

// ErrMentionQuotaLow is what a MentionGate or searcher returns when the
// shared GitHub quota says to stand down.
var ErrMentionQuotaLow = errors.New("customers: github quota low")

// MentionSearch is the budgeted accept-time search. The zero value is not
// usable; build one with NewMentionSearch.
type MentionSearch struct {
	searcher MentionSearcher
	index    *Index

	// Enabled, HourlyLimit, Repos and QuotaLow are read at call time so a
	// settings change applies without a restart. A nil Enabled is off.
	Enabled     func() bool
	HourlyLimit func() int
	Repos       func() []string
	// QuotaLow reports a non-empty reason when the shared GitHub quota is
	// low or the secondary-limit caution window is engaged. Nil means no
	// gate.
	QuotaLow func() string

	now func() time.Time

	mu    sync.Mutex
	spent []time.Time // request times inside the rolling hour
}

// NewMentionSearch wires a searcher to the edge index.
func NewMentionSearch(searcher MentionSearcher, index *Index) *MentionSearch {
	return &MentionSearch{searcher: searcher, index: index, now: time.Now}
}

// MentionSearchResult counts what one accept's search did. It holds ids and
// counts only, so the whole value is safe to log.
type MentionSearchResult struct {
	Person   string `json:"person"`
	Skipped  string `json:"skipped,omitempty"`
	Requests int    `json:"requests"`
	Found    int    `json:"found"`
	Edges    int    `json:"edges"`
	// Truncated is true when a page came back full, so more history exists
	// than the budget reads.
	Truncated bool `json:"truncated,omitempty"`
}

// reserve takes up to want requests from the rolling hourly budget and
// returns how many it got.
func (m *MentionSearch) reserve(want, limit int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	kept := m.spent[:0]
	for _, t := range m.spent {
		if now.Sub(t) < MentionSearchWindow {
			kept = append(kept, t)
		}
	}
	m.spent = kept
	free := limit - len(m.spent)
	if free <= 0 {
		return 0
	}
	if want > free {
		want = free
	}
	for i := 0; i < want; i++ {
		m.spent = append(m.spent, now)
	}
	return want
}

// refund hands back requests reserved but not made.
func (m *MentionSearch) refund(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ; n > 0 && len(m.spent) > 0; n-- {
		m.spent = m.spent[:len(m.spent)-1]
	}
}

// Run searches GitHub for the activity of one accepted GitHub login and
// writes edges for person. It never returns the login. Only GitHub logins
// are searched: an email address is never sent to GitHub.
func (m *MentionSearch) Run(ctx context.Context, person string, ident Identity) MentionSearchResult {
	res := MentionSearchResult{Person: person}
	switch {
	case m == nil || m.searcher == nil || m.index == nil:
		res.Skipped = SkipNoSearcher
		return res
	case m.Enabled == nil || !m.Enabled():
		res.Skipped = SkipDisabled
		return res
	case ident.Kind != KindGitHub || strings.TrimSpace(ident.Value) == "" || IsBotLogin(ident.Value):
		res.Skipped = SkipNotLogin
		return res
	}
	var repos []string
	if m.Repos != nil {
		repos = m.Repos()
	}
	if len(repos) == 0 {
		res.Skipped = SkipNoRepos
		return res
	}
	if m.QuotaLow != nil && m.QuotaLow() != "" {
		res.Skipped = SkipQuotaLow
		return res
	}
	limit := 0
	if m.HourlyLimit != nil {
		limit = m.HourlyLimit()
	}
	granted := m.reserve(MentionSearchMaxRequests, limit)
	if granted == 0 {
		res.Skipped = SkipHourlyBudget
		return res
	}
	login := strings.TrimPrefix(strings.TrimSpace(ident.Value), "@")
	via := IdentityKey(KindGitHub, ident.Value)

	kinds := [MentionSearchMaxRequests]string{MentionKindMentions, MentionKindCommenter}
	seen := map[string]bool{}
	var edges []Edge
	made := 0
	for _, kind := range kinds[:granted] {
		if ctx.Err() != nil {
			break
		}
		// The quota can drop between the two requests: re-check, and hand
		// back what is not spent.
		if made > 0 && m.QuotaLow != nil && m.QuotaLow() != "" {
			res.Skipped = SkipQuotaLow
			break
		}
		room := MentionSearchMaxResults - len(edges)
		if room <= 0 {
			break
		}
		if room > MentionSearchPageSize {
			room = MentionSearchPageSize
		}
		hits, err := m.searcher.SearchMentions(ctx, kind, login, repos, room)
		made++
		if err != nil {
			if errors.Is(err, ErrMentionQuotaLow) {
				res.Skipped = SkipQuotaLow
			}
			break
		}
		if len(hits) > room {
			hits = hits[:room]
		}
		if len(hits) >= MentionSearchPageSize {
			res.Truncated = true
		}
		pred := PredMentionedIn
		if kind == MentionKindCommenter {
			pred = PredCommentedOn
		}
		for _, h := range hits {
			if h.Repo == "" || h.Number <= 0 {
				continue
			}
			ref := IssueRef(h.Repo, h.Number)
			if h.IsPR {
				ref = PRRef(h.Repo, h.Number)
			}
			key := pred + "\x00" + ref
			if seen[key] {
				continue
			}
			seen[key] = true
			at := h.At
			if at.IsZero() {
				at = m.now()
			}
			edges = append(edges, Edge{
				Person: person, Predicate: pred, Artefact: ref,
				Via: via, Source: MentionSearchSource, At: at, Confidence: 1,
			})
		}
	}
	if made < granted {
		m.refund(granted - made)
	}
	res.Requests = made
	res.Found = len(edges)
	if len(edges) > 0 {
		n, err := m.index.PutNew(edges)
		if err != nil {
			res.Skipped = "index_error"
			return res
		}
		res.Edges = n
	}
	return res
}

// String renders the result for a log line: ids and counts only.
func (r MentionSearchResult) String() string {
	return fmt.Sprintf("person=%s requests=%d found=%d edges=%d skipped=%s", r.Person, r.Requests, r.Found, r.Edges, r.Skipped)
}
