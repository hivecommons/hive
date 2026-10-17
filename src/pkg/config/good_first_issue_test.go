package config

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/gfi"
	"gopkg.in/yaml.v3"
)

func TestGoodFirstIssueDefaultsAndPolicy(t *testing.T) {
	var g GoodFirstIssueConfig
	if !g.IsEnabled() {
		t.Fatal("must default on when enabled is omitted")
	}
	off := false
	if (GoodFirstIssueConfig{Enabled: &off}).IsEnabled() {
		t.Fatal("explicit enabled: false must opt out")
	}
	if DefaultGoodFirstIssueInterval >= time.Hour || gfi.DefaultMinAge > time.Hour {
		t.Fatalf("defaults too slow for a scanner that claims within minutes: interval=%v min_age=%v", DefaultGoodFirstIssueInterval, gfi.DefaultMinAge)
	}
	if g.IntervalOrDefault() != DefaultGoodFirstIssueInterval {
		t.Fatalf("interval %v", g.IntervalOrDefault())
	}
	p := g.Policy([]string{"needs-human"})
	if p.MaxPerRepo != gfi.DefaultMaxPerRepo || p.MaxPerSweep != gfi.DefaultMaxPerSweep || p.MinAge != gfi.DefaultMinAge || len(p.HardSuppressLabels) != 1 {
		t.Fatalf("policy %+v", p)
	}
	g = GoodFirstIssueConfig{Interval: 2 * time.Hour, MaxPerRepo: 9, ExcludeLabels: []string{"x"}, Repos: []string{"acme/widgets"}}
	if g.IntervalOrDefault() != 2*time.Hour || g.Policy(nil).MaxPerRepo != 9 || len(g.Policy(nil).ExcludeLabels) != 1 {
		t.Fatalf("explicit values lost: %+v", g.Policy(nil))
	}
	if !g.RepoAllowed("ACME/widgets") || g.RepoAllowed("acme/other") || !(GoodFirstIssueConfig{}).RepoAllowed("any/repo") {
		t.Fatal("RepoAllowed")
	}
}

func TestValidateGoodFirstIssue(t *testing.T) {
	if err := ValidateGoodFirstIssue(GoodFirstIssueConfig{}); err != nil {
		t.Fatalf("zero config invalid: %v", err)
	}
	cases := map[string]GoodFirstIssueConfig{
		"max_per_repo":     {MaxPerRepo: -1},
		"durations":        {MinAge: -time.Second},
		"min_body_chars":   {MinBodyChars: 500, MaxBodyChars: 100},
		"comment_template": {CommentTemplate: "no marker"},
	}
	for name, g := range cases {
		err := ValidateGoodFirstIssue(g)
		if err == nil || !strings.Contains(err.Error(), "good_first_issue") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if err := ValidateGoodFirstIssue(GoodFirstIssueConfig{CommentTemplate: "{{ .Marker }} claim {{ .ClaimURL }}"}); err != nil {
		t.Fatalf("valid override rejected: %v", err)
	}
}

func TestGoodFirstIssueYAML(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(`
good_first_issue:
  enabled: true
  interval: 30m
  max_per_repo: 3
  min_age: 48h
  exclude_labels: [meta]
  repos: [acme/widgets]
`), &cfg); err != nil {
		t.Fatal(err)
	}
	g := cfg.GoodFirstIssue
	if !g.IsEnabled() || g.Interval != 30*time.Minute || g.MaxPerRepo != 3 || g.MinAge != 48*time.Hour || len(g.ExcludeLabels) != 1 || len(g.Repos) != 1 {
		t.Fatalf("parsed %+v", g)
	}
	var optOut Config
	if err := yaml.Unmarshal([]byte("good_first_issue:\n  enabled: false\n"), &optOut); err != nil {
		t.Fatal(err)
	}
	if optOut.GoodFirstIssue.IsEnabled() {
		t.Fatal("enabled: false not honoured")
	}
	var omitted Config
	if err := yaml.Unmarshal([]byte("good_first_issue: {}\n"), &omitted); err != nil {
		t.Fatal(err)
	}
	if !omitted.GoodFirstIssue.IsEnabled() {
		t.Fatal("omitted enabled should default on")
	}
}
