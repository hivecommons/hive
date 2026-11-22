package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

const editPRTestBot = "hive[bot]"

// editPRPatch is one PATCH /pulls/{n} the edit_pr relay sent. Title and Body
// are nil when the field was absent from the JSON, i.e. left as it was.
type editPRPatch struct {
	Title *string `json:"title"`
	Body  *string `json:"body"`
}

// newEditPRMockServer serves GET /repos/o/r/pulls/{n} with the given author
// and merged state and records every PATCH of the same path.
func newEditPRMockServer(t *testing.T, authorLogin, authorType string, merged bool, patches *[]editPRPatch) *httptest.Server {
	t.Helper()
	return newEditPRMockServerWithBody(t, authorLogin, authorType, merged, "", patches)
}

// newEditPRMockServerWithBody is newEditPRMockServer with the PR's current body.
func newEditPRMockServerWithBody(t *testing.T, authorLogin, authorType string, merged bool, currentBody string, patches *[]editPRPatch) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/repos/o/r/pulls/")
		n, err := strconv.Atoi(rest)
		if !ok || err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			pr := map[string]any{
				"number": n,
				"state":  "open",
				"merged": merged,
				"user":   map[string]any{"login": authorLogin, "type": authorType},
				"body":   currentBody,
			}
			_ = json.NewEncoder(w).Encode(pr)
		case http.MethodPatch:
			var p editPRPatch
			_ = json.NewDecoder(r.Body).Decode(&p)
			*patches = append(*patches, p)
			_, _ = io.WriteString(w, `{"number":`+rest+`}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func editPRTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c := issueTestClient(t, srvURL)
	c.SetAppBotLogin(editPRTestBot)
	return c
}

// An edit_pr request on a PR the hive's App opened replaces its title and
// body, runs the body through the outbound prose filter, and is audited with
// typed repo/target and the fields edited (#19399).
func TestIssueRequestWatcher_EditPREditsHiveAuthoredPR(t *testing.T) {
	var patches []editPRPatch
	srv := newEditPRMockServer(t, editPRTestBot, "Bot", false, &patches)
	defer srv.Close()
	c := editPRTestClient(t, srv.URL)
	recs := captureAudit(c)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "edit_pr", Repo: "o/r", Number: 42, Agent: "sec-check",
		Title: "✨ feature: drop the marker",
		Body:  "Refreshed after review — the marker paragraph is gone.",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(patches) != 1 {
		t.Fatalf("PATCH calls = %d, want 1", len(patches))
	}
	p := patches[0]
	if p.Title == nil || *p.Title != "✨ feature: drop the marker" {
		t.Errorf("title sent = %v, want the requested title", p.Title)
	}
	if p.Body == nil || !strings.Contains(*p.Body, "the marker paragraph is gone") {
		t.Fatalf("body sent = %v, want the requested body", p.Body)
	}
	if strings.Contains(*p.Body, "—") {
		t.Errorf("body sent still carries an em-dash; the relay must apply the outbound prose filter: %q", *p.Body)
	}
	resp := readIssueResultFile(t, reqPath)
	if !resp.OK || resp.Number != 42 || strings.Join(resp.Edited, ",") != "title,body" {
		t.Fatalf("result = %+v, want ok on #42 with edited=[title body]", resp)
	}
	rec, ok := findAudit(*recs, AuditActionAgentPREdited)
	if !ok {
		t.Fatalf("edit was not audited; records: %+v", *recs)
	}
	if rec.Repo != "o/r" || rec.Target != 42 || rec.Agent != "sec-check" {
		t.Errorf("audit typed fields = %+v, want repo=o/r target=42 agent=sec-check", rec)
	}
	if !strings.Contains(rec.Detail, "edited=title body") {
		t.Errorf("audit detail = %q, want the edited fields", rec.Detail)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("a fulfilled edit_pr request should be consumed")
	}
}

// A body-only request leaves the title alone: the PATCH carries no title.
func TestIssueRequestWatcher_EditPRBodyOnlyLeavesTitle(t *testing.T) {
	var patches []editPRPatch
	srv := newEditPRMockServer(t, editPRTestBot, "Bot", false, &patches)
	defer srv.Close()
	c := editPRTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "edit_pr", Repo: "o/r", Number: 7, Agent: "scanner", Body: "New description.",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(patches) != 1 {
		t.Fatalf("PATCH calls = %d, want 1", len(patches))
	}
	if patches[0].Title != nil {
		t.Errorf("a body-only edit sent title %q; it must leave the title as it is", *patches[0].Title)
	}
	if resp := readIssueResultFile(t, reqPath); !resp.OK || strings.Join(resp.Edited, ",") != "body" {
		t.Errorf("result = %+v, want ok with edited=[body]", resp)
	}
}

// A body rewrite that drops the carried-fence marker gets it back: the marker
// is the only thing keeping an adopted contributor PR out of unattended merge
// (hivecommons/hive#19401 review). The audit entry records the body digest.
func TestIssueRequestWatcher_EditPRKeepsCarriedFenceMarker(t *testing.T) {
	const marker = "<!-- hive:carried-fence reason=tests-removed from=#123 -->"
	current := "Supersedes #123.\n\n" + marker + "\n\n**Merge stays manual**: tests removed.\n"
	var patches []editPRPatch
	srv := newEditPRMockServerWithBody(t, editPRTestBot, "Bot", false, current, &patches)
	defer srv.Close()
	c := editPRTestClient(t, srv.URL)
	recs := captureAudit(c)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "edit_pr", Repo: "o/r", Number: 5, Agent: "review-fix",
		Body: "Refreshed description.\n\nFixes #99\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(patches) != 1 || patches[0].Body == nil {
		t.Fatalf("PATCH calls = %+v, want one with a body", patches)
	}
	got := *patches[0].Body
	if !strings.Contains(got, marker) {
		t.Fatalf("body sent dropped the carried-fence marker:\n%s", got)
	}
	if !strings.Contains(got, "Refreshed description.") {
		t.Errorf("body sent lost the requested text:\n%s", got)
	}
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Fatalf("result = %+v, want ok", resp)
	}
	rec, ok := findAudit(*recs, AuditActionAgentPREdited)
	if !ok {
		t.Fatalf("edit was not audited; records: %+v", *recs)
	}
	if want := "body_sha256=" + editPRBodyDigest(got); !strings.Contains(rec.Detail, want) {
		t.Errorf("audit detail = %q, want %q", rec.Detail, want)
	}

	// A rewrite that kept the marker passes through the relay filter intact
	// and is not given a second copy.
	if _, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "edit_pr", Repo: "o/r", Number: 5, Agent: "review-fix",
		Body: "Second refresh.\n\n" + marker + "\n",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if len(patches) != 2 || patches[1].Body == nil {
		t.Fatalf("PATCH calls = %d, want 2", len(patches))
	}
	if n := strings.Count(*patches[1].Body, marker); n != 1 {
		t.Errorf("marker appears %d times, want exactly 1:\n%s", n, *patches[1].Body)
	}
}

func TestPreserveCarriedFenceMarkers(t *testing.T) {
	const m1 = "<!-- hive:carried-fence reason=tests-removed from=#1 -->"
	const m2 = "<!-- hive:carried-fence reason=workflow-path -->"
	cases := map[string]struct{ current, next, want string }{
		"no marker":        {"old body", "new body", "new body"},
		"kept":             {"a\n" + m1, "b\n" + m1 + "\nc", "b\n" + m1 + "\nc"},
		"dropped":          {"a\n" + m1 + "\n", "b\n", "b\n\n" + m1 + "\n"},
		"two, one dropped": {m1 + "\n" + m2, "x " + m2, "x " + m2 + "\n\n" + m1 + "\n"},
		"duplicate":        {m1 + "\n" + m1, "x", "x\n\n" + m1 + "\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := preserveCarriedFenceMarkers(tc.current, tc.next); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The authorship gate: the relay never edits a PR that is not positively one
// of this hive's own, whoever's it is, and never a merged one. Each refusal is
// a policy denial that reaches GitHub only for the read.
func TestIssueRequestWatcher_EditPRRefusesPRsNotOurs(t *testing.T) {
	for name, tc := range map[string]struct {
		login, typ string
		merged     bool
		identity   bool
		want       string
	}{
		"human author":       {login: "alice", typ: "User", identity: true, want: "never edits a human-authored"},
		"another bot":        {login: "dependabot[bot]", typ: "Bot", identity: true, want: "not by one of this hive's accounts"},
		"no author":          {login: "", typ: "", identity: true, want: "unidentifiable account"},
		"merged hive PR":     {login: editPRTestBot, typ: "Bot", merged: true, identity: true, want: "is merged"},
		"no identity at all": {login: editPRTestBot, typ: "Bot", identity: false, want: "no configured identity"},
	} {
		t.Run(name, func(t *testing.T) {
			var patches []editPRPatch
			srv := newEditPRMockServer(t, tc.login, tc.typ, tc.merged, &patches)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			if tc.identity {
				c.SetAppBotLogin(editPRTestBot)
			}
			recs := captureAudit(c)
			dir := withIssueDir(t)

			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "edit_pr", Repo: "o/r", Number: 9, Agent: "scanner", Body: "rewritten",
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			if len(patches) != 0 {
				t.Fatalf("PR was edited (%d PATCH calls); it must be refused", len(patches))
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Errorf("refused request was not quarantined as .denied: %v", err)
			}
			if resp := readIssueResultFile(t, reqPath); resp.OK || !strings.Contains(resp.Error, tc.want) {
				t.Errorf("result = %+v, want a refusal containing %q", resp, tc.want)
			}
			if _, ok := findAudit(*recs, AuditActionAgentPREdited); ok {
				t.Error("a refused edit was audited as an edit")
			}
		})
	}
}

// The lane allowlist governs edit_pr like every other operation: a lane whose
// entry does not list it is refused before the PR is even read.
func TestIssueRequestWatcher_EditPRRefusedOutsideLaneAllowlist(t *testing.T) {
	var patches []editPRPatch
	srv := newEditPRMockServer(t, editPRTestBot, "Bot", false, &patches)
	defer srv.Close()
	c := editPRTestClient(t, srv.URL)
	c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment, WriteOpLabel))
	recs := captureAudit(c)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "edit_pr", Repo: "o/r", Number: 11, Agent: "scanner", Body: "rewritten",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(patches) != 0 {
		t.Fatalf("%d PATCH calls by a lane not allowed to edit PRs, want 0", len(patches))
	}
	if resp := readIssueResultFile(t, reqPath); resp.OK || !strings.Contains(resp.Error, WriteOpEditPR) {
		t.Errorf("result does not name the refused operation: %+v", resp)
	}
	assertRefusalAudited(t, *recs, "scanner", WriteOpEditPR, "o/r", 11)
}

// A request that can never succeed is quarantined as malformed and never
// reaches GitHub.
func TestIssueRequestWatcher_EditPRMalformed(t *testing.T) {
	for name, req := range map[string]IssueRequest{
		"nothing to edit":  {Kind: "edit_pr", Repo: "o/r", Number: 3, Agent: "scanner"},
		"blank fields":     {Kind: "edit_pr", Repo: "o/r", Number: 3, Agent: "scanner", Title: " ", Body: "\n"},
		"no number":        {Kind: "edit_pr", Repo: "o/r", Agent: "scanner", Body: "x"},
		"no agent":         {Kind: "edit_pr", Repo: "o/r", Number: 3, Body: "x"},
		"multi-line title": {Kind: "edit_pr", Repo: "o/r", Number: 3, Agent: "scanner", Title: "a\nb"},
		"mis-escaped body": {Kind: "edit_pr", Repo: "o/r", Number: 3, Agent: "scanner", Body: `line one\n\nline two\n`},
	} {
		t.Run(name, func(t *testing.T) {
			var patches []editPRPatch
			srv := newEditPRMockServer(t, editPRTestBot, "Bot", false, &patches)
			defer srv.Close()
			c := editPRTestClient(t, srv.URL)
			dir := withIssueDir(t)

			reqPath, err := WriteIssueRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			if len(patches) != 0 {
				t.Fatalf("malformed request reached GitHub (%d PATCH calls)", len(patches))
			}
			if _, err := os.Stat(reqPath + ".bad"); err != nil {
				t.Errorf("malformed request was not quarantined as .bad: %v", err)
			}
		})
	}
}
