package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Last-good per-repo enumeration (hivecommons/hive, 2026-10-21 incident).
//
// EnumerateActionable walks every governed repo and only fails the whole
// pass when EVERY repo fails. A pass where one repo's listing is refused by
// GitHub — secondary rate limit, 429, 5xx, or go-github's pre-emptive
// "not making remote request" refusal — therefore returned a result that
// silently omitted that repo: the dashboard published TOTAL OPEN ISSUES 0
// and the activity monitor dropped from ~20 rows to 1 while the real repo
// had 19 open issues, and Overview KPI history recorded the dip as real.
//
// A transient refusal says nothing about the repo's work. When a repo's
// fetch fails transiently, the previous successful enumeration of THAT repo
// is substituted and the repo is listed in ActionableResult.StaleRepos so
// consumers (overviewKPIIncomplete, the activity monitor) can mark the
// snapshot stale instead of recording it. The all-repos-failed guard is
// unchanged: when nothing fresh was fetched the caller keeps its prior
// state exactly as before.

// lastGoodEnumerationMaxAge bounds how long a repo's last-good enumeration
// may be substituted for a failed fetch. Past this the repo is omitted as it
// was before, so a repo whose listing is permanently refused cannot show
// hours-old work as current forever.
const lastGoodEnumerationMaxAge = 2 * time.Hour

// repoEnumeration is one repo's share of an EnumerateActionable pass.
type repoEnumeration struct {
	issues      []Issue
	held        []HoldItem
	prs         []PullRequest
	heldPRs     []PullRequest
	staleDrafts []PullRequest
	attributed  []PullRequest
	counts      RepoCounts
	breakdown   RepoWorkBreakdown
	details     RepoWorkDetails
	fetchedAt   time.Time
}

// lastGoodEnumeration remembers the newest successful per-repo enumeration.
// Zero value is ready to use.
type lastGoodEnumeration struct {
	mu     sync.Mutex
	byRepo map[string]repoEnumeration
	maxAge time.Duration
	now    func() time.Time
}

func (l *lastGoodEnumeration) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *lastGoodEnumeration) store(repo string, enum repoEnumeration) {
	if l == nil {
		return
	}
	enum.fetchedAt = l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byRepo == nil {
		l.byRepo = map[string]repoEnumeration{}
	}
	l.byRepo[strings.ToLower(repo)] = enum
}

// get returns the last-good enumeration for repo when one exists and is
// younger than the max age.
func (l *lastGoodEnumeration) get(repo string) (repoEnumeration, bool) {
	if l == nil {
		return repoEnumeration{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	enum, ok := l.byRepo[strings.ToLower(repo)]
	if !ok {
		return repoEnumeration{}, false
	}
	maxAge := l.maxAge
	if maxAge <= 0 {
		maxAge = lastGoodEnumerationMaxAge
	}
	if l.clock().Sub(enum.fetchedAt) > maxAge {
		return repoEnumeration{}, false
	}
	return enum, true
}

// isTransientEnumerationError reports whether a repo fetch failed for a
// reason that says nothing about the repo's work: a rate-limit or abuse
// refusal (including go-github's pre-emptive client-side refusal), 429,
// 5xx, a transport error or a deadline. A permission 403, 404 or 401 is
// NOT transient — those describe the repo and must keep failing as before.
func isTransientEnumerationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if isRetryableGitHubError(err) {
		return true
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusForbidden {
		return strings.Contains(strings.ToLower(ghErr.Message), "rate limit")
	}
	return false
}

// RateLimitCautionEngaged reports whether the process-wide slow-start
// caution window (slowstart.go) is active: GitHub has recently refused a
// request for a primary or secondary rate limit and every request is being
// paced. Optional housekeeping that can serve a cached answer should stand
// down while this is true rather than queue for a slot.
func RateLimitCautionEngaged() bool {
	return sharedSlowStartState.cautious(time.Now())
}
