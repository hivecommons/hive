package proxy

import (
	"net/http"
	"testing"
)

// ---------- githubWriteBodyMayLeak ----------

// githubWriteBodyMayLeak is the gate that decides whether a GitHub write body
// must be scanned for secret egress before it leaves the proxy. A false
// negative here lets an agent exfiltrate data through an issue/PR/comment
// body, so every branch of the classifier is pinned down.
func TestGithubWriteBodyMayLeak(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		// Read-only methods never leak regardless of path.
		{"get repos issues", http.MethodGet, "/repos/o/r/issues", false},
		{"head graphql", http.MethodHead, "/graphql", false},
		{"delete comment", http.MethodDelete, "/repos/o/r/issues/comments/1", false},
		{"options pulls", http.MethodOptions, "/repos/o/r/pulls", false},

		// Non-/repos/ writes leak only when they hit GraphQL.
		{"post graphql", http.MethodPost, "/graphql", true},
		{"put graphql", http.MethodPut, "/graphql", true},
		{"patch graphql", http.MethodPatch, "/graphql", true},
		{"post non-repos non-graphql", http.MethodPost, "/user/repos", false},
		{"post orgs", http.MethodPost, "/orgs/o/teams", false},

		// /repos/ writes leak on user-content surfaces.
		{"post issue", http.MethodPost, "/repos/o/r/issues", true},
		{"patch issue", http.MethodPatch, "/repos/o/r/issues/5", true},
		{"post issue comment", http.MethodPost, "/repos/o/r/issues/5/comments", true},
		{"post pull", http.MethodPost, "/repos/o/r/pulls", true},
		{"put pull merge", http.MethodPut, "/repos/o/r/pulls/7/merge", true},
		{"post review", http.MethodPost, "/repos/o/r/pulls/7/reviews", true},
		{"post commit comment", http.MethodPost, "/repos/o/r/comments/9", true},
		{"post git blob", http.MethodPost, "/repos/o/r/git/blobs", true},
		{"post git tree", http.MethodPost, "/repos/o/r/git/trees", true},

		// /repos/ writes on non-content surfaces do not leak.
		{"put topics", http.MethodPut, "/repos/o/r/topics", false},
		{"patch repo settings", http.MethodPatch, "/repos/o/r", false},
		{"post labels", http.MethodPost, "/repos/o/r/labels", false},
		{"put branch protection", http.MethodPut, "/repos/o/r/branches/main/protection", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := githubWriteBodyMayLeak(tt.method, tt.path); got != tt.want {
				t.Errorf("githubWriteBodyMayLeak(%s, %s) = %v, want %v", tt.method, tt.path, got, tt.want)
			}
		})
	}
}

// ---------- skipGraphQLString ----------

// skipGraphQLString is part of the lexer the Linear write gate uses to
// classify GraphQL documents. A wrong offset here shifts every subsequent
// token, which can misclassify a mutation as a permitted read — so the
// escape and termination branches are pinned exactly.
func TestSkipGraphQLString(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		i      int
		want   int
		wantOK bool
	}{
		{"simple string", `"abc" rest`, 0, 5, true},
		{"empty string", `"" rest`, 0, 2, true},
		{"escaped quote", `"a\"b" rest`, 0, 6, true},
		{"escaped backslash then close", `"a\\" rest`, 0, 5, true},
		{"escape at end unterminated", `"abc\`, 0, 0, false},
		{"unterminated", `"abc`, 0, 0, false},
		{"newline inside single-quoted", "\"ab\ncd\"", 0, 0, false},
		{"block string", `"""abc""" rest`, 0, 9, true},
		{"empty block string", `""""""`, 0, 6, true},
		{"block string with quotes inside", `"""a"b""" rest`, 0, 9, true},
		{"block string with newline", "\"\"\"a\nb\"\"\"", 0, 9, true},
		{"unterminated block string", `"""abc`, 0, 0, false},
		{"offset start", `x "hi" y`, 2, 6, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := skipGraphQLString(tt.query, tt.i)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Errorf("skipGraphQLString(%q, %d) = (%d, %v), want (%d, %v)",
					tt.query, tt.i, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
