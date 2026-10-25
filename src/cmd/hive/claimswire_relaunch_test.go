package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A relaunched agent's CLI context is gone with the process, so the claims
// it held are released at once (hivecommons/hive 2026-10-20: #13083 sat at
// stage/020-claimed by the scanner for two hours across two rollouts).
func TestReleaseClaimsOnAgentRelaunch_ReleasesOnlyThatAgentsClaims(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, restoreTestLogger(), crux.ProjectContext{})
	var mu sync.Mutex
	var released []string
	l, err := claims.New("", claims.DefaultPolicy(), claims.Hooks{OnReleased: func(c claims.Claim, reason string) {
		mu.Lock()
		defer mu.Unlock()
		released = append(released, c.Key()+":"+reason)
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []claims.Request{
		{Repo: "acme/hive", Issue: 13083, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent},
		{Repo: "acme/hive", Issue: 13093, Holder: "quality", HolderID: "quality", Kind: claims.KindAgent},
		{Repo: "acme/hive", Issue: 1, Holder: "scanner", HolderID: "scanner", Kind: claims.KindHuman},
	} {
		if _, err := l.Claim(req); err != nil {
			t.Fatal(err)
		}
	}
	// The manager wiring is nil-safe; the observer body is exercised directly
	// because a real relaunch needs a tmux pane.
	releaseClaimsOnAgentRelaunch(mgr, func() *claims.Ledger { return l }, restoreTestLogger())
	releaseClaimsOnAgentRelaunch(nil, func() *claims.Ledger { return l }, nil)
	releaseClaimsOnAgentRelaunch(mgr, nil, nil)

	agentRelaunchReleaser(func() *claims.Ledger { return l }, restoreTestLogger())("scanner")
	mu.Lock()
	got := append([]string(nil), released...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "acme/hive#13083:"+claims.ReasonHolderRelaunched {
		t.Fatalf("released = %v", got)
	}
	if _, held := l.Lookup("acme/hive", 13093); !held {
		t.Fatal("another agent's claim must survive")
	}
	if _, held := l.Lookup("acme/hive", 1); !held {
		t.Fatal("a human claim must survive an agent relaunch")
	}

	// A ledger that is not (yet) built releases nothing.
	agentRelaunchReleaser(func() *claims.Ledger { return nil }, nil)("quality")
	if _, held := l.Lookup("acme/hive", 13093); !held {
		t.Fatal("observer over a nil ledger must release nothing")
	}
	// No claims, no log: the nil-logger path.
	agentRelaunchReleaser(func() *claims.Ledger { return l }, nil)("quality")
	if _, held := l.Lookup("acme/hive", 13093); held {
		t.Fatal("quality's claim should now be released")
	}
}

// The 🔒 marker comment is never edited, so a release that ends the claim
// before its until= must post the release marker the forge reader honours
// (issueclaim.Latest); a plain expiry posts nothing — the marker lapses by
// itself.
func TestClaimHooks_RelaunchReleasePostsReleaseMarker(t *testing.T) {
	var mu sync.Mutex
	var comments []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":7,"labels":[{"name":"stage/020-claimed"},{"name":"hive/claimed-by-scanner"}]}`))
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		comments = append(comments, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gh := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := &config.Config{}
	cfg.Governor.Claims.Label = boolPtr(false)
	cfg.Governor.Claims.Comment = boolPtr(true)
	hooks := githubClaimHooks(context.Background(), cfg, func() *github.Client { return gh }, nil)
	claim := claims.Claim{Repo: "acme/widgets", Issue: 7, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent}

	// The expiry release is fired first; if it commented, its body would
	// land before (or beside) the relaunch marker asserted below.
	hooks.OnReleased(claim, "expired")
	hooks.OnReleased(claim, claims.ReasonHolderRelaunched)
	testutil.Eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(comments) >= 1
	}, "relaunch release never posted the release marker")
	mu.Lock()
	got := append([]string(nil), comments...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expiry must not comment; got %d comments: %v", len(got), got)
	}
	if !strings.Contains(got[0], "hive:release who=scanner kind=agent reason=holding_agent_relaunched at=") {
		t.Fatalf("release comment:\n%s", got[0])
	}
}
