package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/github"
)

func TestIssueBandGoodFirstIssueSignal(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	info := IssueBand(github.Issue{Repo: "widgets", Number: 1, Labels: []string{"Good First Issue", "help wanted"}}, false, config.DashboardIssueBandsConfig{}, now)
	if info.Band != "ready" {
		t.Fatalf("band %q", info.Band)
	}
	found := false
	for _, s := range info.Signals {
		if s.Glyph == "🌱" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no 🌱 signal in %+v", info.Signals)
	}
	plain := IssueBand(github.Issue{Repo: "widgets", Number: 2, Labels: []string{"bug"}}, false, config.DashboardIssueBandsConfig{}, now)
	for _, s := range plain.Signals {
		if s.Glyph == "🌱" {
			t.Fatal("signal without the label")
		}
	}
}

func TestFeaturesOpsGoodFirstIssueRoundTrip(t *testing.T) {
	cur := &config.Config{}
	var body featuresOpsBody
	if err := json.Unmarshal([]byte(`{"goodFirstIssueEnabled":true,"goodFirstIssueInterval":"30m","goodFirstIssueMaxPerRepo":3,"goodFirstIssueMaxPerSweep":1,"goodFirstIssueMinAge":"48h"}`), &body); err != nil {
		t.Fatal(err)
	}
	apply, err := body.prepare(cur)
	if err != nil {
		t.Fatal(err)
	}
	apply(cur)
	g := cur.GoodFirstIssue
	if !g.Enabled || g.Interval != 30*time.Minute || g.MaxPerRepo != 3 || g.MaxPerSweep != 1 || g.MinAge != 48*time.Hour {
		t.Fatalf("applied %+v", g)
	}
	resp := featuresOpsResponse(cur)
	if resp["goodFirstIssueEnabled"] != true || resp["goodFirstIssueInterval"] != "30m0s" || resp["goodFirstIssueMaxPerRepo"] != 3 || resp["goodFirstIssueMinAge"] != "48h0m0s" {
		t.Fatalf("response %v", resp)
	}
	defaults := resp["goodFirstIssueDefaults"].(map[string]string)
	if defaults["goodFirstIssueMaxPerRepo"] != "5" || defaults["goodFirstIssueMinAge"] != gfi.DefaultMinAge.String() {
		t.Fatalf("defaults %v", defaults)
	}

	for _, bad := range []string{`{"goodFirstIssueMaxPerRepo":-1}`, `{"goodFirstIssueInterval":"soon"}`, `{"goodFirstIssueMinAge":"-1h"}`} {
		var b featuresOpsBody
		_ = json.Unmarshal([]byte(bad), &b)
		if _, err := b.prepare(cur); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// Absent keys leave the block untouched.
	var empty featuresOpsBody
	apply, err = empty.prepare(cur)
	if err != nil {
		t.Fatal(err)
	}
	apply(cur)
	if cur.GoodFirstIssue.Enabled != g.Enabled || cur.GoodFirstIssue.Interval != g.Interval || cur.GoodFirstIssue.MaxPerRepo != g.MaxPerRepo {
		t.Fatalf("empty body mutated config: %+v", cur.GoodFirstIssue)
	}
}

func TestGoodFirstIssueStatsInFeaturesView(t *testing.T) {
	s := &Server{}
	cfg := &config.Config{}
	if _, ok := s.featuresSectionWithLinked(cfg)["goodFirstIssueStats"]; ok {
		t.Fatal("stats present without a provider")
	}
	s.SetGoodFirstIssueStats(func() any { return gfi.Stats{Selected: 2, Converted: 1} })
	got, ok := s.featuresSectionWithLinked(cfg)["goodFirstIssueStats"].(gfi.Stats)
	if !ok || got.Selected != 2 || got.Converted != 1 {
		t.Fatalf("stats %v %v", got, ok)
	}
	s.SetGoodFirstIssueStats(nil)
	if _, ok := s.featuresSectionWithLinked(cfg)["goodFirstIssueStats"]; ok {
		t.Fatal("stats present after removal")
	}
}

func TestGoodFirstIssueLabelMirrorsGFI(t *testing.T) {
	if goodFirstIssueLabel != gfi.Label {
		t.Fatalf("goodFirstIssueLabel = %q, gfi.Label = %q", goodFirstIssueLabel, gfi.Label)
	}
	if config.DefaultGoodFirstIssueMaxPerRepo != gfi.DefaultMaxPerRepo || config.DefaultGoodFirstIssueMaxPerSweep != gfi.DefaultMaxPerSweep || config.DefaultGoodFirstIssueMinAge != gfi.DefaultMinAge {
		t.Fatal("config good-first-issue defaults drifted from pkg/gfi")
	}
}
