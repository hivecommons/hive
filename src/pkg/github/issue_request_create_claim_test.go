package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// createClaimFake mocks the create, claim-gate read, ownership label add and
// help-wanted removal an "issue" request with Claim set performs
// (hivecommons/hive#17299).
type createClaimFake struct {
	mu            sync.Mutex
	created       int
	createdBody   string
	issueBody     string // body GET /issues/99 returns
	labelAdds     []string
	helpRemoved   int
	failLabelAdds bool
}

func (f *createClaimFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			f.created++
			var in struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.createdBody = in.Body
			_, _ = io.WriteString(w, `{"id":990099,"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/99":
			b, _ := json.Marshal(map[string]any{
				"number": 99, "state": "open", "body": f.issueBody,
				"labels": []map[string]string{{"name": "help wanted"}},
			})
			_, _ = w.Write(b)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/99/labels":
			if f.failLabelAdds {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"message":"label add refused"}`)
				return
			}
			var added []string
			_ = json.NewDecoder(r.Body).Decode(&added)
			f.labelAdds = append(f.labelAdds, added...)
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/99/labels/help"):
			f.helpRemoved++
			_, _ = io.WriteString(w, `[]`)
		case strings.Contains(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func readIssueResult(t *testing.T, reqPath string) IssueResponse {
	t.Helper()
	data, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("reading result file: %v", err)
	}
	var resp IssueResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}
	return resp
}

// An "issue" request with Claim set files and claims the issue in one
// request: ownership label applied, help wanted removed, no filed-for-help
// marker, and the result says so.
func TestIssueRequestWatcher_CreateWithClaimClaimsInSameRequest(t *testing.T) {
	f := &createClaimFake{}
	srv := f.server(t)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)
	var starts []string
	c.SetAgentStartHook(func(agent, repo string, number int, signal string) {
		starts = append(starts, fmt.Sprintf("%s %s#%d %s", agent, repo, number, signal))
	})

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] stage 2 of my epic", Body: "next step", Agent: "scanner", Claim: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if f.created != 1 {
		t.Fatalf("expected 1 issue created, got %d", f.created)
	}
	// The hub's ledger claim is written from this signal; it is the
	// filed-claim signal, not a plain `claim`, because no kick listed the
	// new issue and the hub only ledgers a plain claim on a listed issue.
	if want := []string{"scanner o/r#99 " + AgentStartSignalFiledClaim}; strings.Join(starts, ";") != strings.Join(want, ";") {
		t.Fatalf("agent start signals = %v, want %v", starts, want)
	}
	if strings.Join(f.labelAdds, ",") != claimOwnershipLabel("scanner") {
		t.Fatalf("label adds on #99 = %v, want only %q", f.labelAdds, claimOwnershipLabel("scanner"))
	}
	if f.helpRemoved != 1 {
		t.Fatalf("help wanted removals = %d, want 1", f.helpRemoved)
	}
	if strings.Contains(f.createdBody, "hive:filed-for-help") {
		t.Fatalf("a claimed create must not carry the filed-for-help marker: %q", f.createdBody)
	}
	resp := readIssueResult(t, reqPath)
	if !resp.OK || resp.Number != 99 || !resp.Claimed || resp.ClaimError != "" {
		t.Fatalf("expected OK claimed result for #99, got %+v", resp)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request file should be removed after success")
	}
}

// A claim that fails after the create is reported in claim_error; the create
// stands and is not retried (a retry would not re-run the claim anyway).
func TestIssueRequestWatcher_CreateWithClaimReportsClaimFailure(t *testing.T) {
	f := &createClaimFake{failLabelAdds: true}
	srv := f.server(t)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] claim fails", Body: "next step", Agent: "scanner", Claim: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if f.created != 1 {
		t.Fatalf("expected 1 issue created, got %d", f.created)
	}
	resp := readIssueResult(t, reqPath)
	if !resp.OK || resp.Number != 99 || resp.Claimed {
		t.Fatalf("expected OK, unclaimed result for #99, got %+v", resp)
	}
	if !strings.Contains(resp.ClaimError, "not claimed") {
		t.Fatalf("claim failure must be reported in claim_error, got %q", resp.ClaimError)
	}
	if f.helpRemoved != 0 {
		t.Fatalf("help wanted must stay when the claim failed, removals = %d", f.helpRemoved)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request file should be consumed, not retried, once the issue exists")
	}
}

// Without Claim, the created issue is marked as filed for outside help by
// the filing agent, and nothing is claimed.
func TestIssueRequestWatcher_CreateWithoutClaimMarksFiledForHelp(t *testing.T) {
	f := &createClaimFake{}
	srv := f.server(t)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] outside help", Body: "someone take this", Agent: "scanner",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if got := ParseFiledForHelpMarker(f.createdBody); got != "scanner" {
		t.Fatalf("filed-for-help marker agent = %q, want scanner (body %q)", got, f.createdBody)
	}
	if len(f.labelAdds) != 0 || f.helpRemoved != 0 {
		t.Fatalf("an unclaimed create must not claim: adds=%v removals=%d", f.labelAdds, f.helpRemoved)
	}
	resp := readIssueResult(t, reqPath)
	if !resp.OK || resp.Claimed || resp.ClaimError != "" {
		t.Fatalf("expected OK result with no claim fields, got %+v", resp)
	}
}

// A decomposition child (--parent) is the hive's own backlog: no marker.
func TestIssueRequestWatcher_CreateWithParentIsNotMarkedFiledForHelp(t *testing.T) {
	f := &createClaimFake{}
	srv := f.server(t)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	if _, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] child step", Body: "Part of #5", Agent: "scanner", Parent: 5,
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if f.createdBody == "" {
		t.Fatal("expected the child issue to be created")
	}
	if got := ParseFiledForHelpMarker(f.createdBody); got != "" {
		t.Fatalf("a --parent child must not carry the filed-for-help marker, got %q (body %q)", got, f.createdBody)
	}
}

// A later claim by the agent that filed the issue for outside help is
// refused; another agent may still claim it.
func TestIssueRequestWatcher_ClaimRefusedForOwnFiledForHelpIssue(t *testing.T) {
	f := &createClaimFake{issueBody: "someone take this\n\n" + FiledForHelpMarker("scanner")}
	srv := f.server(t)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{Kind: "claim", Repo: "o/r", Number: 99, Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(f.labelAdds) != 0 {
		t.Fatalf("filer's claim of its own filed-for-help issue wrote labels: %v", f.labelAdds)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Fatalf("filer's claim should be denied: %v", err)
	}
	resp := readIssueResult(t, reqPath)
	if !strings.Contains(resp.Error, ClaimBlockReasonFiledForHelpPrefix+"scanner") {
		t.Fatalf("denial should name the filed-for-help reason, got %q", resp.Error)
	}

	otherPath, err := WriteIssueRequest(dir, IssueRequest{Kind: "claim", Repo: "o/r", Number: 99, Agent: "quality"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if strings.Join(f.labelAdds, ",") != claimOwnershipLabel("quality") {
		t.Fatalf("another agent's claim label adds = %v, want %q", f.labelAdds, claimOwnershipLabel("quality"))
	}
	if _, err := os.Stat(otherPath); !os.IsNotExist(err) {
		t.Errorf("another agent's claim should be consumed on success")
	}
}

func TestFiledForHelpMarkerRoundTrip(t *testing.T) {
	body := withFiledForHelpMarker("text\n", "scanner")
	if body != "text\n\n"+FiledForHelpMarker("scanner") {
		t.Fatalf("withFiledForHelpMarker = %q", body)
	}
	if got := ParseFiledForHelpMarker(body); got != "scanner" {
		t.Fatalf("ParseFiledForHelpMarker = %q, want scanner", got)
	}
	if again := withFiledForHelpMarker(body, "scanner"); again != body {
		t.Fatalf("marker added twice: %q", again)
	}
	if got := withFiledForHelpMarker("text", "bad agent!"); got != "text" {
		t.Fatalf("an agent name the marker cannot carry must leave the body alone, got %q", got)
	}
	// A marker the filer seeded in its own body names someone else: it is
	// dropped, so the filer is still the one bound by the create.
	seeded := "text\n\n" + FiledForHelpMarker("other") + "\nmore"
	if got := withFiledForHelpMarker(seeded, "scanner"); ParseFiledForHelpMarker(got) != "scanner" || strings.Contains(got, "other") {
		t.Fatalf("a foreign marker must be replaced by the filer's own, got %q", got)
	}
}

// An unclaimed create whose body already carries a filed-for-help marker
// naming another agent is still recorded as filed by the requesting agent:
// the body is filer-controlled and must not pick who the binding names.
func TestIssueRequestWatcher_CreateWithoutClaimReplacesForeignMarker(t *testing.T) {
	f := &createClaimFake{}
	srv := f.server(t)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	if _, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] someone take this", Agent: "scanner",
		Body: "please\n\n" + FiledForHelpMarker("other"),
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if f.created != 1 {
		t.Fatalf("expected 1 issue created, got %d", f.created)
	}
	if got := ParseFiledForHelpMarker(f.createdBody); got != "scanner" {
		t.Fatalf("filed-for-help marker names %q, want scanner: %q", got, f.createdBody)
	}
	if strings.Count(f.createdBody, filedForHelpMarkerPrefix) != 1 || strings.Contains(f.createdBody, "filed-for-help other") {
		t.Fatalf("the seeded marker must be gone and exactly one marker remain: %q", f.createdBody)
	}
}

// A finding folded into another issue is posted as a comment there; the
// filed-for-help marker is read only from an issue's own body, so the fold
// comments carry none of it and leave no stray blank lines where it was.
func TestFoldedFindingCommentsDropFiledForHelpMarker(t *testing.T) {
	body := "finding text\n\n" + FiledForHelpMarker("scanner") + "\n\n— hive: agent=scanner"
	for name, comment := range map[string]string{
		"subject":   subjectMatchFindingComment("t", body),
		"file_set":  consolidatedFindingComment("t", body, map[string]bool{"a.go": true}),
		"component": foldedComponentFindingComment("t", body, "pkg", 2, 0),
	} {
		if ParseFiledForHelpMarker(comment) != "" {
			t.Fatalf("%s fold comment carries the filed-for-help marker: %q", name, comment)
		}
		if !strings.HasSuffix(comment, "finding text\n\n— hive: agent=scanner") {
			t.Fatalf("%s fold comment body not preserved around the dropped marker: %q", name, comment)
		}
	}
}
