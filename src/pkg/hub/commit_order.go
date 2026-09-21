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

// commitOrderCacheMax bounds the resolved-ancestry cache. Entries are tiny and
// ancestry never changes, but an unbounded map keyed by attacker-influencable
// SHAs (spokes report GitHash) must not grow forever. On overflow the whole
// cache is dropped: ancestry is re-resolvable on demand, so eviction costs one
// API call per pair still in use — simpler and safer than an LRU here.
const commitOrderCacheMax = 512

// commitCompareTimeout bounds one GitHub compare API round-trip. Resolution
// runs in a background goroutine, so this is a resource bound, not a latency
// bound on any heartbeat.
const commitCompareTimeout = 10 * time.Second

// commitOrderKey identifies one immutable ancestry question:
// "is `reported` at or ahead of `target`?" Both are canonical short SHAs.
type commitOrderKey struct {
	target   string
	reported string
}

var (
	commitOrderMu sync.Mutex
	// commitOrderCache maps a resolved pair to its answer (true = reported is
	// the same commit as, or a descendant of, target). Only definitive API
	// answers are cached; errors are not, so a transient failure retries on
	// the next call instead of poisoning the pair forever.
	commitOrderCache = map[commitOrderKey]bool{}
	// commitOrderInFlight dedupes background resolves so a hive heartbeating
	// every couple of minutes cannot stack up parallel identical API calls.
	commitOrderInFlight = map[commitOrderKey]bool{}
)

// fetchCommitCompareStatus asks GitHub how `head` relates to `base` and
// returns the compare status: "identical", "ahead", "behind" or "diverged"
// ("ahead" means head is a strict descendant of base). A var so tests can
// stub the network round-trip; stub and restore it only while holding
// commitOrderMu (commitAtOrAheadOfTarget captures it under that mutex).
var fetchCommitCompareStatus = func(base, head string, logger *slog.Logger) (string, error) {
	client := &http.Client{Timeout: commitCompareTimeout}
	compareURL := fmt.Sprintf("%s/repos/hivecommons/hive/compare/%s...%s",
		githubAPIBase, url.PathEscape(base), url.PathEscape(head))
	req, err := http.NewRequest(http.MethodGet, compareURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authGitHubRequest(req)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("compare %s...%s: HTTP %d", base, head, resp.StatusCode)
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Status == "" {
		return "", fmt.Errorf("compare %s...%s: empty status", base, head)
	}
	return result.Status, nil
}

// commitAtOrAheadOfTarget reports whether `reported` (a spoke's GitHash) is
// the same commit as `target` or a descendant of it — i.e. the target was
// reached or surpassed. It is safe to call while holding s.mu: it answers
// from the cache (plus the trivial same-commit case) and NEVER performs I/O
// inline. An unresolved pair returns false and starts one deduplicated
// background resolve, so the caller's next pass gets the real answer.
//
// Empty inputs are false: an unknown state must never read as "complete".
// logger may be nil.
func commitAtOrAheadOfTarget(reported, target string, logger *slog.Logger) bool {
	if sameCommit(reported, target) {
		return true
	}
	reported = shortSHA(reported)
	target = shortSHA(target)
	if reported == "" || target == "" {
		return false
	}
	key := commitOrderKey{target: target, reported: reported}

	commitOrderMu.Lock()
	if atOrAhead, ok := commitOrderCache[key]; ok {
		commitOrderMu.Unlock()
		return atOrAhead
	}
	if commitOrderInFlight[key] {
		commitOrderMu.Unlock()
		return false
	}
	commitOrderInFlight[key] = true
	// Capture the fetcher under the same mutex tests use to stub it, so a
	// background resolve can never race a reassignment of the package var.
	fetch := fetchCommitCompareStatus
	commitOrderMu.Unlock()

	go resolveCommitOrder(key, fetch, logger)
	return false
}

// resolveCommitOrder performs the one compare API call for key and stores the
// definitive answer. Errors are logged and NOT cached, so the pair is retried
// on a later commitAtOrAheadOfTarget call.
func resolveCommitOrder(key commitOrderKey, fetch func(base, head string, logger *slog.Logger) (string, error), logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	status, err := fetch(key.target, key.reported, logger)

	commitOrderMu.Lock()
	defer commitOrderMu.Unlock()
	delete(commitOrderInFlight, key)
	if err != nil {
		logger.Debug("commit-order resolve failed (will retry on a later beat)",
			"target", key.target, "reported", key.reported, "error", err)
		return
	}
	if len(commitOrderCache) >= commitOrderCacheMax {
		// See commitOrderCacheMax: reset rather than LRU — ancestry is
		// immutable and cheap to re-resolve for pairs still in use.
		commitOrderCache = map[commitOrderKey]bool{}
	}
	atOrAhead := status == "ahead" || status == "identical"
	commitOrderCache[key] = atOrAhead
	logger.Info("commit-order resolved",
		"target", key.target, "reported", key.reported,
		"status", status, "at_or_ahead", atOrAhead)
}
