package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestStageTrackerSHAControlLabel(t *testing.T) {
	label := "wait-for-sha"
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{Issues: config.IssuesConfig{SHAHoldTrustedLabel: &label}}}
	status := minimalPayload()
	status.Repos = []FrontendRepo{{Name: "r", Full: "o/r", ActionableIssues: []any{
		github.Issue{Number: 1, Labels: []string{"WAIT-FOR-SHA"}},
		github.Issue{Number: 2, Labels: []string{"needs-sha"}},
		github.Issue{Number: 3, Labels: []string{"bug"}},
	}}}
	resp := s.buildStageTracker(status, nil, time.Now())
	for number, want := range map[int]string{1: "WAIT-FOR-SHA", 2: "needs-sha", 3: ""} {
		if got := trackerItem(t, resp, "issue", number).SHAHoldLabel; got != want {
			t.Errorf("issue %d SHA hold label = %q, want %q", number, got, want)
		}
	}
}
