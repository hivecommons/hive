package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rfccoverage"
)

type rfcCoverageFakeForge struct {
	lookups  int
	comments map[string]string
}

func (f *rfcCoverageFakeForge) FindMarkerComment(_ context.Context, repo string, number int, marker string) (bool, error) {
	f.lookups++
	return strings.Contains(f.comments[rfccoverage.Candidate{Repo: repo, Number: number}.Key()], marker), nil
}

func (f *rfcCoverageFakeForge) CreateComment(_ context.Context, repo string, number int, body string) error {
	if f.comments == nil {
		f.comments = map[string]string{}
	}
	f.comments[rfccoverage.Candidate{Repo: repo, Number: number}.Key()] = body
	return nil
}

func rfcCoverageSnapshot() *github.ActionableResult {
	return &github.ActionableResult{Issues: github.IssueResult{Items: []github.Issue{
		{
			Repo: "widgets", Number: 41, State: "open", Labels: []string{"enhancement"}, CreatedAt: time.Now().Add(-time.Hour),
			Title: "Feature: close issues automatically when the fixing PR merges",
			Body:  "Maintainers close every issue by hand after its pull request merges. Please close the issue on merge, with a backfill sweep for older merges.",
		},
		{Repo: "widgets", Number: 42, State: "closed", Labels: []string{"enhancement"}, Title: "close on merge"},
		{Repo: "widgets", Number: 43, State: "open", Labels: []string{"bug"}, Title: "close on merge broken"},
	}}}
}

func TestRunRFCExistingCheckDisabledIsNoOp(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	forge := &rfcCoverageFakeForge{}
	state := newRFCCoverageState()
	state.page = func() ([]byte, error) { t.Fatal("disabled check must not read the index"); return nil, nil }
	res := runRFCExistingCheck(context.Background(), forge, cfg, nil, state, rfcCoverageSnapshot(), nil)
	if res.Scanned != 0 || forge.lookups != 0 || len(forge.comments) != 0 {
		t.Fatalf("disabled check acted: %+v lookups=%d comments=%v", res, forge.lookups, forge.comments)
	}
	cfg.Issues.RFCExistingCheck.Enabled = true
	if res := runRFCExistingCheck(context.Background(), nil, cfg, nil, state, rfcCoverageSnapshot(), nil); res.Scanned != 0 {
		t.Fatal("no forge must no-op")
	}
	if newRFCCoverageForge(nil) != nil {
		t.Fatal("nil client must give a nil forge")
	}
}

func TestRunRFCExistingCheckKnownDuplicate(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Dashboard.PublicURL = "https://hive.example"
	cfg.Issues.RFCExistingCheck.Enabled = true
	forge := &rfcCoverageFakeForge{}
	state := newRFCCoverageState()
	res := runRFCExistingCheck(context.Background(), forge, cfg, nil, state, rfcCoverageSnapshot(), nil)
	if len(res.Commented) != 1 || res.Commented[0] != "acme/widgets#41" {
		t.Fatalf("result = %+v", res)
	}
	body := forge.comments["acme/widgets#41"]
	if !strings.Contains(body, rfccoverage.Marker) || !strings.Contains(body, "https://hive.example/#config/governor/Features/issues.close_on_merge") {
		t.Fatalf("comment must list close-on-merge with its Settings link:\n%s", body)
	}
	if again := runRFCExistingCheck(context.Background(), forge, cfg, nil, state, rfcCoverageSnapshot(), nil); len(again.Commented) != 0 {
		t.Fatalf("reposted: %+v", again)
	}
}

func TestRFCCoverageIndexAndKeyHelpers(t *testing.T) {
	state := newRFCCoverageState()
	state.page = func() ([]byte, error) { return nil, errors.New("gone") }
	if got := state.index(slog.New(slog.NewTextHandler(io.Discard, nil))); got != nil {
		t.Fatalf("unreadable page must give no settings: %v", got)
	}
	if repo, n := rfcCoverageIssueKey("acme/w#12"); repo != "acme/w" || n != 12 {
		t.Fatalf("key = %s %d", repo, n)
	}
	if repo, n := rfcCoverageIssueKey("acme/w"); repo != "acme/w" || n != 0 {
		t.Fatalf("key = %s %d", repo, n)
	}
	if repo, n := rfcCoverageIssueKey("acme/w#x"); repo != "acme/w#x" || n != 0 {
		t.Fatalf("key = %s %d", repo, n)
	}
	if rfcCoverageCandidates("acme", nil) != nil {
		t.Fatal("nil snapshot must give no candidates")
	}
	b := &boot{}
	if b.rfcCoverageState() == nil || b.rfcCoverageState() != b.rfcCoverage {
		t.Fatal("state must be built once")
	}
}
