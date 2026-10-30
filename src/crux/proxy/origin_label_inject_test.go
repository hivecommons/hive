package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// An agent's `gh issue create` never passes through the issue-request watcher,
// so the proxy injects agent/<lane> itself. Redundant origin/<lane> and bare
// lane provenance aliases are dropped from issue creates.
func TestInjectOriginLabelAddsLaneOnceAndKeepsAgentChoice(t *testing.T) {
	const path = "/repos/org/repo/issues"
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		agent      string
		wantOK     bool
		wantLabels []string
		wantTitle  string
	}{
		{name: "no labels field", method: http.MethodPost, path: path, body: `{"title":"t","body":"b"}`, agent: "scanner", wantOK: true, wantLabels: []string{"agent/scanner"}, wantTitle: "[scanner] t"},
		{name: "already tagged title kept, replica suffix stripped", method: http.MethodPost, path: path, body: `{"title":"[scanner] t","body":"b","labels":["agent/scanner"]}`, agent: "scanner-2", wantOK: false},
		{name: "foreign tag not double-prefixed", method: http.MethodPost, path: path, body: `{"title":"[Tests] t","body":"b"}`, agent: "tester", wantOK: true, wantLabels: []string{"agent/tester"}, wantTitle: "[Tests] t"},
		{name: "bare lane label dropped, replica suffix stripped", method: http.MethodPost, path: path, body: `{"title":"t","body":"b","labels":["agent/scanner","scanner","kind/bug"]}`, agent: "scanner-2", wantOK: true, wantLabels: []string{"agent/scanner", "kind/bug"}},
		{name: "agent-supplied origin kept while bare filing lane drops", method: http.MethodPost, path: path, body: `{"title":"t","body":"b","labels":["origin/dashboard","agent/scanner","scanner"]}`, agent: "scanner", wantOK: true, wantLabels: []string{"origin/dashboard", "agent/scanner"}},
		{name: "agent-supplied matching origin drops redundant origin and bare label", method: http.MethodPost, path: path, body: `{"title":"t","body":"b","labels":["origin/scanner","agent/scanner","scanner","kind/bug"]}`, agent: "scanner", wantOK: true, wantLabels: []string{"agent/scanner", "kind/bug"}},
		{name: "null labels", method: http.MethodPost, path: path, body: `{"title":"t","body":"b","labels":null}`, agent: "guide", wantOK: true, wantLabels: []string{"agent/guide"}},
		{name: "label objects forwarded as written", method: http.MethodPost, path: path, body: `{"title":"t","body":"b","labels":[{"name":"kind/bug"}]}`, agent: "guide", wantOK: false},
		{name: "comment route untouched", method: http.MethodPost, path: "/repos/org/repo/issues/1/comments", body: `{"body":"b"}`, agent: "guide", wantOK: false},
		{name: "issue edit untouched", method: http.MethodPatch, path: "/repos/org/repo/issues/1", body: `{"title":"t"}`, agent: "guide", wantOK: false},
		{name: "internal caller untouched", method: http.MethodPost, path: path, body: `{"title":"t","body":"b"}`, agent: internalCallerName, wantOK: false},
		{name: "unidentified caller untouched", method: http.MethodPost, path: path, body: `{"title":"t","body":"b"}`, agent: "", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := injectOriginLabel(tc.method, tc.path, []byte(tc.body), tc.agent)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (body %s)", ok, tc.wantOK, got)
			}
			if !ok && string(got) != tc.body {
				t.Fatalf("unchanged body must be returned verbatim: %s", got)
			}
			if tc.wantLabels == nil {
				return
			}
			var payload struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("rewritten body is not JSON: %v: %s", err, got)
			}
			wantTitle := tc.wantTitle
			if wantTitle == "" {
				wantTitle = "[" + strings.TrimSuffix(tc.agent, "-2") + "] t"
			}
			if payload.Title != wantTitle || payload.Body != "b" {
				t.Fatalf("title = %q, want %q; body must survive the rewrite: %s", payload.Title, wantTitle, got)
			}
			if strings.Join(payload.Labels, ",") != strings.Join(tc.wantLabels, ",") {
				t.Fatalf("labels = %v, want %v", payload.Labels, tc.wantLabels)
			}
			laneLabels := 0
			for _, l := range payload.Labels {
				if strings.HasPrefix(l, "agent/") {
					laneLabels++
				}
			}
			if laneLabels != 1 {
				t.Fatalf("exactly one agent/* label expected, got %v", payload.Labels)
			}
		})
	}
}

// The shape gate and lane-label injection share one buffered read: a body
// that fails shape validation is denied unmodified, and one that passes is
// forwarded with the lane's agent label and a matching Content-Length.
func TestEnforceIssueShapeForInjectsOriginAfterShapePasses(t *testing.T) {
	p := &GitHubProxy{logger: discardLogger()}
	body := `{"title":"Real title","body":"real body","labels":["agent/tester","tester","kind/bug"]}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	reason, deny, err := p.enforceIssueShapeFor(req, "tester")
	if err != nil || deny || reason != "" {
		t.Fatalf("well-formed body denied: reason=%q deny=%v err=%v", reason, deny, err)
	}
	forwarded := readAllString(t, req)
	if strings.Contains(forwarded, `"origin/tester"`) || !strings.Contains(forwarded, `"agent/tester"`) || strings.Contains(forwarded, `"tester"`) || !strings.Contains(forwarded, `"kind/bug"`) {
		t.Fatalf("forwarded body = %s", forwarded)
	}
	if !strings.Contains(forwarded, `"title":"[tester] Real title"`) {
		t.Fatalf("forwarded title must carry the filing lane: %s", forwarded)
	}
	if req.ContentLength != int64(len(forwarded)) {
		t.Fatalf("ContentLength %d != forwarded %d", req.ContentLength, len(forwarded))
	}

	bad := `{"title":"[guide] <specific description of the documentation gap>","body":"real body"}`
	req, _ = http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues", strings.NewReader(bad))
	if _, deny, _ := p.enforceIssueShapeFor(req, "guide"); !deny {
		t.Fatal("malformed body must still be denied")
	}
	if got := readAllString(t, req); got != bad {
		t.Fatalf("denied body must be preserved verbatim: %s", got)
	}
}

func readAllString(t *testing.T, req *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
