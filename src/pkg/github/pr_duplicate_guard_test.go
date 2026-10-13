package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

type guardPR struct {
	Number int
	Ref    string
	Body   string
	User   string
	Merged bool
	Draft  bool
	Files  []string
}

type guardServer struct {
	open, closed []guardPR
	headFiles    []string
	// openIssues lists issue numbers GET /issues/N reports as open; every
	// other issue is closed.
	openIssues []int
	// existingComments is returned for every GET .../comments.
	existingComments []string
	// failStatus, when non-zero, is returned for any path containing failPath.
	// failMethod, when set, narrows the injected failure to that HTTP method so
	// a GET and a POST on the same path can fail independently.
	failPath   string
	failStatus int
	failMethod string

	mu       sync.Mutex
	created  int
	comments int
}

func (g *guardServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	enc := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	prJSON := func(p guardPR) map[string]any {
		m := map[string]any{
			"number": p.Number, "body": p.Body, "html_url": fmt.Sprintf("https://github.com/o/r/pull/%d", p.Number),
			"head": map[string]any{"ref": p.Ref, "sha": fmt.Sprintf("sha%d", p.Number)},
			"user": map[string]any{"login": p.User},
		}
		if p.Merged {
			m["merged_at"] = "2026-01-01T00:00:00Z"
		}
		if p.Draft {
			m["draft"] = true
		}
		return m
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if g.failStatus != 0 && g.failPath != "" && strings.Contains(path+"?"+r.URL.RawQuery, g.failPath) &&
			(g.failMethod == "" || g.failMethod == r.Method) {
			w.WriteHeader(g.failStatus)
			_, _ = io.WriteString(w, `{"message":"injected"}`)
			return
		}
		switch {
		case r.Method == "GET" && strings.Contains(path, "/git/ref/heads/"):
			enc(w, map[string]any{"ref": "x", "object": map[string]any{"sha": "headsha"}})
		case r.Method == "GET" && strings.Contains(path, "/git/commits/"):
			sha := path[strings.LastIndex(path, "/")+1:]
			enc(w, map[string]any{"sha": sha, "tree": map[string]any{"sha": "tree-" + sha}})
		case r.Method == "GET" && strings.Contains(path, "/compare/"):
			files := []map[string]any{}
			for _, f := range g.headFiles {
				files = append(files, map[string]any{"filename": f})
			}
			enc(w, map[string]any{"files": files})
		case r.Method == "GET" && strings.HasSuffix(path, "/files"):
			var num int
			fmt.Sscanf(strings.TrimPrefix(path, "/repos/o/r/pulls/"), "%d", &num)
			files := []map[string]any{}
			for _, p := range g.open {
				if p.Number == num {
					for _, f := range p.Files {
						files = append(files, map[string]any{"filename": f, "patch": "p"})
					}
				}
			}
			enc(w, files)
		case r.Method == "GET" && strings.HasSuffix(path, "/pulls"):
			if r.URL.Query().Get("head") != "" {
				enc(w, []any{})
				return
			}
			src := g.open
			if r.URL.Query().Get("state") == "closed" {
				src = g.closed
			}
			out := []map[string]any{}
			for _, p := range src {
				out = append(out, prJSON(p))
			}
			enc(w, out)
		case r.Method == "GET" && strings.HasSuffix(path, "/comments"):
			out := []map[string]any{}
			for i, body := range g.existingComments {
				out = append(out, map[string]any{"id": i + 1, "body": body})
			}
			enc(w, out)
		case r.Method == "GET" && strings.Contains(path, "/issues/"):
			var num int
			fmt.Sscanf(strings.TrimPrefix(path, "/repos/o/r/issues/"), "%d", &num)
			state := "closed"
			for _, n := range g.openIssues {
				if n == num {
					state = "open"
				}
			}
			enc(w, map[string]any{"number": num, "state": state})
		case r.Method == "POST" && strings.HasSuffix(path, "/comments"):
			g.mu.Lock()
			g.comments++
			g.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.Method == "POST" && strings.HasSuffix(path, "/pulls"):
			g.mu.Lock()
			g.created++
			g.mu.Unlock()
			enc(w, map[string]any{"number": 99, "html_url": "https://github.com/o/r/pull/99"})
		case r.Method == "GET" && strings.HasSuffix(path, "/repos/o/r"):
			enc(w, map[string]any{"name": "r", "default_branch": "main"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreatePR_DuplicateGuard(t *testing.T) {
	const bot = "hive[bot]"
	tests := []struct {
		name         string
		g            *guardServer
		body         string
		wantReason   string
		wantNumber   int
		wantComments int
	}{
		{
			name:         "open PR already closes same issue",
			g:            &guardServer{open: []guardPR{{Number: 7, Ref: "a", Body: "Fixes #42", User: "human"}}},
			body:         "Closes #42",
			wantReason:   DuplicateReasonClosesIssue,
			wantNumber:   7,
			wantComments: 1,
		},
		{
			name:         "overlapping file set with bot PR",
			g:            &guardServer{open: []guardPR{{Number: 8, Ref: "b", User: bot, Files: []string{"a.go", "b.go"}}}, headFiles: []string{"a.go"}},
			body:         "no keyword",
			wantReason:   DuplicateReasonOverlappingFiles,
			wantNumber:   8,
			wantComments: 1,
		},
		{
			name:       "merged PR already fixed issue",
			g:          &guardServer{closed: []guardPR{{Number: 5, Ref: "c", Body: "resolves #42", Merged: true}}},
			body:       "Closes #42",
			wantReason: DuplicateReasonMergedFix,
			wantNumber: 5,
		},
		{
			name: "different issue and disjoint files opens a PR",
			g: &guardServer{
				open:      []guardPR{{Number: 9, Ref: "d", Body: "Closes #1", User: bot, Files: []string{"x.go", "y.go"}}},
				closed:    []guardPR{{Number: 4, Ref: "e", Body: "Closes #2", Merged: true}},
				headFiles: []string{"a.go", "x.go"},
			},
			body:       "Closes #42",
			wantNumber: 99,
		},
		{
			name: "single shared file by a non-bot PR does not block",
			g: &guardServer{
				open:      []guardPR{{Number: 9, Ref: "d", User: "human", Files: []string{"a.go"}}},
				headFiles: []string{"a.go"},
			},
			body:       "x",
			wantNumber: 99,
		},
		{
			name:       "merged fix for a reopened issue does not block the follow-up",
			g:          &guardServer{closed: []guardPR{{Number: 5, Ref: "c", Body: "resolves #42", Merged: true}}, openIssues: []int{42}},
			body:       "Closes #42",
			wantNumber: 99,
		},
		{
			name:       "closed but unmerged PR is not a merged fix",
			g:          &guardServer{closed: []guardPR{{Number: 5, Ref: "c", Body: "resolves #42", Merged: false}}},
			body:       "Closes #42",
			wantNumber: 99,
		},
		{
			name:       "draft PR closing the same issue is skipped",
			g:          &guardServer{open: []guardPR{{Number: 7, Ref: "a", Body: "Fixes #42", User: "human", Draft: true}}},
			body:       "Closes #42",
			wantNumber: 99,
		},
		{
			name: "redirect comment is not stacked on retry",
			g: &guardServer{
				open:             []guardPR{{Number: 7, Ref: "a", Body: "Fixes #42", User: "human"}},
				existingComments: []string{duplicateGuardMarker + " from `new-branch`"},
			},
			body:         "Closes #42",
			wantReason:   DuplicateReasonClosesIssue,
			wantNumber:   7,
			wantComments: 0,
		},
		{
			name: "head file set containing the bot PR's set is a duplicate",
			g: &guardServer{
				open:      []guardPR{{Number: 8, Ref: "b", User: bot, Files: []string{"a.go"}}},
				headFiles: []string{"a.go", "b.go"},
			},
			body:         "no keyword",
			wantReason:   DuplicateReasonOverlappingFiles,
			wantNumber:   8,
			wantComments: 1,
		},
		{
			name: "terminal lookup failure degrades to opening the PR",
			g: &guardServer{
				open:       []guardPR{{Number: 7, Ref: "a", Body: "Fixes #42", User: "human"}},
				failPath:   "per_page=100&state=open",
				failStatus: http.StatusForbidden,
			},
			body:       "Closes #42",
			wantNumber: 99,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := tt.g
			srv := g.start(t)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			res, err := c.CreatePR(context.Background(), "o/r", "new-branch", "main", "title", tt.body)
			if err != nil {
				t.Fatalf("CreatePR: %v", err)
			}
			if res.Number != tt.wantNumber || res.DuplicateReason != tt.wantReason {
				t.Errorf("got #%d reason %q, want #%d reason %q", res.Number, res.DuplicateReason, tt.wantNumber, tt.wantReason)
			}
			if wantDup := tt.wantReason != ""; res.IsDuplicate() != wantDup || res.AlreadyExisted != wantDup {
				t.Errorf("IsDuplicate=%v AlreadyExisted=%v, want %v", res.IsDuplicate(), res.AlreadyExisted, wantDup)
			}
			wantCreated := 0
			if tt.wantReason == "" {
				wantCreated = 1
			}
			if g.created != wantCreated {
				t.Errorf("created %d PRs, want %d", g.created, wantCreated)
			}
			if g.comments != tt.wantComments {
				t.Errorf("posted %d comments, want %d", g.comments, tt.wantComments)
			}
		})
	}
}

func TestCreatePR_DuplicateGuardRetryableFailuresFailClosed(t *testing.T) {
	for _, tc := range []struct{ name, failPath string }{
		{"open PR list", "per_page=100&state=open"},
		{"compare", "/compare/"},
		{"merged fix list", "state=closed"},
		{"issue state", "/issues/42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &guardServer{
				open:      []guardPR{{Number: 8, Ref: "b", User: "hive[bot]", Files: []string{"a.go"}}},
				closed:    []guardPR{{Number: 5, Ref: "c", Body: "resolves #42", Merged: true}},
				headFiles: []string{"z.go"},
				failPath:  tc.failPath, failStatus: http.StatusBadGateway,
			}
			srv := g.start(t)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			_, err := c.CreatePR(context.Background(), "o/r", "new-branch", "main", "title", "Closes #42")
			// The open-PR list is shared with the duplicate-tree guard, which
			// may report the failure first; either way the request stays queued.
			if err == nil || !strings.Contains(err.Error(), "lookup failed with retryable error") {
				t.Fatalf("CreatePR err = %v, want retryable lookup error", err)
			}
			if g.created != 0 {
				t.Errorf("created %d PRs, want 0 (request must stay queued)", g.created)
			}
		})
	}
}

// A candidate whose file set cannot be read definitively (files endpoint
// failing, or an empty list GitHub may return for a capped diff) is skipped
// rather than blocking the PR, and the failure is not escalated to a retryable
// lookup error: the request must not stay queued on one unreadable sibling.
// PR numbers are unique to this test because prChangedFiles caches fingerprints
// process-wide by repo/number/head SHA.
func TestCreatePR_DuplicateGuardSkipsUnreadableCandidate(t *testing.T) {
	const bot = "hive[bot]"
	for _, tc := range []struct {
		name string
		g    *guardServer
	}{
		{
			name: "files endpoint fails for the candidate",
			g: &guardServer{
				open:       []guardPR{{Number: 801, Ref: "b", User: bot, Files: []string{"a.go"}}},
				headFiles:  []string{"a.go"},
				failPath:   "/pulls/801/files",
				failStatus: http.StatusBadGateway,
			},
		},
		{
			name: "candidate reports no files",
			g: &guardServer{
				open:      []guardPR{{Number: 802, Ref: "b", User: bot}},
				headFiles: []string{"a.go"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.g.start(t)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			res, err := c.CreatePR(context.Background(), "o/r", "new-branch", "main", "title", "no keyword")
			if err != nil {
				t.Fatalf("CreatePR: %v", err)
			}
			if res.Number != 99 || res.IsDuplicate() {
				t.Errorf("got #%d reason %q, want a freshly opened #99", res.Number, res.DuplicateReason)
			}
			if tc.g.created != 1 {
				t.Errorf("created %d PRs, want 1", tc.g.created)
			}
		})
	}
}

// The file-set walk reads at most maxDuplicateTreeCandidates bot PRs: a
// duplicate sitting past the cap is not detected, so the PR opens. Pins the
// bound so a change to it is a deliberate one.
func TestCreatePR_DuplicateGuardStopsAtCandidateCap(t *testing.T) {
	const bot = "hive[bot]"
	g := &guardServer{headFiles: []string{"dup.go"}}
	for i := 0; i < maxDuplicateTreeCandidates; i++ {
		n := 1000 + i
		g.open = append(g.open, guardPR{Number: n, Ref: fmt.Sprintf("b%d", n), User: bot, Files: []string{fmt.Sprintf("f%d.go", n)}})
	}
	// Identical file set, but only reachable as candidate index == cap.
	g.open = append(g.open, guardPR{Number: 1999, Ref: "b1999", User: bot, Files: []string{"dup.go"}})
	srv := g.start(t)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
	res, err := c.CreatePR(context.Background(), "o/r", "new-branch", "main", "title", "no keyword")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if res.Number != 99 || res.IsDuplicate() {
		t.Errorf("got #%d reason %q, want a freshly opened #99 (candidate past the cap must not be inspected)", res.Number, res.DuplicateReason)
	}
	// Move the duplicate inside the cap and the same guard refuses the PR.
	g.open = append([]guardPR{g.open[len(g.open)-1]}, g.open[:len(g.open)-1]...)
	g.created = 0
	res, err = c.CreatePR(context.Background(), "o/r", "new-branch", "main", "title", "no keyword")
	if err != nil {
		t.Fatalf("CreatePR (dup inside cap): %v", err)
	}
	if res.Number != 1999 || res.DuplicateReason != DuplicateReasonOverlappingFiles {
		t.Errorf("got #%d reason %q, want #1999 %q", res.Number, res.DuplicateReason, DuplicateReasonOverlappingFiles)
	}
	if g.created != 0 {
		t.Errorf("created %d PRs, want 0", g.created)
	}
}

// The redirect comment is courtesy; the refusal is the enforcement. A failure
// reading or posting the comment must not surface as a CreatePR error, and
// must not turn the refusal into a freshly opened PR.
func TestCreatePR_DuplicateGuardRedirectCommentFailureIsNotFatal(t *testing.T) {
	for _, tc := range []struct {
		name, method string
	}{
		{"listing existing comments fails", http.MethodGet},
		{"posting the comment fails", http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &guardServer{
				open:       []guardPR{{Number: 7, Ref: "a", Body: "Fixes #42", User: "human"}},
				failPath:   "/issues/7/comments",
				failMethod: tc.method,
				failStatus: http.StatusInternalServerError,
			}
			srv := g.start(t)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			res, err := c.CreatePR(context.Background(), "o/r", "new-branch", "main", "title", "Closes #42")
			if err != nil {
				t.Fatalf("CreatePR: %v", err)
			}
			if res.Number != 7 || res.DuplicateReason != DuplicateReasonClosesIssue {
				t.Errorf("got #%d reason %q, want #7 %q", res.Number, res.DuplicateReason, DuplicateReasonClosesIssue)
			}
			if g.created != 0 {
				t.Errorf("created %d PRs, want 0", g.created)
			}
			if g.comments != 0 {
				t.Errorf("recorded %d posted comments, want 0", g.comments)
			}
		})
	}
}

func TestFileSetContained(t *testing.T) {
	head := map[string]bool{"a.go": true, "b.go": true}
	for _, tc := range []struct {
		name  string
		other []string
		want  bool
	}{
		{"empty other is never contained", nil, false},
		{"other inside head", []string{"a.go"}, true},
		{"identical sets", []string{"a.go", "b.go"}, true},
		{"head inside other", []string{"a.go", "b.go", "c.go"}, true},
		{"partial overlap", []string{"a.go", "c.go"}, false},
		{"disjoint", []string{"c.go"}, false},
	} {
		if got := fileSetContained(head, tc.other); got != tc.want {
			t.Errorf("%s: fileSetContained(head, %v) = %v, want %v", tc.name, tc.other, got, tc.want)
		}
	}
}

func TestDuplicateGuardComment(t *testing.T) {
	pr := &gh.PullRequest{Number: gh.Ptr(7)}
	for _, tc := range []struct {
		reason, want string
	}{
		{DuplicateReasonClosesIssue, "this PR already closes #42"},
		{DuplicateReasonMergedFix, "this merged PR already closed #42"},
		{DuplicateReasonOverlappingFiles, "overlapping set of files"},
	} {
		got := duplicateGuardComment(&duplicateFinding{pr: pr, reason: tc.reason, issue: 42}, "new-branch")
		if !strings.Contains(got, duplicateGuardMarker) || !strings.Contains(got, "`new-branch`") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: comment %q lacks marker, head or %q", tc.reason, got, tc.want)
		}
	}
}

func TestClosingIssueRefs(t *testing.T) {
	got := closingIssueRefs("Closes #1, fixes O/R#2, Resolved: https://github.com/o/r/issues/3, closes x/y#4, Refs #5, fixed #6, closed #7", "o", "r")
	for _, n := range []int{1, 2, 3, 6, 7} {
		if !got[n] {
			t.Errorf("missing #%d in %v", n, got)
		}
	}
	if got[4] || got[5] {
		t.Errorf("foreign-repo or non-closing refs leaked: %v", got)
	}
}
