package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// contributorPRLinkFixture answers the PR lookup LinkContributorPRToIssue
// issues, the body PATCH it may send, and the EnsureClosingIssueLink lookup +
// addCloseIssueReferences mutation that follow.
type contributorPRLinkFixture struct {
	body        string
	base        string
	author      string
	state       string
	patches     atomic.Int32
	patchedBody string
	mutations   atomic.Int32
}

func (f *contributorPRLinkFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/pulls/1" {
			var req struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.patches.Add(1)
			f.patchedBody = req.Body
			f.body = req.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "body": req.Body})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &req)
		state := f.state
		if state == "" {
			state = "OPEN"
		}
		switch {
		case strings.Contains(req.Query, "addCloseIssueReferences"):
			f.mutations.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"addCloseIssueReferences": map[string]any{"issue": map[string]any{"number": 7}},
			}})
		case strings.Contains(req.Query, "closingIssuesReferences"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"id":                      "PR_1",
						"state":                   state,
						"baseRefName":             f.base,
						"closingIssuesReferences": map[string]any{"nodes": []map[string]any{}},
					},
					"issue": map[string]any{"id": "I_7", "state": "OPEN"},
				},
			}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"state":       state,
						"baseRefName": f.base,
						"body":        f.body,
						"author":      map[string]any{"login": f.author},
					},
				},
			}})
		}
	}
}

func newContributorPRLinkClient(t *testing.T, f *contributorPRLinkFixture) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
}

// hivecommons/hive#14763: a contributor PR reported for a dispatched issue
// with no closing keyword (hivecommons/hive#14701 arrived with an empty body)
// gets `Fixes #N` appended and the sidebar link, even on a release-line base.
func TestLinkContributorPRToIssueAppendsFixesAndSidebarLink(t *testing.T) {
	f := &contributorPRLinkFixture{body: "", base: "v6", author: "karibhou"}
	c := newContributorPRLinkClient(t, f)
	got, err := c.LinkContributorPRToIssue(context.Background(), "hivecommons/hive", 1, 7, "Karibhou")
	if err != nil {
		t.Fatalf("LinkContributorPRToIssue: %v", err)
	}
	if !got.EditedBody || got.Skipped != "" || f.patches.Load() != 1 {
		t.Fatalf("result=%+v patches=%d, want one body edit", got, f.patches.Load())
	}
	if f.patchedBody != "Fixes #7" {
		t.Fatalf("patched body = %q, want bare Fixes trailer on an empty body", f.patchedBody)
	}
	if !got.SidebarLinked || f.mutations.Load() != 1 {
		t.Fatalf("result=%+v mutations=%d, want the sidebar link too", got, f.mutations.Load())
	}
	if got.Base != "v6" {
		t.Fatalf("base = %q, want v6", got.Base)
	}
}

func TestLinkContributorPRToIssueAppendsAfterExistingBody(t *testing.T) {
	f := &contributorPRLinkFixture{body: "## Summary\n\nAdds the PKCE cookie.\n\n", base: "v5", author: "karibhou"}
	c := newContributorPRLinkClient(t, f)
	got, err := c.LinkContributorPRToIssue(context.Background(), "hivecommons/hive", 1, 7, "karibhou")
	if err != nil {
		t.Fatalf("LinkContributorPRToIssue: %v", err)
	}
	if !got.EditedBody || f.patchedBody != "## Summary\n\nAdds the PKCE cookie.\n\nFixes #7" {
		t.Fatalf("result=%+v patched body = %q", got, f.patchedBody)
	}
}

func TestLinkContributorPRToIssueLeavesClosingAndPartialBodiesAlone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantSkipped string
		wantSidebar bool
	}{
		{"already-closing", "Closes #7", "already-closing", true},
		{"already-closing-full-ref", "Fixes hivecommons/hive#7", "already-closing", true},
		{"part-of", "Part of #7 — first slice only.", "partial-reference", false},
		{"refs", "Refs #7", "partial-reference", false},
		{"stays-open", "Groundwork; leave #7 open.", "partial-reference", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &contributorPRLinkFixture{body: tc.body, base: "v6", author: "karibhou"}
			c := newContributorPRLinkClient(t, f)
			got, err := c.LinkContributorPRToIssue(context.Background(), "hivecommons/hive", 1, 7, "karibhou")
			if err != nil {
				t.Fatalf("LinkContributorPRToIssue: %v", err)
			}
			if got.EditedBody || f.patches.Load() != 0 || got.Skipped != tc.wantSkipped {
				t.Fatalf("result=%+v patches=%d, want skipped=%q and no edit", got, f.patches.Load(), tc.wantSkipped)
			}
			if got.SidebarLinked != tc.wantSidebar || (f.mutations.Load() == 1) != tc.wantSidebar {
				t.Fatalf("result=%+v mutations=%d, want sidebar=%v", got, f.mutations.Load(), tc.wantSidebar)
			}
		})
	}
}

func TestLinkContributorPRToIssueNeverEditsAnotherAuthorsPR(t *testing.T) {
	f := &contributorPRLinkFixture{body: "", base: "v6", author: "someone-else"}
	c := newContributorPRLinkClient(t, f)
	got, err := c.LinkContributorPRToIssue(context.Background(), "hivecommons/hive", 1, 7, "karibhou")
	if err != nil {
		t.Fatalf("LinkContributorPRToIssue: %v", err)
	}
	if got.EditedBody || got.Skipped != "author-mismatch" || f.patches.Load() != 0 || f.mutations.Load() != 0 {
		t.Fatalf("result=%+v patches=%d mutations=%d, want untouched", got, f.patches.Load(), f.mutations.Load())
	}
	if _, err := c.LinkContributorPRToIssue(context.Background(), "hivecommons/hive", 1, 7, ""); err != nil {
		t.Fatalf("empty login: %v", err)
	}
	if f.patches.Load() != 0 {
		t.Fatalf("empty login must never edit")
	}
}

func TestLinkContributorPRToIssueSkipsClosedPR(t *testing.T) {
	f := &contributorPRLinkFixture{body: "", base: "v6", author: "karibhou", state: "MERGED"}
	c := newContributorPRLinkClient(t, f)
	got, err := c.LinkContributorPRToIssue(context.Background(), "hivecommons/hive", 1, 7, "karibhou")
	if err != nil {
		t.Fatalf("LinkContributorPRToIssue: %v", err)
	}
	if got.EditedBody || got.Skipped != "pr-not-open" || f.patches.Load() != 0 || f.mutations.Load() != 0 {
		t.Fatalf("result=%+v, want closed PR left alone", got)
	}
}
