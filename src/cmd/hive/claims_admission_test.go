package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

func TestKickClaimsRespectNeedsHumanAndReleaseOnTick(t *testing.T) {
	var label atomic.Value
	label.Store("needs-human")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"number":1,"labels":[{"name":%q}]}`, label.Load())
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	ledger := testClaimsLedger(t)
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })
	srv := dashboard.NewServer(0, logger)
	srv.RegisterAPI(&dashboard.Dependencies{Config: &config.Config{}, IssueClaims: ledger})
	t.Cleanup(srv.CloseContributeHub)

	recordAgentKickListings(srv, "o", "scanner", []string{"r#1"}, logger)
	if _, listed := ledger.Listed("o/r", 1); !listed {
		t.Fatal("kick did not record listing")
	}
	recordAgentStart(ledger, "o", "scanner", "r", 1, "comment", logger)
	if _, ok := ledger.Lookup("o/r", 1); ok {
		t.Fatal("needs-human issue claimed on start")
	}
	label.Store("bug")
	recordAgentStart(ledger, "o", "scanner", "r", 1, "comment", logger)
	if c, ok := ledger.Lookup("o/r", 1); !ok || c.Kind != claims.KindAgent {
		t.Fatal("ordinary issue not claimed")
	}
	label.Store("needs-human")
	if n := ledger.ReleaseBlocked(); n != 1 {
		t.Fatalf("cleanup tick released %d claims, want 1", n)
	}
	if _, held := claimsInflightLookup(ledger, "o")(github.Issue{Repo: "r", Number: 1}); held {
		t.Fatal("released claim still reported In Flight")
	}
}

// hivecommons/hive#12994: a review-swarm fix kick listed the PR in the issue
// ledger, the author's next comment on its own PR was taken as an issue start
// signal, and the resulting agent claim staged the PR
// stage/030-pr-in-progress — an issue-only stage. Even when a PR number is
// listed, admission refuses the claim: no hook fires, no label is written.
func TestKickClaimsRefusePullRequests(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":12994,"labels":[{"name":"size/L"}],"pull_request":{"url":"https://api.github.example/repos/o/r/pulls/12994"}}`)
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	var claimed atomic.Int32
	ledger, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{
		OnClaimed: func(claims.Claim, claims.Outcome) { claimed.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

	ledger.MarkListed("o/r", 12994, "scanner")
	recordAgentStart(ledger, "o", "scanner", "r", 12994, github.AgentStartSignalComment, logger)
	if c, ok := ledger.Lookup("o/r", 12994); ok {
		t.Fatalf("pull request claimed as an issue on start: %+v", c)
	}
	if n := claimed.Load(); n != 0 {
		t.Fatalf("OnClaimed fired %d times for a pull request", n)
	}
	res, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 12994, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent})
	if err != nil || res.Outcome != claims.OutcomeRefused || res.Reason != github.ClaimBlockReasonPullRequest {
		t.Fatalf("direct claim on a pull request = %+v, %v; want refused with %q", res, err, github.ClaimBlockReasonPullRequest)
	}
}
