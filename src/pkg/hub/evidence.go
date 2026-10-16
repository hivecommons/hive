package hub

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
)

const (
	evidencePath = "/api/hub/evidence"

	// minEvidenceN is the smallest sample a rate is shown for; below it the
	// hub reports "unknown" (null) rather than a rate a single PR can swing.
	minEvidenceN = 10

	maxEvidenceCount       = 10_000_000
	maxEvidenceFamilies    = 16
	maxEvidenceFamilyRunes = 64

	// EvidenceCalibrationBuckets is the number of confidence buckets (0–5).
	EvidenceCalibrationBuckets = 6
)

// EvidenceBucket is one calibration bucket: n verdicts, bad of which had a bad
// outcome.
type EvidenceBucket struct {
	N   int `json:"n"`
	Bad int `json:"bad"`
}

// HeartbeatEvidence is a spoke's evidence-pipeline summary (#12063). nil means
// the beat carried none (old spoke) and the stored summary is kept.
type HeartbeatEvidence struct {
	Verdicts90d           int                                        `json:"verdicts_90d"`
	FalseApprove90d       int                                        `json:"false_approve_90d"`
	FalseBlock90d         int                                        `json:"false_block_90d"`
	Merged90d             int                                        `json:"merged_90d"`
	Reverted14d90d        int                                        `json:"reverted_14d_90d"`
	AutomergeSelfAuthored *bool                                      `json:"automerge_self_authored,omitempty"`
	ReviewerFamilies      []string                                   `json:"reviewer_families,omitempty"`
	AuthorFamilies        []string                                   `json:"author_families,omitempty"`
	Calibration           [EvidenceCalibrationBuckets]EvidenceBucket `json:"calibration"`
}

func sanitizeEvidenceFamilies(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range in {
		f = truncateReachRunes(strings.TrimSpace(sanitizeField(f)), maxEvidenceFamilyRunes)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) == maxEvidenceFamilies {
			break
		}
	}
	return out
}

// sanitizeEvidence returns a bounded, internally consistent copy of a
// spoke-reported evidence summary, never the raw payload.
func sanitizeEvidence(in *HeartbeatEvidence) *HeartbeatEvidence {
	if in == nil {
		return nil
	}
	c := func(v int) int { return clampInt(v, 0, maxEvidenceCount) }
	out := &HeartbeatEvidence{
		Verdicts90d:           c(in.Verdicts90d),
		Merged90d:             c(in.Merged90d),
		AutomergeSelfAuthored: in.AutomergeSelfAuthored,
		ReviewerFamilies:      sanitizeEvidenceFamilies(in.ReviewerFamilies),
		AuthorFamilies:        sanitizeEvidenceFamilies(in.AuthorFamilies),
	}
	// A subset can never exceed its total.
	out.FalseApprove90d = clampInt(in.FalseApprove90d, 0, out.Verdicts90d)
	out.FalseBlock90d = clampInt(in.FalseBlock90d, 0, out.Verdicts90d)
	out.Reverted14d90d = clampInt(in.Reverted14d90d, 0, out.Merged90d)
	for i, b := range in.Calibration {
		n := c(b.N)
		out.Calibration[i] = EvidenceBucket{N: n, Bad: clampInt(b.Bad, 0, n)}
	}
	return out
}

// evidenceRate returns num/den, or false when den is below minEvidenceN.
func evidenceRate(num, den int) (float64, bool) {
	if den < minEvidenceN || den <= 0 {
		return 0, false
	}
	return float64(num) / float64(den), true
}

// wilson returns the 95% Wilson score interval for k successes in n trials.
func wilson(k, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 0
	}
	const z = 1.96
	nf, p := float64(n), float64(k)/float64(n)
	d := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / d
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / d
	return math.Max(0, centre-half), math.Min(1, centre+half)
}

// EvidenceRate is a rate with its sample size and Wilson interval. Rate is nil
// ("—") when N is below minEvidenceN.
type EvidenceRate struct {
	N    int      `json:"n"`
	Rate *float64 `json:"rate"`
	Lo   *float64 `json:"lo,omitempty"`
	Hi   *float64 `json:"hi,omitempty"`
}

func newEvidenceRate(num, den int) EvidenceRate {
	out := EvidenceRate{N: den}
	if r, ok := evidenceRate(num, den); ok {
		lo, hi := wilson(num, den)
		out.Rate, out.Lo, out.Hi = &r, &lo, &hi
	}
	return out
}

// Reviewer/author family mixes of a hive (#12116). The heartbeat carries the
// family sets, not a per-verdict pairing, so a hive is classed as a whole:
// same when every reviewer family is also an author family, cross when the
// sets are disjoint, mixed otherwise, and unknown when either set is empty.
const (
	evidenceFamilySame    = "same"
	evidenceFamilyCross   = "cross"
	evidenceFamilyMixed   = "mixed"
	evidenceFamilyUnknown = "unknown"
)

func evidenceFamilyMix(reviewers, authors []string) string {
	if len(reviewers) == 0 || len(authors) == 0 {
		return evidenceFamilyUnknown
	}
	author := map[string]bool{}
	for _, a := range authors {
		author[a] = true
	}
	shared := 0
	for _, r := range reviewers {
		if author[r] {
			shared++
		}
	}
	switch shared {
	case len(reviewers):
		return evidenceFamilySame
	case 0:
		return evidenceFamilyCross
	}
	return evidenceFamilyMixed
}

// EvidenceHive is one hive's row in the evidence view.
type EvidenceHive struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name,omitempty"`
	Online                bool           `json:"online"`
	Reported              bool           `json:"reported"`
	ACMMLevel             int            `json:"acmm_level"`
	PRsMerged90d          *int           `json:"prs_merged_90d,omitempty"`
	RecordCoverage        EvidenceRate   `json:"record_coverage"`
	FamilyMix             string         `json:"family_mix,omitempty"`
	Verdicts90d           int            `json:"verdicts_90d"`
	Merged90d             int            `json:"merged_90d"`
	FalseApproveRate      EvidenceRate   `json:"false_approve_rate"`
	FalseBlockRate        EvidenceRate   `json:"false_block_rate"`
	RevertRate            EvidenceRate   `json:"revert_rate"`
	AutomergeSelfAuthored *bool          `json:"automerge_self_authored,omitempty"`
	ReviewerFamilies      []string       `json:"reviewer_families,omitempty"`
	AuthorFamilies        []string       `json:"author_families,omitempty"`
	Calibration           []EvidenceRate `json:"calibration"`
}

// EvidenceCalibrationBucket is one pooled fleet calibration bucket.
type EvidenceCalibrationBucket struct {
	Bucket int `json:"bucket"`
	EvidenceRate
}

// EvidenceFamilyFalseApprove is the pooled false-approve rate per family mix.
type EvidenceFamilyFalseApprove struct {
	Same  EvidenceRate `json:"same"`
	Cross EvidenceRate `json:"cross"`
	Mixed EvidenceRate `json:"mixed"`
}

// EvidenceACMMLevel is the pooled evidence of the reporting hives at one ACMM
// level.
type EvidenceACMMLevel struct {
	Level      int          `json:"level"`
	Hives      int          `json:"hives"`
	Merged90d  int          `json:"merged_90d"`
	RevertRate EvidenceRate `json:"revert_rate"`
}

// EvidenceView is the GET /api/hub/evidence body.
type EvidenceView struct {
	Spokes                 int                         `json:"spokes"`
	Reporting              int                         `json:"reporting"`
	Calibration            []EvidenceCalibrationBucket `json:"calibration"`
	ByACMMLevel            []EvidenceACMMLevel         `json:"by_acmm_level"`
	FamilyFalseApproveRate EvidenceFamilyFalseApprove  `json:"family_false_approve_rate"`
	RecordCoverage         EvidenceRate                `json:"record_coverage"`
	Hives                  []EvidenceHive              `json:"hives"`
}

// buildEvidenceView pools the evidence summaries of every spoke. Spokes that
// never sent one are listed with reported=false so missing data is visible.
func buildEvidenceView(hives []RegistryEntry) EvidenceView {
	out := EvidenceView{Hives: []EvidenceHive{}}
	var pooled [EvidenceCalibrationBuckets]EvidenceBucket
	family := map[string]*EvidenceBucket{
		evidenceFamilySame: {}, evidenceFamilyCross: {}, evidenceFamilyMixed: {},
	}
	type acmmPool struct{ hives, merged, reverted int }
	acmm := map[int]*acmmPool{}
	var covered, mergedTotal int
	for _, h := range hives {
		out.Spokes++
		row := EvidenceHive{ID: h.ID, Name: h.Name, Online: h.Online, ACMMLevel: h.ACMMLevel,
			PRsMerged90d: h.PRsMerged90d, Calibration: []EvidenceRate{}}
		if e := h.Evidence; e != nil {
			out.Reporting++
			row.Reported = true
			if h.PRsMerged90d != nil {
				// Both counts are 90-day merged PRs; the record count comes from
				// retained bundles so it is capped at the GitHub count.
				recorded := clampInt(e.Merged90d, 0, *h.PRsMerged90d)
				row.RecordCoverage = newEvidenceRate(recorded, *h.PRsMerged90d)
				covered += recorded
				mergedTotal += *h.PRsMerged90d
			}
			row.FamilyMix = evidenceFamilyMix(e.ReviewerFamilies, e.AuthorFamilies)
			if b := family[row.FamilyMix]; b != nil {
				b.N += e.Verdicts90d
				b.Bad += e.FalseApprove90d
			}
			a := acmm[h.ACMMLevel]
			if a == nil {
				a = &acmmPool{}
				acmm[h.ACMMLevel] = a
			}
			a.hives++
			a.merged += e.Merged90d
			a.reverted += e.Reverted14d90d
			row.Verdicts90d, row.Merged90d = e.Verdicts90d, e.Merged90d
			row.FalseApproveRate = newEvidenceRate(e.FalseApprove90d, e.Verdicts90d)
			row.FalseBlockRate = newEvidenceRate(e.FalseBlock90d, e.Verdicts90d)
			row.RevertRate = newEvidenceRate(e.Reverted14d90d, e.Merged90d)
			row.AutomergeSelfAuthored = e.AutomergeSelfAuthored
			row.ReviewerFamilies, row.AuthorFamilies = e.ReviewerFamilies, e.AuthorFamilies
			for i, b := range e.Calibration {
				row.Calibration = append(row.Calibration, newEvidenceRate(b.Bad, b.N))
				pooled[i].N += b.N
				pooled[i].Bad += b.Bad
			}
		}
		out.Hives = append(out.Hives, row)
	}
	out.Calibration = make([]EvidenceCalibrationBucket, 0, EvidenceCalibrationBuckets)
	for i, b := range pooled {
		out.Calibration = append(out.Calibration, EvidenceCalibrationBucket{Bucket: i, EvidenceRate: newEvidenceRate(b.Bad, b.N)})
	}
	out.ByACMMLevel = make([]EvidenceACMMLevel, 0, len(acmm))
	for level, a := range acmm {
		out.ByACMMLevel = append(out.ByACMMLevel, EvidenceACMMLevel{Level: level, Hives: a.hives,
			Merged90d: a.merged, RevertRate: newEvidenceRate(a.reverted, a.merged)})
	}
	sort.Slice(out.ByACMMLevel, func(i, j int) bool { return out.ByACMMLevel[i].Level < out.ByACMMLevel[j].Level })
	out.FamilyFalseApproveRate = EvidenceFamilyFalseApprove{
		Same:  newEvidenceRate(family[evidenceFamilySame].Bad, family[evidenceFamilySame].N),
		Cross: newEvidenceRate(family[evidenceFamilyCross].Bad, family[evidenceFamilyCross].N),
		Mixed: newEvidenceRate(family[evidenceFamilyMixed].Bad, family[evidenceFamilyMixed].N),
	}
	out.RecordCoverage = newEvidenceRate(covered, mergedTotal)
	sort.Slice(out.Hives, func(i, j int) bool { return out.Hives[i].ID < out.Hives[j].ID })
	return out
}

// handleEvidence serves GET /api/hub/evidence (admin only): the read-only
// cross-hive evidence drill-down behind the Fleet quadrant card.
func (s *HubServer) handleEvidence(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	view := buildEvidenceView(s.registry.Hives)
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}
