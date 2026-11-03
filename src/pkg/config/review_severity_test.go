package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReviewSeverityBacklogDefaults(t *testing.T) {
	var rc ReviewConfig
	if !rc.Severity.CommentBelowEnabled() || !rc.Severity.BacklogBelowEnabled() {
		t.Fatal("comment_below and backlog_below default to true")
	}
	if got := rc.Backlog.EffectiveDestination(""); got != ReviewBacklogGitHubIssue {
		t.Fatalf("destination default = %q", got)
	}
	if got := rc.Backlog.EffectiveLabels(); !reflect.DeepEqual(got, []string{"agent/reviewer"}) {
		t.Fatalf("labels default = %v", got)
	}
	if got := rc.Backlog.EffectiveMaxPerPRPerDay(); got != 10 {
		t.Fatalf("max_per_pr_per_day default = %d", got)
	}
	if err := rc.Severity.Validate(); err != nil {
		t.Fatalf("zero severity must validate: %v", err)
	}
	if err := rc.Backlog.Validate(); err != nil {
		t.Fatalf("zero backlog must validate: %v", err)
	}
	off := false
	rc.Severity.CommentBelow, rc.Severity.BacklogBelow = &off, &off
	if rc.Severity.CommentBelowEnabled() || rc.Severity.BacklogBelowEnabled() {
		t.Fatal("explicit false must be honoured")
	}
}

func TestReviewSeverityBacklogValidate(t *testing.T) {
	cases := []struct {
		name     string
		severity ReviewSeverityConfig
		backlog  ReviewBacklogConfig
		wantErr  string
	}{
		{name: "empty"},
		{name: "ship fast", severity: ReviewSeverityConfig{BlockAt: "P1"}},
		{name: "strict lower-case", severity: ReviewSeverityConfig{BlockAt: " p2 "}},
		{name: "everything", severity: ReviewSeverityConfig{BlockAt: "P3"}},
		{name: "P0 rejected", severity: ReviewSeverityConfig{BlockAt: "P0"}, wantErr: "review.severity.block_at"},
		{name: "typo", severity: ReviewSeverityConfig{BlockAt: "high"}, wantErr: "review.severity.block_at"},
		{name: "github issue", backlog: ReviewBacklogConfig{Destination: "github_issue"}},
		{name: "linear", backlog: ReviewBacklogConfig{Destination: "linear", LinearState: "Backlog"}},
		{name: "jira", backlog: ReviewBacklogConfig{Destination: "JIRA", JiraStatus: "To Do"}},
		{name: "project", backlog: ReviewBacklogConfig{Destination: "github_project", ProjectColumnID: "f75ad846"}},
		{name: "project without column", backlog: ReviewBacklogConfig{Destination: "github_project"}, wantErr: "review.backlog.project_column_id"},
		{name: "unknown destination", backlog: ReviewBacklogConfig{Destination: "trello"}, wantErr: "review.backlog.destination"},
		{name: "blank label", backlog: ReviewBacklogConfig{Labels: []string{"from-review", " "}}, wantErr: "review.backlog.labels[1]"},
		{name: "negative cap", backlog: ReviewBacklogConfig{MaxPerPRPerDay: -1}, wantErr: "review.backlog.max_per_pr_per_day"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.severity.Validate()
			if err == nil {
				err = tc.backlog.Validate()
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

func TestReviewBacklogEffectiveDestinationFollowsWorkSource(t *testing.T) {
	cases := []struct {
		dest, ws, want string
	}{
		{"linear", "linear", "linear"},
		{"linear", "", "github_issue"},
		{"jira", "jira", "jira"},
		{"jira", "linear", "github_issue"},
		{"github_project", "github_projects", "github_project"},
		{"github_project", "github", "github_issue"},
		{"github_issue", "jira", "github_issue"},
		{"bogus", "jira", "github_issue"},
	}
	for _, tc := range cases {
		if got := (ReviewBacklogConfig{Destination: tc.dest}).EffectiveDestination(tc.ws); got != tc.want {
			t.Errorf("EffectiveDestination(%q, ws=%q) = %q, want %q", tc.dest, tc.ws, got, tc.want)
		}
	}
}

func TestReviewSeverityBacklogYAMLRoundTrip(t *testing.T) {
	src := `
review:
  severity:
    block_at: P2
    comment_below: true
    backlog_below: false
  backlog:
    destination: linear
    linear_state: Triage
    labels: [from-review, nit]
    max_per_pr_per_day: 5
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	s, b := cfg.Review.Severity, cfg.Review.Backlog
	if s.BlockAt != "P2" || !s.CommentBelowEnabled() || s.BacklogBelowEnabled() {
		t.Fatalf("severity = %+v", s)
	}
	if b.Destination != "linear" || b.LinearState != "Triage" || !reflect.DeepEqual(b.Labels, []string{"from-review", "nit"}) || b.EffectiveMaxPerPRPerDay() != 5 {
		t.Fatalf("backlog = %+v", b)
	}
	// The retired name still loads, but filing uses the agent lane replacement.
	if got := b.EffectiveLabels(); !reflect.DeepEqual(got, []string{"agent/reviewer", "nit"}) {
		t.Fatalf("effective labels = %v, want from-review mapped to agent/reviewer", got)
	}
}

func TestCanonicalOriginLabelRenameMapping(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		renamed bool
	}{
		{"from-review", "agent/reviewer", true},
		{" From-Review ", "agent/reviewer", true},
		{"origin/review", "agent/reviewer", true},
		{" Origin/Review ", "agent/reviewer", true},
		{"origin/reviewer", "agent/reviewer", true},
		{"origin/scanner", "agent/scanner", true},
		{"nit", "nit", false},
		{"agent/reviewer", "agent/reviewer", false},
	}
	for _, tc := range cases {
		got, renamed := CanonicalOriginLabel(tc.in)
		if got != tc.want || renamed != tc.renamed {
			t.Errorf("CanonicalOriginLabel(%q) = (%q, %v), want (%q, %v)", tc.in, got, renamed, tc.want, tc.renamed)
		}
	}
	if DefaultReviewBacklogLabel != "agent/reviewer" || OriginReviewerLabel != "origin/reviewer" {
		t.Fatalf("default backlog label = %q, origin reviewer = %q", DefaultReviewBacklogLabel, OriginReviewerLabel)
	}
	if LegacyOriginReviewLabel != "origin/review" || LegacyReviewBacklogLabel != "from-review" {
		t.Fatalf("legacy names = %q, %q", LegacyOriginReviewLabel, LegacyReviewBacklogLabel)
	}
}

func TestReviewBacklogEffectiveLabelsNeverDoubleLabels(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"all three spellings", []string{"from-review", "origin/review", "origin/reviewer", "nit"}, []string{"agent/reviewer", "nit"}},
		{"legacy plus new", []string{"from-review", "origin/reviewer", "nit"}, []string{"agent/reviewer", "nit"}},
		{"v1 plus new", []string{"origin/review", "origin/reviewer"}, []string{"agent/reviewer"}},
		{"new plus legacy", []string{"origin/reviewer", "from-review"}, []string{"agent/reviewer"}},
		{"case duplicates", []string{"Nit", "nit", "FROM-REVIEW", "Origin/Review"}, []string{"Nit", "agent/reviewer"}},
		{"only legacy", []string{"from-review"}, []string{"agent/reviewer"}},
		{"only v1", []string{"origin/review"}, []string{"agent/reviewer"}},
		{"empty slice keeps no default", []string{}, []string{}},
	}
	for _, tc := range cases {
		got := ReviewBacklogConfig{Labels: tc.in}.EffectiveLabels()
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: EffectiveLabels(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// block_at drives classification.review_bots.min_priority only when no
// explicit min_priority is set in hive.yaml or hive-project.yaml.
func TestEffectiveReviewBotsBlockAtPrecedence(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.yaml")
	project := filepath.Join(dir, "hive-project.yaml")
	if err := os.WriteFile(project, []byte("classification:\n  review_bots:\n    logins: [Copilot]\n    min_priority: P0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectNoPriority := filepath.Join(dir, "hive-project-noprio.yaml")
	if err := os.WriteFile(projectNoPriority, []byte("classification:\n  review_bots:\n    logins: [Copilot]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		hiveBots ReviewBotsConfig
		blockAt  string
		project  string
		want     string
	}{
		{name: "nothing set", project: missing, want: ""},
		{name: "block_at only", blockAt: "p2", project: missing, want: "P2"},
		{name: "block_at feeds project logins", blockAt: "P3", project: projectNoPriority, want: "P3"},
		{name: "project min_priority wins", blockAt: "P3", project: project, want: "P0"},
		{name: "hive.yaml override wins", hiveBots: ReviewBotsConfig{MinPriority: "P1"}, blockAt: "P3", project: project, want: "P1"},
		{name: "explicit all wins", hiveBots: ReviewBotsConfig{MinPriority: "all"}, blockAt: "P2", project: missing, want: "all"},
		{name: "hive.yaml logins without priority", hiveBots: ReviewBotsConfig{Logins: []string{"Copilot"}}, blockAt: "P1", project: missing, want: "P1"},
		{name: "hive.yaml logins with priority", hiveBots: ReviewBotsConfig{Logins: []string{"Copilot"}, MinPriority: "P0"}, blockAt: "P3", project: missing, want: "P0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Classification.ReviewBots = tc.hiveBots
			cfg.Review.Severity.BlockAt = tc.blockAt
			rb, err := cfg.EffectiveReviewBots(tc.project)
			if err != nil {
				t.Fatal(err)
			}
			if rb.MinPriority != tc.want {
				t.Fatalf("min_priority = %q, want %q", rb.MinPriority, tc.want)
			}
			if cfg.Classification.ReviewBots.MinPriority != tc.hiveBots.MinPriority {
				t.Fatal("EffectiveReviewBots must not write block_at back into hive.yaml")
			}
		})
	}
}
