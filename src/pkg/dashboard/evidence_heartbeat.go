package dashboard

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/review"
)

// evidenceHeartbeatWindow is the look-back of the heartbeat evidence block.
const evidenceHeartbeatWindow = 90 * 24 * time.Hour

// EvidenceHeartbeat builds the evidence block of the hub heartbeat (#12114):
// a 90-day summary of the review-evidence bundles behind
// GET /api/evidence/summary plus the self-authored auto-merge posture. It is
// read-only. It returns nil when the bundles cannot be read, so the hub keeps
// the summary it already stored instead of seeing a false zero.
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
	return buildHeartbeatEvidence(bundles, time.Now().UTC(), selfMerge)
}

// buildHeartbeatEvidence aggregates the newest bundle per PR created within
// evidenceHeartbeatWindow of now. Same-PR dedup matches buildEvidenceSummary.
func buildHeartbeatEvidence(bundles []*evidence.Bundle, now time.Time, selfMerge *bool) *spoke.HeartbeatEvidence {
	out := &spoke.HeartbeatEvidence{AutomergeSelfAuthored: selfMerge}
	cutoff := now.Add(-evidenceHeartbeatWindow)

	latest := map[string]*evidence.Bundle{}
	for _, b := range bundles {
		if b == nil || b.CreatedAt.Before(cutoff) {
			continue
		}
		key := strings.ToLower(b.Repo) + "#" + strconv.Itoa(b.Number)
		if cur, ok := latest[key]; !ok || b.CreatedAt.After(cur.CreatedAt) {
			latest[key] = b
		}
	}

	reviewers := map[string]bool{}
	authors := map[string]bool{}
	for _, b := range latest {
		merged := b.Merge != nil
		failed := merged && (len(b.RevertedBy) > 0 || len(b.FollowedUpBy) > 0)
		if merged {
			out.Merged90d++
		}
		if failed {
			out.Reverted14d90d++
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
	return out
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
