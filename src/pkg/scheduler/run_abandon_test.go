package scheduler

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type abandonedRunTriageDeps struct{ fakeRunTriageDeps }

func (f *abandonedRunTriageDeps) RunAbandoned(key string) bool { return key == "o/r#1" }

func TestRunTriageDoesNotReadmitOrDirectFixAbandonedRun(t *testing.T) {
	cfg := &config.Config{Runs: config.RunsConfig{Triage: config.TriageConfig{Enabled: true}}}
	s := New(cfg, slog.Default())
	deps := &abandonedRunTriageDeps{}
	s.SetRunTriageDeps(deps, deps)
	for _, labels := range [][]string{{"run/spec"}, {"kind/bug"}} {
		issues := s.applyRunTriage(context.Background(), []github.Issue{{Repo: "o/r", Number: 1, Title: "abandoned", Labels: labels}})
		if len(issues) != 0 || len(deps.admitted) != 0 || len(deps.comments) != 0 {
			t.Fatalf("abandoned work escaped suppression: %+v %+v", issues, deps)
		}
	}
}
