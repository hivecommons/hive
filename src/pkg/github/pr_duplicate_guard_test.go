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
)

type guardPR struct {
	Number int
	Ref    string
	Body   string
	User   string
	Merged bool
	Files  []string
}

type guardServer struct {
	open, closed []guardPR
	headFiles    []string

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
		return m
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
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
			enc(w, []any{})
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
		g            guardServer
		body         string
		wantReason   string
		wantNumber   int
		wantComments int
	}{
		{
			name:         "open PR already closes same issue",
			g:            guardServer{open: []guardPR{{Number: 7, Ref: "a", Body: "Fixes #42", User: "human"}}},
			body:         "Closes #42",
			wantReason:   DuplicateReasonClosesIssue,
			wantNumber:   7,
			wantComments: 1,
		},
		{
			name:         "overlapping file set with bot PR",
			g:            guardServer{open: []guardPR{{Number: 8, Ref: "b", User: bot, Files: []string{"a.go", "b.go"}}}, headFiles: []string{"a.go"}},
			body:         "no keyword",
			wantReason:   DuplicateReasonOverlappingFiles,
			wantNumber:   8,
			wantComments: 1,
		},
		{
			name:       "merged PR already fixed issue",
			g:          guardServer{closed: []guardPR{{Number: 5, Ref: "c", Body: "resolves #42", Merged: true}}},
			body:       "Closes #42",
			wantReason: DuplicateReasonMergedFix,
			wantNumber: 5,
		},
		{
			name: "different issue and disjoint files opens a PR",
			g: guardServer{
				open:      []guardPR{{Number: 9, Ref: "d", Body: "Closes #1", User: bot, Files: []string{"x.go", "y.go"}}},
				closed:    []guardPR{{Number: 4, Ref: "e", Body: "Closes #2", Merged: true}},
				headFiles: []string{"a.go", "x.go"},
			},
			body:       "Closes #42",
			wantNumber: 99,
		},
		{
			name: "single shared file by a non-bot PR does not block",
			g: guardServer{
				open:      []guardPR{{Number: 9, Ref: "d", User: "human", Files: []string{"a.go"}}},
				headFiles: []string{"a.go"},
			},
			body:       "x",
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

func TestClosingIssueRefs(t *testing.T) {
	got := closingIssueRefs("Closes #1, fixes o/r#2, Resolved: https://github.com/o/r/issues/3, closes x/y#4, Refs #5", "o", "r")
	for _, n := range []int{1, 2, 3} {
		if !got[n] {
			t.Errorf("missing #%d in %v", n, got)
		}
	}
	if got[4] || got[5] {
		t.Errorf("foreign-repo or non-closing refs leaked: %v", got)
	}
}
