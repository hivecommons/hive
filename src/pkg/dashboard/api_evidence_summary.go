package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/evidence/complexity"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

const evidenceSummaryModelUnknown = "unknown"

// evidenceSummaryBuckets are the confidence calibration buckets, lower bound
// inclusive; the last bucket also includes 1.0.
var evidenceSummaryBuckets = []struct {
	name string
	low  float64
}{{"0.0-0.5", 0}, {"0.5-0.7", 0.5}, {"0.7-0.9", 0.7}, {"0.9-1.0", 0.9}}

// EvidenceSummary is the GET /api/evidence/summary response. It is read-only
// reporting over stored evidence bundles; nothing here gates or alters a merge.
type EvidenceSummary struct {
	Hours      int    `json:"hours"`
	AllTime    bool   `json:"all_time"`
	Repo       string `json:"repo,omitempty"`
	AuthorKind string `json:"author_kind,omitempty"`
	// N is the number of PRs (latest bundle per PR) in the window.
	N                int                             `json:"n"`
	Outcomes         map[string]EvidenceOutcomes     `json:"outcomes_by_author_kind"`
	Calibration      []EvidencePerspectiveCalib      `json:"reviewer_calibration"`
	ReviewerModels   []EvidenceModelAccuracy         `json:"reviewer_accuracy_by_model"`
	SentinelPerWeek  EvidenceSentinelSeries          `json:"sentinel_hits_per_week"`
	ShadowGate       EvidenceShadowGate              `json:"shadow_gate"`
	ReviewerAccuracy *review.ReviewerAccuracySummary `json:"reviewer_accuracy,omitempty"`
	// WriteCounters is the proxy's attempted-vs-blocked agent write totals per
	// intent tier (#12069); omitted when the counter file is absent or unreadable.
	WriteCounters map[string]EvidenceWriteCount `json:"write_counters,omitempty"`
	// ClaimVsVerdict is panel 5 (#12365): per ISO week, agent-authored PRs
	// whose author said "done" versus what CI and the swarm said.
	ClaimVsVerdict []EvidenceClaimWeek `json:"claim_vs_verdict"`
	// IntentCounters is the intent gate's evaluated-vs-would-block totals per
	// tier (#12362), same shape as WriteCounters: Attempted is evaluations,
	// Blocked is the subset BlocksMerge(true) would have withheld, whether or
	// not intent.enforce was on. Omitted when the counter file is absent or
	// unreadable. Record-only; it never feeds back into eligibility.
	IntentCounters map[string]EvidenceWriteCount `json:"intent_counters,omitempty"`
	// ComplexityByRepo is the latest evidenceComplexityMonths monthly
	// complexity snapshots per managed repo (#12363), oldest first; omitted
	// when no repo has a snapshot yet.
	ComplexityByRepo []EvidenceComplexityRepo `json:"complexity_by_repo,omitempty"`
	Missing          EvidenceMissing          `json:"missing"`
}

// EvidenceClaimWeek is one ISO week of agent-authored PRs (latest bundle per
// PR). Claimed counts every such PR: opening or marking it ready is the agent
// saying "done". CINotGreen counts PRs with at least one captured check whose
// conclusion is not success; CICaptured is that column's denominator, since a
// record with no captured checks is disclosed under missing.records_empty_ci
// and never counted as agreement. VerdictNotApprove counts PRs with any
// perspective verdict other than approve; Both counts PRs in both columns.
type EvidenceClaimWeek struct {
	Week              string `json:"week"`
	N                 int    `json:"n"`
	Claimed           int    `json:"claimed"`
	CICaptured        int    `json:"ci_captured"`
	CINotGreen        int    `json:"ci_not_green"`
	VerdictNotApprove int    `json:"verdict_not_approve"`
	Both              int    `json:"both"`
}

// evidenceCINotGreen reports whether any captured check concluded with
// something other than success. Empty CI is not disagreement; callers must
// check len(b.CI.Checks) first.
func evidenceCINotGreen(b *evidence.Bundle) bool {
	for _, c := range b.CI.Checks {
		if c.Conclusion != "success" {
			return true
		}
	}
	return false
}

// evidenceVerdictNotApprove reports whether any perspective verdict is
// something other than approve. No verdicts is not disagreement.
func evidenceVerdictNotApprove(b *evidence.Bundle) bool {
	for _, v := range b.Verdicts {
		if v.Verdict != string(review.VerdictApprove) {
			return true
		}
	}
	return false
}

// evidenceComplexityMonths caps how many monthly snapshots per repo the
// summary carries; the full retention (complexity.RetainMonths) stays on disk.
const evidenceComplexityMonths = 12

// EvidenceComplexityRepo is one repo's monthly complexity series.
type EvidenceComplexityRepo struct {
	Repo   string                    `json:"repo"`
	Months []EvidenceComplexityMonth `json:"months"`
}

// EvidenceComplexityMonth is one monthly snapshot without its hotspot list.
type EvidenceComplexityMonth struct {
	Month                  string         `json:"month"`
	Ref                    string         `json:"ref,omitempty"`
	CapturedAt             time.Time      `json:"captured_at"`
	Files                  int            `json:"files"`
	Functions              int            `json:"functions"`
	MeanCyclomatic         float64        `json:"mean_cyclomatic"`
	MedianCyclomatic       int            `json:"median_cyclomatic"`
	P90Cyclomatic          int            `json:"p90_cyclomatic"`
	MaxCyclomatic          int            `json:"max_cyclomatic"`
	FunctionsOverThreshold int            `json:"functions_over_threshold"`
	Threshold              int            `json:"threshold"`
	StaticWarnings         int            `json:"static_warnings"`
	StaticWarningsByTool   map[string]int `json:"static_warnings_by_tool,omitempty"`
}

// loadEvidenceComplexity reads the per-repo monthly series under root; any
// failure yields nil (the summary omits the field rather than failing).
func loadEvidenceComplexity(root string) []EvidenceComplexityRepo {
	series, err := complexity.LoadSeries(root, evidenceComplexityMonths)
	if err != nil || len(series) == 0 {
		return nil
	}
	out := make([]EvidenceComplexityRepo, 0, len(series))
	for _, rs := range series {
		r := EvidenceComplexityRepo{Repo: rs.Repo, Months: make([]EvidenceComplexityMonth, 0, len(rs.Snapshots))}
		for _, s := range rs.Snapshots {
			r.Months = append(r.Months, EvidenceComplexityMonth{
				Month: s.Month, Ref: s.Ref, CapturedAt: s.CapturedAt,
				Files: s.Files, Functions: s.Functions,
				MeanCyclomatic: s.MeanCyclo, MedianCyclomatic: s.MedianCyclo,
				P90Cyclomatic: s.P90Cyclo, MaxCyclomatic: s.MaxCyclo,
				FunctionsOverThreshold: s.OverThreshold, Threshold: s.Threshold,
				StaticWarnings: s.Warnings, StaticWarningsByTool: s.WarningsByTool,
			})
		}
		out = append(out, r)
	}
	return out
}

// EvidenceWriteCount is one tier's attempted and proxy-blocked write counts.
type EvidenceWriteCount struct {
	Attempted int64 `json:"attempted"`
	Blocked   int64 `json:"blocked"`
}

// loadEvidenceWriteCounters reads the durable counter file; any failure yields nil.
func loadEvidenceWriteCounters(path string) map[string]EvidenceWriteCount {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		Totals map[string]EvidenceWriteCount `json:"totals"`
	}
	if json.Unmarshal(data, &f) != nil || len(f.Totals) == 0 {
		return nil
	}
	return f.Totals
}

// EvidenceOutcomes counts PRs for one author kind. Closed is always 0 today:
// a v1 bundle records a merge but not a close-without-merge. RevertedWithin14d
// is nil because revert tracking is not recorded in bundles.
type EvidenceOutcomes struct {
	N                 int  `json:"n"`
	Merged            int  `json:"merged"`
	Closed            int  `json:"closed"`
	Open              int  `json:"open"`
	RevertedWithin14d *int `json:"reverted_14d"`
}

type EvidencePerspectiveCalib struct {
	Perspective string                `json:"perspective"`
	N           int                   `json:"n"`
	Buckets     []EvidenceCalibBucket `json:"buckets"`
}

type EvidenceCalibBucket struct {
	Bucket    string `json:"bucket"`
	N         int    `json:"n"`
	Approvals int    `json:"approvals"`
	Blocks    int    `json:"blocks"`
	Merged    int    `json:"merged"`
}

type EvidenceModelAccuracy struct {
	Model    string                       `json:"model"`
	N        int                          `json:"n"`
	ByAuthor map[string]EvidenceModelStat `json:"by_author_kind"`
}

// EvidenceModelStat: Approvals/Blocks are verdict counts; MergedAfterBlock and
// ApprovedNotMerged compare the verdict with the PR's recorded outcome.
type EvidenceModelStat struct {
	N                 int `json:"n"`
	Approvals         int `json:"approvals"`
	Blocks            int `json:"blocks"`
	MergedAfterBlock  int `json:"merged_after_block"`
	ApprovedNotMerged int `json:"approved_not_merged"`
}

type EvidenceSentinelSeries struct {
	N     int                    `json:"n"`
	Weeks []EvidenceSentinelWeek `json:"weeks"`
}

type EvidenceSentinelWeek struct {
	Week string `json:"week"`
	Hits int    `json:"hits"`
}

// EvidenceShadowGate summarises shadow-mode gate observations (#12068). N is
// the PRs whose latest bundle carries any shadow observation; WouldHaveHeld
// (review gate) and WouldHaveBlocked (intent) count the PRs the gate would
// have stopped. ReviewGate and Intent compare those PRs' recorded outcomes
// with the PRs the gate would have let through.
type EvidenceShadowGate struct {
	N                int                    `json:"n"`
	WouldHaveHeld    int                    `json:"would_have_held"`
	WouldHaveBlocked int                    `json:"would_have_blocked"`
	ReviewGate       EvidenceShadowGateSide `json:"review_gate"`
	Intent           EvidenceShadowGateSide `json:"intent"`
}

// EvidenceShadowGateSide is one shadow gate: N PRs evaluated, split into the
// ones it would have stopped (Held) and the ones it would have passed.
type EvidenceShadowGateSide struct {
	N      int                   `json:"n"`
	Held   EvidenceShadowOutcome `json:"held"`
	Passed EvidenceShadowOutcome `json:"passed"`
}

// EvidenceShadowOutcome is how a set of PRs actually turned out. Reverted is
// a subset of Merged (the bundle has reverted_by links). Closed is always 0
// today because a v1 bundle does not record a close-without-merge.
type EvidenceShadowOutcome struct {
	N        int `json:"n"`
	Merged   int `json:"merged"`
	Closed   int `json:"closed"`
	Open     int `json:"open"`
	Reverted int `json:"reverted"`
}

func (o *EvidenceShadowOutcome) add(b *evidence.Bundle) {
	o.N++
	if b.Merge == nil {
		o.Open++
		return
	}
	o.Merged++
	if len(b.RevertedBy) > 0 {
		o.Reverted++
	}
}

func (s *EvidenceShadowGateSide) add(b *evidence.Bundle, flag *bool) {
	if flag == nil {
		return
	}
	s.N++
	if *flag {
		s.Held.add(b)
	} else {
		s.Passed.add(b)
	}
}

type EvidenceMissing struct {
	VerdictsModelUnknown int  `json:"verdicts_model_unknown"`
	RecordsEmptyCI       int  `json:"records_empty_ci"`
	RecordsUnreadable    int  `json:"records_unreadable"`
	ReviewerAccuracy     bool `json:"reviewer_accuracy_unavailable"`
}

// handleEvidenceSummary serves GET /api/evidence/summary?hours=&repo=&author_kind=.
func (s *Server) handleEvidenceSummary(w http.ResponseWriter, r *http.Request) {
	hours := prThroughputDefaultHours
	if raw := strings.TrimSpace(r.URL.Query().Get("hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			jsonError(w, "hours must be a non-negative integer (0 = all time)", http.StatusBadRequest)
			return
		}
		hours = n
	}
	if hours > prThroughputMaxHours {
		hours = prThroughputMaxHours
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo != "" && !validPRThroughputRepo(repo) {
		jsonError(w, "repo must be owner/name", http.StatusBadRequest)
		return
	}
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("author_kind")))
	switch kind {
	case "", evidence.AuthorHuman, evidence.AuthorAgent, evidence.AuthorBot:
	default:
		jsonError(w, "author_kind must be human, agent or bot", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	bundles, unreadable, err := loadEvidenceBundles(ghpkg.ReviewEvidenceRoot)
	if err != nil {
		jsonError(w, "review evidence unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := buildEvidenceSummary(bundles, now, hours, repo, kind)
	resp.Missing.RecordsUnreadable = unreadable
	resp.WriteCounters = loadEvidenceWriteCounters(writeCountersPath)
	resp.IntentCounters = loadEvidenceWriteCounters(intentCountersPath)
	resp.ComplexityByRepo = loadEvidenceComplexity(ghpkg.ReviewEvidenceRoot)

	days := (hours + 23) / 24
	if days < 1 || days > reviewerAccuracyMaxWindowDays {
		days = reviewerAccuracyMaxWindowDays
		if hours > 0 && hours <= reviewerAccuracyMaxWindowDays*24 {
			days = (hours + 23) / 24
		}
	}
	artifact, aerr := review.LoadArtifact("")
	ledger, lerr := review.LoadOutcomeLedger("")
	if (aerr != nil && !errors.Is(aerr, os.ErrNotExist)) || lerr != nil {
		resp.Missing.ReviewerAccuracy = true
	} else {
		acc := review.SummarizeReviewerAccuracy(now, time.Duration(days)*24*time.Hour, artifact, ledger, s.reviewerAccuracyEvidence())
		resp.ReviewerAccuracy = &acc
	}
	jsonResponse(w, resp)
}

// loadEvidenceBundles reads every bundle under root
// (<owner>/<repo>/<pr>/<sha>.json). Unparseable files are counted, not fatal.
func loadEvidenceBundles(root string) ([]*evidence.Bundle, int, error) {
	if root == "" {
		root = ghpkg.ReviewEvidenceRoot
	}
	var out []*evidence.Bundle
	unreadable := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			// Monthly complexity snapshots (#12363) share the root but are
			// not review bundles.
			if d.Name() == complexity.DirName && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		b, lerr := ghpkg.LoadReviewEvidence(path)
		if lerr != nil || b.SchemaVersion != evidence.SchemaVersion {
			unreadable++
			return nil
		}
		out = append(out, b)
		return nil
	})
	return out, unreadable, err
}

// buildEvidenceSummary aggregates the newest bundle per PR whose creation
// falls in the window. hours==0 means all time.
func buildEvidenceSummary(bundles []*evidence.Bundle, now time.Time, hours int, repo, kind string) EvidenceSummary {
	resp := EvidenceSummary{
		Hours: hours, AllTime: hours == 0, Repo: repo, AuthorKind: kind,
		Outcomes:        map[string]EvidenceOutcomes{},
		Calibration:     []EvidencePerspectiveCalib{},
		ReviewerModels:  []EvidenceModelAccuracy{},
		SentinelPerWeek: EvidenceSentinelSeries{Weeks: []EvidenceSentinelWeek{}},
		ClaimVsVerdict:  []EvidenceClaimWeek{},
	}
	for _, k := range []string{evidence.AuthorHuman, evidence.AuthorAgent, evidence.AuthorBot} {
		resp.Outcomes[k] = EvidenceOutcomes{}
	}
	cutoff := time.Time{}
	if hours > 0 {
		cutoff = now.Add(-time.Duration(hours) * time.Hour)
	}

	latest := map[string]*evidence.Bundle{}
	for _, b := range bundles {
		if repo != "" && !strings.EqualFold(b.Repo, repo) {
			continue
		}
		if kind != "" && b.Author.Kind != kind {
			continue
		}
		if hours > 0 && b.CreatedAt.Before(cutoff) {
			continue
		}
		key := strings.ToLower(b.Repo) + "#" + strconv.Itoa(b.Number)
		if cur, ok := latest[key]; !ok || b.CreatedAt.After(cur.CreatedAt) {
			latest[key] = b
		}
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type calib struct {
		n       int
		buckets []EvidenceCalibBucket
	}
	calibs := map[string]*calib{}
	models := map[string]*EvidenceModelAccuracy{}
	weeks := map[string]int{}
	claims := map[string]*EvidenceClaimWeek{}

	for _, key := range keys {
		b := latest[key]
		resp.N++
		o := resp.Outcomes[b.Author.Kind]
		o.N++
		merged := b.Merge != nil
		if merged {
			o.Merged++
		} else {
			o.Open++
		}
		resp.Outcomes[b.Author.Kind] = o
		if len(b.CI.Checks) == 0 {
			resp.Missing.RecordsEmptyCI++
		}
		if b.Author.Kind == evidence.AuthorAgent {
			y, w := b.CreatedAt.UTC().ISOWeek()
			wk := fmt.Sprintf("%04d-W%02d", y, w)
			c := claims[wk]
			if c == nil {
				c = &EvidenceClaimWeek{Week: wk}
				claims[wk] = c
			}
			c.N++
			c.Claimed++
			ciBad := false
			if len(b.CI.Checks) > 0 {
				c.CICaptured++
				ciBad = evidenceCINotGreen(b)
			}
			swarmBad := evidenceVerdictNotApprove(b)
			if ciBad {
				c.CINotGreen++
			}
			if swarmBad {
				c.VerdictNotApprove++
			}
			if ciBad && swarmBad {
				c.Both++
			}
		}

		if sh := b.Shadow; sh != nil {
			var held, blocked *bool
			if sh.ReviewGate != nil {
				held = sh.ReviewGate.WouldHaveHeld
			}
			if sh.Intent != nil {
				blocked = sh.Intent.WouldHaveBlocked
			}
			if held != nil || blocked != nil {
				resp.ShadowGate.N++
			}
			resp.ShadowGate.ReviewGate.add(b, held)
			resp.ShadowGate.Intent.add(b, blocked)
		}

		for range b.Sentinel {
			y, w := b.CreatedAt.UTC().ISOWeek()
			weeks[fmt.Sprintf("%04d-W%02d", y, w)]++
			resp.SentinelPerWeek.N++
		}

		for _, v := range b.Verdicts {
			model := strings.TrimSpace(v.Model)
			if model == "" || model == evidenceSummaryModelUnknown {
				model = evidenceSummaryModelUnknown
				resp.Missing.VerdictsModelUnknown++
			}
			approve := v.Verdict == string(review.VerdictApprove)
			block := v.Verdict == string(review.VerdictChangesRequested) || v.Verdict == string(review.VerdictReject)

			c := calibs[v.Perspective]
			if c == nil {
				c = &calib{buckets: make([]EvidenceCalibBucket, len(evidenceSummaryBuckets))}
				for i, bk := range evidenceSummaryBuckets {
					c.buckets[i].Bucket = bk.name
				}
				calibs[v.Perspective] = c
			}
			c.n++
			idx := 0
			for i, bk := range evidenceSummaryBuckets {
				if v.Confidence >= bk.low {
					idx = i
				}
			}
			c.buckets[idx].N++
			if approve {
				c.buckets[idx].Approvals++
			}
			if block {
				c.buckets[idx].Blocks++
			}
			if merged {
				c.buckets[idx].Merged++
			}

			m := models[model]
			if m == nil {
				m = &EvidenceModelAccuracy{Model: model, ByAuthor: map[string]EvidenceModelStat{}}
				models[model] = m
			}
			m.N++
			st := m.ByAuthor[b.Author.Kind]
			st.N++
			if approve {
				st.Approvals++
				if !merged {
					st.ApprovedNotMerged++
				}
			}
			if block {
				st.Blocks++
				if merged {
					st.MergedAfterBlock++
				}
			}
			m.ByAuthor[b.Author.Kind] = st
		}
	}

	persp := make([]string, 0, len(calibs))
	for p := range calibs {
		persp = append(persp, p)
	}
	sort.Strings(persp)
	for _, p := range persp {
		resp.Calibration = append(resp.Calibration, EvidencePerspectiveCalib{Perspective: p, N: calibs[p].n, Buckets: calibs[p].buckets})
	}
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		resp.ReviewerModels = append(resp.ReviewerModels, *models[n])
	}
	wk := make([]string, 0, len(weeks))
	for w := range weeks {
		wk = append(wk, w)
	}
	sort.Strings(wk)
	for _, w := range wk {
		resp.SentinelPerWeek.Weeks = append(resp.SentinelPerWeek.Weeks, EvidenceSentinelWeek{Week: w, Hits: weeks[w]})
	}
	cw := make([]string, 0, len(claims))
	for w := range claims {
		cw = append(cw, w)
	}
	sort.Strings(cw)
	for _, w := range cw {
		resp.ClaimVsVerdict = append(resp.ClaimVsVerdict, *claims[w])
	}
	resp.ShadowGate.WouldHaveHeld = resp.ShadowGate.ReviewGate.Held.N
	resp.ShadowGate.WouldHaveBlocked = resp.ShadowGate.Intent.Held.N
	return resp
}
