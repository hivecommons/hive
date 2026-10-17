package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
)

const (
	evidencePath = "/api/hub/evidence"

	// minEvidenceN is the smallest sample a rate is shown for; below it the
	// hub reports "unknown" (null) rather than a rate a single PR can swing.
	minEvidenceN = 10

	maxEvidenceCount       = 10_000_000
	maxEvidenceFamilies    = 16
	maxEvidenceFamilyRunes = 64
	maxEvidenceWriteTiers  = 16
	maxEvidenceTierRunes   = 32

	// maxEvidenceTimeToMergeSeconds bounds a reported median time-to-merge
	// (ten years); anything larger is clamped.
	maxEvidenceTimeToMergeSeconds = 10 * 365 * 24 * 3600

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
// the beat carried none (old spoke) and the stored summary is kept. HiveState
// is the spoke's 24 h / 7 d hive state trends (#12339).
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

	// Fleet-panel aggregates (#12370); nil/absent on old spokes, which the
	// view reports as "not reported" rather than zero. The JSON shape matches
	// spoke.HeartbeatEvidence.
	OutcomesByAuthorKind map[string]EvidenceOutcome    `json:"outcomes_by_author_kind,omitempty"`
	WriteCounters        map[string]EvidenceWriteCount `json:"write_counters,omitempty"`
	ShadowGate           *EvidenceShadow               `json:"shadow_gate,omitempty"`

	// TimeToMerge is the hive's median time-to-merge over the merged PRs that
	// recorded merge.opened_at (#12464), matching
	// spoke.HeartbeatEvidence.TimeToMerge. nil on old spokes and when no
	// merged PR recorded an open time: "not reported", never a zero median.
	TimeToMerge *EvidenceTimeToMerge `json:"time_to_merge,omitempty"`

	// Windows holds the same counts over trailing 24h / 7d / 30d windows
	// (keys of evidenceWindowHours), matching spoke.HeartbeatEvidence.Windows.
	// Absent on old spokes; the hub then reports the hive as "not reported
	// for this window" rather than reusing the 90-day block.
	Windows map[string]*HeartbeatEvidence `json:"windows,omitempty"`

	// HiveState is the spoke's 24 h / 7 d hive-state trends (#12339).
	HiveState *hivestate.Trends `json:"hive_state,omitempty"`
}

// evidenceWindowHours maps a selectable window key to its hours. The empty
// key is the 90-day block every spoke reports.
var evidenceWindowHours = map[string]int{"24h": 24, "7d": 7 * 24, "30d": 30 * 24}

// evidenceDefaultWindowHours is the spoke heartbeat look-back (90 days).
const evidenceDefaultWindowHours = 90 * 24

// evidenceMaxRangeDays caps a custom ?from=&to= range, as on the spoke.
const evidenceMaxRangeDays = 366

// EvidenceOutcome counts PRs: N total, Merged merged, Reverted (a subset of
// Merged) reverted or followed up within 14 days; N-Merged are open.
type EvidenceOutcome struct {
	N        int `json:"n"`
	Merged   int `json:"merged"`
	Reverted int `json:"reverted"`
}

func (o EvidenceOutcome) add(x EvidenceOutcome) EvidenceOutcome {
	return EvidenceOutcome{N: o.N + x.N, Merged: o.Merged + x.Merged, Reverted: o.Reverted + x.Reverted}
}

// EvidenceTimeToMerge is one hive's median time-to-merge in whole seconds
// over N merged PRs with a recorded open time.
type EvidenceTimeToMerge struct {
	MedianSeconds int64 `json:"median_s"`
	N             int   `json:"n"`
}

// sanitizeEvidenceTimeToMerge bounds a reported median: the sample is a
// subset of the window's merged PRs, and an empty sample is "not reported".
func sanitizeEvidenceTimeToMerge(in *EvidenceTimeToMerge, merged int) *EvidenceTimeToMerge {
	if in == nil {
		return nil
	}
	n := clampInt(in.N, 0, merged)
	if n == 0 {
		return nil
	}
	return &EvidenceTimeToMerge{MedianSeconds: clampInt64(in.MedianSeconds, 0, maxEvidenceTimeToMergeSeconds), N: n}
}

// EvidenceLevelTimeToMerge is the time-to-merge of one ACMM level. Medians
// cannot be averaged or summed, and the heartbeat carries one median per
// hive rather than a distribution, so the level reports the median of the
// per-hive medians (MedianOfHiveMediansSeconds; with an even number of hives
// the mean of the two middle ones, rounded down to the second). Every
// reporting hive weighs the same however many PRs it merged; N is the total
// merged PRs with an open time behind those medians, Hives how many hives
// reported one.
type EvidenceLevelTimeToMerge struct {
	Hives                      int   `json:"hives"`
	N                          int   `json:"n"`
	MedianOfHiveMediansSeconds int64 `json:"median_of_hive_medians_s"`
}

// newEvidenceLevelTimeToMerge pools per-hive medians; nil when no hive
// reported one.
func newEvidenceLevelTimeToMerge(hives []EvidenceTimeToMerge) *EvidenceLevelTimeToMerge {
	if len(hives) == 0 {
		return nil
	}
	medians := make([]int64, 0, len(hives))
	out := &EvidenceLevelTimeToMerge{Hives: len(hives)}
	for _, h := range hives {
		medians = append(medians, h.MedianSeconds)
		out.N += h.N
	}
	sort.Slice(medians, func(i, j int) bool { return medians[i] < medians[j] })
	mid := len(medians) / 2
	if len(medians)%2 == 1 {
		out.MedianOfHiveMediansSeconds = medians[mid]
	} else {
		out.MedianOfHiveMediansSeconds = medians[mid-1] + (medians[mid]-medians[mid-1])/2
	}
	return out
}

// EvidenceWriteCount is one intent tier's attempted and proxy-blocked agent
// writes since the spoke's proxy started.
type EvidenceWriteCount struct {
	Attempted int `json:"attempted"`
	Blocked   int `json:"blocked"`
}

// EvidenceShadow is a spoke's shadow-gate block: the shadow review gate and
// the shadow intent verification, each split by what it would have done.
type EvidenceShadow struct {
	ReviewGate EvidenceShadowGate `json:"review_gate"`
	Intent     EvidenceShadowGate `json:"intent"`
}

// EvidenceShadowGate splits the PRs one shadow gate evaluated into the ones
// it would have stopped (Held) and let through (Passed).
type EvidenceShadowGate struct {
	Held   EvidenceOutcome `json:"held"`
	Passed EvidenceOutcome `json:"passed"`
}

func (g EvidenceShadowGate) add(x EvidenceShadowGate) EvidenceShadowGate {
	return EvidenceShadowGate{Held: g.Held.add(x.Held), Passed: g.Passed.add(x.Passed)}
}

// evidenceAuthorKinds is the fixed key set of OutcomesByAuthorKind; any other
// key from a spoke is dropped.
var evidenceAuthorKinds = []string{"human", "agent", "bot"}

func sanitizeEvidenceOutcome(o EvidenceOutcome) EvidenceOutcome {
	n := clampInt(o.N, 0, maxEvidenceCount)
	merged := clampInt(o.Merged, 0, n)
	return EvidenceOutcome{N: n, Merged: merged, Reverted: clampInt(o.Reverted, 0, merged)}
}

func sanitizeEvidenceOutcomes(in map[string]EvidenceOutcome) map[string]EvidenceOutcome {
	if in == nil {
		return nil
	}
	out := map[string]EvidenceOutcome{}
	for _, k := range evidenceAuthorKinds {
		if o, ok := in[k]; ok {
			out[k] = sanitizeEvidenceOutcome(o)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeEvidenceWriteCounters(in map[string]EvidenceWriteCount) map[string]EvidenceWriteCount {
	if in == nil {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]EvidenceWriteCount{}
	for _, k := range keys {
		// A tier is an ACMM mode name; anything outside the safe name
		// alphabet is dropped rather than escaped.
		tier := truncateReachRunes(strings.TrimSpace(k), maxEvidenceTierRunes)
		if !safeNamePattern.MatchString(tier) {
			continue
		}
		if _, dup := out[tier]; dup {
			continue
		}
		c := in[k]
		attempted := clampInt(c.Attempted, 0, maxEvidenceCount)
		out[tier] = EvidenceWriteCount{Attempted: attempted, Blocked: clampInt(c.Blocked, 0, attempted)}
		if len(out) == maxEvidenceWriteTiers {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeEvidenceShadow(in *EvidenceShadow) *EvidenceShadow {
	if in == nil {
		return nil
	}
	gate := func(g EvidenceShadowGate) EvidenceShadowGate {
		return EvidenceShadowGate{Held: sanitizeEvidenceOutcome(g.Held), Passed: sanitizeEvidenceOutcome(g.Passed)}
	}
	return &EvidenceShadow{ReviewGate: gate(in.ReviewGate), Intent: gate(in.Intent)}
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
	out.OutcomesByAuthorKind = sanitizeEvidenceOutcomes(in.OutcomesByAuthorKind)
	out.WriteCounters = sanitizeEvidenceWriteCounters(in.WriteCounters)
	out.ShadowGate = sanitizeEvidenceShadow(in.ShadowGate)
	out.TimeToMerge = sanitizeEvidenceTimeToMerge(in.TimeToMerge, out.Merged90d)
	for key := range evidenceWindowHours {
		w := in.Windows[key]
		if w == nil {
			continue
		}
		// One level only: a window never carries windows or lifetime fields.
		w.Windows, w.AutomergeSelfAuthored, w.WriteCounters = nil, nil, nil
		if out.Windows == nil {
			out.Windows = map[string]*HeartbeatEvidence{}
		}
		out.Windows[key] = sanitizeEvidence(w)
	}
	out.HiveState = sanitizeHiveState(in.HiveState)
	return out
}

// maxHiveStatePerDay bounds the per-day rates a spoke can report.
const maxHiveStatePerDay = 1e12

// sanitizeHiveState returns a bounded copy of a spoke's hive state trends:
// counts and per-day rates non-negative, ratios in [0,1], coverage within
// the window, p90 never below the median.
func sanitizeHiveState(in *hivestate.Trends) *hivestate.Trends {
	if in == nil {
		return nil
	}
	return &hivestate.Trends{
		Last24h: sanitizeHiveStateWindow(in.Last24h, 24),
		Last7d:  sanitizeHiveStateWindow(in.Last7d, 7*24),
	}
}

func sanitizeHiveStateWindow(w hivestate.Window, hours float64) hivestate.Window {
	c := func(v int) int { return clampInt(v, 0, maxEvidenceCount) }
	q := func(x hivestate.Quantiles) hivestate.Quantiles {
		m := c(x.Median)
		return hivestate.Quantiles{Median: m, P90: clampInt(x.P90, m, maxEvidenceCount)}
	}
	return hivestate.Window{
		Samples:          c(w.Samples),
		CoverageHours:    clampEvidenceFloat(w.CoverageHours, hours),
		QueueIssues:      q(w.QueueIssues),
		QueuePRs:         q(w.QueuePRs),
		QueueHold:        q(w.QueueHold),
		SLAViolationRate: clampEvidenceFloat(w.SLAViolationRate, 1),
		AgentSamples:     c(w.AgentSamples),
		AgentIdleRatio:   clampEvidenceFloat(w.AgentIdleRatio, 1),
		KicksPerDay:      clampEvidenceFloat(w.KicksPerDay, maxHiveStatePerDay),
		TokensPerDay:     clampEvidenceFloat(w.TokensPerDay, maxHiveStatePerDay),
	}
}

// clampEvidenceFloat clamps v to [0, hi]; NaN becomes 0.
func clampEvidenceFloat(v, hi float64) float64 {
	if !(v > 0) {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
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
	ID             string       `json:"id"`
	Name           string       `json:"name,omitempty"`
	Online         bool         `json:"online"`
	Reported       bool         `json:"reported"`
	ACMMLevel      int          `json:"acmm_level"`
	PRsMerged90d   *int         `json:"prs_merged_90d,omitempty"`
	RecordCoverage EvidenceRate `json:"record_coverage"`
	FamilyMix      string       `json:"family_mix,omitempty"`
	// WindowUnreported is true when the hive reports evidence but not the
	// selected shorter window (its spoke predates heartbeat windows).
	WindowUnreported      bool           `json:"window_unreported,omitempty"`
	Verdicts90d           int            `json:"verdicts_90d"`
	Merged90d             int            `json:"merged_90d"`
	FalseApproveRate      EvidenceRate   `json:"false_approve_rate"`
	FalseBlockRate        EvidenceRate   `json:"false_block_rate"`
	RevertRate            EvidenceRate   `json:"revert_rate"`
	AutomergeSelfAuthored *bool          `json:"automerge_self_authored,omitempty"`
	ReviewerFamilies      []string       `json:"reviewer_families,omitempty"`
	AuthorFamilies        []string       `json:"author_families,omitempty"`
	Calibration           []EvidenceRate `json:"calibration"`
	// Counts carries the raw sanitized counts behind the rates above (and the
	// fleet-panel aggregates) so the drill-down can re-pool a hive / ACMM
	// level selection client-side (#12370). nil when the hive never reported.
	Counts *EvidenceHiveCounts `json:"counts,omitempty"`
}

// EvidenceHiveCounts is the raw per-hive evidence behind EvidenceHive's rates.
// Recorded90d is the hive's merged-with-record count capped at prs_merged_90d
// (nil when GitHub reported no merged count).
type EvidenceHiveCounts struct {
	Verdicts90d          int                           `json:"verdicts_90d"`
	FalseApprove90d      int                           `json:"false_approve_90d"`
	FalseBlock90d        int                           `json:"false_block_90d"`
	Merged90d            int                           `json:"merged_90d"`
	Reverted14d90d       int                           `json:"reverted_14d_90d"`
	Recorded90d          *int                          `json:"recorded_90d,omitempty"`
	Calibration          []EvidenceBucket              `json:"calibration"`
	OutcomesByAuthorKind map[string]EvidenceOutcome    `json:"outcomes_by_author_kind,omitempty"`
	WriteCounters        map[string]EvidenceWriteCount `json:"write_counters,omitempty"`
	ShadowGate           *EvidenceShadow               `json:"shadow_gate,omitempty"`
	TimeToMerge          *EvidenceTimeToMerge          `json:"time_to_merge,omitempty"`
}

// EvidenceAuthorKindOutcome is the pooled outcome of one author kind: Hives
// is the reporting hives, MergeRate merged/n, RevertRate reverted/merged.
type EvidenceAuthorKindOutcome struct {
	Kind       string       `json:"kind"`
	Hives      int          `json:"hives"`
	N          int          `json:"n"`
	Merged     int          `json:"merged"`
	Reverted   int          `json:"reverted"`
	MergeRate  EvidenceRate `json:"merge_rate"`
	RevertRate EvidenceRate `json:"revert_rate"`
}

// EvidenceWriteTier is the pooled boundary enforcement of one intent tier.
type EvidenceWriteTier struct {
	Tier      string       `json:"tier"`
	Hives     int          `json:"hives"`
	Attempted int          `json:"attempted"`
	Blocked   int          `json:"blocked"`
	BlockRate EvidenceRate `json:"block_rate"`
}

// EvidenceShadowOutcomeRate is a pooled shadow-gate side with its revert rate
// (reverted over merged).
type EvidenceShadowOutcomeRate struct {
	EvidenceOutcome
	Open       int          `json:"open"`
	RevertRate EvidenceRate `json:"revert_rate"`
}

func newEvidenceShadowOutcomeRate(o EvidenceOutcome) EvidenceShadowOutcomeRate {
	return EvidenceShadowOutcomeRate{EvidenceOutcome: o, Open: o.N - o.Merged, RevertRate: newEvidenceRate(o.Reverted, o.Merged)}
}

// EvidenceShadowGateView is one pooled shadow gate.
type EvidenceShadowGateView struct {
	Hives  int                       `json:"hives"`
	N      int                       `json:"n"`
	Held   EvidenceShadowOutcomeRate `json:"held"`
	Passed EvidenceShadowOutcomeRate `json:"passed"`
}

func newEvidenceShadowGateView(hives int, g EvidenceShadowGate) EvidenceShadowGateView {
	return EvidenceShadowGateView{Hives: hives, N: g.Held.N + g.Passed.N,
		Held: newEvidenceShadowOutcomeRate(g.Held), Passed: newEvidenceShadowOutcomeRate(g.Passed)}
}

// EvidenceShadowView is the pooled shadow-gate panel. Hives is the number of
// hives that reported a shadow block at all.
type EvidenceShadowView struct {
	Hives      int                    `json:"hives"`
	ReviewGate EvidenceShadowGateView `json:"review_gate"`
	Intent     EvidenceShadowGateView `json:"intent"`
}

// evidencePool accumulates the fleet-panel aggregates of a set of hives.
type evidencePool struct {
	outcomes     map[string]*EvidenceAuthorKindOutcome
	writes       map[string]*EvidenceWriteTier
	outcomeHives int
	writeHives   int
	shadowHives  int
	reviewHives  int
	intentHives  int
	reviewGate   EvidenceShadowGate
	intentGate   EvidenceShadowGate
}

func newEvidencePool() *evidencePool {
	return &evidencePool{outcomes: map[string]*EvidenceAuthorKindOutcome{}, writes: map[string]*EvidenceWriteTier{}}
}

func (p *evidencePool) add(e *HeartbeatEvidence) {
	if len(e.OutcomesByAuthorKind) > 0 {
		p.outcomeHives++
		for kind, o := range e.OutcomesByAuthorKind {
			a := p.outcomes[kind]
			if a == nil {
				a = &EvidenceAuthorKindOutcome{Kind: kind}
				p.outcomes[kind] = a
			}
			a.Hives++
			a.N += o.N
			a.Merged += o.Merged
			a.Reverted += o.Reverted
		}
	}
	if len(e.WriteCounters) > 0 {
		p.writeHives++
		for tier, c := range e.WriteCounters {
			a := p.writes[tier]
			if a == nil {
				a = &EvidenceWriteTier{Tier: tier}
				p.writes[tier] = a
			}
			a.Hives++
			a.Attempted += c.Attempted
			a.Blocked += c.Blocked
		}
	}
	if sh := e.ShadowGate; sh != nil {
		p.shadowHives++
		if sh.ReviewGate.Held.N+sh.ReviewGate.Passed.N > 0 {
			p.reviewHives++
		}
		if sh.Intent.Held.N+sh.Intent.Passed.N > 0 {
			p.intentHives++
		}
		p.reviewGate = p.reviewGate.add(sh.ReviewGate)
		p.intentGate = p.intentGate.add(sh.Intent)
	}
}

func (p *evidencePool) outcomesView() []EvidenceAuthorKindOutcome {
	if p.outcomeHives == 0 {
		return nil
	}
	out := make([]EvidenceAuthorKindOutcome, 0, len(evidenceAuthorKinds))
	for _, kind := range evidenceAuthorKinds {
		a := p.outcomes[kind]
		if a == nil {
			a = &EvidenceAuthorKindOutcome{Kind: kind}
		}
		a.MergeRate = newEvidenceRate(a.Merged, a.N)
		a.RevertRate = newEvidenceRate(a.Reverted, a.Merged)
		out = append(out, *a)
	}
	return out
}

func (p *evidencePool) writesView() []EvidenceWriteTier {
	if p.writeHives == 0 {
		return nil
	}
	out := make([]EvidenceWriteTier, 0, len(p.writes))
	for _, a := range p.writes {
		a.BlockRate = newEvidenceRate(a.Blocked, a.Attempted)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tier < out[j].Tier })
	return out
}

func (p *evidencePool) shadowView() *EvidenceShadowView {
	if p.shadowHives == 0 {
		return nil
	}
	return &EvidenceShadowView{Hives: p.shadowHives,
		ReviewGate: newEvidenceShadowGateView(p.reviewHives, p.reviewGate),
		Intent:     newEvidenceShadowGateView(p.intentHives, p.intentGate)}
}

// EvidenceCalibrationBucket is one pooled fleet calibration bucket. Bad is
// the pooled bad-outcome count so the view layer can regroup the six
// heartbeat buckets into the spoke card's confidence ranges (#12456).
type EvidenceCalibrationBucket struct {
	Bucket int `json:"bucket"`
	Bad    int `json:"bad"`
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
	Level                int                         `json:"level"`
	Hives                int                         `json:"hives"`
	Merged90d            int                         `json:"merged_90d"`
	RevertRate           EvidenceRate                `json:"revert_rate"`
	OutcomesByAuthorKind []EvidenceAuthorKindOutcome `json:"outcomes_by_author_kind,omitempty"`
	WriteCounters        []EvidenceWriteTier         `json:"write_counters,omitempty"`
	ShadowGate           *EvidenceShadowView         `json:"shadow_gate,omitempty"`
	// TimeToMerge is the median of the level's per-hive median
	// time-to-merge (see EvidenceLevelTimeToMerge); omitted when no hive at
	// the level reports one.
	TimeToMerge *EvidenceLevelTimeToMerge `json:"time_to_merge,omitempty"`
}

// EvidenceView is the GET /api/hub/evidence body.
type EvidenceView struct {
	// Window is the trailing window the counts cover (RFC #12062).
	Window    EvidenceWindow `json:"window"`
	Spokes    int            `json:"spokes"`
	Reporting int            `json:"reporting"`
	// WindowUnreported counts hives that report the 90-day block but not
	// the selected shorter window (spoke predates windows).
	WindowUnreported int `json:"window_unreported"`
	// Findings are the plain-language sentences the view leads with
	// (#12456), generated from the numbers below by evidenceFindings and
	// reproduced client-side for a filtered selection.
	Findings               []string                    `json:"findings"`
	Calibration            []EvidenceCalibrationBucket `json:"calibration"`
	ByACMMLevel            []EvidenceACMMLevel         `json:"by_acmm_level"`
	FamilyFalseApproveRate EvidenceFamilyFalseApprove  `json:"family_false_approve_rate"`
	RecordCoverage         EvidenceRate                `json:"record_coverage"`
	// Fleet analogues of the spoke evidence panels (#12370): outcome by
	// author kind, boundary enforcement per tier, shadow gates × outcome.
	// Each is omitted when no spoke reports the block yet.
	OutcomesByAuthorKind []EvidenceAuthorKindOutcome `json:"outcomes_by_author_kind,omitempty"`
	WriteCounters        []EvidenceWriteTier         `json:"write_counters,omitempty"`
	ShadowGate           *EvidenceShadowView         `json:"shadow_gate,omitempty"`
	Hives                []EvidenceHive              `json:"hives"`
}

// EvidenceWindow describes the window a view covers. Hours is the trailing
// window actually used (24, 168, 720 or 2160); Key is its spoke heartbeat
// key ("" for the 90-day block). From/To echo a custom request; because
// spokes report fixed windows, a custom range is served from the smallest
// reported window that covers it and Approximate is then true. Coverage is
// false when record coverage cannot be computed (its denominator, GitHub's
// merged count, exists only for 90 days).
type EvidenceWindow struct {
	Hours       int    `json:"hours"`
	Key         string `json:"key,omitempty"`
	Label       string `json:"label"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	Approximate bool   `json:"approximate,omitempty"`
	Coverage    bool   `json:"coverage_available"`
}

func evidenceWindowFor(hours int) EvidenceWindow {
	for key, h := range evidenceWindowHours {
		if h == hours {
			return EvidenceWindow{Hours: hours, Key: key, Label: "last " + key}
		}
	}
	return EvidenceWindow{Hours: evidenceDefaultWindowHours, Label: "last 90d", Coverage: true}
}

// parseEvidenceTime accepts RFC3339 or YYYY-MM-DD (UTC midnight).
func parseEvidenceTime(raw string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// parseEvidenceWindow reads hours= (24, 168, 720; anything else or absent →
// 90 days) or from=/to= from the query. A custom range maps onto the
// smallest reported window that covers it, flagged Approximate; a date-only
// `to` is inclusive and a missing `to` means now.
func parseEvidenceWindow(q url.Values, now time.Time) (EvidenceWindow, error) {
	fromRaw, toRaw := strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to"))
	if fromRaw == "" && toRaw != "" {
		return EvidenceWindow{}, errors.New("to requires from")
	}
	if fromRaw != "" {
		from, ok := parseEvidenceTime(fromRaw)
		if !ok {
			return EvidenceWindow{}, errors.New("from must be RFC3339 or YYYY-MM-DD")
		}
		to := now
		if toRaw != "" {
			t, ok := parseEvidenceTime(toRaw)
			if !ok {
				return EvidenceWindow{}, errors.New("to must be RFC3339 or YYYY-MM-DD")
			}
			if t.Before(from) {
				return EvidenceWindow{}, errors.New("to must not be before from")
			}
			if len(toRaw) == len("2006-01-02") {
				t = t.Add(24 * time.Hour)
			}
			to = t
		}
		if to.Before(from) {
			return EvidenceWindow{}, errors.New("to must not be before from")
		}
		if to.Sub(from) > evidenceMaxRangeDays*24*time.Hour {
			return EvidenceWindow{}, fmt.Errorf("range must not exceed %d days", evidenceMaxRangeDays)
		}
		// The window must reach back to `from` from now, not just span it.
		need := int((now.Sub(from) + time.Hour - 1) / time.Hour)
		best := evidenceDefaultWindowHours
		for _, h := range evidenceWindowHours {
			if h >= need && h < best {
				best = h
			}
		}
		win := evidenceWindowFor(best)
		win.From, win.To, win.Approximate = from.Format(time.RFC3339), to.Format(time.RFC3339), true
		return win, nil
	}
	hours := evidenceDefaultWindowHours
	if raw := strings.TrimSpace(q.Get("hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return EvidenceWindow{}, errors.New("hours must be a non-negative integer")
		}
		if n > 0 {
			hours = n
		}
	}
	return evidenceWindowFor(hours), nil
}

// buildEvidenceView pools the 90-day evidence summaries of every spoke.
// Spokes that never sent one are listed with reported=false so missing data
// is visible.
func buildEvidenceView(hives []RegistryEntry) EvidenceView {
	return buildEvidenceViewWindow(hives, evidenceWindowFor(evidenceDefaultWindowHours))
}

// buildEvidenceViewWindow is buildEvidenceView over one trailing window. For
// a non-default window each hive's Windows[key] block is pooled instead of
// its 90-day block; hives without it count toward WindowUnreported.
func buildEvidenceViewWindow(hives []RegistryEntry, win EvidenceWindow) EvidenceView {
	out := EvidenceView{Window: win, Hives: []EvidenceHive{}}
	var pooled [EvidenceCalibrationBuckets]EvidenceBucket
	family := map[string]*EvidenceBucket{
		evidenceFamilySame: {}, evidenceFamilyCross: {}, evidenceFamilyMixed: {},
	}
	type acmmPool struct {
		hives, merged, reverted int
		panels                  *evidencePool
		timeToMerge             []EvidenceTimeToMerge
	}
	acmm := map[int]*acmmPool{}
	fleet := newEvidencePool()
	var covered, mergedTotal int
	for _, h := range hives {
		out.Spokes++
		row := EvidenceHive{ID: h.ID, Name: h.Name, Online: h.Online, ACMMLevel: h.ACMMLevel,
			PRsMerged90d: h.PRsMerged90d, Calibration: []EvidenceRate{}}
		e := h.Evidence
		if e != nil && win.Key != "" {
			if e = e.Windows[win.Key]; e == nil {
				out.WindowUnreported++
				row.WindowUnreported = true
			}
		}
		if e != nil {
			out.Reporting++
			row.Reported = true
			counts := &EvidenceHiveCounts{Verdicts90d: e.Verdicts90d, FalseApprove90d: e.FalseApprove90d,
				FalseBlock90d: e.FalseBlock90d, Merged90d: e.Merged90d, Reverted14d90d: e.Reverted14d90d,
				Calibration: e.Calibration[:], OutcomesByAuthorKind: e.OutcomesByAuthorKind,
				WriteCounters: e.WriteCounters, ShadowGate: e.ShadowGate, TimeToMerge: e.TimeToMerge}
			row.Counts = counts
			if h.PRsMerged90d != nil && win.Coverage {
				// Both counts are 90-day merged PRs; the record count comes from
				// retained bundles so it is capped at the GitHub count.
				recorded := clampInt(e.Merged90d, 0, *h.PRsMerged90d)
				counts.Recorded90d = &recorded
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
				a = &acmmPool{panels: newEvidencePool()}
				acmm[h.ACMMLevel] = a
			}
			a.hives++
			a.merged += e.Merged90d
			a.reverted += e.Reverted14d90d
			a.panels.add(e)
			if e.TimeToMerge != nil && e.TimeToMerge.N > 0 {
				a.timeToMerge = append(a.timeToMerge, *e.TimeToMerge)
			}
			fleet.add(e)
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
		out.Calibration = append(out.Calibration, EvidenceCalibrationBucket{Bucket: i, Bad: b.Bad, EvidenceRate: newEvidenceRate(b.Bad, b.N)})
	}
	out.ByACMMLevel = make([]EvidenceACMMLevel, 0, len(acmm))
	for level, a := range acmm {
		out.ByACMMLevel = append(out.ByACMMLevel, EvidenceACMMLevel{Level: level, Hives: a.hives,
			Merged90d: a.merged, RevertRate: newEvidenceRate(a.reverted, a.merged),
			OutcomesByAuthorKind: a.panels.outcomesView(), WriteCounters: a.panels.writesView(),
			ShadowGate: a.panels.shadowView(), TimeToMerge: newEvidenceLevelTimeToMerge(a.timeToMerge)})
	}
	out.OutcomesByAuthorKind = fleet.outcomesView()
	out.WriteCounters = fleet.writesView()
	out.ShadowGate = fleet.shadowView()
	sort.Slice(out.ByACMMLevel, func(i, j int) bool { return out.ByACMMLevel[i].Level < out.ByACMMLevel[j].Level })
	out.FamilyFalseApproveRate = EvidenceFamilyFalseApprove{
		Same:  newEvidenceRate(family[evidenceFamilySame].Bad, family[evidenceFamilySame].N),
		Cross: newEvidenceRate(family[evidenceFamilyCross].Bad, family[evidenceFamilyCross].N),
		Mixed: newEvidenceRate(family[evidenceFamilyMixed].Bad, family[evidenceFamilyMixed].N),
	}
	out.RecordCoverage = newEvidenceRate(covered, mergedTotal)
	sort.Slice(out.Hives, func(i, j int) bool { return out.Hives[i].ID < out.Hives[j].ID })
	out.Findings = evidenceFindings(out)
	return out
}

// filterEvidenceRegistry narrows the registry to the hives named by
// `hive=` (repeatable or comma-separated ids) and/or `level=` (ACMM level),
// the same selection the drill-down's chips make client-side. Unknown ids
// simply match nothing; an absent filter returns every hive.
func filterEvidenceRegistry(hives []RegistryEntry, q url.Values) []RegistryEntry {
	ids := map[string]bool{}
	for _, raw := range q["hive"] {
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids[id] = true
			}
		}
	}
	level, hasLevel := -1, false
	if raw := strings.TrimSpace(q.Get("level")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			level, hasLevel = n, true
		}
	}
	if len(ids) == 0 && !hasLevel {
		return hives
	}
	out := make([]RegistryEntry, 0, len(hives))
	for _, h := range hives {
		if len(ids) > 0 && !ids[h.ID] {
			continue
		}
		if hasLevel && h.ACMMLevel != level {
			continue
		}
		out = append(out, h)
	}
	return out
}

// handleEvidence serves GET /api/hub/evidence (admin only): the read-only
// cross-hive evidence drill-down behind the Fleet quadrant card.
func (s *HubServer) handleEvidence(w http.ResponseWriter, r *http.Request) {
	win, err := parseEvidenceWindow(r.URL.Query(), time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	view := buildEvidenceViewWindow(filterEvidenceRegistry(s.registry.Hives, r.URL.Query()), win)
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}
