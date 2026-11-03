package dashboard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	// prLinkCacheTTL is how long a resolved (or resolved-empty) PR-link result is
	// trusted before a re-lookup. Short enough that a freshly opened/merged PR
	// surfaces within a minute or two, long enough that repeatedly rendering the
	// triage view for a ~150-issue backlog does not re-hit the Search API on every
	// request. Mirrors the "cheap cached projection" cadence of the metrics rollup.
	prLinkCacheTTL = 90 * time.Second

	// prLinkLookupTimeout bounds one whole lookup — the GitHub Search call plus
	// the issue-timeline read that follows it (at most prLinkTimelineMaxPages
	// requests) — so one slow response can never wedge the resolver (and,
	// transitively, the triage endpoint). On timeout the issue degrades to "no
	// linked PR" for this cycle and is retried after the TTL, exactly like an
	// error.
	prLinkLookupTimeout = 4 * time.Second

	// prLinkTimelineMaxPages caps how many 100-event timeline pages one lookup
	// reads. Like prLinkSearchPerPage, the point is to stay cheap: a real issue
	// has a few dozen events, and an issue busy enough to exceed 500 almost
	// always has its referencing PR among the earlier ones. Past the cap the
	// lookup keeps what it has read instead of walking the rest.
	prLinkTimelineMaxPages = 5

	// prLinkStateOpen / prLinkStateMerged are the two surfaced link states. A PR
	// that is closed-but-not-merged is treated as "no active link" (nil) — it did
	// not land the fix and is not awaiting review — so only these two states ever
	// reach the UI.
	prLinkStateOpen   = "open"
	prLinkStateMerged = "merged"
)

// prLinkStatus is the resolved fix-PR for one issue, or nil when none is linked.
// Number/URL/State are the only fields the UI needs to render a small
// "PR #NNN (open|merged)" badge and to feed the triage-level derivation.
type prLinkStatus struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"` // prLinkStateOpen | prLinkStateMerged
}

// prLinkCacheEntry is one memoised lookup. status may be nil (no linked PR); the
// nil result is cached too so a genuinely-unlinked issue is not re-searched every
// render within the TTL window.
type prLinkCacheEntry struct {
	status   *prLinkStatus
	resolved time.Time
}

// prLinkResolver memoises issue→PR lookups behind a short TTL. It owns ONLY the
// cache + the GitHub Search call; it holds no issue state of its own, so it is a
// pure projection over live GitHub data. Safe for concurrent use.
type prLinkResolver struct {
	mu    sync.Mutex
	cache map[string]prLinkCacheEntry
	// now/lookup are overridable in tests so the resolver can be exercised against
	// an httptest GitHub fake and a controllable clock without a real Server.
	now    func() time.Time
	lookup func(ctx context.Context, repo string, number int) (*prLinkStatus, error)
}

// newPRLinkResolver builds a resolver whose lookups run against the given
// go-github client. A nil client yields a resolver that always returns "no linked
// PR" (the degrade path), so callers never need to nil-check the GitHub wiring.
func newPRLinkResolver(gc *gh.Client) *prLinkResolver {
	r := &prLinkResolver{
		cache: make(map[string]prLinkCacheEntry),
		now:   time.Now,
	}
	r.lookup = func(ctx context.Context, repo string, number int) (*prLinkStatus, error) {
		return searchLinkedPR(ctx, gc, repo, number)
	}
	return r
}

// prLinkKey is the canonical "owner/repo#number" cache/identity key, matching the
// key format activeIssueKeys/selectTask use so a triage row and a fleet item line
// up on the same string.
func prLinkKey(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

// resolve returns the cached PR-link for an issue, refreshing it through GitHub
// when the entry is missing or past its TTL. On any lookup error it caches and
// returns nil ("no linked PR") for the TTL window rather than propagating the
// error, so a caller rendering a whole queue never fails on one bad issue.
func (r *prLinkResolver) resolve(ctx context.Context, repo string, number int) *prLinkStatus {
	if r == nil || repo == "" || number <= 0 {
		return nil
	}
	key := prLinkKey(repo, number)

	r.mu.Lock()
	if e, ok := r.cache[key]; ok && r.now().Sub(e.resolved) < prLinkCacheTTL {
		r.mu.Unlock()
		return e.status
	}
	r.mu.Unlock()

	lookup := r.lookup
	var status *prLinkStatus
	if lookup != nil {
		lctx, cancel := context.WithTimeout(ctx, prLinkLookupTimeout)
		s, err := lookup(lctx, repo, number)
		cancel()
		if err == nil {
			status = s
		}
		// err != nil → status stays nil (degrade to "no linked PR"); still cached
		// below so we don't retry a flapping call on every render within the TTL.
	}

	r.mu.Lock()
	r.cache[key] = prLinkCacheEntry{status: status, resolved: r.now()}
	r.mu.Unlock()
	return status
}

// searchLinkedPR asks GitHub which PR (if any) fixes this issue. It runs one
// Search.Issues query scoped to the issue's repo, matching PRs that mention the
// issue number — GitHub indexes any plain "#N" body/title mention, so each hit
// is then checked for a closing keyword (Closes/Fixes/Resolves #N) in its
// title or body; a passing "see also #N" is not a linked PR. A merged PR wins
// over a merely-open one (the fix landed), so results are scanned for a merged
// hit first, then an open one. When the search yields no merged PR it also
// reads the issue's timeline for additional candidates. Timeline candidates
// must have a closing keyword in their title/body too: a comment from any
// same-repo commenter can create a cross-reference, but is not fix evidence.
// A nil client or empty result is "no linked PR" (nil, nil) — never an error
// the caller must handle.
func searchLinkedPR(ctx context.Context, gc *gh.Client, repo string, number int) (*prLinkStatus, error) {
	if gc == nil || repo == "" || number <= 0 {
		return nil, nil
	}
	// repo is "owner/name". Scope the search to that repo and to PRs referencing
	// the issue number. This is the same Search.Issues primitive the fleet-stats
	// PR counters use; the "#N" qualifier is GitHub's own linked-reference index.
	query := fmt.Sprintf("repo:%s type:pr %d", repo, number)
	result, _, err := gc.Search.Issues(ctx, query, &gh.SearchOptions{
		Sort:        "updated",
		Order:       "desc",
		ListOptions: gh.ListOptions{PerPage: prLinkSearchPerPage},
	})
	if err != nil {
		return nil, err
	}

	var open *prLinkStatus
	if result != nil {
		for _, iss := range result.Issues {
			if iss == nil || iss.PullRequestLinks == nil {
				continue // search can return issues too; keep only PRs
			}
			if !searchHitClosesIssue(iss, repo, number) {
				continue
			}
			st, merged := prLinkCandidate(iss)
			if merged {
				return st, nil
			}
			if st != nil && open == nil {
				open = st
			}
		}
	}

	// The timeline supplements the bounded search with cross-referencing PRs.
	// These events include comment-only references, which anyone able to
	// comment can create; require closing intent in the PR's own title/body
	// before promoting dashboard state. A timeline error keeps search results.
	for _, iss := range crossReferencingPRs(ctx, gc, repo, number) {
		if !prClosesIssue(iss, repo, number) {
			continue
		}
		st, merged := prLinkCandidate(iss)
		if merged {
			return st, nil
		}
		if st != nil && open == nil {
			open = st
		}
	}
	return open, nil
}

// prLinkCandidate grades one referencing PR: merged (the fix landed), open
// (awaiting review), or nil for a PR closed without merging.
func prLinkCandidate(iss *gh.Issue) (*prLinkStatus, bool) {
	st := &prLinkStatus{Number: iss.GetNumber(), URL: iss.GetHTMLURL()}
	// A closed PR that carries a merged_at (via PullRequestLinks) landed the
	// fix — surface it as merged immediately (it also implies the issue is
	// Closed in the triage ladder). Prefer it over any open candidate.
	if iss.PullRequestLinks != nil && iss.PullRequestLinks.MergedAt != nil {
		st.State = prLinkStateMerged
		return st, true
	}
	if iss.GetState() == "open" {
		st.State = prLinkStateOpen
		return st, false
	}
	return nil, false
}

// crossReferencingPRs returns the same-repo pull requests named by the
// issue's timeline cross-referenced events — a reference from a PR's title,
// body, or comments (commit-message references are referenced events and are
// not matched). It reads at most prLinkTimelineMaxPages
// pages; errors and the page cap yield what was read so far.
func crossReferencingPRs(ctx context.Context, gc *gh.Client, repo string, number int) []*gh.Issue {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil
	}
	var out []*gh.Issue
	seen := map[int]bool{}
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < prLinkTimelineMaxPages; page++ {
		events, resp, err := gc.Issues.ListIssueTimeline(ctx, owner, name, number, opts)
		if err != nil {
			return out
		}
		for _, ev := range events {
			if ev == nil || !strings.EqualFold(ev.GetEvent(), "cross-referenced") {
				continue
			}
			src := ev.GetSource()
			if src == nil || src.Issue == nil || !src.Issue.IsPullRequest() {
				continue
			}
			if full := src.Issue.GetRepository().GetFullName(); full != "" && !strings.EqualFold(full, repo) {
				continue
			}
			if n := src.Issue.GetNumber(); n > 0 && !seen[n] {
				seen[n] = true
				out = append(out, src.Issue)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return out
		}
		opts.Page = resp.NextPage
	}
	return out
}

// prLinkSearchPerPage caps how many candidate PRs one lookup inspects. A handful
// is plenty: a real issue has at most a few referencing PRs, and we only need to
// know whether an open or merged one exists. Small page keeps the Search call
// cheap and within a single request.
const prLinkSearchPerPage = 10

// searchHitClosesIssue reports whether a Search.Issues hit should count as
// the issue's linked PR. The "#N" search qualifier indexes bare mentions, so
// the hit must name the issue with a closing keyword (Closes/Fixes/Resolves)
// in its title or body; a passing "see also #N" is not promoted. A hit with
// no title or body (omitted by the API on some responses) is accepted, since
// the number qualifier already scoped it.
func searchHitClosesIssue(iss *gh.Issue, repo string, number int) bool {
	title, body := iss.GetTitle(), iss.GetBody()
	if title == "" && body == "" {
		return true
	}
	_, ok := github.FindClosingKeywordReference(title+"\n"+body, repo, repo, number)
	return ok
}

// prClosesIssue requires explicit closing intent in a timeline PR's own
// title/body. A cross-reference alone (including a comment-only reference or
// a source with missing text) must not promote an issue to Reviewing/Closed.
func prClosesIssue(iss *gh.Issue, repo string, number int) bool {
	_, ok := github.FindClosingKeywordReference(iss.GetTitle()+"\n"+iss.GetBody(), repo, repo, number)
	return ok
}

// prReferencesIssue reports whether the PR's title or body names the issue
// with a bare "#N" mention (any keyword or none). Missing body/title (the API
// omits them on some responses) is treated as a match; this helper does not
// prove closing intent for dashboard state.
func prReferencesIssue(iss *gh.Issue, number int) bool {
	needle := fmt.Sprintf("#%d", number)
	body := iss.GetBody()
	title := iss.GetTitle()
	if body == "" && title == "" {
		return true
	}
	hay := title + "\n" + body
	for _, tok := range strings.Fields(hay) {
		// Trim surrounding punctuation so "#123." / "#123," / "(#123)" still match.
		tok = strings.TrimRight(strings.TrimLeft(tok, "([<"), ".,);:]>")
		if tok == needle {
			return true
		}
	}
	return false
}

// contributePRLinkResolver lazily builds the per-Server resolver, wired to the
// hive's existing authenticated GitHub client. Guarded by a sync.Once so the
// zero-value Server needs no constructor change (matching the lazy
// contributeMetricsStore accessor). A missing GHClient yields a resolver whose
// lookups degrade to nil, so the triage/queue views still render "no linked PR".
func (s *Server) contributePRLinkResolver() *prLinkResolver {
	s.contributePRLinkOnce.Do(func() {
		var gc *gh.Client
		if s.deps != nil && s.deps.GHClient != nil {
			gc = s.deps.GHClient.GoGitHub()
		}
		s.contributePRLink = newPRLinkResolver(gc)
	})
	return s.contributePRLink
}
