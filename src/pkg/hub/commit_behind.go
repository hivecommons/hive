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

// Keep this client's transport independent of the process-global default.
// Some tests temporarily replace http.DefaultTransport; a background
// commit-behind lookup may outlive the request that started it, so allowing
// Client.Do to resolve a nil Transport through that mutable global races with
// those tests.
var commitBehindHTTPClient = &http.Client{
	Transport: http.DefaultTransport.(*http.Transport).Clone(),
	Timeout:   commitBehindCompareTimeout,
}

var fetchCommitBehindCount = func(base, head string, logger *slog.Logger) (count int, known bool, err error) {
	compareURL := fmt.Sprintf("%s/repos/hivecommons/hive/compare/%s...%s",
		githubAPIBase, url.PathEscape(base), url.PathEscape(head))
	req, err := http.NewRequest(http.MethodGet, compareURL, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := commitBehindHTTPClient.Do(req)
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

// fallbackReleaseLine is the branch assumed when neither the :stable channel
// nor any tracked v<N> line resolves (cold cache, GHCR unreachable). It is the
// line :stable was promoted from at #7721 Phase 1 and exists only so a hub
// that cannot identify anything still resolves SOME branch rather than "";
// every runtime path prefers stableReleaseLine().
const fallbackReleaseLine = "v5"

// stableReleaseLine returns the branch the :stable release channel currently
// resolves to (#7721 Phase 1 moved it from v4 to v5; hard-coding a line here
// re-creates the "152 behind" bug at every rollover). Falls back to the
// highest-numbered tracked v<N> line, then fallbackReleaseLine.
//
// Resolution may consult GHCR (through getChannelTargets' 5-minute cache), so
// request handlers resolve it ONCE per request and pass the line down rather
// than calling this per hive.
func stableReleaseLine(logger *slog.Logger) string {
	return resolveStableReleaseLine(logger)
}

// resolveStableReleaseLine is the swappable implementation behind
// stableReleaseLine; tests pin it to avoid registry lookups.
var resolveStableReleaseLine = func(logger *slog.Logger) string {
	shas := getDisplaySHAs()
	if line := activeReleaseLine(getChannelTargets(shas, logger), shas); line != "" {
		return line
	}
	return fallbackReleaseLine
}

// commitsBehindStableLine counts how far base sits behind the BRANCH tip of
// line (the stable release line, resolved once by the caller). That is the right number for a spoke on a branch tag and
// the wrong one for a spoke on a release channel — see commitsBehindTarget
// and behindTargetFor. Kept for the tooltip's secondary "and the channel
// itself is N behind the tip" note. The JSON field it feeds keeps its
// historical commitsBehindStableV4 name so mixed hub/asset versions agree.
func commitsBehindStableLine(base, line string, logger *slog.Logger) (int, bool) {
	return commitsBehindTarget(base, getLatestSHAForBranch(line), logger)
}

// commitsBehindTarget counts how far base sits behind head, resolving through
// the GitHub compare API in the background and caching by (base, head). The
// first call for a pair reports unknown and dispatches the compare; a later
// call returns the cached answer. A base that already IS head short-circuits
// to 0 with no dispatch.
func commitsBehindTarget(base, head string, logger *slog.Logger) (int, bool) {
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
