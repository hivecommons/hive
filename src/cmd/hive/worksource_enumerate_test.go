package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A Linear-sourced hive must not abort the eval cycle when GitHub cannot
// list issues (e.g. 403 from an App without the Issues permission): the
// work-source overlay that runs next is what populates its backlog.
func TestActionableAfterGitHubEnumerate_NonGitHubWorkSourceContinues(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	enumErr := errors.New("all 1 repos failed to enumerate (last error: 403 Resource not accessible by integration)")

	for _, wsType := range []string{"linear", "jira"} {
		cfg := &config.Config{}
		cfg.Governor.WorkSource.Type = wsType
		got, ok := actionableAfterGitHubEnumerate(cfg, nil, enumErr, logger)
		if !ok {
			t.Fatalf("work_source=%s: cycle aborted on GitHub enumeration failure; work source never consulted", wsType)
		}
		if got == nil {
			t.Fatalf("work_source=%s: expected an empty actionable result to overlay onto, got nil", wsType)
		}
		if n := len(got.Issues.Items); n != 0 {
			t.Fatalf("work_source=%s: expected no GitHub issues, got %d", wsType, n)
		}
	}

	// A partial GitHub result (PRs obtained) is kept for PR maintenance.
	cfg := &config.Config{}
	cfg.Governor.WorkSource.Type = "linear"
	partial := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{{Number: 7}}}}
	got, ok := actionableAfterGitHubEnumerate(cfg, partial, enumErr, logger)
	if !ok || got != partial {
		t.Fatalf("expected partial result to be kept, ok=%v got=%v", ok, got)
	}
}

// The GitHub-sourced (default) behavior is unchanged: a failed enumeration
// aborts the cycle so prior state is kept instead of idling agents on an
// empty queue.
func TestActionableAfterGitHubEnumerate_GitHubWorkSourceAborts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	enumErr := errors.New("all 1 repos failed to enumerate")
	for _, wsType := range []string{"", "github"} {
		cfg := &config.Config{}
		cfg.Governor.WorkSource.Type = wsType
		if _, ok := actionableAfterGitHubEnumerate(cfg, nil, enumErr, logger); ok {
			t.Fatalf("work_source=%q: expected cycle to abort on GitHub enumeration failure", wsType)
		}
	}
	// No error: pass-through.
	cfg := &config.Config{}
	res := &github.ActionableResult{}
	if got, ok := actionableAfterGitHubEnumerate(cfg, res, nil, logger); !ok || got != res {
		t.Fatalf("expected pass-through on success")
	}
}
