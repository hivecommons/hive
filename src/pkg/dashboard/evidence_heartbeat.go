package dashboard

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/review"
)

// evidenceHeartbeatWindow is the look-back of the heartbeat evidence block.
const evidenceHeartbeatWindow = 90 * 24 * time.Hour

// evidenceHeartbeatRefreshInterval bounds how often the bundle tree under
// ghpkg.ReviewEvidenceRoot is re-walked for the heartbeat. The block is a
// 90-day aggregate, so a few minutes of lag is invisible on the hub; the
// walk itself is what must never run on the beat path (see
// evidenceHeartbeatCache).
const evidenceHeartbeatRefreshInterval = 5 * time.Minute

// evidenceHeartbeatCache holds the bundle-derived part of the heartbeat
// evidence block and refreshes it on its own goroutine.
//
// WHY: the heartbeat collect has a 10 s budget. loadEvidenceBundles walks
// and parses every bundle under the evidence root; on the hosted hive that
// is ~22k directory entries and ~11.6k JSON files on NFSv3 /data, measured
// at ~6 s for the walk alone and ~27 s to read every file — on EVERY beat,
// because nothing cached it. The collect therefore never finished inside
// its budget, the last-good cache never filled, and the spoke sent the
// identity-only MINIMAL beat forever: the hub showed a healthy hive with
// no stats. Reading a cached aggregate here costs microseconds; the walk
// runs at most once per evidenceHeartbeatRefreshInterval, off the beat.
type evidenceHeartbeatCache struct {
	mu          sync.Mutex
	block       *spoke.HeartbeatEvidence
	refreshedAt time.Time
	refreshing  bool
	// done is closed when the most recently scheduled refresh completes; nil
	// before the first. Tests wait on it instead of sleeping.
	done chan struct{}
}

// snapshot returns the cached block (nil until the first refresh lands) and
// starts a background refresh when the cache is stale and none is running.
func (c *evidenceHeartbeatCache) snapshot(now time.Time, refresh func()) *spoke.HeartbeatEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	stale := c.refreshedAt.IsZero() || now.Sub(c.refreshedAt) >= evidenceHeartbeatRefreshInterval
	if stale && !c.refreshing {
		c.refreshing = true
		c.done = make(chan struct{})
		go refresh()
	}
	return c.block
}

// store records the outcome of a refresh. A nil block (unreadable bundles)
// keeps the previous block so the hub keeps the summary it already stored
// instead of seeing a false zero; refreshedAt advances either way so a
// persistently failing root is retried once per interval, not every beat.
func (c *evidenceHeartbeatCache) store(block *spoke.HeartbeatEvidence, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if block != nil {
		c.block = block
	}
	c.refreshedAt = now
	if c.refreshing {
		c.refreshing = false
		close(c.done)
	}
}

// refreshDone returns the channel closed when the most recently scheduled
// refresh finishes (already closed if it has), or nil before any refresh.
func (c *evidenceHeartbeatCache) refreshDone() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

// isRefreshing reports whether a refresh is currently running.
func (c *evidenceHeartbeatCache) isRefreshing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshing
}

// EvidenceHeartbeat builds the evidence block of the hub heartbeat (#12114):
// a 90-day summary of the review-evidence bundles behind
// GET /api/evidence/summary, the self-authored auto-merge posture, and the
// 24 h / 7 d hive state trends (#12339). It is read-only and never walks the
// bundle tree itself: the aggregate comes from evidenceHeartbeatCache, which
// refreshes in the background. It returns nil until the first refresh has
// landed (and on a nil server), so the hub keeps the summary it already
// stored instead of seeing a false zero. The cheap, in-memory or single-file
// fields (auto-merge posture, write counters, hive-state trends) are read live.
func (s *Server) EvidenceHeartbeat() *spoke.HeartbeatEvidence {
	if s == nil {
		return nil
	}
	// Capture the root on the caller's goroutine: tests may restore or replace
	// the package default as soon as this beat returns.
	root := ghpkg.ReviewEvidenceRoot
	cached := s.evidenceHB.snapshot(time.Now(), func() { s.refreshEvidenceHeartbeat(root) })
	if cached == nil {
		return nil
	}
	out := *cached
	if s.deps != nil && s.deps.Config != nil {
		cfg := s.deps.Config
		v := cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel)
		out.AutomergeSelfAuthored = &v
	}
	out.WriteCounters = heartbeatWriteCounters(loadEvidenceWriteCounters(writeCountersPath))
	out.HiveState = s.hiveStateTrends()
	return &out
}

// refreshEvidenceHeartbeat is the background walk behind EvidenceHeartbeat:
// load every bundle, fold, aggregate, store. Runs on its own goroutine.
func (s *Server) refreshEvidenceHeartbeat(root string) {
	started := time.Now()
	var (
		block   *spoke.HeartbeatEvidence
		bundles []*evidence.Bundle
	)
	verifier, err := s.reviewEvidenceVerifier()
	if err == nil {
		bundles, _, err = loadEvidenceBundles(root, verifier)
		if err == nil {
			block = buildHeartbeatEvidence(bundles, time.Now().UTC(), nil)
		}
	}
	if err != nil && s.logger != nil {
		s.logger.Warn("heartbeat evidence refresh: bundles unverifiable; keeping previous summary", "error", err)
	}
	took := time.Since(started)
	if took > evidenceHeartbeatRefreshInterval && s.logger != nil {
		s.logger.Warn("heartbeat evidence refresh outlived its interval", "took", took.Round(time.Millisecond).String(), "bundles", len(bundles))
	}
	s.evidenceHB.store(block, time.Now())
}

// heartbeatWriteCounters converts the proxy's per-tier totals (#12069) into
// the heartbeat shape; nil when the counter file is absent, so the hub shows
// "not reported" rather than zero attempts.
func heartbeatWriteCounters(totals map[string]EvidenceWriteCount) map[string]spoke.HeartbeatEvidenceWriteCount {
	if len(totals) == 0 {
		return nil
	}
	out := make(map[string]spoke.HeartbeatEvidenceWriteCount, len(totals))
	for tier, c := range totals {
		out[tier] = spoke.HeartbeatEvidenceWriteCount{Attempted: int(c.Attempted), Blocked: int(c.Blocked)}
	}
	return out
}

// hiveStateTrends is the 24 h / 7 d summary of the hive state time series
// (#12339); nil when no series is configured or no sample is recent.
func (s *Server) hiveStateTrends() *hivestate.Trends {
	if s == nil || s.deps == nil {
		return nil
	}
	return s.deps.Nous.HiveState().Trends()
}

// buildHeartbeatEvidence aggregates the PRs opened within
// evidenceHeartbeatWindow of now, plus the same counts for each shorter
// window in spoke.HeartbeatEvidenceWindowKeys. Bundles are folded per PR by
// evidence.FoldPRs and windowed on PR open time, exactly as
// buildEvidenceSummary does, so a PR with a live and a backfilled bundle is
// one PR with the live verdicts and the forge outcome.
func buildHeartbeatEvidence(bundles []*evidence.Bundle, now time.Time, selfMerge *bool) *spoke.HeartbeatEvidence {
	prs := evidence.FoldPRs(bundles, "", "")
	out := buildHeartbeatEvidenceWindow(prs, now, evidenceHeartbeatWindow)
	out.AutomergeSelfAuthored = selfMerge
	out.Windows = make(map[string]*spoke.HeartbeatEvidence, len(spoke.HeartbeatEvidenceWindowKeys))
	for key, hours := range spoke.HeartbeatEvidenceWindowKeys {
		out.Windows[key] = buildHeartbeatEvidenceWindow(prs, now, time.Duration(hours)*time.Hour)
	}
	return out
}

// buildHeartbeatEvidenceWindow is buildHeartbeatEvidence for one trailing
// window over already-folded PRs; it leaves the lifetime fields (auto-merge
// posture, write counters, Windows) empty.
func buildHeartbeatEvidenceWindow(prs map[string]*evidence.PR, now time.Time, window time.Duration) *spoke.HeartbeatEvidence {
	out := &spoke.HeartbeatEvidence{ByAuthorKind: map[string]spoke.HeartbeatEvidenceBucket{
		evidence.AuthorHuman: {}, evidence.AuthorAgent: {}, evidence.AuthorBot: {},
	}}
	cutoff := now.Add(-window)

	reviewers := map[string]bool{}
	authors := map[string]bool{}
	outcomes := map[string]spoke.HeartbeatEvidenceOutcome{}
	var shadow spoke.HeartbeatEvidenceShadow
	shadowSeen := false
	var mergedPRs []*evidence.Bundle
	for _, b := range prs {
		if b.OpenedAt.Before(cutoff) {
			continue
		}
		merged := b.Merge != nil
		failed := merged && (len(b.RevertedBy) > 0 || len(b.FollowedUpBy) > 0)
		if merged {
			out.Merged90d++
			mergedPRs = append(mergedPRs, &evidence.Bundle{Merge: b.Merge})
			if counts, ok := out.ByAuthorKind[b.Author.Kind]; ok {
				counts.N++
				// Follow-ups are not reverts. Backlinks are captured by the
				// evidence linker within its 14-day post-merge window.
				if len(b.RevertedBy) > 0 {
					counts.Bad++
				}
				out.ByAuthorKind[b.Author.Kind] = counts
			}
		}
		if failed {
			out.Reverted14d90d++
		}
		switch b.Author.Kind {
		case evidence.AuthorHuman, evidence.AuthorAgent, evidence.AuthorBot:
			outcomes[b.Author.Kind] = addHeartbeatOutcome(outcomes[b.Author.Kind], merged, failed)
		}
		if sh := b.Shadow; sh != nil {
			if sh.ReviewGate != nil && sh.ReviewGate.WouldHaveHeld != nil {
				shadowSeen = true
				addHeartbeatShadow(&shadow.ReviewGate, *sh.ReviewGate.WouldHaveHeld, merged, failed)
			}
			if sh.Intent != nil && sh.Intent.WouldHaveBlocked != nil {
				shadowSeen = true
				addHeartbeatShadow(&shadow.Intent, *sh.Intent.WouldHaveBlocked, merged, failed)
			}
		}
		if f := knownModelFamily(b.Author.Model); f != "" {
			authors[f] = true
		}
		for _, v := range b.Verdicts {
			out.Verdicts90d++
			if f := knownModelFamily(v.Model); f != "" {
				reviewers[f] = true
			}
			bad := false
			switch v.Verdict {
			case string(review.VerdictApprove):
				if failed {
					out.FalseApprove90d++
					bad = true
				}
			case string(review.VerdictChangesRequested), string(review.VerdictReject):
				if merged {
					out.FalseBlock90d++
					bad = true
				}
			}
			i := evidenceCalibrationBucket(v.Confidence)
			out.Calibration[i].N++
			if bad {
				out.Calibration[i].Bad++
			}
		}
	}
	out.ReviewerFamilies = sortedEvidenceFamilies(reviewers)
	out.AuthorFamilies = sortedEvidenceFamilies(authors)
	if len(outcomes) > 0 {
		out.OutcomesByAuthorKind = outcomes
	}
	if shadowSeen {
		out.ShadowGate = &shadow
	}
	out.TimeToMerge = heartbeatTimeToMerge(mergedPRs)
	return out
}

// heartbeatTimeToMerge is the median time-to-merge of the folded merged PRs
// (each carries its PR's merge event, which holds opened_at when captured).
// PRs without opened_at are skipped by evidence.MedianTimeToMerge, never
// counted as zero; nil when none qualify.
func heartbeatTimeToMerge(merged []*evidence.Bundle) *spoke.HeartbeatEvidenceTimeToMerge {
	median, n := evidence.MedianTimeToMerge(merged)
	if n == 0 {
		return nil
	}
	return &spoke.HeartbeatEvidenceTimeToMerge{MedianSeconds: int64((median + time.Second/2) / time.Second), N: n}
}

func addHeartbeatOutcome(o spoke.HeartbeatEvidenceOutcome, merged, failed bool) spoke.HeartbeatEvidenceOutcome {
	o.N++
	if merged {
		o.Merged++
	}
	if failed {
		o.Reverted++
	}
	return o
}

func addHeartbeatShadow(g *spoke.HeartbeatEvidenceShadowGate, stopped, merged, failed bool) {
	if stopped {
		g.Held = addHeartbeatOutcome(g.Held, merged, failed)
	} else {
		g.Passed = addHeartbeatOutcome(g.Passed, merged, failed)
	}
}

// evidenceCalibrationBucket maps a confidence in [0,1] to bucket 0–5.
func evidenceCalibrationBucket(c float64) int {
	n := spoke.HeartbeatEvidenceCalibrationBuckets
	if !(c > 0) {
		return 0
	}
	i := int(c * float64(n))
	if i >= n {
		return n - 1
	}
	return i
}

func knownModelFamily(model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}
	f := ghpkg.ModelFamily(model)
	if f == "unknown" {
		return ""
	}
	return f
}

func sortedEvidenceFamilies(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
