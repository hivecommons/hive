package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
	"github.com/hivecommons/hive/pkg/stagelabels"
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

// A hive agent's claim is a fact the engine derives to stage/030-pr-in-progress;
// a human or contributor claim to stage/020-claimed — one stage label per
// issue, never the retired `claimed` / `hive/in-progress` pair
// (hivecommons/hive#12436). The hooks submit the fact; a sink over the real
// reconciler and a fake writer shows the label each kind ends up as, and the
// release fact takes it off again (010 while the issue is open).
func TestClaimHooks_SubmitClaimFacts(t *testing.T) {
	cases := []struct {
		kind claims.Kind
		want stagelabels.Stage
	}{
		{claims.KindAgent, stagelabels.PRInProgress},
		{claims.KindHuman, stagelabels.Claimed},
		{claims.KindContributor, stagelabels.Claimed},
		{claims.KindExternal, stagelabels.Claimed},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			sink, writer := stageenginetest.NewSink(stageenginetest.Options{Issue: github.IssueDeriver()})
			gh := github.NewClientForTest("http://127.0.0.1:0", "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			gh.SetStageFactSink(sink)
			ref := gh.IssueStageRef("acme/widgets", 7)
			writer.Seed(ref, "bug", stagelabels.Actionable.Label())
			cfg := &config.Config{}
			cfg.Governor.Claims.Label = boolPtr(true)
			cfg.Governor.Claims.Comment = boolPtr(false)
			hooks := githubClaimHooks(context.Background(), cfg, func() *github.Client { return gh }, nil)
			claim := claims.Claim{Repo: "acme/widgets", Issue: 7, Holder: "ann", Kind: tc.kind}
			hooks.OnClaimed(claim, claims.OutcomeClaimed)
			testutil.Eventually(t, 5*time.Second, func() bool {
				return stagelabels.Current(writer.LabelsOf(ref)) == tc.want
			}, "claim fact never staged the issue")
			if got := stagelabels.StageLabels(writer.LabelsOf(ref)); len(got) != 1 || got[0] != tc.want.Label() {
				t.Fatalf("stage labels after claim = %v, want only %s", got, tc.want.Label())
			}
			hooks.OnReleased(claim, "expired")
			testutil.Eventually(t, 5*time.Second, func() bool {
				return stagelabels.Current(writer.LabelsOf(ref)) == stagelabels.Actionable
			}, "release fact never re-staged the issue actionable")
			if got := writer.LabelsOf(ref); !hasLabel(got, "bug") {
				t.Fatalf("non-stage labels must survive: %v", got)
			}
		})
	}
}

// The claim hooks submit the claim as a fact; the engine writes the stage: an issue the
// reconciler had just staged 010 that takes a contributor claim ends up with
// exactly stage/020-claimed, not both (hivecommons/hive#12512).
func TestClaimHooks_ClaimReplacesStageLabel(t *testing.T) {
	var mu sync.Mutex
	var added, removed []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":7,"labels":[{"name":"bug"},{"name":"stage/010-actionable"},{"name":"hive/claimed-by-ann"}]}`))
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		var body []string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		added = append(added, body...)
		mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		removed = append(removed, strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/issues/7/labels/"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gh := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := &config.Config{}
	cfg.Governor.Claims.Label = boolPtr(true)
	cfg.Governor.Claims.Comment = boolPtr(false)
	hooks := githubClaimHooks(context.Background(), cfg, func() *github.Client { return gh }, nil)
	hooks.OnClaimed(claims.Claim{Repo: "acme/widgets", Issue: 7, Holder: "ann", Kind: claims.KindContributor}, claims.OutcomeClaimed)
	testutil.Eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(added) > 0
	}, "claim hook never added a stage label")
	mu.Lock()
	defer mu.Unlock()
	if len(added) != 1 || added[0] != "stage/020-claimed" {
		t.Fatalf("added %v, want only stage/020-claimed", added)
	}
	if len(removed) != 1 || removed[0] != "stage%2F010-actionable" && removed[0] != "stage/010-actionable" {
		t.Fatalf("removed %v, want stage/010-actionable", removed)
	}
}

func TestInProgressSetting(t *testing.T) {
	if set := inProgressSetting(nil); set.Enabled {
		t.Fatal("nil config must disable")
	}
	cfg := &config.Config{HiveID: "hive-a"}
	set := inProgressSetting(cfg)
	if !set.Enabled || set.HiveID != "hive-a" || set.TTL != claims.DefaultAgentTTL {
		t.Fatalf("default setting = %+v", set)
	}
	off := false
	cfg.Governor.Claims = config.ClaimsConfig{Enabled: true, Label: &off, AgentTTLS: 600}
	set = inProgressSetting(cfg)
	if set.Enabled {
		t.Fatal("claims on with label:false must disable the in-progress label too")
	}
	cfg.Governor.Claims.Label = nil
	if set = inProgressSetting(cfg); !set.Enabled || set.TTL != 10*time.Minute {
		t.Fatalf("agent_ttl_s not honoured: %+v", set)
	}
}

// A human claim relayed through the ledger (dashboard, /claim, hivectl, a
// Copilot CLI session acting for the person) mirrors `hive/claimed-by-<login>`
// and replaces a hand-applied backend-named label; release removes the
// holder's label and nothing else.
func TestClaimHooks_ClaimedByLabelUsesLoginNeverBackend(t *testing.T) {
	var mu sync.Mutex
	var added, removed []string
	// The release path re-reads the issue and removes only what it still
	// carries (RemoveLabelsFresh), so the mock keeps live label truth.
	truth := []string{"stage/010-actionable", "hive/claimed-by-copilot"}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/9", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		labels := make([]map[string]string, 0, len(truth))
		for _, l := range truth {
			labels = append(labels, map[string]string{"name": l})
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "labels": labels})
	})
	mux.HandleFunc("/repos/acme/widgets/issues/9/labels", func(w http.ResponseWriter, r *http.Request) {
		var body []string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		added = append(added, body...)
		truth = append(truth, body...)
		mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/repos/acme/widgets/issues/9/labels/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/issues/9/labels/")
		if u, err := url.PathUnescape(name); err == nil {
			name = u
		}
		mu.Lock()
		removed = append(removed, name)
		kept := truth[:0]
		for _, l := range truth {
			if l != name {
				kept = append(kept, l)
			}
		}
		truth = kept
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gh := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := &config.Config{}
	cfg.Governor.Claims.Label = boolPtr(true)
	cfg.Governor.Claims.Comment = boolPtr(false)
	hooks := githubClaimHooks(context.Background(), cfg, func() *github.Client { return gh }, nil)
	claim := claims.Claim{Repo: "acme/widgets", Issue: 9, Holder: "clubanderson", Kind: claims.KindHuman, Session: "copilot"}
	hooks.OnClaimed(claim, claims.OutcomeClaimed)
	has := func(list []string, want string) bool {
		for _, l := range list {
			if l == want || l == strings.ReplaceAll(want, "/", "%2F") {
				return true
			}
		}
		return false
	}
	testutil.Eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return has(added, "hive/claimed-by-clubanderson")
	}, "claim hook never added the login claimed-by label")
	mu.Lock()
	if !has(removed, "hive/claimed-by-copilot") {
		t.Fatalf("removed %v, want the backend-named label replaced", removed)
	}
	for _, l := range added {
		if claims.ClaimedByLabelIsBackend(l) {
			t.Fatalf("backend name leaked into labels: %v", added)
		}
	}
	removed = nil
	mu.Unlock()

	hooks.OnReleased(claim, "released by clubanderson")
	testutil.Eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return has(removed, "hive/claimed-by-clubanderson")
	}, "release never removed the holder's claimed-by label")
}
