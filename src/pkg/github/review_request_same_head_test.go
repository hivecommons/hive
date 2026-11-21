package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// sameHeadWatcherFixture is a review watcher against a mock GitHub that
// records every review it is asked to post.
type sameHeadWatcherFixture struct {
	t      *testing.T
	c      *Client
	dir    string
	head   string
	posts  int
	events []string
	bodies []string
	// patches counts in-place review edits (--revise), statusBodies the
	// status comments created (quiet mode folds).
	patches      int
	statusBodies []string
}

func newSameHeadWatcherFixture(t *testing.T, state review.DispatchState) *sameHeadWatcherFixture {
	t.Helper()
	f := &sameHeadWatcherFixture{t: t, head: "48b9e51d43185ed8a038e2cec2b379240e3c7e14"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews"):
			f.posts++
			var body struct {
				Event string `json:"event"`
				Body  string `json:"body"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			f.events = append(f.events, body.Event)
			f.bodies = append(f.bodies, body.Body)
			fmt.Fprintf(w, `{"id":%d,"state":"COMMENTED","commit_id":%q,"html_url":"https://x/pr/5#r%d"}`, f.posts, f.head, f.posts)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/5/reviews"):
			// The App's standing review, asking for a person.
			_, _ = io.WriteString(w, `[{"id":1,"user":{"login":"kubestellar-hive[bot]"},"state":"COMMENTED","body":"Security: a person should decide this."}]`)
		case (r.Method == "PUT" || r.Method == "PATCH") && strings.Contains(r.URL.Path, "/pulls/5/reviews/"):
			f.patches++
			_, _ = io.WriteString(w, `{"id":1,"state":"COMMENTED"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/5"):
			fmt.Fprintf(w, `{"number":5,"head":{"sha":%q}}`, f.head)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues/5/comments"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues/5/comments"):
			var body struct {
				Body string `json:"body"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			f.statusBodies = append(f.statusBodies, body.Body)
			_, _ = io.WriteString(w, `{"id":9}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f.c = reviewTestClient(t, srv.URL)
	f.c.SetAppBotLogin("kubestellar-hive[bot]")
	// One review per perspective, no per-head cap: the cap is not under test.
	f.c.SetReviewCadenceLimits(func() bool { return false }, func() int { return -1 })
	f.dir = withReviewDir(t)
	oldLinks, oldBacklog, oldReports := ReviewLinksPath, ReviewBacklogPath, review.DefaultReportDir
	ReviewLinksPath = filepath.Join(t.TempDir(), "review-links.json")
	ReviewBacklogPath = filepath.Join(t.TempDir(), reviewBacklogFile)
	review.DefaultReportDir = t.TempDir()
	t.Cleanup(func() { ReviewLinksPath, ReviewBacklogPath, review.DefaultReportDir = oldLinks, oldBacklog, oldReports })
	withVerdictDispatchState(t, state)
	return f
}

func (f *sameHeadWatcherFixture) submit(event, body, report string) ReviewResponse {
	f.t.Helper()
	return f.submitRequest(ReviewRequest{Repo: "o/r", Number: 5, Event: event, Agent: "reviewer", Body: body, Report: report})
}

func (f *sameHeadWatcherFixture) submitRequest(req ReviewRequest) ReviewResponse {
	f.t.Helper()
	path, err := WriteReviewRequest(f.dir, req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.c.ProcessReviewRequestsOnce(context.Background())
	raw, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".result.json")
	if err != nil {
		f.t.Fatal(err)
	}
	var resp ReviewResponse
	_ = json.Unmarshal(raw, &resp)
	return resp
}

func (f *sameHeadWatcherFixture) storedSecurity() review.Verdict {
	f.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(review.DefaultReportDir, review.ReviewReportFilePrefix+"*security*"+review.ReviewReportFileSuffix))
	if len(matches) != 1 {
		f.t.Fatalf("want one security report, got %v", matches)
	}
	r, _, ok := review.ReadReportFile(matches[0])
	if !ok {
		f.t.Fatalf("stored report unreadable")
	}
	return r.Verdict
}

func (f *sameHeadWatcherFixture) linkNotApproved() {
	f.t.Helper()
	links, _ := LoadReviewLinks("")
	if l := links["o/r#5"]; strings.EqualFold(l.State, "approved") {
		f.t.Fatalf("review-links.json marked the head approved: %+v", l)
	}
}

// projectbluefin/dakota-iso#376: a review whose verdict the relay will refuse
// (a less strict verdict over a stricter one on the same head, from a review
// not asked for after it) is not posted either. Posting it put an approve on
// GitHub over the hive's own "needs a person" review and marked the head
// approved in review-links.json.
func TestReviewRequestWatcher_RefusedSameHeadVerdictIsNotPosted(t *testing.T) {
	f := newSameHeadWatcherFixture(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: "o/r", Number: 5, HeadSHA: "48b9e51d43185ed8a038e2cec2b379240e3c7e14", Perspective: review.PerspectiveSecurity, Agent: "reviewer", Dispatched: time.Now().UTC().Add(-time.Minute)},
	}})
	head := f.head
	submit := func(event, body, verdict string) ReviewResponse {
		t.Helper()
		return f.submit(event, body, validVerdictJSONWithHead(t, "o/r", 5, "security", verdict, head))
	}

	if resp := submit("comment", "Security: this changes who may merge; a person should decide.", "requires_human"); !resp.OK || f.posts != 1 {
		t.Fatalf("first review must post: %+v posts=%d", resp, f.posts)
	}
	resp := submit("approve", "", "approve")
	if !resp.OK || resp.State != ReviewEventRecordVerdict || f.posts != 1 {
		t.Fatalf("refused approve must not post: %+v posts=%d events=%v", resp, f.posts, f.events)
	}
	if !strings.Contains(resp.Note, "review not posted") || !strings.Contains(resp.Note, "security: requires_human") {
		t.Fatalf("note must say why: %q", resp.Note)
	}
	f.linkNotApproved()
	if got := f.storedSecurity(); got != review.VerdictRequiresHuman {
		t.Fatalf("stored verdict = %s, want requires_human kept", got)
	}
}

// The production layout: one agent's queue-mode session records
// requires_human after its swarm kick, planned earlier, was already typed
// into the session (delivered). The kick's approve, same agent and same
// dispatch time, is refused and nothing is posted.
func TestReviewRequestWatcher_SameAgentQueueThenSwarmApproveIsNotPosted(t *testing.T) {
	row := review.PendingReview{Repo: "o/r", Number: 5, HeadSHA: "48b9e51d43185ed8a038e2cec2b379240e3c7e14", Perspective: review.PerspectiveSecurity, Agent: "reviewer", Dispatched: time.Now().UTC().Add(-time.Minute)}
	row.Delivered = row.Dispatched.Add(3 * time.Second)
	f := newSameHeadWatcherFixture(t, review.DispatchState{Pending: []review.PendingReview{row}})
	if resp := f.submit("comment", "Security: a person should decide this.", validVerdictJSONWithHead(t, "o/r", 5, "security", "requires_human", f.head)); !resp.OK || f.posts != 1 {
		t.Fatalf("queue session's review must post: %+v posts=%d", resp, f.posts)
	}
	resp := f.submit("approve", "", validVerdictJSONWithHead(t, "o/r", 5, "security", "approve", f.head))
	if !resp.OK || resp.State != ReviewEventRecordVerdict || f.posts != 1 {
		t.Fatalf("the same agent's approve must not post: %+v posts=%d events=%v", resp, f.posts, f.events)
	}
	f.linkNotApproved()
	if got := f.storedSecurity(); got != review.VerdictRequiresHuman {
		t.Fatalf("stored verdict = %s, want requires_human kept", got)
	}
}

// When the perspectives the relay accepts carry findings, they still reach
// GitHub: as a COMMENT (never an approve) with one line saying the stricter
// verdict still stands, and the refused perspective's approve text left out.
func TestReviewRequestWatcher_PartialRefusalPostsAcceptedFindingsAsComment(t *testing.T) {
	head := "48b9e51d43185ed8a038e2cec2b379240e3c7e14"
	dispatched := time.Now().UTC().Add(-time.Minute)
	f := newSameHeadWatcherFixture(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: "o/r", Number: 5, HeadSHA: head, Perspective: review.PerspectiveSecurity, Agent: "reviewer", Dispatched: dispatched},
		{Repo: "o/r", Number: 5, HeadSHA: head, Perspective: review.PerspectiveCorrectness, Agent: "reviewer", Dispatched: dispatched},
	}})
	if resp := f.submit("comment", "Security: a person should decide this.", validVerdictJSONWithHead(t, "o/r", 5, "security", "requires_human", head)); !resp.OK || f.posts != 1 {
		t.Fatalf("first review must post: %+v posts=%d", resp, f.posts)
	}
	// The confidence line would score the refused approve too.
	f.c.SetConfidenceScore(func() bool { return true })

	resp := f.submit("approve", "LGTM overall; all clear on security.", acceptedFindingsReport(t, head))
	if !resp.OK || f.posts != 2 {
		t.Fatalf("accepted findings must post: %+v posts=%d", resp, f.posts)
	}
	if f.events[1] != "COMMENT" {
		t.Fatalf("event = %q, want COMMENT", f.events[1])
	}
	body := f.bodies[1]
	for _, want := range []string{"stricter verdict on this head still stands", "security: requires_human", "correctness", "nil map write on first call", "pkg/x/x.go:12"} {
		if !strings.Contains(body, want) {
			t.Fatalf("posted body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "all clear on security") {
		t.Fatalf("posted body kept the refused perspective's text:\n%s", body)
	}
	if strings.Contains(body, "**Confidence:") {
		t.Fatalf("posted body carries a confidence score over the refused perspective too:\n%s", body)
	}
	if !strings.Contains(resp.Note, "posted as COMMENT") {
		t.Fatalf("note = %q", resp.Note)
	}
	f.linkNotApproved()
	if got := f.storedSecurity(); got != review.VerdictRequiresHuman {
		t.Fatalf("stored security = %s, want requires_human kept", got)
	}
}

// acceptedFindingsReport is a combined verdict whose security approve the
// relay refuses (a requires_human stands) and whose correctness
// changes_requested, with one finding, it accepts.
func acceptedFindingsReport(t *testing.T, head string) string {
	t.Helper()
	var correctness map[string]any
	if err := json.Unmarshal([]byte(validVerdictJSONWithHead(t, "o/r", 5, "correctness", "changes_requested", head)), &correctness); err != nil {
		t.Fatal(err)
	}
	correctness["findings"] = []any{map[string]any{"title": "nil map write on first call", "severity": "high", "summary": "initialise the map before writing", "file": "pkg/x/x.go", "line": 12}}
	cRaw, _ := json.Marshal(correctness)
	return "[" + validVerdictJSONWithHead(t, "o/r", 5, "security", "approve", head) + "," + string(cRaw) + "]"
}

// sameHeadFixtureWithStoredRequiresHuman is a fixture whose security
// requires_human is already on file, recorded straight through the relay.
func sameHeadFixtureWithStoredRequiresHuman(t *testing.T) *sameHeadWatcherFixture {
	t.Helper()
	head := "48b9e51d43185ed8a038e2cec2b379240e3c7e14"
	dispatched := time.Now().UTC().Add(-time.Minute)
	f := newSameHeadWatcherFixture(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: "o/r", Number: 5, HeadSHA: head, Perspective: review.PerspectiveSecurity, Agent: "reviewer", Dispatched: dispatched},
		{Repo: "o/r", Number: 5, HeadSHA: head, Perspective: review.PerspectiveCorrectness, Agent: "reviewer", Dispatched: dispatched},
	}})
	f.c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer",
		Report: validVerdictJSONWithHead(t, "o/r", 5, "security", "requires_human", head)}, "")
	if got := f.storedSecurity(); got != review.VerdictRequiresHuman {
		t.Fatalf("fixture: stored security = %s", got)
	}
	return f
}

// A refused --revise posts nothing, not even the accepted findings as a
// COMMENT: the downgrade would edit the App's standing review in place and
// could wipe the "needs a person" question it carries.
func TestReviewRequestWatcher_RefusedReviseLeavesTheStandingReviewUntouched(t *testing.T) {
	f := sameHeadFixtureWithStoredRequiresHuman(t)
	f.c.SetReviseRepos([]string{"o/r"})
	resp := f.submitRequest(ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Agent: "reviewer", Revise: true,
		Body: "Revised: all clear on security.", Report: acceptedFindingsReport(t, f.head)})
	if !resp.OK || resp.State != ReviewEventRecordVerdict {
		t.Fatalf("refused revise: %+v", resp)
	}
	if f.patches != 0 || f.posts != 0 {
		t.Fatalf("refused revise touched GitHub: patches=%d posts=%d", f.patches, f.posts)
	}
	if got := f.storedSecurity(); got != review.VerdictRequiresHuman {
		t.Fatalf("stored security = %s, want requires_human", got)
	}
}

// Quiet mode folds only the accepted perspectives' findings into the status
// comment, never the refused review's own text, and nothing when the
// accepted perspectives carry none.
func TestReviewRequestWatcher_QuietFoldCarriesOnlyAcceptedFindings(t *testing.T) {
	f := sameHeadFixtureWithStoredRequiresHuman(t)
	f.c.SetNotificationsQuiet(func() bool { return true })
	f.c.SetReviseRepos([]string{"o/r"})

	resp := f.submit("comment", "Nothing new; all clear on security.", validVerdictJSONWithHead(t, "o/r", 5, "security", "approve", f.head))
	if !resp.OK || resp.State != ReviewEventRecordVerdict || f.posts != 0 {
		t.Fatalf("refused approve with nothing accepted: %+v posts=%d", resp, f.posts)
	}
	if len(f.statusBodies) != 0 {
		t.Fatalf("folded the refused review's text: %q", f.statusBodies)
	}

	// A refused revise takes the post-nothing branch with accepted
	// findings: they, and only they, are folded.
	resp = f.submitRequest(ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Agent: "reviewer", Revise: true,
		Body: "Revised: all clear on security.", Report: acceptedFindingsReport(t, f.head)})
	if !resp.OK || f.posts != 0 || f.patches != 0 {
		t.Fatalf("refused revise in quiet mode: %+v posts=%d patches=%d", resp, f.posts, f.patches)
	}
	if len(f.statusBodies) != 1 {
		t.Fatalf("status comments = %q, want one fold", f.statusBodies)
	}
	fold := f.statusBodies[0]
	if !strings.Contains(fold, "nil map write on first call") || strings.Contains(fold, "all clear on security") {
		t.Fatalf("fold must carry the accepted finding and not the refused text:\n%s", fold)
	}
}
