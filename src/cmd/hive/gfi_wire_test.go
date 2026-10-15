package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/github"
)

func gfiTestConfig() *config.Config {
	cfg := &config.Config{HiveID: "acme-hive"}
	cfg.Project.Org = "acme"
	cfg.GoodFirstIssue.Enabled = true
	cfg.Dashboard.PublicURL = "https://acme.hive.example/"
	return cfg
}

func TestGFIPublicOrigin(t *testing.T) {
	if gfiPublicOrigin(nil) != "" {
		t.Fatal("nil cfg")
	}
	cfg := &config.Config{}
	if gfiPublicOrigin(cfg) != "" {
		t.Fatal("empty cfg should have no origin")
	}
	cfg.Hub.DashboardURL = "https://hub-assigned.example/"
	if got := gfiPublicOrigin(cfg); got != "https://hub-assigned.example" {
		t.Fatalf("hub url: %q", got)
	}
	cfg.Dashboard.PublicURL = "https://public.example"
	if got := gfiPublicOrigin(cfg); got != "https://public.example" {
		t.Fatalf("public url should win: %q", got)
	}
}

func TestGFICandidatesProjection(t *testing.T) {
	now := time.Now()
	res := &github.ActionableResult{Issues: github.IssueResult{Items: []github.Issue{
		{Repo: "widgets", Number: 1, Title: "a", Body: "b", Labels: []string{"Good First Issue"}, CreatedAt: now},
		{Repo: "acme/other", Number: 2, Title: "c", Labels: []string{"bug"}, CreatedAt: now},
		{Repo: "widgets", Number: 0, Title: "linear", SourceType: "linear"},
		{Repo: "widgets", Number: 3, State: "closed"},
		{Repo: "widgets", Number: 4, SourceType: "linear", ExternalID: "LIN-4"},
	}}}
	cands, labelled := gfiCandidates("acme", res, nil)
	if len(cands) != 2 || cands[0].Repo != "acme/widgets" || cands[1].Repo != "acme/other" {
		t.Fatalf("cands %+v", cands)
	}
	if !labelled["acme/widgets#1"] || labelled["acme/other#2"] {
		t.Fatalf("labelled %v", labelled)
	}
	only, _ := gfiCandidates("acme", res, func(repo string) bool { return repo == "acme/other" })
	if len(only) != 1 || only[0].Number != 2 {
		t.Fatalf("filter %+v", only)
	}
	if c, l := gfiCandidates("acme", nil, nil); c != nil || len(l) != 0 {
		t.Fatal("nil result")
	}
}

// gfiFakeGitHub serves the handful of endpoints the sweep and release touch.
type gfiFakeGitHub struct {
	mu       sync.Mutex
	comments []map[string]any
	added    []string
	removed  []string
	ensured  []string
	agentsMD bool
}

func (f *gfiFakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/contents/AGENTS.md"):
			if !f.agentsMD {
				http.Error(w, "nope", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "encoding": "base64", "content": "IyBBR0VOVFM=", "name": "AGENTS.md", "path": "AGENTS.md"})
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/issues/7/comments"):
			_ = json.NewEncoder(w).Encode(f.comments)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/issues/7/comments"):
			b, _ := io.ReadAll(r.Body)
			var in struct{ Body string }
			_ = json.Unmarshal(b, &in)
			f.comments = append(f.comments, map[string]any{"id": len(f.comments) + 1, "body": in.Body})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.comments), "body": in.Body})
		case r.Method == http.MethodPatch && strings.Contains(p, "/issues/comments/"):
			b, _ := io.ReadAll(r.Body)
			var in struct{ Body string }
			_ = json.Unmarshal(b, &in)
			f.comments[0]["body"] = in.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": in.Body})
		case r.Method == http.MethodGet && strings.Contains(p, "/labels/"):
			f.ensured = append(f.ensured, p[strings.LastIndex(p, "/")+1:])
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "x"})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/issues/7/labels"):
			b, _ := io.ReadAll(r.Body)
			var in []string
			_ = json.Unmarshal(b, &in)
			f.added = append(f.added, in...)
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodDelete && strings.Contains(p, "/issues/7/labels/"):
			f.removed = append(f.removed, p[strings.LastIndex(p, "/")+1:])
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, r.Method+" "+p, http.StatusNotFound)
		}
	})
}

func TestGFISweepReleaseAndConvertWiring(t *testing.T) {
	fake := &gfiFakeGitHub{agentsMD: true}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	logger := slog.New(slog.DiscardHandler)
	gh := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	cfg := gfiTestConfig()
	state := newGFIState()
	body := strings.Repeat("The last page is dropped when total is a multiple of page size. ", 2) +
		"\n\n## Acceptance criteria\n- `pkg/paginate.Pages(10, 5)` returns 2\n- add a table test\n"
	last := &github.ActionableResult{Issues: github.IssueResult{Items: []github.Issue{
		{Repo: "widgets", Number: 7, Title: "Fix off-by-one", Body: body, Labels: []string{"kind/bug", "size/S"}, CreatedAt: time.Now().Add(-48 * time.Hour)},
	}}}

	// Disabled: nothing happens.
	cfg.GoodFirstIssue.Enabled = false
	var lastRun time.Time
	runGoodFirstIssueSweepIfDue(context.Background(), gh, cfg, nil, state, last, &lastRun, logger)
	if len(fake.added) != 0 || !lastRun.IsZero() {
		t.Fatal("disabled sweep acted")
	}
	cfg.GoodFirstIssue.Enabled = true

	// No public URL: skipped, but the interval is still armed.
	cfg.Dashboard.PublicURL = ""
	runGoodFirstIssueSweepIfDue(context.Background(), gh, cfg, nil, state, last, &lastRun, logger)
	if len(fake.added) != 0 || lastRun.IsZero() {
		t.Fatal("no-public-url sweep acted or did not arm")
	}
	cfg.Dashboard.PublicURL = "https://acme.hive.example"

	// Due → labels + comment, AGENTS.md link detected.
	lastRun = time.Time{}
	runGoodFirstIssueSweepIfDue(context.Background(), gh, cfg, nil, state, last, &lastRun, logger)
	if strings.Join(fake.added, ",") != gfi.Label+","+gfi.HelpWantedLabel || len(fake.comments) != 1 {
		t.Fatalf("added=%v comments=%d", fake.added, len(fake.comments))
	}
	c := fake.comments[0]["body"].(string)
	if !strings.Contains(c, "https://acme.hive.example/contribute?issue=acme%2Fwidgets%237") || !strings.Contains(c, "AGENTS.md") || !strings.Contains(c, "acme-hive picked") {
		t.Fatalf("comment: %s", c)
	}
	if !state.known("acme/widgets#7") || state.sweeper.Stats().Selected != 1 {
		t.Fatalf("state: known=%v stats=%+v", state.known("acme/widgets#7"), state.sweeper.Stats())
	}
	// Not due again within the interval.
	runGoodFirstIssueSweepIfDue(context.Background(), gh, cfg, nil, state, last, &lastRun, logger)
	if len(fake.comments) != 1 {
		t.Fatal("second comment within interval")
	}

	// Ledger: a contributor claim on the labelled issue is a conversion; a
	// claim on some other issue is not.
	var baseCalled int
	hooks := gfiClaimHooks(context.Background(), cfg, state, func() *github.Client { return gh }, nil, logger,
		claims.Hooks{OnClaimed: func(claims.Claim, claims.Outcome) { baseCalled++ }})
	hooks.OnClaimed(claims.Claim{Repo: "acme/widgets", Issue: 7, Holder: "octocat", Kind: claims.KindContributor}, claims.OutcomeClaimed)
	hooks.OnClaimed(claims.Claim{Repo: "acme/widgets", Issue: 8, Holder: "octocat", Kind: claims.KindContributor}, claims.OutcomeClaimed)
	hooks.OnClaimed(claims.Claim{Repo: "acme/widgets", Issue: 7, Holder: "octocat", Kind: claims.KindContributor}, claims.OutcomeRenewed)
	if baseCalled != 3 || state.sweeper.Stats().Converted != 1 {
		t.Fatalf("base=%d stats=%+v", baseCalled, state.sweeper.Stats())
	}

	// A hive agent start on an unknown issue is ignored without API calls.
	release := gfiReleaseOnHiveStart(context.Background(), cfg, state, func() *github.Client { return gh }, nil, logger)
	release("worker", "widgets", 99, github.AgentStartSignalClaim)
	// On the labelled issue: labels off, comment superseded.
	release("worker", "widgets", 7, github.AgentStartSignalClaim)
	testutil.Eventually(t, 5*time.Second, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.removed) == 2 && len(fake.comments) > 0 && strings.Contains(fake.comments[0]["body"].(string), gfi.SupersededMarker)
	}, "release did not remove labels and supersede the comment")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if strings.Join(fake.removed, ",") != "good%20first%20issue,help%20wanted" && strings.Join(fake.removed, ",") != gfi.Label+","+gfi.HelpWantedLabel {
		t.Fatalf("removed %v", fake.removed)
	}
	if !strings.Contains(fake.comments[0]["body"].(string), gfi.SupersededMarker) {
		t.Fatalf("comment not superseded: %v", fake.comments[0]["body"])
	}
	if state.known("acme/widgets#7") || state.sweeper.Stats().Released != 1 {
		t.Fatalf("state after release: %+v", state.sweeper.Stats())
	}
}
