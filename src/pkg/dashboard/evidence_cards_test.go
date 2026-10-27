package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/review"
)

var evidenceCardKeys = []string{
	evidenceCardOutcome, evidenceCardCalibration, evidenceCardCalibrationDerived, evidenceCardAccuracy, evidenceCardMaintainability,
	evidenceCardBoundary, evidenceCardCIGate, evidenceCardWorkBalance, evidenceCardShadow, evidenceCardOpensMerges,
}

// TestEvidenceCardsEmptySummary pins the "—" grade and zero-data sentence on
// every card when nothing has been recorded.
func TestEvidenceCardsEmptySummary(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	resp := buildEvidenceSummary(nil, now, 24, "", "")
	cards := evidenceCards(&resp, evidenceWindow{Hours: 24, From: now.Add(-24 * time.Hour), To: now}, now)
	for _, key := range evidenceCardKeys {
		c, ok := cards[key]
		if !ok {
			t.Errorf("card %q missing", key)
			continue
		}
		if c.Grade.Letter != evidence.NoGradeLetter || c.Grade.Band != evidence.BandNone || c.Grade.EnoughData || c.Grade.Reason == "" || c.Grade.Rubric == "" {
			t.Errorf("%s: empty summary must grade — with a reason and rubric, got %+v", key, c.Grade)
		}
		if c.PlainSummary == "" || len(strings.Fields(c.PlainSummary)) > evidence.PlainSummaryMaxWords {
			t.Errorf("%s: plain summary %q", key, c.PlainSummary)
		}
	}
	if len(cards) != len(evidenceCardKeys) {
		t.Errorf("cards = %d, want %d", len(cards), len(evidenceCardKeys))
	}
}

// TestEvidenceCardsFromSummaryFields pins the mapping from summary fields to
// the grade inputs: reverts, calibration buckets summed across perspectives,
// the rework-based accuracy ledger, write activity, the CI gate, work balance
// and the shadow gates.
func TestEvidenceCardsFromSummaryFields(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rev := 1
	q := 7
	med := 1.0
	resp := EvidenceSummary{
		Outcomes: map[string]EvidenceOutcomes{
			"agent": {N: 23, Merged: 17, Open: 3, Unknown: 3, RevertedWithin14d: &rev},
			"human": {N: 2, Merged: 2},
		},
		Calibration: []EvidencePerspectiveCalib{
			{Perspective: "security", Buckets: []EvidenceCalibBucket{{Bucket: "0.55", N: 6, Merged: 2, Unknown: 1}, {Bucket: "0.65"}, {Bucket: "0.75"}, {Bucket: "0.85"}, {Bucket: "0.95", N: 12, Merged: 11}}},
			{Perspective: "quality", Buckets: []EvidenceCalibBucket{{Bucket: "0.55"}, {Bucket: "0.65"}, {Bucket: "0.75"}, {Bucket: "0.85"}, {Bucket: "0.95", N: 10, Merged: 9, Unknown: 1}}},
		},
		ReviewerModels: []EvidenceModelAccuracy{{Model: "m1", N: 30}},
		ReviewerAccuracy: &review.ReviewerAccuracySummary{Samples: 40, ReviewerModels: []review.ReviewerAccuracyGroup{
			{Name: "m1", Approvals: 30, FalseApprovals: 1, Blocks: 10, FalseBlocks: 0},
		}},
		ComplexityByRepo: []EvidenceComplexityRepo{{Repo: "o/r", Months: []EvidenceComplexityMonth{
			{Month: "2026-10", MedianCyclomatic: 3, FunctionsOverThreshold: 12},
			{Month: "2026-09", MedianCyclomatic: 3, FunctionsOverThreshold: 14},
			{Month: "2020-01", MedianCyclomatic: 9, FunctionsOverThreshold: 90},
		}}},
		WriteActivity:   EvidenceWriteActivity{TotalWrites: 4210, Blocked: 3},
		SentinelPerWeek: EvidenceSentinelSeries{N: 1, Weeks: []EvidenceSentinelWeek{{Week: "2026-W42", Hits: 1}}},
		CIRecovery: EvidenceCIRecovery{
			Gate:        EvidenceCIGate{MergedN: 60, WithChecks: 58, Green: 58},
			Convergence: EvidenceCIConvergence{N: 45, Settled: 40, MedianPasses: &med},
		},
		WorkBalance: []EvidenceWorkBalanceWeek{
			{Week: "2026-W41", Opened: map[string]int{"human": 10, "agent": 50}, Closed: 70, PRsOpened: map[string]int{"agent": 40}, PRsMerged: 35, QueueIssues: &q},
			{Week: "2026-W42", Opened: map[string]int{"agent": 60}, Closed: 60, PRsOpened: map[string]int{"agent": 50}, PRsMerged: 45},
		},
		ShadowGate: EvidenceShadowGate{N: 60, ReviewGate: EvidenceShadowGateSide{Held: EvidenceShadowOutcome{N: 3, Merged: 3}}, Intent: EvidenceShadowGateSide{Held: EvidenceShadowOutcome{N: 1}}},
	}
	cards := evidenceCards(&resp, evidenceWindow{Hours: 720, From: now.Add(-720 * time.Hour), To: now}, now)

	want := map[string]struct{ letter, summary string }{
		evidenceCardOutcome:         {"B-", "Agents: 17 merged, 1 reverted, 0 re-opened (5.9%). Humans: 2 merged, 0 undone. 3 outcomes not recorded yet."},
		evidenceCardCalibration:     {"A", "Not enough data to compare slopes: each series needs two bins with at least five known outcomes."},
		evidenceCardAccuracy:        {"A", "Reviewers wrongly approved 3% and wrongly blocked 0% of 40 PRs with a known result."},
		evidenceCardMaintainability: {"A", "Code complexity across 1 repo: median flat, 12 complex functions (was 14)."},
		evidenceCardBoundary:        {"A+", "4210 agent writes to GitHub; 3 were refused by the proxy (0.07%). 1 sentinel hit."},
		evidenceCardCIGate:          {"A+", "All 58 merged PRs with a known state had green required checks. Agent PRs needed a median 1 CI pass."},
		evidenceCardWorkBalance:     {"A-", "120 issues opened, 130 closed. 90 PRs opened, 80 merged. The issue queue is shrinking."},
		evidenceCardShadow:          {"A", "The shadow gates would have held 4 of 60 PRs; nothing was actually blocked."},
	}
	for key, w := range want {
		c := cards[key]
		if c.Grade.Letter != w.letter {
			t.Errorf("%s: letter %q, want %q (reason %q)", key, c.Grade.Letter, w.letter, c.Grade.Reason)
		}
		if c.PlainSummary != w.summary {
			t.Errorf("%s: summary\n got %q\nwant %q", key, c.PlainSummary, w.summary)
		}
		if !c.Grade.EnoughData || c.Grade.Band != evidence.BandOf(w.letter) {
			t.Errorf("%s: grade %+v", key, c.Grade)
		}
	}
	// Outcome: 19 of 22 known merged (86%) × (1 − 1/19 reverted) ≈ 0.82 → B-;
	// the reason must carry both numbers.
	if r := cards[evidenceCardOutcome].Grade.Reason; !strings.Contains(r, "86% of 22") || !strings.Contains(r, "1 of 19 merged") {
		t.Errorf("outcome reason %q", r)
	}

	// The 2020 month is outside a 30-day window and must not shape the trend;
	// an all-time window includes it and the median then falls 9 → 3 (A+).
	all := evidenceCards(&resp, evidenceWindow{Hours: 0, To: now}, now)
	if g := all[evidenceCardMaintainability].Grade; g.Letter != "A+" {
		t.Errorf("all-time maintainability = %+v", g)
	}
	// Repo filter is case-insensitive like the client.
	resp.Repo = "O/R"
	if g := evidenceCards(&resp, evidenceWindow{Hours: 720, From: now.Add(-720 * time.Hour), To: now}, now)[evidenceCardMaintainability].Grade; g.Letter != "A" {
		t.Errorf("repo-filtered maintainability = %+v", g)
	}
	resp.Repo = "x/y"
	if c := evidenceCards(&resp, evidenceWindow{Hours: 720, From: now.Add(-720 * time.Hour), To: now}, now)[evidenceCardMaintainability]; c.Grade.EnoughData || c.PlainSummary != "No monthly code-complexity snapshots in this window." {
		t.Errorf("other-repo maintainability = %+v", c)
	}

	// Without the outcome ledger the accuracy card stays ungraded but still
	// tells the reader how many verdicts exist.
	resp.ReviewerAccuracy = nil
	c := evidenceCards(&resp, evidenceWindow{Hours: 720, From: now.Add(-720 * time.Hour), To: now}, now)[evidenceCardAccuracy]
	if c.Grade.EnoughData || !strings.Contains(c.Grade.Reason, "ledger") || c.PlainSummary != "30 reviewer verdicts recorded; accuracy is graded once the outcome ledger covers them." {
		t.Errorf("accuracy without ledger = %+v", c)
	}
}

// TestHandleEvidenceSummaryCards pins the additive `cards` object on the
// JSON response: every panel key present with grade and plain_summary.
func TestHandleEvidenceSummaryCards(t *testing.T) {
	reviewEvidenceTestEnv(t)
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var raw struct {
		Cards map[string]struct {
			Grade struct {
				Letter     string `json:"letter"`
				Band       string `json:"band"`
				Reason     string `json:"reason"`
				EnoughData *bool  `json:"enough_data"`
				Rubric     string `json:"rubric"`
			} `json:"grade"`
			PlainSummary string `json:"plain_summary"`
		} `json:"cards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range evidenceCardKeys {
		c, ok := raw.Cards[key]
		if !ok {
			t.Errorf("cards.%s missing", key)
			continue
		}
		if c.Grade.Letter != evidence.NoGradeLetter || c.Grade.Band != "none" || c.Grade.EnoughData == nil || *c.Grade.EnoughData || c.Grade.Reason == "" || c.Grade.Rubric == "" || c.PlainSummary == "" {
			t.Errorf("cards.%s = %+v", key, c)
		}
	}
}
