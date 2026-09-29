package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestIssueBandAgentFiledHonorsSelfAuthorizationHold9443(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	tr := true

	tests := []struct {
		name   string
		cfg    *config.Config
		labels []string
		want   string
	}{
		{
			name:   "hold on bands unacknowledged agent filed issue",
			cfg:    configAtACMM9443(config.MaxACMMLevel - 1),
			labels: []string{"agent/strategist"},
			want:   "agent-filed",
		},
		{
			name:   "hold off lets unacknowledged agent filed issue fall through",
			cfg:    configAtACMM9443(config.MaxACMMLevel),
			labels: []string{"agent/strategist"},
			want:   "ready",
		},
		{
			name: "explicit hold true at L6 still bands",
			cfg: func() *config.Config {
				cfg := configAtACMM9443(config.MaxACMMLevel)
				cfg.GitHub.SelfAuthorizationHold = &tr
				return cfg
			}(),
			labels: []string{"agent/strategist"},
			want:   "agent-filed",
		},
		{
			name:   "approved direction bypasses band while hold on",
			cfg:    configAtACMM9443(config.MaxACMMLevel - 1),
			labels: []string{"agent/strategist", "approved-direction"},
			want:   "ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer()
			s.deps = &Dependencies{Config: tt.cfg}
			info := IssueBand(github.Issue{
				Repo:   "octo/demo",
				Number: 9443,
				Labels: tt.labels,
			}, false, config.DashboardIssueBandsConfig{}, now, s.selfAuthorizationHoldActiveForRepo)
			if info.Band != tt.want {
				t.Fatalf("IssueBand() band = %q, want %q (info=%+v)", info.Band, tt.want, info)
			}
		})
	}
}

func TestOverviewIssueItemsResolveRepoHoldBeforeClassifying9443(t *testing.T) {
	level := config.MaxACMMLevel
	cfg := configAtACMM9443(level)
	status := &StatusPayload{Repos: []FrontendRepo{{
		Full: "octo/demo",
		ActionableIssues: []any{github.Issue{
			Number: 9443,
			Labels: []string{"agent/triage"},
		}},
	}}}
	s := newTestServer()
	s.deps = &Dependencies{Config: cfg}

	items := overviewIssueItems(status, config.DashboardIssueBandsConfig{}, overviewFilters{}, time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC), s.selfAuthorizationHoldActiveForRepo)
	if len(items) != 1 {
		t.Fatalf("overviewIssueItems() returned %d items, want 1", len(items))
	}
	if items[0].issue.Repo != "octo/demo" {
		t.Fatalf("overviewIssueItems() repo = %q, want octo/demo", items[0].issue.Repo)
	}
	if items[0].info.Band != "ready" {
		t.Fatalf("overviewIssueItems() band = %q, want ready", items[0].info.Band)
	}
}

func configAtACMM9443(level int) *config.Config {
	return &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org:   "octo",
			Repos: []string{"demo"},
		},
	}
}
