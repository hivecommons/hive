package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/dashboard"
	"github.com/kubestellar/hive/pkg/github"
)

func TestPrimaryAdvisoryRepo(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"nil config", nil, ""},
		{"primary wins", &config.Config{Project: config.ProjectConfig{PrimaryRepo: "org/a", Repos: []string{"org/b"}}}, "org/a"},
		{"falls back to first repo", &config.Config{Project: config.ProjectConfig{Repos: []string{"org/b", "org/c"}}}, "org/b"},
		{"nothing configured", &config.Config{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := primaryAdvisoryRepo(tc.cfg); got != tc.want {
				t.Fatalf("primaryAdvisoryRepo = %q, want %q", got, tc.want)
			}
		})
	}
}

// The per-cycle re-ensure must fire for a repo that never resolved AND for one
// whose recorded number is the failed-ensure zero value — the state that left
// certus posting nowhere for six days (#4167).
func TestAdvisoryIssueUnresolved(t *testing.T) {
	issues := map[string]int{"org/zero": 0, "org/ok": 42}
	if !advisoryIssueUnresolved(issues, "org/missing") {
		t.Error("a repo with no entry must be treated as unresolved")
	}
	if !advisoryIssueUnresolved(issues, "org/zero") {
		t.Error("a recorded issue number of 0 must be treated as unresolved")
	}
	if advisoryIssueUnresolved(issues, "org/ok") {
		t.Error("a resolved issue number must not trigger a re-ensure")
	}
}

// A hive with findings but no advisory issue must report a post ERROR to the
// hub. Before #4167 this path was a silent skip, so the spoke reported neither a
// post time nor an error and the hub read it as "not an advisory participant" —
// a wedged digest that was indistinguishable from a healthy PR-only hive.
func TestMissingAdvisoryIssueIsReportedAsPostError(t *testing.T) {
	msg := advisoryIssueMissingError("org/repo", nil)
	if !strings.Contains(msg, "org/repo") {
		t.Fatalf("the recorded error must name the repo, got %q", msg)
	}

	srv := dashboard.NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, _, errMsg := srv.AdvisoryState(); errMsg != "" {
		t.Fatalf("fresh server should report no advisory error, got %q", errMsg)
	}

	srv.RecordAdvisoryError(msg)
	postedAt, _, errMsg := srv.AdvisoryState()
	if errMsg != msg {
		t.Fatalf("advisory error not surfaced to the heartbeat: got %q, want %q", errMsg, msg)
	}
	if !postedAt.IsZero() {
		t.Fatal("recording an error must not advance the last-successful-post time")
	}

	// A later successful post clears it, so the pill self-heals.
	srv.RecordAdvisoryPost(3)
	if _, _, errMsg := srv.AdvisoryState(); errMsg != "" {
		t.Fatalf("a successful post must clear the advisory error, got %q", errMsg)
	}
}

// #4329: when the ensure failed because the target repo has Issues DISABLED
// (has_issues=false — the fork case), the recorded post error must carry that
// cause into the fleet stale-advisory alert text, so the operator reads the
// repo-settings remedy instead of suspecting App auth.
func TestMissingAdvisoryIssueNamesIssuesDisabledCause(t *testing.T) {
	cause := &github.IssuesDisabledError{Repo: "jeejz/incubator-kie-drools", Fork: true}
	msg := advisoryIssueMissingError("jeejz/incubator-kie-drools", cause)
	if !strings.Contains(msg, "no advisory issue resolved") {
		t.Fatalf("the base symptom text must be preserved for the hub's staleness matcher, got %q", msg)
	}
	if !strings.Contains(msg, "Issues are disabled") || !strings.Contains(msg, "Settings > General > Features") {
		t.Fatalf("the alert text must name the Issues-disabled cause and remedy, got %q", msg)
	}

	// A wrapped cause must still be recognized.
	wrapped := fmt.Errorf("ensuring advisory issue: %w", cause)
	if got := advisoryIssueMissingError("jeejz/incubator-kie-drools", wrapped); !strings.Contains(got, "Issues are disabled") {
		t.Fatalf("a wrapped IssuesDisabledError must still fold into the alert, got %q", got)
	}

	// Any OTHER ensure failure (403 App-permission, rate limit, 5xx) keeps the
	// unadorned symptom text — those classes have their own banner/diagnosis
	// paths and must remain distinguishable.
	forbidden := errors.New("403 Resource not accessible by integration")
	if got := advisoryIssueMissingError("org/repo", forbidden); got != advisoryIssueMissingError("org/repo", nil) {
		t.Fatalf("a non-Issues-disabled cause must not change the alert text, got %q", got)
	}
}
