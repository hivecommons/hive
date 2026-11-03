package search

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

var healthNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// healthBundles returns k bundles of org/a numbered from start, each passed
// through mut with its index.
func healthBundles(k, start int, mut func(i int, b *evidence.Bundle)) []*evidence.Bundle {
	out := make([]*evidence.Bundle, 0, k)
	for i := 0; i < k; i++ {
		b := &evidence.Bundle{Repo: "org/a", Number: start + i, HeadSHA: fmt.Sprintf("h%d", start+i),
			Author: evidence.Author{Kind: "agent"}, CreatedAt: healthNow.Add(-48 * time.Hour), UpdatedAt: healthNow.Add(-24 * time.Hour)}
		if mut != nil {
			mut(i, b)
		}
		out = append(out, b)
	}
	return out
}

func merged(b *evidence.Bundle, ttm time.Duration) {
	at := healthNow.Add(-24 * time.Hour)
	b.Merge = &evidence.MergeEvent{Actor: "hive", At: at, OpenedAt: at.Add(-ttm), SHA: "m" + b.HeadSHA}
}

func component(t *testing.T, h Health, name string) HealthComponent {
	t.Helper()
	for _, c := range h.Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("component %s missing", name)
	return HealthComponent{}
}

type healthWant struct {
	status string
	score  float64
	n      int
}

func checkComponent(t *testing.T, c HealthComponent, want healthWant) {
	t.Helper()
	if c.Status != want.status || c.N != want.n {
		t.Fatalf("%s: status %s n %d, want %s n %d (inputs %v, reason %q)", c.Name, c.Status, c.N, want.status, want.n, c.Inputs, c.Reason)
	}
	if c.Inputs == nil {
		t.Fatalf("%s: nil inputs", c.Name)
	}
	if want.status == HealthNotReported {
		if c.Score != nil || c.Contribution != nil || c.Reason == "" {
			t.Fatalf("%s: not_reported must have no score/contribution and a reason, got %+v", c.Name, c)
		}
		return
	}
	if c.Score == nil || abs(*c.Score-want.score) > 1e-9 {
		t.Fatalf("%s: score %v, want %v (inputs %v)", c.Name, c.Score, want.score, c.Inputs)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestHealthCIPassRate(t *testing.T) {
	ci := func(status string, at time.Time) func(int, *evidence.Bundle) {
		return func(_ int, b *evidence.Bundle) { b.CI = evidence.CI{Status: status, CapturedAt: at} }
	}
	recent := healthNow.Add(-time.Hour)
	cases := []struct {
		name    string
		bundles []*evidence.Bundle
		want    healthWant
	}{
		{"below min n", append(healthBundles(8, 1, ci("success", recent)), healthBundles(1, 100, ci("failure", recent))...),
			healthWant{HealthNotReported, 0, 9}},
		{"rate", append(healthBundles(8, 1, ci("success", recent)), healthBundles(2, 100, ci("failure", recent))...),
			healthWant{HealthReported, 0.8, 10}},
		{"pending not counted", append(healthBundles(9, 1, ci("success", recent)), healthBundles(5, 100, ci("pending", recent))...),
			healthWant{HealthNotReported, 0, 9}},
		{"outside 90d window", healthBundles(10, 1, ci("success", healthNow.Add(-91*24*time.Hour))),
			healthWant{HealthNotReported, 0, 0}},
		{"falls back to updated_at", healthBundles(10, 1, ci("success", time.Time{})),
			healthWant{HealthReported, 1, 10}},
		{"other repo ignored", healthBundles(10, 1, func(i int, b *evidence.Bundle) {
			ci("success", recent)(i, b)
			b.Repo = "org/b"
		}), healthWant{HealthNotReported, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ComputeHealth("org/a", append(tc.bundles, nil), HealthSignals{}, HealthOptions{}, healthNow)
			checkComponent(t, component(t, h, HealthCIPassRate), tc.want)
		})
	}
}

func TestHealthTimeToMerge(t *testing.T) {
	ttm := func(d time.Duration) func(int, *evidence.Bundle) {
		return func(_ int, b *evidence.Bundle) { merged(b, d) }
	}
	cases := []struct {
		name    string
		bundles []*evidence.Bundle
		want    healthWant
	}{
		{"fast scores 1", healthBundles(10, 1, ttm(12*time.Hour)), healthWant{HealthReported, 1, 10}},
		{"slow scores 0", healthBundles(10, 1, ttm(20*24*time.Hour)), healthWant{HealthReported, 0, 10}},
		{"linear between", healthBundles(10, 1, ttm(7*24*time.Hour+12*time.Hour)), healthWant{HealthReported, 0.5, 10}},
		{"below min n", healthBundles(9, 1, ttm(time.Hour)), healthWant{HealthNotReported, 0, 9}},
		{"no opened_at is skipped, not zero", healthBundles(10, 1, func(_ int, b *evidence.Bundle) {
			merged(b, time.Hour)
			b.Merge.OpenedAt = time.Time{}
		}), healthWant{HealthNotReported, 0, 0}},
		{"merged before window", healthBundles(10, 1, func(_ int, b *evidence.Bundle) {
			merged(b, time.Hour)
			b.Merge.At = healthNow.Add(-100 * 24 * time.Hour)
			b.Merge.OpenedAt = b.Merge.At.Add(-time.Hour)
		}), healthWant{HealthNotReported, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ComputeHealth("org/a", tc.bundles, HealthSignals{}, HealthOptions{}, healthNow)
			checkComponent(t, component(t, h, HealthTimeToMerge), tc.want)
		})
	}
}

func TestHealthChangeFailure(t *testing.T) {
	failing := func(i int, b *evidence.Bundle) {
		merged(b, time.Hour)
		switch i {
		case 0, 1:
			b.RevertedBy = []string{"org/a#900@r"}
		case 2:
			b.FollowedUpBy = []string{"org/a#901@f"}
		case 3:
			b.IssueReopens = []evidence.IssueReopen{{Issue: 7, At: b.Merge.At.Add(time.Hour), Source: "claim-ledger"}}
		case 4:
			// Reopened after the link window: not a change failure.
			b.IssueReopens = []evidence.IssueReopen{{Issue: 8, At: b.Merge.At.Add(evidence.LinkWindow + time.Hour), Source: "claim-ledger"}}
		}
	}
	cases := []struct {
		name    string
		bundles []*evidence.Bundle
		want    healthWant
	}{
		{"rate", healthBundles(10, 1, failing), healthWant{HealthReported, 0.6, 10}},
		{"none failed", healthBundles(10, 1, func(_ int, b *evidence.Bundle) { merged(b, time.Hour) }), healthWant{HealthReported, 1, 10}},
		{"below min n", healthBundles(9, 1, failing), healthWant{HealthNotReported, 0, 9}},
		{"unmerged ignored", healthBundles(12, 1, nil), healthWant{HealthNotReported, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ComputeHealth("org/a", tc.bundles, HealthSignals{}, HealthOptions{}, healthNow)
			c := component(t, h, HealthChangeFailure)
			checkComponent(t, c, tc.want)
			if tc.name == "rate" && (c.Inputs["reverted"] != 2 || c.Inputs["followed_up"] != 1 || c.Inputs["issue_reopened"] != 1 || c.Inputs["failed"] != 4) {
				t.Fatalf("inputs %v", c.Inputs)
			}
		})
	}
}

func TestHealthChangeFailureReportsIssueReopenRate(t *testing.T) {
	bs := healthBundles(10, 1, func(i int, b *evidence.Bundle) {
		merged(b, time.Hour)
		b.IssueClosures = []evidence.IssueClosure{{Repo: "org/a", Issue: 50 + i, At: healthNow.Add(-2 * 24 * time.Hour)}}
		if i == 0 {
			b.IssueReopens = []evidence.IssueReopen{{Issue: 50, At: healthNow.Add(-time.Hour), Source: "claim-ledger"}}
		}
	})
	c := component(t, ComputeHealth("org/a", bs, HealthSignals{}, HealthOptions{}, healthNow), HealthChangeFailure)
	if c.Inputs["issue_closures_30d"] != 10 || c.Inputs["issue_reopens_30d"] != 1 {
		t.Fatalf("inputs %v", c.Inputs)
	}
}

func TestHealthVerdictMix(t *testing.T) {
	verdict := func(v string, at time.Time) evidence.Verdict {
		return evidence.Verdict{Perspective: "correctness", Model: "m", Verdict: v, RecordedAt: at}
	}
	recent := healthNow.Add(-time.Hour)
	mix := func(i int, b *evidence.Bundle) {
		v := "approve"
		if i < 3 {
			v = "changes_requested"
		}
		b.Verdicts = []evidence.Verdict{verdict(v, recent)}
	}
	cases := []struct {
		name    string
		bundles []*evidence.Bundle
		want    healthWant
	}{
		{"approve share", healthBundles(10, 1, mix), healthWant{HealthReported, 0.7, 10}},
		{"below min n", healthBundles(9, 1, mix), healthWant{HealthNotReported, 0, 9}},
		{"old verdicts ignored", healthBundles(10, 1, func(_ int, b *evidence.Bundle) {
			b.Verdicts = []evidence.Verdict{verdict("approve", healthNow.Add(-100*24*time.Hour))}
		}), healthWant{HealthNotReported, 0, 0}},
		{"newest head only", append(healthBundles(10, 1, func(_ int, b *evidence.Bundle) {
			b.Verdicts = []evidence.Verdict{verdict("approve", recent)}
		}), healthBundles(10, 1, func(_ int, b *evidence.Bundle) {
			// An older head of the same PRs: superseded by the fold.
			b.HeadSHA = "old" + b.HeadSHA
			b.CreatedAt = healthNow.Add(-72 * time.Hour)
			b.Verdicts = []evidence.Verdict{verdict("changes_requested", recent)}
		})...), healthWant{HealthReported, 1, 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ComputeHealth("org/a", tc.bundles, HealthSignals{}, HealthOptions{}, healthNow)
			checkComponent(t, component(t, h, HealthVerdictMix), tc.want)
		})
	}
	c := component(t, ComputeHealth("org/a", healthBundles(10, 1, mix), HealthSignals{}, HealthOptions{}, healthNow), HealthVerdictMix)
	if got := c.Inputs["by_perspective"].(map[string]map[string]int)["correctness"]; got["approve"] != 7 || got["changes_requested"] != 3 {
		t.Fatalf("by_perspective %v", c.Inputs["by_perspective"])
	}
}

func TestHealthStaleIssues(t *testing.T) {
	activity := func(fresh, stale int) []time.Time {
		out := []time.Time{}
		for i := 0; i < fresh; i++ {
			out = append(out, healthNow.Add(-10*24*time.Hour))
		}
		for i := 0; i < stale; i++ {
			out = append(out, healthNow.Add(-45*24*time.Hour))
		}
		return out
	}
	cases := []struct {
		name string
		in   []time.Time
		opt  HealthOptions
		want healthWant
	}{
		{"no local data", nil, HealthOptions{}, healthWant{HealthNotReported, 0, 0}},
		{"none stale at 60d default", activity(7, 3), HealthOptions{}, healthWant{HealthReported, 1, 10}},
		{"stale_after 30d", activity(7, 3), HealthOptions{StaleAfter: 30 * 24 * time.Hour}, healthWant{HealthReported, 0.7, 10}},
		{"below min n", activity(3, 2), HealthOptions{}, healthWant{HealthNotReported, 0, 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ComputeHealth("org/a", nil, HealthSignals{OpenIssueActivity: tc.in}, tc.opt, healthNow)
			checkComponent(t, component(t, h, HealthStaleIssues), tc.want)
		})
	}
}

func TestHealthCountComponents(t *testing.T) {
	cases := []struct {
		name string
		comp string
		sig  HealthSignals
		want healthWant
	}{
		{"signoff no data", HealthSignOff, HealthSignals{}, healthWant{HealthNotReported, 0, 0}},
		{"signoff rate", HealthSignOff, HealthSignals{SignOff: &HealthCount{Count: 9, Total: 10}}, healthWant{HealthReported, 0.9, 10}},
		{"signoff below min n", HealthSignOff, HealthSignals{SignOff: &HealthCount{Count: 5, Total: 5}}, healthWant{HealthNotReported, 0, 5}},
		{"signoff negative total", HealthSignOff, HealthSignals{SignOff: &HealthCount{Total: -3}}, healthWant{HealthNotReported, 0, 0}},
		{"deps no data", HealthDependencies, HealthSignals{}, healthWant{HealthNotReported, 0, 0}},
		{"deps open share", HealthDependencies, HealthSignals{Dependencies: &HealthCount{Count: 2, Total: 10}}, healthWant{HealthReported, 0.8, 10}},
		{"deps below min n", HealthDependencies, HealthSignals{Dependencies: &HealthCount{Count: 0, Total: 4}}, healthWant{HealthNotReported, 0, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ComputeHealth("org/a", nil, tc.sig, HealthOptions{}, healthNow)
			checkComponent(t, component(t, h, tc.comp), tc.want)
		})
	}
}

func TestHealthSecurityDoc(t *testing.T) {
	ago := func(days float64) *time.Time {
		t := healthNow.Add(-time.Duration(days * 24 * float64(time.Hour)))
		return &t
	}
	zero := time.Time{}
	cases := []struct {
		name string
		at   *time.Time
		opt  HealthOptions
		want healthWant
	}{
		{"no data", nil, HealthOptions{}, healthWant{HealthNotReported, 0, 0}},
		{"zero time", &zero, HealthOptions{}, healthWant{HealthNotReported, 0, 0}},
		{"fresh", ago(100), HealthOptions{}, healthWant{HealthReported, 1, 1}},
		{"half rotted", ago(365 + 182.5), HealthOptions{}, healthWant{HealthReported, 0.5, 1}},
		{"fully rotted", ago(1000), HealthOptions{}, healthWant{HealthReported, 0, 1}},
		{"future is fresh", ago(-3), HealthOptions{}, healthWant{HealthReported, 1, 1}},
		{"custom max age", ago(150), HealthOptions{SecurityDocMaxAge: 100 * 24 * time.Hour}, healthWant{HealthReported, 0.5, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ComputeHealth("org/a", nil, HealthSignals{SecurityDocChangedAt: tc.at}, tc.opt, healthNow)
			checkComponent(t, component(t, h, HealthSecurityDoc), tc.want)
		})
	}
}

func TestHealthWeighting(t *testing.T) {
	ciGreen := healthBundles(10, 1, func(_ int, b *evidence.Bundle) {
		b.CI = evidence.CI{Status: "success", CapturedAt: healthNow.Add(-time.Hour)}
	})
	half := HealthSignals{SignOff: &HealthCount{Count: 5, Total: 10}}
	cases := []struct {
		name    string
		bundles []*evidence.Bundle
		sig     HealthSignals
		opt     HealthOptions
		score   *float64
		contrib map[string]float64
	}{
		{"default weights renormalised over reported", ciGreen, half, HealthOptions{}, ptr(83.3),
			map[string]float64{HealthCIPassRate: 66.7, HealthSignOff: 16.7}},
		{"configured weights", ciGreen, half, HealthOptions{Weights: map[string]float64{HealthCIPassRate: 10, HealthSignOff: 30}}, ptr(62.5),
			map[string]float64{HealthCIPassRate: 25, HealthSignOff: 37.5}},
		{"zero weight reported but unscored", ciGreen, half, HealthOptions{Weights: map[string]float64{HealthCIPassRate: 0}}, ptr(50),
			map[string]float64{HealthSignOff: 50}},
		{"negative weight is zero", ciGreen, half, HealthOptions{Weights: map[string]float64{HealthCIPassRate: -5}}, ptr(50),
			map[string]float64{HealthSignOff: 50}},
		{"nothing reported", nil, HealthSignals{}, HealthOptions{}, nil, nil},
		{"only zero-weight components reported", ciGreen, HealthSignals{}, HealthOptions{Weights: map[string]float64{HealthCIPassRate: 0}}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ComputeHealth("org/a", tc.bundles, tc.sig, tc.opt, healthNow)
			if tc.score == nil {
				if h.Score != nil || h.Status != HealthNotReported {
					t.Fatalf("score %v status %s, want not_reported", h.Score, h.Status)
				}
			} else if h.Score == nil || *h.Score != *tc.score || h.Status != HealthReported {
				t.Fatalf("score %v status %s, want %v", h.Score, h.Status, *tc.score)
			}
			for _, c := range h.Components {
				want, ok := tc.contrib[c.Name]
				if !ok {
					if c.Contribution != nil {
						t.Fatalf("%s: unexpected contribution %v", c.Name, *c.Contribution)
					}
					continue
				}
				if c.Contribution == nil || *c.Contribution != want {
					t.Fatalf("%s: contribution %v, want %v", c.Name, c.Contribution, want)
				}
			}
		})
	}
}

// A component without enough data never drags the score down as a zero.
func TestHealthNotReportedNeverZero(t *testing.T) {
	ciGreen := healthBundles(10, 1, func(_ int, b *evidence.Bundle) {
		b.CI = evidence.CI{Status: "success", CapturedAt: healthNow.Add(-time.Hour)}
	})
	alone := ComputeHealth("org/a", ciGreen, HealthSignals{}, HealthOptions{}, healthNow)
	withSparse := ComputeHealth("org/a", ciGreen, HealthSignals{
		SignOff:           &HealthCount{Count: 0, Total: 3},
		OpenIssueActivity: []time.Time{healthNow.Add(-400 * 24 * time.Hour)},
	}, HealthOptions{}, healthNow)
	if alone.Score == nil || withSparse.Score == nil || *alone.Score != 100 || *withSparse.Score != 100 {
		t.Fatalf("scores %v / %v, want 100 / 100", alone.Score, withSparse.Score)
	}
}

func TestHealthReportsInputsForEveryComponent(t *testing.T) {
	h := ComputeHealth("org/a", nil, HealthSignals{}, HealthOptions{}, healthNow)
	if len(h.Components) != len(HealthComponents) {
		t.Fatalf("components %d, want %d", len(h.Components), len(HealthComponents))
	}
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Score      *float64 `json:"score"`
		MinN       int      `json:"min_n"`
		WindowDays int      `json:"window_days"`
		ComputedAt string   `json:"computed_at"`
		Components []struct {
			Name   string          `json:"name"`
			Status string          `json:"status"`
			N      *int            `json:"n"`
			Inputs json.RawMessage `json:"inputs"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Score != nil || got.MinN != HealthMinN || got.WindowDays != 90 || got.ComputedAt == "" {
		t.Fatalf("header %s", data)
	}
	for i, c := range got.Components {
		if c.Name != HealthComponents[i] || c.Status != HealthNotReported || c.N == nil || len(c.Inputs) == 0 || string(c.Inputs) == "null" {
			t.Fatalf("component %d: %+v", i, c)
		}
	}
}

func TestDefaultHealthWeightsSumTo100(t *testing.T) {
	sum := 0.0
	w := DefaultHealthWeights()
	for _, name := range HealthComponents {
		sum += w[name]
	}
	if sum != 100 || len(w) != len(HealthComponents) {
		t.Fatalf("default weights %v sum %v", w, sum)
	}
}

// The score is report-only (RFC #14491 Decision 7): the search package must
// not import ACMM or merge-decision code.
func TestHealthHasNoACMMOrMergeImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		pf, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range pf.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(path, "acmm") || strings.Contains(path, "merge") || strings.HasSuffix(path, "/governor") {
				t.Fatalf("%s imports %s", f, path)
			}
		}
	}
}

func TestIndexerRecomputesHealthOnRebuild(t *testing.T) {
	f, ix := newIndexerFixture(t)
	var asked []string
	ix.HealthSignals = func(repo string) HealthSignals {
		asked = append(asked, repo)
		return HealthSignals{SignOff: &HealthCount{Count: 10, Total: 10}}
	}
	ix.HealthOptions = HealthOptions{Weights: map[string]float64{HealthVerdictMix: 0}}
	if _, ok := ix.RepoHealth("org/a"); ok {
		t.Fatal("health before any rebuild")
	}
	updated := f.clock.t.Add(-time.Hour)
	for n := 1; n <= 10; n++ {
		writeBundle(t, f.root, testBundle("org/a", n, fmt.Sprintf("head%d", n), updated))
	}
	rebuildOK(t, ix)
	h, ok := ix.RepoHealth(" ORG/A ")
	if !ok {
		t.Fatal("no health for org/a")
	}
	checkComponent(t, component(t, h, HealthCIPassRate), healthWant{HealthReported, 1, 10})
	if h.Repo != "org/a" || len(asked) != 1 || asked[0] != "org/a" || !h.ComputedAt.Equal(f.clock.t) {
		t.Fatalf("health %+v asked %v", h, asked)
	}
	if h.Score == nil || *h.Score != 100 {
		t.Fatalf("score %v", h.Score)
	}

	// A new failing head is picked up on the next rebuild; unchanged bundles
	// still count although they are not re-read.
	b := testBundle("org/a", 11, "head11", updated)
	b.CI.Status = "failure"
	writeBundle(t, f.root, b)
	f.clock.t = f.clock.t.Add(time.Minute)
	rebuildOK(t, ix)
	h, _ = ix.RepoHealth("org/a")
	checkComponent(t, component(t, h, HealthCIPassRate), healthWant{HealthReported, 10.0 / 11, 11})
	if !h.ComputedAt.Equal(f.clock.t) {
		t.Fatalf("computed_at %v, want %v", h.ComputedAt, f.clock.t)
	}
	if _, ok := ix.RepoHealth("org/none"); ok {
		t.Fatal("health for a repo without evidence")
	}
}

func TestIndexerKeepsHealthWhenRebuildSkipped(t *testing.T) {
	f, ix := newIndexerFixture(t)
	writeBundle(t, f.root, testBundle("org/a", 1, "head1", f.clock.t.Add(-time.Hour)))
	rebuildOK(t, ix)
	before, ok := ix.RepoHealth("org/a")
	if !ok {
		t.Fatal("no health")
	}
	ix.StartupBudget, ix.Started = time.Hour, f.clock.t
	f.clock.t = f.clock.t.Add(time.Minute)
	if st := rebuildOK(t, ix); !st.Skipped {
		t.Fatal("rebuild not skipped")
	}
	after, _ := ix.RepoHealth("org/a")
	if !after.ComputedAt.Equal(before.ComputedAt) {
		t.Fatalf("skipped rebuild recomputed health: %v -> %v", before.ComputedAt, after.ComputedAt)
	}
}

func ptr(v float64) *float64 { return &v }
