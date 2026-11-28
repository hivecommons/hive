package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestReviewConversationBlocker(t *testing.T) {
	ctx := context.Background()
	status := &statusCommentServer{}
	comments := status.start(t).Config.Handler
	var resolved, pageFailed atomic.Bool
	var optional, policyUnreadable, slashBase atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.EscapedPath(), "%25") {
			t.Errorf("branch name escaped twice: %s", r.URL.EscapedPath())
		}
		switch r.URL.Path {
		case "/repos/o/r/rules/branches/release/x":
			if r.URL.EscapedPath() != "/repos/o/r/rules/branches/release%2Fx" {
				t.Errorf("rules path = %s, want the branch escaped once", r.URL.EscapedPath())
			}
			fmt.Fprint(w, `[]`)
		case "/repos/o/r/branches/release/x/protection":
			if r.URL.EscapedPath() != "/repos/o/r/branches/release%2Fx/protection" {
				t.Errorf("protection path = %s, want the branch escaped once", r.URL.EscapedPath())
			}
			fmt.Fprint(w, `{"required_conversation_resolution":{"enabled":true}}`)
		case "/repos/o/r/rules/branches/main":
			fmt.Fprint(w, `[{"type":"pull_request","parameters":{"required_review_thread_resolution":true}}]`)
		case "/repos/o/r/rules/branches/optional":
			fmt.Fprint(w, `[{"type":"pull_request","parameters":{"required_review_thread_resolution":false}}]`)
		case "/repos/o/r/branches/optional/protection":
			if policyUnreadable.Load() {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
			} else {
				http.Error(w, `{"message":"Branch not protected"}`, http.StatusNotFound)
			}
		case "/graphql":
			var req struct {
				Query     string
				Variables map[string]any
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				http.Error(w, `{"message":"invalid request"}`, http.StatusBadRequest)
				return
			}
			if strings.Contains(req.Query, "pullRequests(") {
				fmt.Fprint(w, `{"data":{"repository":{"pullRequests":{"nodes":[{"number":5,"comments":{"totalCount":1},"reviewThreads":{"totalCount":2}}]}}}}`)
				return
			}
			base := "main"
			if optional.Load() {
				base = "optional"
			}
			if slashBase.Load() {
				base = "release/x"
			}
			switch req.Variables["after"] {
			case nil:
				fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"headRefOid":"head","baseRefName":%q,"mergeStateStatus":"BLOCKED","reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"next"},"nodes":[{"isResolved":%t,"isOutdated":true,"comments":{"nodes":[{"url":"https://github.com/o/r/pull/5#discussion_r3000000000"}]}}]}}}}}`, base, resolved.Load())
			case "next":
				if pageFailed.Load() {
					fmt.Fprint(w, `{"errors":[{"message":"page unavailable"}]}`)
					return
				}
				fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"headRefOid":"head","baseRefName":%q,"mergeStateStatus":"BLOCKED","reviewThreads":{"nodes":[{"isResolved":true}]}}}}}`, base)
			default:
				t.Errorf("unexpected cursor: %v", req.Variables["after"])
				http.Error(w, `{"message":"unexpected cursor"}`, http.StatusBadRequest)
			}
		default:
			comments.ServeHTTP(w, r)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{}, ""))
	pr := PullRequest{Repo: "o/r", Number: 5, BaseRef: "main", HeadSHA: "head", MergeableState: "blocked", Author: "hive-app[bot]", AppAuthored: true}
	if _, err := c.UpsertPRStatusSection(ctx, pr.Repo, pr.Number, "other", "keep this section"); err != nil {
		t.Fatal(err)
	}
	checkNotice := func(want bool) {
		t.Helper()
		status.mu.Lock()
		defer status.mu.Unlock()
		if len(status.comments) != 1 {
			t.Fatalf("got %d comments, want one shared status comment", len(status.comments))
		}
		body, _ := status.comments[0]["body"].(string)
		if _, got := prstatus.Get(body, reviewConversationStatusSection); got != want {
			t.Fatalf("status comment has blocker section = %t, want %t: %q", got, want, body)
		}
		if section, _ := prstatus.Get(body, "other"); section != "keep this section" {
			t.Fatalf("unrelated section changed: %q", body)
		}
	}
	refresh := func(want string) {
		t.Helper()
		newProtectionCollector(c).attach(ctx, &pr, nil)
		if got, ok := pr.BranchProtectionBlockReason(); got != want || ok != (want != "") {
			t.Fatalf("merge blocker = (%q, %v), want %q", got, ok, want)
		}
		c.PostReviewConversationBlockers(ctx, []PullRequest{pr})
	}
	refresh("1 unresolved review thread")
	if ids := pr.Protection.reviewThreadCommentIDs; len(ids) != 1 || ids[0] != 3000000000 {
		t.Fatalf("unresolved discussion links = %v", ids)
	}
	checkNotice(true)
	pageFailed.Store(true)
	refresh("") // The first page alone is not a complete count.
	checkNotice(true)
	pageFailed.Store(false)
	resolved.Store(true)
	c.SetRepoPausedFunc(func(string) bool { return true })
	refresh("")
	checkNotice(true)
	c.SetRepoPausedFunc(nil)
	refresh("") // Other branch rules can keep mergeStateStatus BLOCKED.
	checkNotice(false)
	resolved.Store(false)
	refresh("1 unresolved review thread")
	pr.MergeableState = "unknown"
	refresh("")
	checkNotice(true)
	pr.MergeableState = "clean"
	refresh("")
	checkNotice(false)
	pr.MergeableState = "blocked"
	refresh("1 unresolved review thread")
	optional.Store(true)
	pr.BaseRef = "optional"
	policyUnreadable.Store(true)
	refresh("")
	checkNotice(true)
	policyUnreadable.Store(false)
	refresh("")
	checkNotice(false)
	// A slash-named base requiring resolution via classic protection.
	optional.Store(false)
	slashBase.Store(true)
	pr.BaseRef = "release/x"
	refresh("1 unresolved review thread")
	checkNotice(true)
}

// A PR with issue comments but no review threads cannot carry a notice, so
// the clear path must not list its comments every cycle.
func TestReviewConversationBlockerSkipsClearWithoutThreads(t *testing.T) {
	ctx := context.Background()
	var commentLists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/graphql":
			fmt.Fprint(w, `{"data":{"repository":{"pullRequests":{"nodes":[{"number":5,"comments":{"totalCount":3},"reviewThreads":{"totalCount":0}}]}}}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			commentLists.Add(1)
			fmt.Fprint(w, `[]`)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{}, ""))
	pr := PullRequest{Repo: "o/r", Number: 5, BaseRef: "main", HeadSHA: "head", MergeableState: "blocked", Author: "hive-app[bot]", AppAuthored: true}
	newProtectionCollector(c).attach(ctx, &pr, nil)
	if pr.CommentCount != 3 || pr.ReviewThreadCount != 0 {
		t.Fatalf("counts = %d comments / %d threads, want 3 / 0", pr.CommentCount, pr.ReviewThreadCount)
	}
	c.PostReviewConversationBlockers(ctx, []PullRequest{pr})
	if n := commentLists.Load(); n != 0 {
		t.Fatalf("listed issue comments %d times for a PR without review threads", n)
	}
}

func TestReviewConversationBlockerNotice(t *testing.T) {
	s := &statusCommentServer{}
	c := testClient(t, s.start(t).URL)
	c.SetAppBotLogin("hive-app[bot]")
	ctx := context.Background()
	prs := []PullRequest{{
		Repo: "o/r", Number: 5, Author: "hive-app[bot]", AppAuthored: true, MergeableState: "blocked",
		Protection: &ProtectionFacts{UnresolvedReviewThreads: 1, reviewThreadCommentIDs: []uint64{3000000000}},
	}}

	c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"o/r": {Mode: writeback.RepoModeOff}}}, writeback.Deps{}, ""))
	c.PostReviewConversationBlockers(ctx, prs)
	c.SetWritebackGate(nil)
	c.SetRepoPausedFunc(func(string) bool { return true })
	c.PostReviewConversationBlockers(ctx, prs)
	c.SetRepoPausedFunc(nil)
	s.mu.Lock()
	writes := s.creates + s.edits
	s.mu.Unlock()
	if writes != 0 {
		t.Fatalf("wrote %d notices despite a write-back restriction", writes)
	}
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{}, ""))
	c.PostReviewConversationBlockers(ctx, prs)
	c.PostReviewConversationBlockers(ctx, prs)
	// A different blocker can appear and then leave the original one open.
	// The earlier body must be allowed again to replace the intermediate one.
	prs[0].Protection.reviewThreadCommentIDs[0] = 4000000000
	c.PostReviewConversationBlockers(ctx, prs)
	prs[0].Protection.reviewThreadCommentIDs[0] = 3000000000
	c.PostReviewConversationBlockers(ctx, prs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creates != 1 || s.edits != 2 {
		t.Fatalf("notice created %d times and edited %d times, want one create and two edits", s.creates, s.edits)
	}
	body, _ := s.comments[0]["body"].(string)
	if section, ok := prstatus.Get(body, reviewConversationStatusSection); !ok || !strings.Contains(section, "#discussion_r3000000000") || !strings.Contains(section, "1 unresolved review thread") {
		t.Fatalf("status comment did not identify the blocking discussion: %q", body)
	}
}

// Only a write that reached GitHub is a write outcome: a clear with nothing
// to remove must not mark the repo writable, and a 403 reading the comments
// must not record a forbidden write.
func TestReviewConversationBlockerWritebackOutcome(t *testing.T) {
	ctx := context.Background()
	var listForbidden atomic.Bool
	s := &statusCommentServer{}
	comments := s.start(t).Config.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if listForbidden.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments") {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
			return
		}
		comments.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	statePath := filepath.Join(t.TempDir(), "writeback.json")
	gate := writeback.New(writeback.Config{}, writeback.Deps{}, statePath)
	c.SetWritebackGate(gate)
	pr := PullRequest{Repo: "o/r", Number: 5, Author: "hive-app[bot]", AppAuthored: true, MergeableState: "clean", CommentCount: 1, reviewConversationKnown: true}
	writable := func() (bool, bool) {
		rs := gate.Snapshot().Repos["o/r"]
		state, _ := os.ReadFile(statePath) // Written only when the gate records an outcome.
		return rs.Writable, strings.Contains(string(state), "last_forbidden")
	}
	c.PostReviewConversationBlockers(ctx, []PullRequest{pr}) // Nothing to clear.
	if w, f := writable(); w || f {
		t.Fatalf("no-op clear recorded writable=%t forbidden=%t", w, f)
	}
	listForbidden.Store(true)
	c.PostReviewConversationBlockers(ctx, []PullRequest{pr})
	if w, f := writable(); w || f {
		t.Fatalf("read 403 recorded writable=%t forbidden=%t", w, f)
	}
	listForbidden.Store(false)
	pr.MergeableState = "blocked"
	pr.Protection = &ProtectionFacts{UnresolvedReviewThreads: 1}
	c.PostReviewConversationBlockers(ctx, []PullRequest{pr})
	if w, _ := writable(); !w {
		t.Fatal("a posted notice did not mark the repo writable")
	}
}
