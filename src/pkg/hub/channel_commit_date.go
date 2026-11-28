package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// commitDateCacheMax bounds the SHA→commit-date cache. A commit's date is
// immutable, so entries never expire; the ceiling only stops unbounded growth
// across a long-lived hub. Dropped wholesale on overflow, like the distance
// cache next door: re-deriving an entry is one cheap GET.
const commitDateCacheMax = 512

// commitDateBackoff is how long a SHA whose commit-date fetch failed (e.g.
// HTTP 403 from an exhausted rate limit) is left alone before it is fetched
// again. Every channel render would otherwise re-hit GitHub for it, spending
// the budget that caused the failure; the row renders without a timestamp
// meanwhile.
//
// A 403/429 is a client-wide condition (rate limit or a token without access),
// not a per-SHA failure, so it also parks EVERY commit-date fetch until
// commitDateGlobalBackoffUntil (the reset hint when GitHub sends one, capped
// at an hour). Otherwise each other channel SHA would spend more of the
// exhausted budget and log its own warning.
const commitDateBackoff = 10 * time.Minute

var (
	commitDateMu                 sync.RWMutex
	commitDateBySHA              = map[string]time.Time{}
	commitDateBackoffUntil       = map[string]time.Time{}
	commitDateGlobalBackoffUntil time.Time
)

// commitDateHTTPError is a non-200 reply to a commit-date fetch. Wait is how
// long to park all commit-date fetches when Status is a 403/429.
type commitDateHTTPError struct {
	SHA    string
	Status int
	Wait   time.Duration
}

func (e *commitDateHTTPError) Error() string {
	return fmt.Sprintf("commit %s: HTTP %d", e.SHA, e.Status)
}

// throttled reports whether the reply is a client-wide 403/429.
func (e *commitDateHTTPError) throttled() bool {
	return e.Status == http.StatusForbidden || e.Status == http.StatusTooManyRequests
}

// fetchCommitDate reads one commit's committer date from GitHub. A var so
// tests can supply dates without a network round-trip.
//
// The committer date, not the author date: for a squash-merge (the repo's
// merge mode) the committer date is when the commit landed on the branch,
// which is what "how long has stable been behind candidate" asks.
var fetchCommitDate = func(sha string, logger *slog.Logger) (time.Time, error) {
	client := hubGitHubHTTPClient()
	ctx, cancel := context.WithTimeout(hubGitHubCallerContext(context.Background(), "channel_commit_date"), channelResolveTimeout)
	defer cancel()
	commitURL := fmt.Sprintf("%s/repos/hivecommons/hive/commits/%s", githubAPIBase, url.PathEscape(sha))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, commitURL, nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authGitHubRequest(req)
	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, &commitDateHTTPError{
			SHA:    sha,
			Status: resp.StatusCode,
			Wait:   commitMsgThrottleWait(resp.Header, githubWarnNow()),
		}
	}
	var result struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return time.Time{}, err
	}
	if result.Commit.Committer.Date.IsZero() {
		return time.Time{}, fmt.Errorf("commit %s: no committer date", sha)
	}
	return result.Commit.Committer.Date, nil
}

// commitDateForSHA returns when sha landed, serving the permanent cache when
// it can and otherwise fetching once. The zero time means unknown, and the
// caller must render nothing rather than a fabricated timestamp.
func commitDateForSHA(sha string, logger *slog.Logger) time.Time {
	if sha == "" {
		return time.Time{}
	}
	key := shortSHA(sha)

	commitDateMu.RLock()
	if d, ok := commitDateBySHA[key]; ok {
		commitDateMu.RUnlock()
		return d
	}
	until, backedOff := commitDateBackoffUntil[key]
	globalUntil := commitDateGlobalBackoffUntil
	commitDateMu.RUnlock()
	now := githubWarnNow()
	if now.Before(globalUntil) || (backedOff && now.Before(until)) {
		return time.Time{}
	}

	d, err := fetchCommitDate(sha, logger)
	if err != nil {
		var httpErr *commitDateHTTPError
		throttled := errors.As(err, &httpErr) && httpErr.throttled()
		commitDateMu.Lock()
		if len(commitDateBackoffUntil) >= commitDateCacheMax {
			commitDateBackoffUntil = map[string]time.Time{}
		}
		commitDateBackoffUntil[key] = githubWarnNow().Add(commitDateBackoff)
		var wait time.Duration
		if throttled {
			wait = commitDateGlobalBackoffWait(httpErr)
			commitDateGlobalBackoffUntil = githubWarnNow().Add(wait)
		}
		commitDateMu.Unlock()
		if throttled {
			// One shared key: the throttle is client-wide, so N channel SHAs
			// must not produce N warnings.
			warnGitHubFetchOnce(logger, "commit-date:global", "channel commit date: fetch throttled, pausing all commit-date fetches; rows render without a timestamp",
				"sha", sha, "error", err, "retry_in", wait.String())
			return time.Time{}
		}
		if logger != nil {
			warnGitHubFetchOnce(logger, "commit-date:"+key, "channel commit date: fetch failed — the row will render without a timestamp",
				"sha", sha, "error", err)
		}
		return time.Time{}
	}

	commitDateMu.Lock()
	delete(commitDateBackoffUntil, key)
	if len(commitDateBySHA) >= commitDateCacheMax {
		commitDateBySHA = map[string]time.Time{}
	}
	commitDateBySHA[key] = d
	commitDateMu.Unlock()
	return d
}

// annotateChannelCommitDates stamps each resolved target with its commit's
// date, in place. Targets with no SHA are skipped: there is nothing to date.
func annotateChannelCommitDates(targets []ChannelTarget, logger *slog.Logger) {
	for i := range targets {
		if targets[i].SHA == "" {
			continue
		}
		if d := commitDateForSHA(targets[i].SHA, logger); !d.IsZero() {
			targets[i].CommittedAt = d.UTC().Format(time.RFC3339)
		}
	}
}

// commitDateGlobalBackoffWait is how long a throttled reply parks every
// commit-date fetch: its reset hint, else commitDateBackoff, capped at
// commitMsgGlobalBackoffMax.
func commitDateGlobalBackoffWait(e *commitDateHTTPError) time.Duration {
	wait := e.Wait
	if wait <= 0 {
		wait = commitDateBackoff
	}
	if wait > commitMsgGlobalBackoffMax {
		wait = commitMsgGlobalBackoffMax
	}
	return wait
}
