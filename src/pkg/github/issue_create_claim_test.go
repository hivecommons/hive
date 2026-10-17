package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// #11579 B.3: the effects claim guarding an issue create was keyed on the
// exact title, so two agents filing the same finding seconds apart with
// different model-authored titles both passed. The key is now the canonical
// subject plus the file-reference set.
func TestIssueCreateClaim_KeysOnCanonicalSubjectAndFileSet(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	a := issueCreateClaim("o/r",
		"[scanner] Split web/e2e/card-cache.spec.ts (1237 lines) — extract cache helpers",
		"web/e2e/card-cache.spec.ts:1 is too long; see web/e2e/helpers.ts", now)
	b := issueCreateClaim("o/r",
		"[scanner] Split web/e2e/card-cache.spec.ts (1237 lines) -- pull out snapshot helpers",
		"Totally different prose. web/e2e/helpers.ts and web/e2e/card-cache.spec.ts:900.", now)
	if a.Target != b.Target {
		t.Fatalf("same subject and file set must share a claim target:\n%s\n%s", a.Target, b.Target)
	}
	if a.Inputs["subject"] != b.Inputs["subject"] || a.Inputs["files"] != b.Inputs["files"] || a.Inputs["window"] != b.Inputs["window"] {
		t.Fatalf("same subject and file set must share journal inputs:\n%v\n%v", a.Inputs, b.Inputs)
	}
	for _, k := range []string{"body", "labels", "title"} {
		if _, ok := a.Inputs[k]; ok {
			t.Errorf("input %q must not be part of the key: it varies between filings of one finding", k)
		}
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("claim must validate: %v", err)
	}

	extraFile := issueCreateClaim("o/r",
		"[scanner] Split web/e2e/card-cache.spec.ts (1237 lines)",
		"web/e2e/card-cache.spec.ts, web/e2e/helpers.ts and web/e2e/fixtures.ts", now)
	if extraFile.Target == a.Target {
		t.Error("a finding citing one more file is a different finding and must not share the target")
	}
	otherSubject := issueCreateClaim("o/r",
		"[scanner] Delete web/e2e/card-cache.spec.ts entirely",
		"web/e2e/card-cache.spec.ts:1 and web/e2e/helpers.ts", now)
	if otherSubject.Target == a.Target {
		t.Error("a different canonical subject over the same files must not share the target")
	}
	later := issueCreateClaim("o/r",
		"[scanner] Split web/e2e/card-cache.spec.ts (1237 lines) — extract cache helpers",
		"web/e2e/card-cache.spec.ts:1 is too long; see web/e2e/helpers.ts", now.Add(issueCreateIdempotencyWindow+time.Hour))
	if later.Target != a.Target {
		t.Error("the ledger target must not depend on time: concurrent creators always serialise")
	}
	if later.Inputs["window"] == a.Inputs["window"] {
		t.Error("a filing past the idempotency window must mint a fresh logical operation, not replay forever")
	}
}

// A title too short or generic to yield a canonical subject, with no file
// references, keeps the exact title as its key — the pre-#11579 behaviour —
// rather than collapsing unrelated short titles onto one claim.
func TestIssueCreateClaim_FallsBackToExactTitle(t *testing.T) {
	now := time.Now()
	a := issueCreateClaim("o/r", "CI failure", "nothing citable", now)
	b := issueCreateClaim("o/r", "CI flake", "nothing citable", now)
	if a.Target == b.Target {
		t.Fatal("distinct short titles without a canonical subject must not share a target")
	}
	if a.Target != issueCreateClaim("o/r", "  CI failure ", "other prose", now).Target {
		t.Error("the fallback key is the trimmed exact title")
	}
}

func TestParseIssueNumberFromURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"https://github.com/o/r/issues/99", 99, true},
		{"https://ghe.example.com/api/v3/o/r/issues/7#issuecomment-1", 7, true},
		{"https://github.com/o/r/pull/99", 0, false},
		{"", 0, false},
		{"https://github.com/o/r/issues/0", 0, false},
	} {
		got, err := parseIssueNumberFromURL(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseIssueNumberFromURL(%q) = %d, %v; want %d, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// The scan→create window: both agents' dedupe scans came back empty (the
// server lists no open issues), so only the journal can stop the second
// create. The second filing has a different title qualifier and body but the
// same subject and files; it must not POST, and the caller must get the
// first issue back rather than a zero number with a nil error.
func TestCreateIssue_SameFindingRewordedReplaysJournaledIssue(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/labels/agent/scanner":
			fmt.Fprint(w, `{"name":"agent/scanner"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"number":99,"id":5001,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetMutationBoundary(replayTestBoundary(t))

	first, err := c.CreateIssue(context.Background(), CreateIssueRequest{Repo: "o/r", Title: "[scanner] Split web/e2e/card-cache.spec.ts (1237 lines) — extract cache helpers", Body: "web/e2e/card-cache.spec.ts:1 is too long; see web/e2e/helpers.ts", Labels: []string{"agent/scanner"}, Origin: "governor"})
	if err != nil {
		t.Fatalf("first CreateIssue: %v", err)
	}
	if first.Number != 99 || first.AlreadyExisted {
		t.Fatalf("first CreateIssue = %+v, want a fresh #99", first)
	}

	second, err := c.CreateIssue(context.Background(), CreateIssueRequest{Repo: "o/r", Title: "[scanner] Split web/e2e/card-cache.spec.ts (1237 lines) -- pull out snapshot helpers", Body: "Different prose entirely: web/e2e/helpers.ts and web/e2e/card-cache.spec.ts:900.", Labels: []string{"agent/scanner"}, Origin: "governor"})
	if err != nil {
		t.Fatalf("second CreateIssue must be idempotent success, got %v", err)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("POST /issues ran %d times, want 1 (the journal must replay the reworded twin)", got)
	}
	if second.Number != 99 || second.URL != first.URL || !second.AlreadyExisted {
		t.Fatalf("second CreateIssue = %+v, want #99 reported as AlreadyExisted", second)
	}

	third, err := c.CreateIssue(context.Background(), CreateIssueRequest{Repo: "o/r", Title: "[scanner] Split web/e2e/card-cache.spec.ts (1237 lines) — and web/e2e/fixtures.ts too", Body: "web/e2e/card-cache.spec.ts, web/e2e/helpers.ts and web/e2e/fixtures.ts", Labels: []string{"agent/scanner"}, Origin: "governor"})
	if err != nil {
		t.Fatalf("third CreateIssue: %v", err)
	}
	if got := creates.Load(); got != 2 || third.AlreadyExisted {
		t.Fatalf("a finding over a different file set must create: POSTs=%d result=%+v", got, third)
	}
}

// The idempotency bucket is epoch-aligned, so a filing at 23:59:59Z and its
// reworded twin at 00:00:01Z are different logical operations; the exact-ID
// replay alone would let the second one create. The previous bucket is
// probed before executing, so the straddling pair is still one issue — and a
// filing two buckets later is a fresh operation.
func TestCreateIssue_SameFindingStraddlingBucketBoundaryReplays(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/labels/agent/scanner":
			fmt.Fprint(w, `{"name":"agent/scanner"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			n := creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d,"id":%d,"html_url":"https://github.example/o/r/issues/%d"}`, 100+n, 5000+n, 100+n)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetMutationBoundary(replayTestBoundary(t))

	midnight := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	clock := midnight.Add(-time.Second)
	prev := issueCreateNow
	issueCreateNow = func() time.Time { return clock }
	t.Cleanup(func() { issueCreateNow = prev })

	title := "[scanner] Split web/e2e/card-cache.spec.ts (1237 lines) — extract cache helpers"
	body := "web/e2e/card-cache.spec.ts:1 is too long; see web/e2e/helpers.ts"
	first, err := c.CreateIssue(context.Background(), CreateIssueRequest{Repo: "o/r", Title: title, Body: body, Labels: []string{"agent/scanner"}, Origin: "governor"})
	if err != nil {
		t.Fatalf("first CreateIssue: %v", err)
	}
	if first.Number != 101 || first.AlreadyExisted {
		t.Fatalf("first CreateIssue = %+v, want a fresh #101", first)
	}

	clock = midnight.Add(time.Second)
	second, err := c.CreateIssue(context.Background(), CreateIssueRequest{Repo: "o/r", Title: "[scanner] Split web/e2e/card-cache.spec.ts (1237 lines) -- pull out snapshot helpers", Body: "Different prose: web/e2e/helpers.ts and web/e2e/card-cache.spec.ts:900.", Labels: []string{"agent/scanner"}, Origin: "governor"})
	if err != nil {
		t.Fatalf("second CreateIssue must be idempotent success, got %v", err)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("POST /issues ran %d times, want 1 (the straddling twin must replay the previous bucket)", got)
	}
	if second.Number != 101 || second.URL != first.URL || !second.AlreadyExisted {
		t.Fatalf("second CreateIssue = %+v, want #101 reported as AlreadyExisted", second)
	}

	clock = midnight.Add(issueCreateIdempotencyWindow + time.Second)
	third, err := c.CreateIssue(context.Background(), CreateIssueRequest{Repo: "o/r", Title: title, Body: body, Labels: []string{"agent/scanner"}, Origin: "governor"})
	if err != nil {
		t.Fatalf("third CreateIssue: %v", err)
	}
	if got := creates.Load(); got != 2 || third.AlreadyExisted || third.Number != 102 {
		t.Fatalf("a filing two buckets on is a fresh operation: POSTs=%d result=%+v", got, third)
	}
}

// Advisory living issue: findAdvisoryIssue is search-then-create with no
// idempotency key, so two concurrent EnsureAdvisoryIssue callers whose
// searches both came back empty both created. The create now runs under the
// mutation boundary; the second caller replays the first's issue.
func TestEnsureAdvisoryIssue_ConcurrentCallersReplayJournaledIssue(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var creates atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"number": 7, "title": advisoryTitle,
				"html_url": fmt.Sprintf("https://github.example/%s/%s/issues/7", org, repo)})
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"full_name": org + "/" + repo, "has_issues": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetMutationBoundary(replayTestBoundary(t))

	first, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("first EnsureAdvisoryIssue: %v", err)
	}
	second, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("second EnsureAdvisoryIssue must be idempotent success, got %v", err)
	}
	if first != 7 || second != 7 {
		t.Fatalf("issue numbers = %d, %d; want 7, 7", first, second)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("POST /issues ran %d times, want 1 (the journal must replay the second create)", got)
	}
}
