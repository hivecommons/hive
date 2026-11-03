package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestRepoItemDecisionCloseWontFixCommentsAndCloses(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"close-wontfix","comment":"out of scope for v5"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "won't fix by @writer") || !strings.Contains(mock.comments[0], "out of scope for v5") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" || mock.edits[0]["state_reason"] != issueStateReasonNotPlanned {
		t.Fatalf("edits = %v", mock.edits)
	}
	if !strings.Contains(strings.Join(decisionAuditActions(srv), ","), auditRepoItemDecisionClose) {
		t.Fatalf("audit missing %s: %v", auditRepoItemDecisionClose, decisionAuditActions(srv))
	}
}

// close-read (hivecommons/hive#15381): an advisory issue is closed as
// completed through the same comment + close path; a PR is refused.
func TestRepoItemDecisionCloseReadClosesAsCompleted(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"triage/advisory"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"close-read"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close-read: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Closed as read (advisory acknowledged) by @writer") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" || mock.edits[0]["state_reason"] != issueStateReasonCompleted {
		t.Fatalf("edits = %v", mock.edits)
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "closed" || resp.Result == nil || resp.Result.Outcome != "Closed as read (advisory acknowledged)." {
		t.Fatalf("resp = %+v", resp)
	}
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-read","type":"pr"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("close-read on a PR: code=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func TestRepoItemDecisionCloseSupersededPRUsesSweepPath(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-superseded","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close superseded: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Label != github.SupersededLabel {
		t.Fatalf("label = %q, want %s", resp.Label, github.SupersededLabel)
	}
	if strings.Join(mock.added, ",") != github.SupersededLabel {
		t.Fatalf("added = %v", mock.added)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" {
		t.Fatalf("edits = %v", mock.edits)
	}
}

func TestRepoItemDecisionCloseSupersededByNamesTheWinner(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-superseded","type":"pr","superseded_by":"#458"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close superseded by: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "superseded by testorg/testrepo#458 by @writer") {
		t.Fatalf("comments = %q", mock.comments)
	}
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-superseded","type":"pr","superseded_by":"#8"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("self supersession: code=%d want 400", w.Code)
	}
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-superseded","type":"pr","superseded_by":"nope"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage ref: code=%d want 400", w.Code)
	}
}

func TestParseRepoItemSupersededBy(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"458":             "o/r#458",
		" #458 ":          "o/r#458",
		"acme/console#12": "acme/console#12",
		"https://github.com/acme/console/pull/12":     "acme/console#12",
		"https://github.com/acme/console/issues/12/":  "acme/console#12",
		"https://ghe.example.com/acme/console/pull/3": "acme/console#3",
	}
	for in, want := range cases {
		got, err := parseRepoItemSupersededBy(in, "o/r")
		if err != nil || got != want {
			t.Errorf("parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"x", "#0", "r#4", "https://github.com/acme/console", "a/b/c#4"} {
		if _, err := parseRepoItemSupersededBy(bad, "o/r"); err == nil {
			t.Errorf("parse(%q) must fail", bad)
		}
	}
}

// The real hivecommons/hive#15381 shape (a dedupe-checklist advisory: the
// triage/advisory label, a "Decision required: no" body, bot author) closes
// as completed with the acknowledgement comment; on a paused repo the same
// request is a 200 refused result whose Outcome says why, and nothing is
// written (hivecommons/hive#15436).
func TestRepoItemDecisionCloseReadAdvisoryFixtureAndPausedRefusal(t *testing.T) {
	mock := &repoItemDecisionMock{
		issueLabels: []string{"help wanted", "agent/dedupe", "triage/advisory"},
		issueTitle:  "triage: duplicate and overlapping open issues/PRs to consolidate",
		issueBody:   "<!-- hive:dedupe-checklist repo=hivecommons/hive -->\nDecision required: no\n\n## Summary\n\nTriage pass over the 28 open issues and 12 open PRs. Found 0 near-duplicate pairs.",
	}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"close-read","type":"issue"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close-read: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "closed" || resp.Result == nil || resp.Result.Action != rowActionClosed {
		t.Fatalf("resp = %+v", resp)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Closed as read (advisory acknowledged) by @writer") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" || mock.edits[0]["state_reason"] != issueStateReasonCompleted {
		t.Fatalf("edits = %v", mock.edits)
	}

	// Paused repo: refused with a reason, nothing posted.
	mock.comments, mock.edits = nil, nil
	srv.deps.Config.Project.PausedRepos = []config.RepoPause{{Repo: "testrepo", By: "clubanderson", Reason: "release freeze"}}
	t.Cleanup(func() { srv.deps.Config.Project.PausedRepos = nil })
	w = postRepoItemDecision(t, srv, 7, "writer", `{"decision":"close-read","type":"issue"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("paused close-read: code=%d body=%s", w.Code, w.Body.String())
	}
	resp = repoItemDecisionResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != rowActionRefused || resp.Result == nil || resp.Result.Action != rowActionRefused {
		t.Fatalf("paused resp = %+v", resp)
	}
	if !strings.HasPrefix(resp.Result.Outcome, "Close refused: repo paused") || !strings.Contains(resp.Result.Outcome, "release freeze") {
		t.Fatalf("outcome = %q, want the pause and its reason", resp.Result.Outcome)
	}
	if len(mock.comments)+len(mock.edits) != 0 {
		t.Fatalf("a refused close must write nothing: comments=%v edits=%v", mock.comments, mock.edits)
	}
}
