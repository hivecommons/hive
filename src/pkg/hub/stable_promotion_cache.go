package hub

import (
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// ttlFlight is a small keyed cache with single-flight refresh: a fresh entry
// (younger than ttl) is served without calling fetch; otherwise exactly one
// caller per key runs fetch while concurrent callers for the same key wait for
// and share its answer. A failed fetch (ok=false) never overwrites the cache, so
// the next call retries, and the previous answer is served while it is younger
// than grace — the same contract channelRevisionSHA follows.
//
// The stable-promotion status is served on a public endpoint and polled by the
// dashboard, so without this every request would fan out to GitHub and GHCR.
type ttlFlight[V any] struct {
	mu       sync.Mutex
	entries  map[string]ttlFlightEntry[V]
	inflight map[string]*ttlFlightCall[V]
}

type ttlFlightEntry[V any] struct {
	v  V
	at time.Time
}

type ttlFlightCall[V any] struct {
	done chan struct{}
	v    V
}

func (c *ttlFlight[V]) get(key string, ttl, grace time.Duration, fetch func() (V, bool)) V {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]ttlFlightEntry[V]{}
		c.inflight = map[string]*ttlFlightCall[V]{}
	}
	prev, had := c.entries[key]
	if had && time.Since(prev.at) < ttl {
		c.mu.Unlock()
		return prev.v
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.v
	}
	call := &ttlFlightCall[V]{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	v, ok := fetch()

	c.mu.Lock()
	if c.entries == nil { // reset() ran while fetch was in flight
		c.entries = map[string]ttlFlightEntry[V]{}
		c.inflight = map[string]*ttlFlightCall[V]{}
	}
	if ok {
		c.entries[key] = ttlFlightEntry[V]{v: v, at: time.Now()}
	} else if had && time.Since(prev.at) < grace {
		v = prev.v
	}
	call.v = v
	delete(c.inflight, key)
	c.mu.Unlock()
	close(call.done)
	return v
}

func (c *ttlFlight[V]) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.inflight = nil
}

// stablePromotionMaxVerifiedRuns caps how many docker.yml runs one
// stable-promotion evaluation verifies against GHCR (each verification is a
// digest lookup plus a generation lookup — several registry round-trips). The
// soaked runs are tried newest-first and the unsoaked ones soonest-eligible
// first, stopping at the first verified build, so the cap only matters when
// many consecutive runs fail verification.
const stablePromotionMaxVerifiedRuns = 8

var (
	ghcrTagGenerationCache    ttlFlight[int]
	stablePromotionRunsCache  ttlFlight[[]stablePromotionWorkflowRun]
	stablePromotionBuildCache ttlFlight[stablePromotionVerifiedBuild]
)

// stablePromotionVerifiedBuild is the GHCR verification outcome for one
// docker.yml run: ok reports that the run's short-SHA tag exists and carries
// the run's generation label.
type stablePromotionVerifiedBuild struct {
	digest string
	ok     bool
}

// cachedGHCRTagGeneration is ghcrTagGeneration's production implementation:
// fetchGHCRTagGeneration cached per (repo, tag) for channelDigestTTL. 0 means
// "unknown" and is never cached.
func cachedGHCRTagGeneration(repo, tag string, logger *slog.Logger) int {
	return ghcrTagGenerationCache.get(repo+":"+tag, channelDigestTTL, channelRevisionStaleGrace, func() (int, bool) {
		gen := fetchGHCRTagGeneration(repo, tag, logger)
		return gen, gen > 0
	})
}

// stablePromotionRuns returns the recent docker.yml runs, cached for
// channelDigestTTL. A failed fetch (nil) is not cached.
func stablePromotionRuns(logger *slog.Logger) []stablePromotionWorkflowRun {
	return stablePromotionRunsCache.get("runs", channelDigestTTL, channelRevisionStaleGrace, func() ([]stablePromotionWorkflowRun, bool) {
		runs := stablePromotionFetchRuns(logger)
		return runs, runs != nil
	})
}

// stablePromotionVerifyBuild checks that the image tagged with short is the
// one run produced, cached per (run, sha) for channelDigestTTL. A negative
// answer is cached too, so a run whose image never landed cannot make every
// request repeat the registry lookups.
func stablePromotionVerifyBuild(runNumber int, short string, logger *slog.Logger) stablePromotionVerifiedBuild {
	key := strconv.Itoa(runNumber) + ":" + short
	return stablePromotionBuildCache.get(key, channelDigestTTL, 0, func() (stablePromotionVerifiedBuild, bool) {
		digest := ghcrTagDigest(ghcrRepoSpoke, short, logger)
		if digest == "" || ghcrTagGeneration(ghcrRepoSpoke, short, logger) != runNumber {
			return stablePromotionVerifiedBuild{}, true
		}
		return stablePromotionVerifiedBuild{digest: digest, ok: true}, true
	})
}

func resetStablePromotionCaches() {
	ghcrTagGenerationCache.reset()
	stablePromotionRunsCache.reset()
	stablePromotionBuildCache.reset()
}
