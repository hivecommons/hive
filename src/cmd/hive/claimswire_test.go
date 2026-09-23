package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

func testClaimsLedger(t *testing.T) *claims.Ledger {
	t.Helper()
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestBuildClaimsLedger_RespectsConfig(t *testing.T) {
	claimsLedgerPath = filepath.Join(t.TempDir(), "issue-claims.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if l := buildClaimsLedger(&config.Config{}, logger); l != nil {
		t.Fatal("claims.enabled=false still built a ledger")
	}
	l := buildClaimsLedger(&config.Config{HiveID: "h1", Governor: config.GovernorConfig{Claims: config.ClaimsConfig{Enabled: true, HumanTTLS: 60}}}, logger)
	if l == nil {
		t.Fatal("enabled config did not build a ledger")
	}
	res, err := l.Claim(claims.Request{Repo: "o/r", Issue: 1, Holder: "a", Kind: claims.KindHuman})
	if err != nil || res.Claim.Hive != "h1" || res.Claim.ExpiresAt.Sub(res.Claim.ClaimedAt).Seconds() != 60 {
		t.Fatalf("policy/hive not applied: %+v err=%v", res.Claim, err)
	}
}

func TestClaimsInflightLookup_WithholdsClaimedIssues(t *testing.T) {
	l := testClaimsLedger(t)
	_, _ = l.Claim(claims.Request{Repo: "myorg/repo", Issue: 7, Holder: "alice", Kind: claims.KindHuman})
	_, _ = l.Claim(claims.Request{Repo: "bare", Issue: 8, Holder: "quality", Kind: claims.KindAgent})

	fn := composeInflight(nil, claimsInflightLookup(l, "myorg"))
	if fn == nil {
		t.Fatal("composeInflight dropped the live lookup")
	}
	if holder, held := fn(github.Issue{Repo: "repo", Number: 7}); !held || holder == "" {
		t.Fatalf("bare repo spelling not matched against org-qualified claim: %q %v", holder, held)
	}
	if _, held := fn(github.Issue{Repo: "bare", Number: 8}); !held {
		t.Fatal("agent claim not treated as in-flight")
	}
	if _, held := fn(github.Issue{Repo: "repo", Number: 9}); held {
		t.Fatal("free issue reported held")
	}
	if composeInflight(nil, nil) != nil {
		t.Fatal("composeInflight of nothing must be nil so the scheduler skips the pass")
	}
	if claimsInflightLookup(nil, "x") != nil {
		t.Fatal("nil ledger must yield nil lookup")
	}
}

func TestRecordAgentKickClaims(t *testing.T) {
	l := testClaimsLedger(t)
	_, _ = l.Claim(claims.Request{Repo: "myorg/repo", Issue: 2, Holder: "relay", HolderID: "relay#s", Kind: claims.KindContributor})
	_, _ = l.Claim(claims.Request{Repo: "myorg/repo", Issue: 3, Holder: "alice", Kind: claims.KindHuman})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	srv.RegisterAPI(&dashboard.Dependencies{Config: &config.Config{}, IssueClaims: l})
	t.Cleanup(srv.CloseContributeHub)

	recordAgentKickClaims(srv, "myorg", "quality", []string{"repo#1", "myorg/repo#2", "myorg/repo#3", "myorg/repo#0", "garbage"}, logger)

	if c, ok := l.Lookup("myorg/repo", 1); !ok || c.Kind != claims.KindAgent || c.Holder != "quality" {
		t.Fatalf("bare-repo ref not claimed under org-qualified key: %+v", c)
	}
	if c, _ := l.Lookup("myorg/repo", 2); c.Holder != "quality" || c.TakenFrom != "relay" {
		t.Fatalf("contributor claim not taken over by agent: %+v", c)
	}
	if c, _ := l.Lookup("myorg/repo", 3); c.Holder != "alice" {
		t.Fatalf("human claim was overridden by agent: %+v", c)
	}
	// A nil ledger is a no-op, not a panic.
	recordAgentKickClaims(dashboard.NewServer(0, logger), "o", "quality", []string{"o/r#1"}, logger)
}
