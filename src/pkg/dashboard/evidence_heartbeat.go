package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/review"
)

// evidenceHeartbeatWindow is the look-back of the heartbeat evidence block.
const evidenceHeartbeatWindow = 90 * 24 * time.Hour

// EvidenceHeartbeat builds the evidence block of the hub heartbeat (#12114):
// a 90-day summary of the review-evidence bundles behind
// GET /api/evidence/summary, the self-authored auto-merge posture, and the
// 24 h / 7 d hive state trends (#12339). It is read-only. It returns nil when
// the bundles cannot be read, so the hub keeps the summary it already stored
// instead of seeing a false zero.
func (s *Server) EvidenceHeartbeat() *spoke.HeartbeatEvidence {
	bundles, _, err := loadEvidenceBundles(ghpkg.ReviewEvidenceRoot)
	if err != nil {
		return nil
	}
	var selfMerge *bool
	if s != nil && s.deps != nil && s.deps.Config != nil {
		cfg := s.deps.Config
		v := cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel)
		selfMerge = &v
	}
	out := buildHeartbeatEvidence(bundles, time.Now().UTC(), selfMerge)
	out.WriteCounters = heartbeatWriteCounters(loadEvidenceWriteCounters(writeCountersPath))
	out.HiveState = s.hiveStateTrends()
	return out
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
