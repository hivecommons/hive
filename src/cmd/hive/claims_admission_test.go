package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

	recordAgentKickClaims(srv, "o", "scanner", []string{"r#1"}, logger)
	if _, ok := ledger.Lookup("o/r", 1); ok {
		t.Fatal("needs-human issue claimed by kick")
	}
	label.Store("bug")
	recordAgentKickClaims(srv, "o", "scanner", []string{"r#1"}, logger)
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
