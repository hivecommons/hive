package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewEditServer records PR comments and review edits on o/r.
type reviewEditServer struct {
	mu       sync.Mutex
	comments []string
	edits    map[int64][]string
}

func newReviewEditServer(t *testing.T) (*reviewEditServer, *httptest.Server) {
	t.Helper()
	rs := &reviewEditServer{edits: map[int64][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rs.mu.Lock()
		defer rs.mu.Unlock()
		var payload struct {
			Body string `json:"body"`
		}
		var pr int
		var id int64
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewDecoder(r.Body).Decode(&payload)
			rs.comments = append(rs.comments, payload.Body)
			_, _ = io.WriteString(w, `{"id":9}`)
		case r.Method == http.MethodPut:
			if _, err := fmt.Sscanf(r.URL.Path, "/repos/o/r/pulls/%d/reviews/%d", &pr, &id); err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			rs.edits[id] = append(rs.edits[id], payload.Body)
			fmt.Fprintf(w, `{"id":%d,"state":"COMMENTED"}`, id)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return rs, srv
}

func postedReview(id int64, login, state, body string) *gh.PullRequestReview {
	return &gh.PullRequestReview{ID: gh.Ptr(id), State: gh.Ptr(state), Body: gh.Ptr(body), User: &gh.User{Login: gh.Ptr(login)}}
}

func headsBacklogClient(t *testing.T, srvURL string, sink *fakeBacklogSink) *Client {
	t.Helper()
	c := reviewTestClient(t, srvURL)
	c.appBotLogin = "hive-app[bot]"
	c.SetReviewBacklog(func() (bool, int) { return true, 3 })
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P2"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, Labels: []string{"followup"}},
			Sink:     sink,
		}
	})
	return c
}

// In heads mode only the decision step's reconciled findings become backlog
// items, and they are listed in the decided review itself (#13833).
func TestReviewBacklogHeadsModeFilesDecidedReportsOnly(t *testing.T) {
	withReviewBacklogState(t)
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: "o/r", Number: 5, HeadSHA: "abc", Perspective: review.PerspectiveCorrectness, Agent: "code-reviewer-sonnet-1", Head: "code-reviewer-sonnet"},
		{Repo: "o/r", Number: 5, HeadSHA: "abc", Perspective: review.PerspectiveCorrectness, Agent: "code-reviewer", Decision: true},
	}})
	rs, srv := newReviewEditServer(t)
	sink := &fakeBacklogSink{dest: config.ReviewBacklogLinear}
	c := headsBacklogClient(t, srv.URL, sink)
	ctx := context.Background()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	report := routingReport(t, 5, "abc", nit("stale docstring", "low", "pkg/a.go", 7, "says title only"))

	// A raw head report files nothing: the decision step will.
	if err := c.fileReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "code-reviewer-sonnet-1", Report: report}, now, nil); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 0 {
		t.Fatalf("raw head report filed %d items, want 0", len(sink.created))
	}

	decided := postedReview(77, "hive-app[bot]", "COMMENTED", "Decided review body.")
	if err := c.fileReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "code-reviewer", Report: report}, now, decided); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 1 {
		t.Fatalf("decided report filed %d items, want 1", len(sink.created))
	}
	if !strings.Contains(strings.Join(sink.created[0].Labels, ","), BlockedLabel) {
		t.Fatalf("decided item labels = %v, want blocked", sink.created[0].Labels)
	}
	if len(rs.comments) != 0 {
		t.Fatalf("heads mode posted a summary comment: %q", rs.comments)
	}
	edits := rs.edits[77]
	if len(edits) != 1 {
		t.Fatalf("review 77 edits = %d, want 1", len(edits))
	}
	if !strings.HasPrefix(edits[0], "Decided review body.") || !strings.Contains(edits[0], reviewFollowUpsMarker) ||
		!strings.Contains(edits[0], "[ENG-1](https://tracker.test/ENG-1) — [reviewer] Review backlog (correctness): stale docstring (filed)") {
		t.Fatalf("follow-ups edit = %q", edits[0])
	}

	ledger, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	var blocked int
	for _, fps := range ledger.Fingerprints {
		for _, r := range fps {
			if r.BlockedOnPR == 5 && r.BlockState == reviewBacklogBlockBlocked {
				blocked++
			}
		}
	}
	if blocked != 1 {
		t.Fatalf("ledger items blocked on PR 5 = %d, want 1", blocked)
	}

	// The same decided findings again: nothing new filed, and the section is
	// rewritten (not duplicated) listing the item as already tracked.
	again := postedReview(77, "hive-app[bot]", "COMMENTED", edits[0])
	if err := c.fileReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "code-reviewer", Report: report}, now.Add(time.Hour), again); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 1 {
		t.Fatalf("re-review filed again: %d items", len(sink.created))
	}
	last := rs.edits[77][len(rs.edits[77])-1]
	if strings.Count(last, reviewFollowUpsMarker) != 1 || !strings.Contains(last, "(already tracked)") {
		t.Fatalf("rewritten follow-ups = %q", last)
	}
}

// The follow-ups edit only ever touches the hive's own COMMENTED review.
func TestReviewFollowUpsLeaveForeignAndFormalReviewsAlone(t *testing.T) {
	withReviewBacklogState(t)
	rs, srv := newReviewEditServer(t)
	c := headsBacklogClient(t, srv.URL, &fakeBacklogSink{dest: config.ReviewBacklogLinear})
	listed := []reviewBacklogFiled{{key: "ENG-1", title: "x", status: reviewBacklogListedFiled}}
	ctx := context.Background()
	req := ReviewRequest{Repo: "o/r", Number: 5, Agent: "code-reviewer"}
	c.appendReviewFollowUps(ctx, req, postedReview(1, "someone-else", "COMMENTED", "theirs"), listed, 0)
	c.appendReviewFollowUps(ctx, req, postedReview(2, "hive-app[bot]", "APPROVED", "formal"), listed, 0)
	c.appendReviewFollowUps(ctx, req, nil, listed, 0)
	if len(rs.edits) != 0 {
		t.Fatalf("edited reviews %v; want none", rs.edits)
	}
	// No write when the section is already current.
	body := withReviewFollowUps("mine", listed, 0)
	c.appendReviewFollowUps(ctx, req, postedReview(3, "hive-app[bot]", "COMMENTED", body), listed, 0)
	if len(rs.edits) != 0 {
		t.Fatalf("unchanged section rewritten: %v", rs.edits)
	}
}

func TestReviewFollowUpsSection(t *testing.T) {
	got := withReviewFollowUps("Body\n\n"+reviewFollowUpsMarker+"\nold section", []reviewBacklogFiled{
		{key: "ENG-2", title: "b", status: reviewBacklogListedUpdated},
		{key: "ENG-1", title: "a", status: reviewBacklogListedFiled},
		{key: "ENG-1", title: "a", status: reviewBacklogListedFiled},
		{number: 41, title: "gh", status: reviewBacklogListedTracked},
	}, 2)
	for _, want := range []string{
		"Body\n\n" + reviewFollowUpsMarker,
		"- ENG-1 — a (filed)\n- ENG-2 — b (re-raised)",
		"- #41 — gh (already tracked)",
		"2 more held back by the per-PR daily cap",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old section") || strings.Count(got, "ENG-1") != 1 {
		t.Fatalf("section not rewritten cleanly:\n%s", got)
	}
}

type fakeResolverSink struct {
	fakeBacklogSink
	lifted, retriaged []string
}

func (f *fakeResolverSink) LiftBlock(_ context.Context, _ string, ref ReviewBacklogRef, note string) error {
	if !strings.Contains(note, blockedUnblockedMarker) {
		return fmt.Errorf("lift note lacks marker: %q", note)
	}
	if strings.Contains(note, reviewBacklogRetriageMarker) {
		f.retriaged = append(f.retriaged, ref.Key)
		return nil
	}
	f.lifted = append(f.lifted, ref.Key)
	return nil
}

// Items wait on their PR: merged lifts the block, closed-unmerged unblocks
// the item for re-triage (never closes it), open leaves it, and legacy items
// without a PR are ignored.
func TestReconcileReviewBacklogBlocks(t *testing.T) {
	withReviewBacklogState(t)
	var lookups []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		lookups = append(lookups, r.URL.Path)
		switch r.URL.Path {
		case "/repos/o/r/pulls/5":
			_, _ = io.WriteString(w, `{"number":5,"state":"closed","merged":true}`)
		case "/repos/o/r/pulls/6":
			_, _ = io.WriteString(w, `{"number":6,"state":"closed","merged":false}`)
		case "/repos/o/r/pulls/7":
			_, _ = io.WriteString(w, `{"number":7,"state":"open"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	sink := &fakeResolverSink{fakeBacklogSink: fakeBacklogSink{dest: config.ReviewBacklogLinear}}
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear}, Sink: sink}
	})
	now := time.Now().UTC()
	ledger := newReviewBacklogLedger()
	for fp, rec := range map[string]reviewBacklogFingerprintRecord{
		"fp-merged": {Destination: config.ReviewBacklogLinear, ItemID: "id-a", ItemKey: "ENG-A", BlockedOnPR: 5, BlockState: reviewBacklogBlockBlocked},
		"fp-closed": {Destination: config.ReviewBacklogLinear, ItemID: "id-b", ItemKey: "ENG-B", BlockedOnPR: 6, BlockState: reviewBacklogBlockBlocked},
		"fp-open":   {Destination: config.ReviewBacklogLinear, ItemID: "id-c", ItemKey: "ENG-C", BlockedOnPR: 7, BlockState: reviewBacklogBlockBlocked},
		"fp-legacy": {Destination: config.ReviewBacklogLinear, ItemID: "id-d", ItemKey: "ENG-D", PRs: []int{5}},
		"fp-other":  {Destination: config.ReviewBacklogJira, ItemID: "J-1", ItemKey: "J-1", BlockedOnPR: 5, BlockState: reviewBacklogBlockBlocked},
	} {
		ledger.setFingerprint("o/r", fp, rec)
	}
	if err := ledger.save("", now); err != nil {
		t.Fatal(err)
	}

	res := ReconcileReviewBacklogBlocks(context.Background(), c, nil)
	if res.Checked != 4 || res.Lifted != 1 || res.Retriaged != 1 || res.Errors != 1 {
		t.Fatalf("result = %+v, want checked 4, lifted 1, retriaged 1, errors 1 (foreign destination)", res)
	}
	if strings.Join(sink.lifted, ",") != "ENG-A" || strings.Join(sink.retriaged, ",") != "ENG-B" {
		t.Fatalf("lifted=%v retriaged=%v", sink.lifted, sink.retriaged)
	}
	saved, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for fp, rec := range saved.Fingerprints["o/r"] {
		states[fp] = rec.BlockState
	}
	if states["fp-merged"] != reviewBacklogBlockLifted || states["fp-closed"] != reviewBacklogBlockRetriage || states["fp-open"] != reviewBacklogBlockBlocked {
		t.Fatalf("states = %v", states)
	}

	// Second pass: only the still-open and the foreign item are looked at,
	// and nothing is resolved twice.
	sink.lifted, sink.retriaged = nil, nil
	res = ReconcileReviewBacklogBlocks(context.Background(), c, nil)
	if res.Lifted+res.Retriaged != 0 || res.Checked != 2 {
		t.Fatalf("second pass = %+v, want only the open and foreign items checked", res)
	}
}

// A closed-unmerged source PR never closes the item: the note unblocks it
// and asks for re-triage (maintainer decision on #13849).
func TestReviewBacklogRetriageNote(t *testing.T) {
	note := reviewBacklogRetriageNote("o/r", 6)
	for _, want := range []string{blockedUnblockedMarker, reviewBacklogRetriageMarker, "source PR o/r#6 closed unmerged — re-triage against current base"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %q", want, note)
		}
	}
}

func TestBlockedOnReviewedPRNote(t *testing.T) {
	note := blockedOnReviewedPRNote("o/r", 12)
	if repo, pr, ok := ParseBlockedOnMarker(note); !ok || repo != "o/r" || pr != 12 {
		t.Fatalf("marker = %q %d %v", repo, pr, ok)
	}
	for _, want := range []string{"Blocked on PR #12.", "unblocks this item for re-triage if the PR is closed without merging", "close this item without a PR"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %q", want, note)
		}
	}
}
func TestReconcileReviewBacklogBlocksSupersede(t *testing.T) {
	withReviewBacklogState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		// PR 10 closed, superseded by #11 (open)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/10":
			_, _ = io.WriteString(w, `{"number":10,"state":"closed","merged":false}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/10/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"Superseded by #11."}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/11":
			_, _ = io.WriteString(w, `{"number":11,"state":"open"}`)

		// PR 20 closed, superseded by #21 (merged)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/20":
			_, _ = io.WriteString(w, `{"number":20,"state":"closed","merged":false}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/20/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"Superseded by #21."}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/21":
			_, _ = io.WriteString(w, `{"number":21,"state":"closed","merged":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()

	sink := &fakeResolverSink{fakeBacklogSink: fakeBacklogSink{dest: config.ReviewBacklogLinear}}
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear}, Sink: sink}
	})
	now := time.Now().UTC()
	ledger := newReviewBacklogLedger()
	ledger.setFingerprint("o/r", "fp-superseded-open", reviewBacklogFingerprintRecord{
		Destination: config.ReviewBacklogLinear, ItemID: "id-1", ItemKey: "ENG-1", BlockedOnPR: 10, BlockState: reviewBacklogBlockBlocked,
	})
	ledger.setFingerprint("o/r", "fp-superseded-merged", reviewBacklogFingerprintRecord{
		Destination: config.ReviewBacklogLinear, ItemID: "id-2", ItemKey: "ENG-2", BlockedOnPR: 20, BlockState: reviewBacklogBlockBlocked,
	})
	if err := ledger.save("", now); err != nil {
		t.Fatal(err)
	}

	res := ReconcileReviewBacklogBlocks(context.Background(), c, nil)
	if res.Checked != 2 || res.Lifted != 1 || res.Retriaged != 0 || res.Errors != 0 {
		t.Fatalf("result = %+v, want Checked=2 Lifted=1 Retriaged=0 Errors=0", res)
	}

	// ENG-2 should have been lifted
	if len(sink.lifted) != 1 || sink.lifted[0] != "ENG-2" {
		t.Fatalf("lifted = %v, want [ENG-2]", sink.lifted)
	}

	saved, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	// fp-superseded-open should now be re-targeted to PR 11 in the ledger and stay blocked
	rec1 := saved.Fingerprints["o/r"]["fp-superseded-open"]
	if rec1.BlockedOnPR != 11 || rec1.BlockState != reviewBacklogBlockBlocked {
		t.Fatalf("rec1 = %+v, want BlockedOnPR=11 BlockState=blocked", rec1)
	}
	// fp-superseded-merged should now be lifted and record PR 21
	rec2 := saved.Fingerprints["o/r"]["fp-superseded-merged"]
	if rec2.BlockedOnPR != 21 || rec2.BlockState != reviewBacklogBlockLifted {
		t.Fatalf("rec2 = %+v, want BlockedOnPR=21 BlockState=lifted", rec2)
	}
}
