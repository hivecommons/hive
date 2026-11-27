package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestReviewConversationBlocker(t *testing.T) {
	var resolved, pageFailed atomic.Bool
	var optional atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/rules/branches/main":
			fmt.Fprint(w, `[{"type":"pull_request","parameters":{"required_review_thread_resolution":true}}]`)
		case "/repos/o/r/rules/branches/optional":
			fmt.Fprint(w, `[{"type":"pull_request","parameters":{"required_review_thread_resolution":false}}]`)
		case "/repos/o/r/branches/optional/protection":
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
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
				fmt.Fprint(w, `{"data":{"repository":{"pullRequests":{"nodes":[{"number":5,"reviewThreads":{"totalCount":2}}]}}}}`)
				return
			}
			base := "main"
			if optional.Load() {
				base = "optional"
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
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	pr := PullRequest{Repo: "o/r", Number: 5, BaseRef: "main", HeadSHA: "head", MergeableState: "blocked"}
	refresh := func(want string) {
		t.Helper()
		newProtectionCollector(c).attach(context.Background(), &pr, nil)
		if got, ok := pr.BranchProtectionBlockReason(); got != want || ok != (want != "") {
			t.Fatalf("merge blocker = (%q, %v), want %q", got, ok, want)
		}
	}
	refresh("1 unresolved review thread")
	if ids := pr.Protection.reviewThreadCommentIDs; len(ids) != 1 || ids[0] != 3000000000 {
		t.Fatalf("unresolved discussion links = %v", ids)
	}
	pageFailed.Store(true)
	refresh("") // The first page alone is not a complete count.
	pageFailed.Store(false)
	resolved.Store(true)
	refresh("") // Other branch rules can keep mergeStateStatus BLOCKED.
	resolved.Store(false)
	optional.Store(true)
	pr.BaseRef = "optional"
	refresh("")
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
	c.PostReviewConversationBlockers(ctx, prs)
	c.PostReviewConversationBlockers(ctx, prs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creates != 1 || s.edits != 0 {
		t.Fatalf("notice created %d times and edited %d times, want one create", s.creates, s.edits)
	}
	body, _ := s.comments[0]["body"].(string)
	if section, ok := prstatus.Get(body, reviewConversationStatusSection); !ok || !strings.Contains(section, "#discussion_r3000000000") || !strings.Contains(section, "1 unresolved review thread") {
		t.Fatalf("status comment did not identify the blocking discussion: %q", body)
	}
}
