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
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// projectbluefin/website#987 (11-03): the review-backlog sweep posted
// "Unblocked: projectbluefin/website#986 merged … actionable now" at
// 23:11:13Z and the blocked sweep "Unblocked: projectbluefin/website#986 is
// no longer open; re-triaging." at 23:11:21Z. The issue had no
// triage/accepted, so neither could be worked.

func commentsOf(bodies ...string) []*gh.IssueComment {
	out := make([]*gh.IssueComment, 0, len(bodies))
	for _, b := range bodies {
		out = append(out, &gh.IssueComment{Body: gh.Ptr(b)})
	}
	return out
}

func TestUnblockAlreadyNoted(t *testing.T) {
	lift := reviewBacklogLiftNote("projectbluefin/website", 986, "")
	settled := blockedPRSettledNote("projectbluefin/website#986", "")
	retriage := reviewBacklogRetriageNote("projectbluefin/website", 986, "")
	reblock := BlockedOnMarker("projectbluefin/website", 986) + "\nBlocked on #986 again."
	for _, tc := range []struct {
		name     string
		comments []*gh.IssueComment
		note     string
		want     bool
	}{
		{name: "website#987: the sweep after the review-backlog lift", comments: commentsOf("triage", lift), note: settled, want: true},
		{name: "the review-backlog lift after the sweep", comments: commentsOf(settled), note: lift, want: true},
		{name: "retriage note names the PR too", comments: commentsOf(retriage), note: settled, want: true},
		{name: "short repo name in one, full in the other", comments: commentsOf(blockedPRSettledNote("website#986", "")), note: lift, want: true},
		{name: "a different PR", comments: commentsOf(lift), note: blockedPRSettledNote("projectbluefin/website#98", ""), want: false},
		{name: "blocked again after the lift", comments: commentsOf(lift, reblock), note: settled, want: false},
		{name: "first lift", comments: commentsOf(reblock), note: settled, want: false},
		{name: "a note naming no PR is never matched", comments: commentsOf(blockedDeadlineNote("2026-11-01T00:00:00Z")), note: blockedDeadlineNote("2026-11-02T00:00:00Z"), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unblockAlreadyNoted(tc.comments, tc.note); got != tc.want {
				t.Fatalf("unblockAlreadyNoted = %v, want %v", got, tc.want)
			}
		})
	}
}

// unblockServer serves issue comments and records label deletes and posted
// comments.
type unblockServer struct {
	mu       sync.Mutex
	comments map[string]string
	deleted  []string
	posted   map[string]string
}

func (u *unblockServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/website/pulls/986":
		_, _ = io.WriteString(w, `{"number":986,"state":"closed","merged":true,"base":{"ref":"main"}}`)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
		_, _ = io.WriteString(w, u.comments[r.URL.Path])
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/website/issues/"):
		_, _ = io.WriteString(w, `{"number":1,"labels":[{"name":"blocked"},{"name":"agent/reviewer"}]}`)
	case r.Method == http.MethodDelete:
		u.deleted = append(u.deleted, r.URL.Path)
		_, _ = io.WriteString(w, `[]`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.posted[r.URL.Path] = body.Body
		_, _ = io.WriteString(w, `{"id":9}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func jsonComments(t *testing.T, bodies ...string) string {
	t.Helper()
	out := make([]map[string]any, 0, len(bodies))
	for i, b := range bodies {
		out = append(out, map[string]any{"id": i + 1, "body": b})
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Both lift paths on one item post one notice: the blocked sweep stays quiet
// on #987 (the review-backlog lift is there) and speaks on #988 (it is not).
func TestBlockedSweepQuietAfterReviewBacklogLift(t *testing.T) {
	u := &unblockServer{posted: map[string]string{}, comments: map[string]string{
		"/repos/o/website/issues/987/comments": jsonComments(t, reviewBacklogLiftNote("o/website", 986, "")),
		"/repos/o/website/issues/988/comments": jsonComments(t, BlockedOnMarker("o/website", 986)+"\nBlocked on #986"),
	}}
	srv := httptest.NewServer(http.HandlerFunc(u.handler))
	defer srv.Close()
	c := testClient(t, srv.URL)
	res := c.reconcileBlockedTargets(context.Background(), []blockedIssueTarget{
		{repo: "o/website", number: 987, labels: []string{"blocked"}, reason: "blocked on o/website#986"},
		{repo: "o/website", number: 988, labels: []string{"blocked"}, reason: "blocked on o/website#986"},
	}, nil)
	if res.Unblocked != 2 {
		t.Fatalf("result = %+v, want both labels lifted", res)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.posted["/repos/o/website/issues/987/comments"]; ok {
		t.Fatalf("#987 got a second unblock note: %q", u.posted["/repos/o/website/issues/987/comments"])
	}
	if !strings.Contains(u.posted["/repos/o/website/issues/988/comments"], "Unblocked: o/website#986 is no longer open") {
		t.Fatalf("#988 note = %q, want the sweep's note", u.posted["/repos/o/website/issues/988/comments"])
	}
}

// The other order: the review-backlog lift stays quiet when the blocked
// sweep already noted the same PR.
func TestReviewBacklogLiftQuietAfterSweepNote(t *testing.T) {
	u := &unblockServer{posted: map[string]string{}, comments: map[string]string{
		"/repos/o/website/issues/987/comments": jsonComments(t, blockedPRSettledNote("o/website#986", "")),
		"/repos/o/website/issues/988/comments": jsonComments(t, "triage"),
	}}
	srv := httptest.NewServer(http.HandlerFunc(u.handler))
	defer srv.Close()
	c := testClient(t, srv.URL)
	sink := githubIssueBacklogSink{c: c}
	for _, n := range []int{987, 988} {
		if err := sink.LiftBlock(context.Background(), "o/website", ReviewBacklogRef{Number: n}, reviewBacklogLiftNote("o/website", 986, "")); err != nil {
			t.Fatalf("LiftBlock #%d: %v", n, err)
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.deleted) != 2 {
		t.Fatalf("label deletes = %v, want both", u.deleted)
	}
	if _, ok := u.posted["/repos/o/website/issues/987/comments"]; ok {
		t.Fatal("#987 got a second unblock note")
	}
	if _, ok := u.posted["/repos/o/website/issues/988/comments"]; !ok {
		t.Fatal("#988 got no unblock note")
	}
}

// An un-admitted issue is told it waits for a maintainer, not that it is
// actionable; an admitted one keeps today's wording.
func TestUnblockNotesAdmissionAware(t *testing.T) {
	c := &Client{}
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"triage/accepted"}})
	unadmitted := []string{"blocked", "agent/reviewer"}
	admitted := []string{"blocked", "triage/accepted"}

	if got := c.unblockAdmission(admitted); got != "" {
		t.Fatalf("admitted issue got an admission sentence: %q", got)
	}
	adm := c.unblockAdmission(unadmitted)
	if !strings.Contains(adm, "`triage/accepted`") || !strings.Contains(adm, "a maintainer adding it admits this one") {
		t.Fatalf("admission sentence = %q", adm)
	}
	for name, note := range map[string]string{
		"review-backlog lift":      reviewBacklogLiftNote("o/website", 986, adm),
		"review-backlog retriage":  reviewBacklogRetriageNote("o/website", 986, adm),
		"blocked sweep settled":    blockedPRSettledNote("o/website#986", adm),
		"blocked sweep superseded": blockedSupersededSettledNote("o/website", 990, 986, adm),
	} {
		if !strings.Contains(note, "Unblocked — waiting for a maintainer to admit it") || strings.Contains(note, "actionable") || strings.Contains(note, "re-triag") {
			t.Errorf("%s note for an un-admitted issue = %q", name, note)
		}
		if !unblockNoteRefRE.MatchString(note) {
			t.Errorf("%s note no longer names its PR for the dedupe: %q", name, note)
		}
	}
	for name, note := range map[string]string{
		"review-backlog lift":      reviewBacklogLiftNote("o/website", 986, ""),
		"blocked sweep superseded": blockedSupersededSettledNote("o/website", 990, 986, ""),
	} {
		if !strings.Contains(note, "actionable") {
			t.Errorf("%s note for an admitted issue lost its wording: %q", name, note)
		}
	}
}

// The review-backlog sweep reads the item's labels for the wording: on a
// GitHub issue without triage/accepted the lift says it waits for a
// maintainer.
func TestReviewBacklogAdmissionReadsTheIssue(t *testing.T) {
	u := &unblockServer{posted: map[string]string{}, comments: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(u.handler))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"triage/accepted"}})
	if got := c.reviewBacklogAdmission(context.Background(), githubIssueBacklogSink{c: c}, "o/website", ReviewBacklogRef{Number: 987}); !strings.Contains(got, "`triage/accepted`") {
		t.Fatalf("admission = %q, want the triage/accepted sentence", got)
	}
	if got := c.reviewBacklogAdmission(context.Background(), githubIssueBacklogSink{c: c}, "o/website", ReviewBacklogRef{}); got != "" {
		t.Fatalf("no issue number: admission = %q, want \"\"", got)
	}
}

// The blocked sweep words its note from the issue's labels: #989 has no
// triage/accepted, #990 has it.
func TestBlockedSweepNoteNamesAdmission(t *testing.T) {
	u := &unblockServer{posted: map[string]string{}, comments: map[string]string{
		"/repos/o/website/issues/989/comments": jsonComments(t, BlockedOnMarker("o/website", 986)+"\nBlocked on #986"),
		"/repos/o/website/issues/990/comments": jsonComments(t, BlockedOnMarker("o/website", 986)+"\nBlocked on #986"),
	}}
	srv := httptest.NewServer(http.HandlerFunc(u.handler))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"triage/accepted"}})
	c.reconcileBlockedTargets(context.Background(), []blockedIssueTarget{
		{repo: "o/website", number: 989, labels: []string{"blocked", "agent/reviewer"}, reason: "blocked on o/website#986"},
		{repo: "o/website", number: 990, labels: []string{"blocked", "triage/accepted"}, reason: "blocked on o/website#986"},
	}, nil)
	u.mu.Lock()
	defer u.mu.Unlock()
	if n := u.posted["/repos/o/website/issues/989/comments"]; !strings.Contains(n, "waiting for a maintainer to admit it") || strings.Contains(n, "re-triaging") {
		t.Fatalf("#989 note = %q, want the admission wording", n)
	}
	if n := u.posted["/repos/o/website/issues/990/comments"]; !strings.Contains(n, "re-triaging") {
		t.Fatalf("#990 note = %q, want today's wording", n)
	}
}

// rejectBlocked admits any issue not labelled `blocked`: the admission check
// must ask about the issue as it is once the block is gone.
type rejectBlocked struct{}

func (rejectBlocked) Admits(labels []string) bool { return !hasBlockedLabel(labels) }

func TestUnblockAdmissionIgnoresTheBlockedLabel(t *testing.T) {
	c := &Client{}
	c.SetIssueFilter(rejectBlocked{})
	if got := c.unblockAdmission([]string{"blocked", "bug"}); got != "" {
		t.Fatalf("admission = %q, want \"\": `blocked` is coming off", got)
	}
}

// End to end through the review-backlog sweep on a GitHub-issue item: the
// lift note says the item waits for a maintainer when it is not admitted.
func TestReviewBacklogReconcileLiftNamesAdmission(t *testing.T) {
	withReviewBacklogState(t)
	var mu sync.Mutex
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/o/r/pulls/5":
			_, _ = io.WriteString(w, `{"number":5,"state":"closed","merged":true,"base":{"ref":"main"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = io.WriteString(w, `{"number":7,"labels":[{"name":"blocked"},{"name":"agent/reviewer"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodDelete:
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			posted = body.Body
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":9}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"triage/accepted"}})
	sink := githubIssueBacklogSink{c: c}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: config.ReviewBacklogGitHubIssue}, Sink: sink}
	})
	ledger := newReviewBacklogLedger()
	ledger.setFingerprint("o/r", "fp-legacy", reviewBacklogFingerprintRecord{Destination: config.ReviewBacklogGitHubIssue, ItemNumber: 7, ItemKey: "o/r#7", BlockedOnPR: 5, BlockState: reviewBacklogBlockBlocked})
	if err := ledger.save("", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if res := ReconcileReviewBacklogBlocks(context.Background(), c, nil); res.Lifted != 1 || res.Errors != 0 {
		t.Fatalf("result = %+v, want one lift", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(posted, "Unblocked: o/r#5 merged") || !strings.Contains(posted, "waiting for a maintainer to admit it") || strings.Contains(posted, "actionable now") {
		t.Fatalf("lift note = %q", posted)
	}
}
