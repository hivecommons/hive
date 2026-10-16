package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// needsDecisionFilter is an IssueAdmitter that names a configured
// needs-decision label, standing in for config.IssueFilterConfig.
type needsDecisionFilter struct{ label string }

func (needsDecisionFilter) Admits([]string) bool         { return true }
func (f needsDecisionFilter) NeedsDecisionLabel() string { return f.label }

// issueCreatePayload is what one POST /issues carried.
type issueCreatePayload struct {
	Labels []string `json:"labels"`
	Body   string   `json:"body"`
}

// newIssueCreateLabelServer mocks the create path and records the labels and
// body sent with each POST /issues.
func newIssueCreateLabelServer(t *testing.T, mu *sync.Mutex, got *[]issueCreatePayload) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues"):
			var body issueCreatePayload
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*got = append(*got, body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":990099,"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func runNeedsDecisionCreate(t *testing.T, filter IssueAdmitter, req IssueRequest) []string {
	t.Helper()
	return runNeedsDecisionCreatePayload(t, filter, req).Labels
}

func runNeedsDecisionCreatePayload(t *testing.T, filter IssueAdmitter, req IssueRequest) issueCreatePayload {
	t.Helper()
	var mu sync.Mutex
	var got []issueCreatePayload
	srv := newIssueCreateLabelServer(t, &mu, &got)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	if filter != nil {
		c.SetIssueFilter(filter)
	}
	dir := withIssueDir(t)
	if _, err := WriteIssueRequest(dir, req); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 issue create, got %d", len(got))
	}
	return got[0]
}

func hasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

// needs_decision=true makes the relay apply the configured needs-decision
// label itself, even though the agent did not name it (#11215).
func TestIssueRequestWatcher_NeedsDecisionAppliesConfiguredLabel(t *testing.T) {
	labels := runNeedsDecisionCreate(t, needsDecisionFilter{label: "owner-call"}, IssueRequest{
		Repo: "o/r", Title: "[architect] Pick A or B", Body: "the maintainer picks A or B",
		Labels: []string{"agent/architect"}, Agent: "architect", NeedsDecision: true,
	})
	if !hasLabelFold(labels, "owner-call") {
		t.Errorf("configured needs-decision label not applied; create labels = %v", labels)
	}
	if !hasLabelFold(labels, "agent/architect") {
		t.Errorf("agent's own labels dropped; create labels = %v", labels)
	}
	if hasLabelFold(labels, issueNeedsDecisionLabel) {
		t.Errorf("default label applied although a different one is configured; create labels = %v", labels)
	}
}

// With no configured filter the built-in needs-decision label is used.
func TestIssueRequestWatcher_NeedsDecisionDefaultsLabel(t *testing.T) {
	labels := runNeedsDecisionCreate(t, nil, IssueRequest{
		Repo: "o/r", Title: "[architect] Pick A or B", Body: "the maintainer picks A or B",
		Agent: "architect", NeedsDecision: true,
	})
	if !hasLabelFold(labels, issueNeedsDecisionLabel) {
		t.Errorf("default needs-decision label not applied; create labels = %v", labels)
	}
}

// Without needs_decision the relay passes the agent's labels through as given.
func TestIssueRequestWatcher_NoNeedsDecisionLeavesLabels(t *testing.T) {
	got := runNeedsDecisionCreatePayload(t, needsDecisionFilter{label: "owner-call"}, IssueRequest{
		Repo: "o/r", Title: "[architect] Refactor X", Body: "plain finding",
		Labels: []string{"agent/architect"}, Agent: "architect",
	})
	labels := got.Labels
	if hasLabelFold(labels, "owner-call") || hasLabelFold(labels, issueNeedsDecisionLabel) {
		t.Errorf("needs-decision label applied without the flag; create labels = %v", labels)
	}
	if strings.Contains(got.Body, unparkOwnerStepMarker) {
		t.Errorf("owner-step marker added without the flag; body = %q", got.Body)
	}
}

// A new issue filed with needs_decision carries the owner-step marker, so
// the "What to reply" notice offers `/hive done` on the issue path as well
// as the comment path (hivecommons/hive#12372).
func TestIssueRequestWatcher_NeedsDecisionIssueCarriesOwnerStepMarker(t *testing.T) {
	got := runNeedsDecisionCreatePayload(t, nil, IssueRequest{
		Repo: "o/r", Title: "[scanner] Enable the ruleset", Body: "Remaining step is yours: enable the ruleset in Settings.",
		Agent: "scanner", NeedsDecision: true,
	})
	if !strings.Contains(got.Body, unparkOwnerStepMarker) {
		t.Fatalf("issue filed with needs_decision must carry the owner-step marker; body = %q", got.Body)
	}
	if m := unparkMenuFromBody(got.Body); !m.ownerStep || !strings.Contains(m.bullets(), "`/hive done`") {
		t.Fatalf("the body-level marker must offer /hive done:\n%s", m.bullets())
	}
}

// An agent that both sets the flag and names the label gets it once.
func TestIssueRequestWatcher_NeedsDecisionDoesNotDuplicateLabel(t *testing.T) {
	labels := runNeedsDecisionCreate(t, nil, IssueRequest{
		Repo: "o/r", Title: "[quality] Pick A or B", Body: "the maintainer picks A or B",
		Labels: []string{"Needs-Decision"}, Agent: "quality", NeedsDecision: true,
	})
	n := 0
	for _, l := range labels {
		if strings.EqualFold(l, issueNeedsDecisionLabel) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("needs-decision label applied %d times; create labels = %v", n, labels)
	}
}

// runNeedsDecisionComment processes one comment request and returns the
// GitHub writes it made, in order: "label:<name>" per label added and
// "comment" per comment posted.
func runNeedsDecisionComment(t *testing.T, filter IssueAdmitter, req IssueRequest) ([]string, IssueResponse) {
	t.Helper()
	return runNeedsDecisionCommentOn(t, filter, req, false, nil)
}

// runNeedsDecisionCommentOn is runNeedsDecisionComment with the target's
// kind (issue or PR) under test control and an optional client hook, so the
// lane allowlist can be narrowed before the request is processed.
func runNeedsDecisionCommentOn(t *testing.T, filter IssueAdmitter, req IssueRequest, targetIsPR bool, setup func(*Client)) ([]string, IssueResponse) {
	t.Helper()
	var mu sync.Mutex
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/issues/") && !strings.Contains(r.URL.Path, "/labels") && !strings.HasSuffix(r.URL.Path, "/comments"):
			if targetIsPR {
				_, _ = io.WriteString(w, `{"number":1,"pull_request":{"url":"https://api.github.example/repos/o/r/pulls/1"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"number":1}`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			for _, l := range body {
				writes = append(writes, "label:"+l)
			}
			mu.Unlock()
			_, _ = io.WriteString(w, `[{"name":"x"}]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			mu.Lock()
			writes = append(writes, "comment")
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	if filter != nil {
		c.SetIssueFilter(filter)
	}
	if setup != nil {
		setup(c)
	}
	dir := withIssueDir(t)
	reqPath, err := WriteIssueRequest(dir, req)
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), writes...), readIssueResultFile(t, reqPath)
}

// needs_decision on a comment parks the EXISTING issue with the configured
// needs-decision label before posting the comment, so a diagnosis that ends
// in an owner-only action reaches the owner's inbox (#12329).
func TestIssueRequestWatcher_CommentNeedsDecisionParksExistingIssue(t *testing.T) {
	writes, resp := runNeedsDecisionComment(t, needsDecisionFilter{label: "owner-call"}, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 102, Agent: "scanner", NeedsDecision: true,
		Body: "Agents do not edit repository settings; the owner runs: gh api --method PUT ...",
	})
	if got := strings.Join(writes, ","); got != "label:owner-call,comment" {
		t.Fatalf("writes = %q, want the configured park label then the comment", got)
	}
	if !resp.OK || resp.Number != 102 {
		t.Fatalf("result = %+v, want ok on #102", resp)
	}
}

// With no configured filter the built-in needs-decision label is used, and a
// comment without the flag adds no label.
func TestIssueRequestWatcher_CommentNeedsDecisionDefaultsAndIsOptIn(t *testing.T) {
	writes, _ := runNeedsDecisionComment(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 5, Agent: "scanner", Body: "owner step", NeedsDecision: true,
	})
	if got := strings.Join(writes, ","); got != "label:"+issueNeedsDecisionLabel+",comment" {
		t.Fatalf("writes = %q, want the default needs-decision label then the comment", got)
	}
	writes, _ = runNeedsDecisionComment(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 5, Agent: "scanner", Body: "plain note",
	})
	if got := strings.Join(writes, ","); got != "comment" {
		t.Fatalf("writes = %q, want only the comment", got)
	}
}

// Parking applies a hive-controlled label, so a lane allowed to comment but
// not to label is refused before any GitHub write, and the refusal names the
// label op (#12334).
func TestIssueRequestWatcher_CommentNeedsDecisionRequiresLabelOp(t *testing.T) {
	writes, resp := runNeedsDecisionCommentOn(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 7, Agent: "scanner", Body: "owner step", NeedsDecision: true,
	}, false, func(c *Client) {
		c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment))
	})
	if len(writes) != 0 {
		t.Fatalf("writes = %v, want none from a lane that may not label", writes)
	}
	if resp.OK || !strings.Contains(resp.Error, WriteOpLabel) {
		t.Fatalf("result = %+v, want a refusal naming %q", resp, WriteOpLabel)
	}
	// The plain comment from the same lane still goes through.
	writes, resp = runNeedsDecisionCommentOn(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 7, Agent: "scanner", Body: "plain note",
	}, false, func(c *Client) {
		c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment))
	})
	if got := strings.Join(writes, ","); got != "comment" || !resp.OK {
		t.Fatalf("writes = %q ok=%v, want only the comment to succeed", got, resp.OK)
	}
}

// A lane holding both comment and label may park.
func TestIssueRequestWatcher_CommentNeedsDecisionAllowedWithLabelOp(t *testing.T) {
	writes, resp := runNeedsDecisionCommentOn(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 7, Agent: "scanner", Body: "owner step", NeedsDecision: true,
	}, false, func(c *Client) {
		c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment, WriteOpLabel))
	})
	if got := strings.Join(writes, ","); got != "label:"+issueNeedsDecisionLabel+",comment" || !resp.OK {
		t.Fatalf("writes = %q ok=%v, want park then comment", got, resp.OK)
	}
}

// needs_decision on a comment parks issues only: a PR target is refused
// before any write, so the hard-suppress label never lands on a PR (#12334).
func TestIssueRequestWatcher_CommentNeedsDecisionRefusesPullRequest(t *testing.T) {
	writes, resp := runNeedsDecisionCommentOn(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 9, Agent: "scanner", Body: "owner step", NeedsDecision: true,
	}, true, nil)
	if len(writes) != 0 {
		t.Fatalf("writes = %v, want none on a pull request", writes)
	}
	if resp.OK || !strings.Contains(resp.Error, "pull request") {
		t.Fatalf("result = %+v, want a refusal naming the pull request", resp)
	}
	// A plain comment on the same PR is unaffected.
	writes, resp = runNeedsDecisionCommentOn(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 9, Agent: "scanner", Body: "plain note",
	}, true, nil)
	if got := strings.Join(writes, ","); got != "comment" || !resp.OK {
		t.Fatalf("writes = %q ok=%v, want only the comment to succeed", got, resp.OK)
	}
}
