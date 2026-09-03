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

const (
	stableReleaseBranch        = "v4"
	commitBehindCompareTimeout = 10 * time.Second
	commitBehindCacheMax       = 1024
)

type commitBehindKey struct {
	base string
	head string
}

type commitBehindValue struct {
	count int
	known bool
}

var (
	commitBehindMu       sync.Mutex
	commitBehindCache    = map[commitBehindKey]commitBehindValue{}
	commitBehindInFlight = map[commitBehindKey]bool{}
)

var fetchCommitBehindCount = func(base, head string, logger *slog.Logger) (count int, known bool, err error) {
	client := &http.Client{Timeout: commitBehindCompareTimeout}
	compareURL := fmt.Sprintf("%s/repos/hivecommons/hive/compare/%s...%s",
		githubAPIBase, url.PathEscape(base), url.PathEscape(head))
	req, err := http.NewRequest(http.MethodGet, compareURL, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("compare %s...%s: HTTP %d", base, head, resp.StatusCode)
	}
	var result struct {
		AheadBy int `json:"ahead_by"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, false, err
	}
	return result.AheadBy, true, nil
}

func commitsBehindStableV4(base string, logger *slog.Logger) (int, bool) {
	head := getLatestSHAForBranch(stableReleaseBranch)
	if sameCommit(base, head) {
		return 0, true
	}
	base = shortSHA(base)
	head = shortSHA(head)
	if base == "" || head == "" {
		return 0, false
	}
	key := commitBehindKey{base: base, head: head}

	commitBehindMu.Lock()
	if v, ok := commitBehindCache[key]; ok {
		commitBehindMu.Unlock()
		return v.count, v.known
	}
	if commitBehindInFlight[key] {
		commitBehindMu.Unlock()
		return 0, false
	}
	commitBehindInFlight[key] = true
	fetch := fetchCommitBehindCount
	commitBehindMu.Unlock()

	go resolveCommitBehind(key, fetch, logger)
	return 0, false
}

func resolveCommitBehind(key commitBehindKey, fetch func(base, head string, logger *slog.Logger) (int, bool, error), logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	count, known, err := fetch(key.base, key.head, logger)

	commitBehindMu.Lock()
	defer commitBehindMu.Unlock()
	delete(commitBehindInFlight, key)
	if err != nil {
		logger.Debug("commit-behind resolve failed (will retry on a later request)",
			"base", key.base, "head", key.head, "error", err)
		return
	}
	if len(commitBehindCache) >= commitBehindCacheMax {
		commitBehindCache = map[commitBehindKey]commitBehindValue{}
	}
	commitBehindCache[key] = commitBehindValue{count: count, known: known}
}
