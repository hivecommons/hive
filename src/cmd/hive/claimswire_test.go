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
	"runtime"
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

// A claim is a fact the engine derives to stage/020-claimed whatever the
// holder's kind; only an agent claim carrying PR evidence (PRStarted — a
// pr_request / branch start signal) derives stage/030-pr-in-progress
// (hivecommons/hive#13069; one stage label per issue, never the retired
// `claimed` / `hive/in-progress` pair, hivecommons/hive#12436). The hooks
// submit the fact; a sink over the real reconciler and a fake writer shows the
// label each kind ends up as, and the release fact takes it off again (010
// while the issue is open).
func TestClaimHooks_SubmitClaimFacts(t *testing.T) {
	cases := []struct {
		name      string
		kind      claims.Kind
		prStarted bool
		want      stagelabels.Stage
	}{
		{"agent", claims.KindAgent, false, stagelabels.Claimed},
		{"agent-pr-started", claims.KindAgent, true, stagelabels.PRInProgress},
		{"human", claims.KindHuman, false, stagelabels.Claimed},
		{"contributor", claims.KindContributor, false, stagelabels.Claimed},
		{"external", claims.KindExternal, false, stagelabels.Claimed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink, writer := stageenginetest.NewSink(stageenginetest.Options{Issue: github.IssueDeriver()})
			gh := github.NewClientForTest("http://127.0.0.1:0", "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			gh.SetStageFactSink(sink)
			ref := gh.IssueStageRef("acme/widgets", 7)
			writer.Seed(ref, "bug", stagelabels.Actionable.Label())
			cfg := &config.Config{}
			cfg.Governor.Claims.Label = boolPtr(true)
			cfg.Governor.Claims.Comment = boolPtr(false)
			hooks := githubClaimHooks(context.Background(), cfg, func() *github.Client { return gh }, nil)
			claim := claims.Claim{Repo: "acme/widgets", Issue: 7, Holder: "ann", Kind: tc.kind, PRStarted: tc.prStarted}
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
		_, _ = w.Write([]byte(`{"number":7,"labels":[{"name":"bug"},{"name":"stage/010-actionable"},{"name":"hive/claimed-by-human"}]}`))
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
// Copilot CLI session acting for the person) mirrors the generic
// `hive/claimed-by-human` — never the login — and replaces a hand-applied
// backend-named label and the retired per-login spelling; release removes
// the holder's labels (generic and legacy) and nothing else.
func TestClaimHooks_ClaimedByLabelIsGenericHumanNeverLogin(t *testing.T) {
	var mu sync.Mutex
	var added, removed []string
	// The release path re-reads the issue and removes only what it still
	// carries (RemoveLabelsFresh), so the mock keeps live label truth.
	truth := []string{"stage/010-actionable", "hive/claimed-by-copilot", "hive/claimed-by-clubanderson"}
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
		return has(added, claims.ClaimedByHumanLabel)
	}, "claim hook never added the generic human claimed-by label")
	testutil.Eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return has(removed, "hive/claimed-by-copilot") && has(removed, "hive/claimed-by-clubanderson")
	}, "claim hook never replaced the backend-named and per-login labels")
	mu.Lock()
	for _, l := range added {
		if claims.ClaimedByLabelIsBackend(l) {
			t.Fatalf("backend name leaked into labels: %v", added)
		}
		if strings.Contains(strings.ToLower(l), "clubanderson") {
			t.Fatalf("login leaked into labels: %v", added)
		}
	}
	// The legacy label is re-applied by hand before release so the release
	// path proves it strips both spellings of the holder's label.
	truth = append(truth, "hive/claimed-by-clubanderson")
	removed = nil
	mu.Unlock()

	hooks.OnReleased(claim, "released by clubanderson")
	testutil.Eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return has(removed, claims.ClaimedByHumanLabel) && has(removed, "hive/claimed-by-clubanderson")
	}, "release never removed the holder's generic and legacy claimed-by labels")
}

// A claim's GitHub side effects and the release of that same claim used to
// run in independent goroutines. On hivecommons/hive#14065 the lease was
// revoked 26s after the claim while the claim's goroutine was still working
// through its stage write: the release re-read the issue, saw no
// `hive/claimed-by-human` yet, finished, and THEN the claim applied the
// label — orphaned (hivecommons/hive#14201). The hooks now run one issue's
// transitions in ledger order, so the release's fresh read happens after
// the claim's label write and strips it. The mock delays the claimed-by add
// to reproduce the ordering.
func TestClaimHooks_ReleaseWaitsForSlowClaimLabel(t *testing.T) {
	var mu sync.Mutex
	var removed []string
	truth := []string{"stage/010-actionable"}
	has := func(list []string, want string) bool {
		for _, l := range list {
			if l == want {
				return true
			}
		}
		return false
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/11", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		labels := make([]map[string]string, 0, len(truth))
		for _, l := range truth {
			labels = append(labels, map[string]string{"name": l})
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 11, "labels": labels})
	})
	mux.HandleFunc("/repos/acme/widgets/issues/11/labels", func(w http.ResponseWriter, r *http.Request) {
		var body []string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if has(body, claims.ClaimedByHumanLabel) {
			// The slow write: without ordering the release's read lands
			// well inside this window.
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		mu.Lock()
		truth = append(truth, body...)
		mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/repos/acme/widgets/issues/11/labels/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/issues/11/labels/")
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
	claim := claims.Claim{Repo: "acme/widgets", Issue: 11, Holder: "arteeh", HolderID: "c-1", Kind: claims.KindContributor}
	hooks.OnClaimed(claim, claims.OutcomeClaimed)
	hooks.OnReleased(claim, "lease revoked")

	testutil.Eventually(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return has(removed, claims.ClaimedByHumanLabel)
	}, "release never removed the claimed-by label its own claim wrote")
	mu.Lock()
	defer mu.Unlock()
	if has(truth, claims.ClaimedByHumanLabel) {
		t.Fatalf("labels after release = %v, want %s gone", truth, claims.ClaimedByHumanLabel)
	}
}

// claimEffectQueue runs one key's work in submission order and different
// keys concurrently.
func TestClaimEffectQueue_OrdersPerKey(t *testing.T) {
	q := &claimEffectQueue{}
	var mu sync.Mutex
	var order []string
	firstDone := make(chan struct{})
	otherRan := make(chan struct{})
	q.run("a", func() {
		// Block until the other key's work has run: proves keys are
		// independent and that "a"'s second step waits for this one.
		<-otherRan
		mu.Lock()
		order = append(order, "a1")
		mu.Unlock()
		close(firstDone)
	})
	q.run("a", func() {
		mu.Lock()
		order = append(order, "a2")
		mu.Unlock()
	})
	q.run("b", func() {
		mu.Lock()
		order = append(order, "b1")
		mu.Unlock()
		close(otherRan)
	})
	<-firstDone
	// Wait for both the work and the worker's idle-key cleanup.
	testutil.Eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(order) != 3 {
			return false
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.pending) == 0
	}, "queued work never finished or idle keys were retained")
	mu.Lock()
	defer mu.Unlock()
	if order[0] != "b1" || order[1] != "a1" || order[2] != "a2" {
		t.Fatalf("order = %v, want [b1 a1 a2]", order)
	}
}

func TestClaimEffectQueue_ChurnDoesNotSpawnWaitingGoroutines(t *testing.T) {
	q := &claimEffectQueue{}
	blocked := make(chan struct{})
	started := make(chan struct{})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(blocked) }) }
	defer release()
	q.run("busy", func() {
		close(started)
		<-blocked
	})
	<-started

	before := runtime.NumGoroutine()
	const transitions = 2048
	var mu sync.Mutex
	var order []int
	for i := 0; i < transitions; i++ {
		q.run("busy", func() {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		})
	}
	// Allow unrelated runtime/HTTP goroutines some headroom. The old
	// goroutine chain adds one waiting goroutine for every transition.
	if growth := runtime.NumGoroutine() - before; growth > 32 {
		t.Errorf("one blocked issue added %d goroutines for %d transitions", growth, transitions)
	}
	release()
	testutil.Eventually(t, 5*time.Second, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.pending) == 0
	}, "churn worker did not drain and remove its idle key")
	mu.Lock()
	defer mu.Unlock()
	if len(order) != transitions {
		t.Fatalf("executed %d transitions, want %d (including the final release)", len(order), transitions)
	}
	for i, got := range order {
		if got != i {
			t.Fatalf("transition %d = %d, want FIFO order", i, got)
		}
	}
}

func TestClaimEffectQueue_RestartsAfterIdle(t *testing.T) {
	q := &claimEffectQueue{}
	for i := 0; i < 20; i++ {
		done := make(chan struct{})
		q.run("same", func() { close(done) })
		<-done
		testutil.Eventually(t, 5*time.Second, func() bool {
			q.mu.Lock()
			defer q.mu.Unlock()
			return len(q.pending) == 0
		}, "idle key was retained")
	}
}
