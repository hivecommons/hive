package scheduler

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/upstreamwatch"
)

// The "port this" handoff (hivecommons/hive#9969): an upstream-watch issue
// kicked to a fixer carries the upstream patch URL in its kick context, so the
// fixer starts from the diff instead of hunting for it.

func newUpstreamPortScheduler(t *testing.T, repos map[string]config.UpstreamWatchRepo) *Scheduler {
	t.Helper()
	cfg := &config.Config{
		Project:       config.ProjectConfig{Org: "myorg", Repos: []string{"myorg/fork"}},
		UpstreamWatch: config.UpstreamWatchConfig{Enabled: true, Repos: repos},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func upstreamPortIssue(labels []string, body string) github.Issue {
	return github.Issue{
		Repo: "myorg/fork", Number: 7, Title: "upstream: fix the thing",
		Labels: labels, Body: body, URL: "https://github.com/myorg/fork/issues/7",
	}
}

func upstreamPortBody(t *testing.T, ref string) string {
	t.Helper()
	return upstreamwatch.RenderIssue("up/stream", upstreamwatch.Item{
		Kind: upstreamwatch.KindPR, Ref: ref, Title: "fix: the thing", HTMLURL: "https://example.test/pr/42",
	}, upstreamwatch.Judgement{Class: upstreamwatch.ClassBugfix, Difficulty: upstreamwatch.DifficultyEasy, Applicable: true},
		"upstream/port").Body
}

func TestUpstreamPortDiffURLInKickList(t *testing.T) {
	s := newUpstreamPortScheduler(t, map[string]config.UpstreamWatchRepo{"myorg/fork": {}})
	issue := upstreamPortIssue([]string{config.DefaultUpstreamWatchLabel}, upstreamPortBody(t, "upstream#42"))
	out, _ := s.formatIssueListWithPolicy([]github.Issue{issue})
	if !strings.Contains(out, "https://github.com/up/stream/pull/42.diff") {
		t.Errorf("kick list missing the upstream patch URL:\n%s", out)
	}
	if !strings.Contains(out, "upstream patch:") {
		t.Errorf("kick list missing the port-this handoff line:\n%s", out)
	}
}

// A repo may rename the label; the handoff follows the configured one.
func TestUpstreamPortDiffURLHonoursConfiguredLabel(t *testing.T) {
	s := newUpstreamPortScheduler(t, map[string]config.UpstreamWatchRepo{"myorg/fork": {Label: "port/me"}})
	body := upstreamPortBody(t, "upstream#42")
	withCustom := upstreamPortIssue([]string{"port/me"}, body)
	if out, _ := s.formatIssueListWithPolicy([]github.Issue{withCustom}); !strings.Contains(out, "/pull/42.diff") {
		t.Errorf("configured label not honoured:\n%s", out)
	}
	withDefault := upstreamPortIssue([]string{config.DefaultUpstreamWatchLabel}, body)
	if out, _ := s.formatIssueListWithPolicy([]github.Issue{withDefault}); strings.Contains(out, "/pull/42.diff") {
		t.Errorf("default label matched although the repo renamed it:\n%s", out)
	}
}

func TestUpstreamPortDiffURLAbsentWhenNotApplicable(t *testing.T) {
	marker := upstreamPortBody(t, "upstream#42")
	for _, tc := range []struct {
		name   string
		repos  map[string]config.UpstreamWatchRepo
		issue  github.Issue
		reason string
	}{
		{
			name:  "watch not configured",
			repos: nil,
			issue: upstreamPortIssue([]string{config.DefaultUpstreamWatchLabel}, marker),
		},
		{
			name:  "no upstream label",
			repos: map[string]config.UpstreamWatchRepo{"myorg/fork": {}},
			issue: upstreamPortIssue([]string{"bug"}, marker),
		},
		{
			name:  "no marker in body",
			repos: map[string]config.UpstreamWatchRepo{"myorg/fork": {}},
			issue: upstreamPortIssue([]string{config.DefaultUpstreamWatchLabel}, "an ordinary issue body"),
		},
		{
			name:  "release marker has no diff",
			repos: map[string]config.UpstreamWatchRepo{"myorg/fork": {}},
			issue: upstreamPortIssue([]string{config.DefaultUpstreamWatchLabel}, "<!-- upstream-ref: up/stream@v1.0 -->"),
		},
		{
			name:  "marker is not echoed as a link",
			repos: map[string]config.UpstreamWatchRepo{"myorg/fork": {}},
			issue: upstreamPortIssue([]string{config.DefaultUpstreamWatchLabel}, "<!-- upstream-ref: https://evil.test/x#1 -->"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newUpstreamPortScheduler(t, tc.repos)
			out, _ := s.formatIssueListWithPolicy([]github.Issue{tc.issue})
			if strings.Contains(out, "upstream patch:") {
				t.Errorf("unexpected port-this handoff line:\n%s", out)
			}
		})
	}
}
