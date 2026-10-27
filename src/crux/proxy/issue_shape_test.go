package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// The issue-request watcher validates the file-drop creation path, but an
// agent's `gh issue create` becomes a raw POST /repos/{o}/{r}/issues that
// never touches the watcher (issue #7014). enforceIssueShape closes that gap by
// applying the same shape rules on the proxy path these requests actually take.
func TestEnforceIssueShapeRejectsMalformedAndPreservesBody(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		wantDeny    bool
		errorSubstr string
	}{
		{
			name:        "issue #7014 title placeholder is rejected",
			method:      http.MethodPost,
			path:        "https://api.github.com/repos/org/repo/issues",
			body:        `{"title":"[guide] <specific description of the documentation gap>","body":"real body"}`,
			wantDeny:    true,
			errorSubstr: "title contains unsubstituted template placeholder",
		},
		{
			name:        "issue #7014 body of literal newline escapes is rejected",
			method:      http.MethodPost,
			path:        "https://api.github.com/repos/org/repo/issues",
			body:        `{"title":"concrete title","body":"## Documentation Gap\\n\\n<what is missing or incorrect>\\n\\n## Recommendation\\n\\n<what should be added>"}`,
			wantDeny:    true,
			errorSubstr: "unsubstituted template placeholder",
		},
		{
			name:        "body-only mis-escaped newlines rejected",
			method:      http.MethodPost,
			path:        "https://api.github.com/repos/org/repo/issues",
			body:        `{"title":"concrete title","body":"## Finding\\n\\nDetails of the defect.\\n\\n## Recommendation\\n\\nFix it."}`,
			wantDeny:    true,
			errorSubstr: "literal newline escape sequences",
		},
		{
			name:        "comment placeholder rejected on comment-create path",
			method:      http.MethodPost,
			path:        "https://api.github.com/repos/org/repo/issues/12/comments",
			body:        `{"body":"<what is missing or incorrect>"}`,
			wantDeny:    true,
			errorSubstr: "unsubstituted template placeholder",
		},
		{
			name:     "legitimate issue with real newlines is accepted",
			method:   http.MethodPost,
			path:     "https://api.github.com/repos/org/repo/issues",
			body:     "{\"title\":\"[guide] document the retry backoff\",\"body\":\"## Documentation Gap\\n\\nThe retry backoff is undocumented.\\n\\n## Recommendation\\n\\nAdd a section.\"}",
			wantDeny: false,
		},
		{
			name:     "angle brackets in a fenced code block are accepted",
			method:   http.MethodPost,
			path:     "https://api.github.com/repos/org/repo/issues",
			body:     "{\"title\":\"[guide] generics example\",\"body\":\"## Finding\\n\\nUse the generic form:\\n\\n```go\\nvar x Foo<Bar>\\n```\\n\\n## Recommendation\\n\\nKeep it.\"}",
			wantDeny: false,
		},
		{
			name:     "label sub-route is not shape-checked",
			method:   http.MethodPost,
			path:     "https://api.github.com/repos/org/repo/issues/12/labels",
			body:     `{"labels":["<not a placeholder we gate>"]}`,
			wantDeny: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &GitHubProxy{logger: slog.Default()}
			req, err := http.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			reason, deny, readErr := p.enforceIssueShape(req)
			if readErr != nil {
				t.Fatalf("unexpected read error: %v", readErr)
			}
			if deny != tt.wantDeny {
				t.Fatalf("deny=%v reason=%q, want deny=%v", deny, reason, tt.wantDeny)
			}
			if tt.wantDeny {
				if !strings.Contains(reason, tt.errorSubstr) {
					t.Fatalf("reason %q does not contain %q", reason, tt.errorSubstr)
				}
			} else if reason != "" {
				t.Fatalf("legitimate request got reason %q", reason)
			}
			// The body must be forwardable regardless of the outcome.
			restored, _ := io.ReadAll(req.Body)
			if string(restored) != tt.body {
				t.Fatalf("body not restored: got %q want %q", restored, tt.body)
			}
		})
	}
}

// A GET or an unrelated write route must be left completely untouched — no
// buffering, no body swap — so enforceIssueShape cannot regress other traffic.
func TestEnforceIssueShapeIgnoresUnrelatedRequests(t *testing.T) {
	p := &GitHubProxy{logger: slog.Default()}
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "https://api.github.com/repos/org/repo/issues"},
		{http.MethodPost, "https://api.github.com/repos/org/repo/pulls"},
		{http.MethodPost, "https://api.github.com/graphql"},
	} {
		req, _ := http.NewRequest(tc.method, tc.path, strings.NewReader(`{"title":"<specific description of the documentation gap>"}`))
		reason, deny, readErr := p.enforceIssueShape(req)
		if deny || reason != "" || readErr != nil {
			t.Fatalf("%s %s: got deny=%v reason=%q err=%v, want no-op", tc.method, tc.path, deny, reason, readErr)
		}
	}
}

func TestIsIssueShapeRESTWrite(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/repos/o/r/issues", true},
		{http.MethodPost, "/repos/o/r/issues?foo=bar", true},
		{http.MethodPatch, "/repos/o/r/issues/42", true},
		{http.MethodPost, "/repos/o/r/issues/42/comments", true},
		{http.MethodPatch, "/repos/o/r/issues/comments/99", true},
		{http.MethodPost, "/repos/o/r/issues/42/labels", false},
		{http.MethodPost, "/repos/o/r/pulls", false},
		{http.MethodGet, "/repos/o/r/issues", false},
		{http.MethodDelete, "/repos/o/r/issues/42", false},
	}
	for _, c := range cases {
		if got := isIssueShapeRESTWrite(c.method, c.path); got != c.want {
			t.Errorf("isIssueShapeRESTWrite(%s,%s)=%v want %v", c.method, c.path, got, c.want)
		}
	}
}
