package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// resetPRReachCaches clears the package-level caches between tests.
func resetPRReachCaches() {
	prReachCacheMu.Lock()
	mergedPRCache = map[string]MergedPR{}
	prFilesCache = map[string][]string{}
	recentMergedCache = map[string]recentMergedEntry{}
	prReachCacheMu.Unlock()
}

// TestMergedPRAndFilesCached: merged-PR facts and the paginated file list
// are fetched once and answered from cache afterwards (they are immutable),
// and the file list follows the Link-header pagination to completion.
func TestMergedPRAndFilesCached(t *testing.T) {
	resetPRReachCaches()
	prGets, fileGets := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/hivecommons/hive/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		prGets++
		fmt.Fprint(w, `{"number":42,"title":"reach 2b","merged_at":"2026-08-10T12:00:00Z","merge_commit_sha":"cafe1234","changed_files":3}`)
	})
	mux.HandleFunc("GET /repos/hivecommons/hive/pulls/42/files", func(w http.ResponseWriter, r *http.Request) {
		fileGets++
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"filename":"v2/proxy/mitm.go"}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/hivecommons/hive/pulls/42/files?page=2>; rel="next"`, r.Host))
		fmt.Fprint(w, `[{"filename":"v2/pkg/hub/saas.go"},{"filename":"docs/x.md"}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := NewClientForTest(server.URL, "hivecommons", []string{"hive"}, slog.Default())

	pr, err := c.MergedPR(context.Background(), "hivecommons", "hive", 42)
	if err != nil {
		t.Fatalf("MergedPR: %v", err)
	}
	wantMerged := MergedPR{Number: 42, Title: "reach 2b", MergeCommitSHA: "cafe1234",
		MergedAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	if pr != wantMerged {
		t.Errorf("MergedPR = %+v, want %+v", pr, wantMerged)
	}

	files, err := c.ListMergedPRFiles(context.Background(), "hivecommons", "hive", 42)
	if err != nil {
		t.Fatalf("ListMergedPRFiles: %v", err)
	}
	if want := []string{"v2/pkg/hub/saas.go", "docs/x.md", "v2/proxy/mitm.go"}; !reflect.DeepEqual(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
	if fileGets != 2 {
		t.Errorf("file pages fetched = %d, want 2 (pagination)", fileGets)
	}

	// Immutable facts: repeat calls are cache hits, zero new requests.
	prevPR, prevFiles := prGets, fileGets
	if _, err := c.MergedPR(context.Background(), "hivecommons", "hive", 42); err != nil {
		t.Fatalf("cached MergedPR: %v", err)
	}
	if _, err := c.ListMergedPRFiles(context.Background(), "hivecommons", "hive", 42); err != nil {
		t.Fatalf("cached ListMergedPRFiles: %v", err)
	}
	if prGets != prevPR || fileGets != prevFiles {
		t.Errorf("cached calls hit the API (pr %d→%d, files %d→%d)", prevPR, prGets, prevFiles, fileGets)
	}
}

// TestMergedPRNotMerged: an unmerged PR is an error for both facts and
// files — treating it as merged would fabricate a merge time, and its file
// list is still mutable so caching it would go stale.
func TestMergedPRNotMerged(t *testing.T) {
	resetPRReachCaches()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/hivecommons/hive/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":7,"title":"open","changed_files":1}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := NewClientForTest(server.URL, "hivecommons", []string{"hive"}, slog.Default())

	if _, err := c.MergedPR(context.Background(), "hivecommons", "hive", 7); err == nil {
		t.Error("MergedPR on open PR: want error")
	}
	if _, err := c.ListMergedPRFiles(context.Background(), "hivecommons", "hive", 7); err == nil {
		t.Error("ListMergedPRFiles on open PR: want error")
	}
}

// TestListMergedPRFilesIncomplete: when GitHub reports more changed files
// than the files endpoint returned (truncation on huge PRs), the call FAILS
// rather than silently under-attributing components.
func TestListMergedPRFilesIncomplete(t *testing.T) {
	resetPRReachCaches()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/hivecommons/hive/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"merged_at":"2026-08-10T12:00:00Z","merge_commit_sha":"beef","changed_files":5}`)
	})
	mux.HandleFunc("GET /repos/hivecommons/hive/pulls/9/files", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"filename":"a.go"}]`) // 1 of a reported 5
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := NewClientForTest(server.URL, "hivecommons", []string{"hive"}, slog.Default())

	if _, err := c.ListMergedPRFiles(context.Background(), "hivecommons", "hive", 9); err == nil {
		t.Fatal("want incomplete-file-list error, got nil")
	}
}

// TestRecentMergedPRs: closed-but-unmerged PRs are filtered out, the limit
// is honored, and the answer is TTL-cached.
func TestRecentMergedPRs(t *testing.T) {
	resetPRReachCaches()
	listGets := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/hivecommons/hive/pulls", func(w http.ResponseWriter, r *http.Request) {
		listGets++
		if got := r.URL.Query().Get("base"); got != "v4" {
			t.Errorf("base = %q, want v4", got)
		}
		fmt.Fprint(w, `[
			{"number":30,"title":"newest","merged_at":"2026-08-12T00:00:00Z","merge_commit_sha":"c30"},
			{"number":29,"title":"closed-not-merged"},
			{"number":28,"title":"older","merged_at":"2026-08-11T00:00:00Z","merge_commit_sha":"c28"},
			{"number":27,"title":"oldest","merged_at":"2026-08-10T00:00:00Z","merge_commit_sha":"c27"}
		]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := NewClientForTest(server.URL, "hivecommons", []string{"hive"}, slog.Default())

	prs, err := c.RecentMergedPRs(context.Background(), "hivecommons", "hive", "v4", 2)
	if err != nil {
		t.Fatalf("RecentMergedPRs: %v", err)
	}
	if len(prs) != 2 || prs[0].Number != 30 || prs[1].Number != 28 {
		t.Errorf("prs = %+v, want #30 then #28 (unmerged #29 skipped)", prs)
	}

	// TTL cache: an immediate repeat answers without a new request.
	prev := listGets
	if _, err := c.RecentMergedPRs(context.Background(), "hivecommons", "hive", "v4", 2); err != nil {
		t.Fatalf("cached RecentMergedPRs: %v", err)
	}
	if listGets != prev {
		t.Errorf("cached listing hit the API (%d→%d)", prev, listGets)
	}
}
