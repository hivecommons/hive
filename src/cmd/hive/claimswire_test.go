package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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

func TestRecordAgentKickListings(t *testing.T) {
	l := testClaimsLedger(t)
	_, _ = l.Claim(claims.Request{Repo: "myorg/repo", Issue: 2, Holder: "relay", HolderID: "relay#s", Kind: claims.KindContributor})
	_, _ = l.Claim(claims.Request{Repo: "myorg/repo", Issue: 3, Holder: "alice", Kind: claims.KindHuman})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	srv.RegisterAPI(&dashboard.Dependencies{Config: &config.Config{}, IssueClaims: l})
	t.Cleanup(srv.CloseContributeHub)

	recordAgentKickListings(srv, "myorg", "quality", []string{"repo#1", "myorg/repo#2", "myorg/repo#3", "myorg/repo#0", "garbage"}, logger)

	// #10527: a kick lists, it does not claim — no 🔒 comment, no attempt.
	if c, ok := l.Lookup("myorg/repo", 1); ok {
		t.Fatalf("kick recorded a claim on a listed issue: %+v", c)
	}
	if li, ok := l.Listed("myorg/repo", 1); !ok || li.Holder != "quality" {
		t.Fatalf("bare-repo ref not listed under org-qualified key: %+v %v", li, ok)
	}
	if c, _ := l.Lookup("myorg/repo", 2); c.Holder != "relay" {
		t.Fatalf("a kick listing displaced a contributor claim: %+v", c)
	}
	if c, _ := l.Lookup("myorg/repo", 3); c.Holder != "alice" {
		t.Fatalf("human claim was overridden by agent: %+v", c)
	}
	if !l.HeldKeys("relay#other")["myorg/repo#1"] {
		t.Fatal("listed issue not withheld from relay contributors")
	}
	fn := claimsInflightLookup(l, "myorg")
	if holder, held := fn(github.Issue{Repo: "repo", Number: 1}); !held || !strings.Contains(holder, "quality") {
		t.Fatalf("listed issue not withheld from other kicks: %q %v", holder, held)
	}
	// A nil ledger is a no-op, not a panic.
	recordAgentKickListings(dashboard.NewServer(0, logger), "o", "quality", []string{"o/r#1"}, logger)
}

func TestRecordAgentStartClaimsOnFirstSignal(t *testing.T) {
	comments := make(chan claims.Claim, 4)
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{
		OnClaimed: func(c claims.Claim, o claims.Outcome) {
			if o == claims.OutcomeClaimed {
				comments <- c
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	recordAgentStart(l, "myorg", "quality", "repo", 1, github.AgentStartSignalComment, logger)
	if _, ok := l.Lookup("myorg/repo", 1); ok {
		t.Fatal("start signal on an issue no kick listed recorded a claim")
	}

	l.MarkListed("myorg/repo", 1, "quality")
	recordAgentStart(l, "myorg", "quality", "repo", 1, github.AgentStartSignalPRRequest, logger)
	recordAgentStart(l, "myorg", "quality", "myorg/repo", 1, github.AgentStartSignalComment, logger)
	c, ok := l.Lookup("myorg/repo", 1)
	if !ok || c.Holder != "quality" || c.Kind != claims.KindAgent {
		t.Fatalf("start signal did not record the agent claim: %+v %v", c, ok)
	}
	if len(comments) != 1 {
		t.Fatalf("claim comments = %d, want exactly one for the first start signal", len(comments))
	}
	recordAgentStart(nil, "myorg", "quality", "repo", 1, github.AgentStartSignalComment, logger)
}

func TestLedgerHasContributor(t *testing.T) {
	if ledgerHasContributor(nil, "relay-dev") {
		t.Error("nil ledger knows no contributors")
	}
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []claims.Request{
		{Repo: "o/r", Issue: 1, Holder: "relay-dev", HolderID: "relay-dev/s1", Kind: claims.KindContributor},
		{Repo: "o/r", Issue: 2, Holder: "person", Kind: claims.KindHuman},
	} {
		if _, err := l.Claim(req); err != nil {
			t.Fatal(err)
		}
	}
	if !ledgerHasContributor(l, " Relay-Dev ") {
		t.Error("contributor claim holder must count as relay provenance")
	}
	if ledgerHasContributor(l, "person") || ledgerHasContributor(l, "") {
		t.Error("only contributor claims count")
	}
	b := &boot{issueClaims: l}
	if !b.relayContributor("relay-dev") {
		t.Error("boot predicate must read the ledger")
	}
}
