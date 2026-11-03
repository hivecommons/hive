package review

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

func TestReportedConfidenceParsing(t *testing.T) {
	for _, tc := range []struct{ raw, want float64 }{
		{-1, .55}, {0, .55}, {.55, .55}, {.599, .55}, {.6, .65}, {.65, .65}, {.7, .75}, {.75, .75}, {.8, .85}, {.85, .85}, {.9, .95}, {.95, .95}, {1.2, .95},
	} {
		r := baseReport(PerspectiveCorrectness, VerdictApprove)
		r.Confidence = &tc.raw
		raw, _ := json.Marshal(r)
		parsed, err := ValidateReport(raw)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Confidence == nil || *parsed.Confidence != tc.want || parsed.ConfidenceRaw == nil || *parsed.ConfidenceRaw != tc.raw {
			t.Fatalf("raw %v: %+v", tc.raw, parsed)
		}
		// Parsing a persisted normalized report preserves the unsnapped value.
		raw, _ = json.Marshal(parsed)
		again, err := ValidateReport(raw)
		if err != nil || *again.ConfidenceRaw != tc.raw {
			t.Fatalf("round trip: %+v, %v", again, err)
		}
	}
	r := baseReport(PerspectiveCorrectness, VerdictApprove)
	raw, _ := json.Marshal(r)
	parsed, err := ValidateReport(raw)
	if err != nil || parsed.Confidence != nil || parsed.ConfidenceRaw != nil {
		t.Fatalf("legacy: %+v, %v", parsed, err)
	}
	bad := strings.Replace(string(raw), `"verdict":"approve"`, `"confidence":"certain","verdict":"approve"`, 1)
	if _, err := ValidateReport([]byte(bad)); err == nil {
		t.Fatal("accepted nonnumeric confidence")
	}
}

func TestConfidenceNeverChangesGating(t *testing.T) {
	for _, verdict := range []Verdict{VerdictApprove, VerdictChangesRequested, VerdictRequiresHuman, VerdictReject} {
		r := baseReport(PerspectiveCorrectness, verdict)
		before := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})
		for _, confidence := range []float64{.55, .95} {
			r.Confidence = &confidence
			after := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})
			if after.Confidence.Score != before.Confidence.Score || after.Verdict != before.Verdict || after.MergeEligible != before.MergeEligible || after.RequiresHuman != before.RequiresHuman {
				t.Fatalf("self-report changed gating: before %+v after %+v", before, after)
			}
		}
	}
}

func TestDerivedConfidenceSignals(t *testing.T) {
	r := baseReport(PerspectiveCorrectness, VerdictApprove)
	r.HeadSHA = "head"
	peer := r
	peer.Perspective = PerspectiveSecurity
	otherHead := peer
	otherHead.HeadSHA = "other"
	otherRepo := peer
	otherRepo.Repo = "other/repo"
	otherPR := peer
	otherPR.Number++
	dissent := peer
	dissent.Verdict = VerdictChangesRequested
	na := peer
	na.NotApplicable = true
	for _, tc := range []struct {
		name     string
		s        ConfidenceSignals
		findings []outputschema.Finding
		want     float64
	}{
		{name: "unknown", want: .55},
		{name: "small diff", s: ConfidenceSignals{DiffKnown: true, ChangedLines: 100}, want: .65},
		{name: "medium diff", s: ConfidenceSignals{DiffKnown: true, ChangedLines: 101}, want: .60},
		{name: "medium upper", s: ConfidenceSignals{DiffKnown: true, ChangedLines: 500}, want: .60},
		{name: "large diff floor", s: ConfidenceSignals{DiffKnown: true, ChangedLines: 501}, want: .55},
		{name: "tests touched", s: ConfidenceSignals{DiffKnown: true, TouchesTests: true, ChangedLines: 50}, want: .75},
		{name: "unknown diff ignores flags", s: ConfidenceSignals{TouchesTests: true}, want: .55},
		{name: "green", s: ConfidenceSignals{CIState: "success"}, want: .60},
		{name: "red", s: ConfidenceSignals{DiffKnown: true, CIState: "failure"}, want: .60},
		{name: "pending", s: ConfidenceSignals{CIState: "pending"}, want: .55},
		{name: "agreement once", s: ConfidenceSignals{Peers: []PerspectiveReport{peer, peer}}, want: .60},
		{name: "last verdict wins", s: ConfidenceSignals{DiffKnown: true, Peers: []PerspectiveReport{peer, dissent}}, want: .60},
		{name: "unbound peers", s: ConfidenceSignals{Peers: []PerspectiveReport{otherHead, otherRepo, otherPR, r, na}}, want: .55},
		{name: "half anchored", findings: []outputschema.Finding{{File: "a.go", Line: 10}, {}}, want: .60},
		{name: "general not anchored", findings: []outputschema.Finding{{File: "(pr)", Line: 10}, {File: "a.go"}}, want: .55},
		{name: "all signals", s: ConfidenceSignals{DiffKnown: true, TouchesTests: true, CIState: "success", Peers: []PerspectiveReport{peer}}, findings: []outputschema.Finding{{File: "a.go", Line: 10}}, want: .95},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.Findings = tc.findings
			if got := DerivedConfidence(r, tc.s); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("got %.3f want %.3f", got, tc.want)
			}
		})
	}
	// The same observable evidence supports either verdict equally.
	s := ConfidenceSignals{DiffKnown: true, TouchesTests: true}
	r.Findings = nil
	approve := DerivedConfidence(r, s)
	r.Verdict = VerdictChangesRequested
	if DerivedConfidence(r, s) != approve {
		t.Fatal("derived score depends on verdict without peer evidence")
	}
}

func TestConfidencePromptAndTestPaths(t *testing.T) {
	for _, prompt := range []string{BuildPerspectivePrompt(PerspectiveCorrectness, PullRequest{Repo: "o/r", Number: 1}), BuildCombinedPrompt(PullRequest{Repo: "o/r", Number: 1}, []Perspective{PerspectiveCorrectness, PerspectiveSecurity}, PromptOptions{})} {
		for _, bin := range []string{"0.55 =", "0.65 =", "0.75 =", "0.85 =", "0.95 ="} {
			if !strings.Contains(prompt, bin) {
				t.Fatalf("prompt lacks %s", bin)
			}
		}
	}
	for _, name := range []string{"pkg/x_test.go", "tests/x.py", "test/test_x.py", "src/x.test.ts", "src/x.spec.js", "src/__tests__/x.js"} {
		if !ConfidenceTestPath(name) {
			t.Errorf("missed %s", name)
		}
	}
	if ConfidenceTestPath("src/contest.go") {
		t.Fatal("ordinary source treated as test")
	}
}
