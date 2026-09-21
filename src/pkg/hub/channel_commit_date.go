package hub

import (
	"encoding/json"
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

var (
	commitDateMu    sync.RWMutex
	commitDateBySHA = map[string]time.Time{}
)

// fetchCommitDate reads one commit's committer date from GitHub. A var so
// tests can supply dates without a network round-trip.
//
// The committer date, not the author date: for a squash-merge (the repo's
// merge mode) the committer date is when the commit landed on the branch,
// which is what "how long has stable been behind candidate" asks.
var fetchCommitDate = func(sha string, logger *slog.Logger) (time.Time, error) {
	client := &http.Client{Timeout: channelResolveTimeout}
	commitURL := fmt.Sprintf("%s/repos/hivecommons/hive/commits/%s", githubAPIBase, url.PathEscape(sha))
	req, err := http.NewRequest(http.MethodGet, commitURL, nil)
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
		return time.Time{}, fmt.Errorf("commit %s: HTTP %d", sha, resp.StatusCode)
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
	commitDateMu.RUnlock()

	d, err := fetchCommitDate(sha, logger)
	if err != nil {
		if logger != nil {
			logger.Warn("channel commit date: fetch failed — the row will render without a timestamp",
				"sha", sha, "error", err)
		}
		return time.Time{}
	}

	commitDateMu.Lock()
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
