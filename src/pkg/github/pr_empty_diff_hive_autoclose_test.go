package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// hivecommons/hive#15499: the hive's own adoption whose change had already
// landed through another PR sat at "needs decision" for a day while
// reviewers asked a person "close as a no-op?" six times. The rule closes
// it; a person's empty PR still gets the prompt.
func TestDecideHiveEmptyDiffAutoClose(t *testing.T) {
	base := HiveEmptyDiffAutoCloseFacts{Enabled: true, HiveAuthored: true, DiffKnown: true, Level: acmmLevelAutonomous, HeadAge: time.Hour}
	with := func(mut func(*HiveEmptyDiffAutoCloseFacts)) HiveEmptyDiffAutoCloseFacts {
		f := base
		mut(&f)
		return f
	}
	cases := []struct {
		name        string
		facts       HiveEmptyDiffAutoCloseFacts
		wantVerdict string
		wantReason  string
	}{
		{"empty hive PR closes", base, hiveEmptyDiffVerdictClose, SupersessionActionClosedEmptyDiff},
		{"disabled keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Enabled = false }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonDisabled},
		{"merged keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Merged = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonMerged},
		{"draft keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Draft = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonDraft},
		{"unknown compare keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.DiffKnown = false }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonUnknown},
		{"real diff keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.DiffFiles = 1 }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHasDiff},
		{"person's empty PR is a prompt, never a close", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.HiveAuthored = false }), hiveEmptyDiffVerdictHumanNote, hiveEmptyDiffReasonNotHive},
		{"held keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Held = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHeld},
		{"keep-open keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.KeptOpen = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonKeepOpen},
		{"parked by a person keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.ParkedByPerson = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonPerson},
		{"below L6 keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Level = acmmLevelAutonomous - 1 }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonLevel},
		{"fresh head keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.HeadAge = EmptyDiffMinHeadAge / 2 }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonTooFresh},
		// hivecommons/hive#15647: the reviewer read this head and said it
		// is already landed; a fresh head is not mid-push, and files that
		// are all identical on the base tip are no diff.
		{"reviewer-confirmed fresh head closes", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.HeadAge = 0; f.ReviewerConfirmed = true }), hiveEmptyDiffVerdictClose, SupersessionActionClosedEmptyDiff},
		{"files identical on base close as already landed", with(func(f *HiveEmptyDiffAutoCloseFacts) {
			f.DiffFiles = 2
			f.TreeMatchesBase = true
			f.ReviewerConfirmed = true
		}), hiveEmptyDiffVerdictClose, SupersessionActionClosedAlreadyLanded},
		{"files identical on base but a person's PR is a prompt", with(func(f *HiveEmptyDiffAutoCloseFacts) {
			f.DiffFiles = 2
			f.TreeMatchesBase = true
			f.HiveAuthored = false
		}), hiveEmptyDiffVerdictHumanNote, hiveEmptyDiffReasonNotHive},
		// hivecommons/hive#15732: a person's empty PR whose landing is
		// proven closes like the hive's own; a real diff still keeps it.
		{"person's empty PR with a proven landing closes", with(func(f *HiveEmptyDiffAutoCloseFacts) {
			f.HiveAuthored = false
			f.LandingProven = true
			f.ReviewerConfirmed = true
		}), hiveEmptyDiffVerdictClose, SupersessionActionClosedEmptyDiff},
		{"person's PR with a real diff keeps even with a landing", with(func(f *HiveEmptyDiffAutoCloseFacts) {
			f.HiveAuthored = false
			f.LandingProven = true
			f.DiffFiles = 1
		}), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHasDiff},
		{"person's proven-landed PR parked by a person keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) {
			f.HiveAuthored = false
			f.LandingProven = true
			f.ParkedByPerson = true
		}), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonPerson},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, r := DecideHiveEmptyDiffAutoClose(tc.facts)
			if v != tc.wantVerdict || r != tc.wantReason {
				t.Fatalf("got (%s, %s), want (%s, %s)", v, r, tc.wantVerdict, tc.wantReason)
			}
		})
	}
}

func TestPRChangedFilesClaimsEmpty(t *testing.T) {
	if PRChangedFilesClaimsEmpty(nil) || PRChangedFilesClaimsEmpty(&gh.PullRequest{}) {
		t.Fatal("missing counts (list payload) must not read as empty")
	}
	if !PRChangedFilesClaimsEmpty(&gh.PullRequest{ChangedFiles: gh.Ptr(0), Additions: gh.Ptr(0), Deletions: gh.Ptr(0)}) {
		t.Fatal("0/0/0 is empty")
	}
	if PRChangedFilesClaimsEmpty(&gh.PullRequest{ChangedFiles: gh.Ptr(0), Additions: gh.Ptr(2), Deletions: gh.Ptr(0)}) {
		t.Fatal("any count above zero is not empty")
	}
}

func TestRenderEmptyDiffCloseCommentCarriesMarkers(t *testing.T) {
	body := renderEmptyDiffCloseComment(supersessionCloser{Repo: "hivecommons/hive", Number: 15427, URL: "https://github.com/hivecommons/hive/pull/15427"}, "closed: empty diff", false)
	for _, want := range []string{supersessionSweepMarker, reviewRedundantOfMarker + " #15427 -->", "[#15427](https://github.com/hivecommons/hive/pull/15427)", "edited in place"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}
	if reviewRedundantOfNumber(body) != 15427 {
		t.Fatalf("redundant-of not readable back: %d", reviewRedundantOfNumber(body))
	}
}

// hivecommons/hive#15499: the adoption's dispatch records carried no
// author_agent, so the relay judged the hive's own PR non-hive and routed
// its decision to the contributor lane.
func TestDispatchRecordHiveAuthored(t *testing.T) {
	if !dispatchRecordHiveAuthored("", true) || !dispatchRecordHiveAuthored("author-1", false) || dispatchRecordHiveAuthored("  ", false) {
		t.Fatal("hive_authored on the record, or an author agent, marks the PR hive-authored; neither does not")
	}
}

func TestReviewRedundantOfNumber(t *testing.T) {
	cases := map[string]int{
		"<!-- hive-verdict: needs-decision -->\n<!-- hive-redundant-of: #15427 -->":                     15427,
		"<!-- hive-redundant-of: hivecommons/hive#15427 -->":                                            15427,
		"<!-- hive-redundant-of: https://github.com/hivecommons/hive/pull/15427 -->\nclose as a no-op?": 15427,
		"<!-- hive-redundant-of: none -->":                                                              0,
		"no marker at all":                                                                              0,
	}
	for body, want := range cases {
		if got := reviewRedundantOfNumber(body); got != want {
			t.Fatalf("%q → %d, want %d", body, got, want)
		}
	}
}

// hivecommons/hive#15603: updated_at is bumped by labels and comments, so a
// recently touched PR with an old head must still count as old — and a PR
// just opened from an old branch must still count as just opened.
func TestEmptyDiffHeadAgeFlooredAtCreatedAt(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	pr := &gh.PullRequest{UpdatedAt: &gh.Timestamp{Time: now.Add(-time.Minute)}}
	if got := emptyDiffHeadAge(pr, now.Add(-time.Hour), now); got != time.Hour {
		t.Fatalf("head age = %v, want 1h from the head commit", got)
	}
	if got := emptyDiffHeadAge(pr, time.Time{}, now); got != time.Minute {
		t.Fatalf("head age = %v, want 1m from updated_at fallback", got)
	}
	// A PR just opened from an old branch: its head commit predates the PR,
	// so the age is the PR's own age and the just-opened grace period holds.
	justOpened := &gh.PullRequest{
		CreatedAt: &gh.Timestamp{Time: now.Add(-2 * time.Minute)},
		UpdatedAt: &gh.Timestamp{Time: now.Add(-time.Minute)},
	}
	if got := emptyDiffHeadAge(justOpened, now.Add(-time.Hour), now); got != 2*time.Minute {
		t.Fatalf("head age = %v, want 2m floored at created_at", got)
	}
	// A head commit pushed after the PR opened measures from the commit.
	if got := emptyDiffHeadAge(justOpened, now.Add(-90*time.Second), now); got != 90*time.Second {
		t.Fatalf("head age = %v, want 90s from the head commit", got)
	}
	// No commit date and no created_at: updated_at still stands in.
	if got := emptyDiffHeadAge(&gh.PullRequest{}, time.Time{}, now); got != 0 {
		t.Fatalf("head age = %v, want 0 with no timestamps", got)
	}
}

// A head already on the base lists no commits in base...head, so the
// compare's commit dates are empty; the merge base is the head itself and
// its date must stand in, not updated_at (hivecommons/hive#15603).
func TestPRDiffAgainstBaseHeadOnBaseUsesMergeBaseDate(t *testing.T) {
	headDate := time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/hivecommons/hive/compare/v5...head999", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "behind",
			"commits": []any{},
			"files":   []any{},
			"merge_base_commit": map[string]any{
				"sha":    "head999",
				"commit": map[string]any{"committer": map[string]any{"date": headDate.Format(time.RFC3339)}},
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, slog.Default())
	pr := &gh.PullRequest{Base: &gh.PullRequestBranch{Ref: gh.Ptr("v5")}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("head999")}}
	files, headAt, known, _ := c.prDiffAgainstBase(context.Background(), "hivecommons", "hive", pr)
	if !known || len(files) != 0 {
		t.Fatalf("diff = %d files, known=%v; want empty and known", len(files), known)
	}
	if !headAt.Equal(headDate) {
		t.Fatalf("headAt = %v, want merge-base (head) date %v", headAt, headDate)
	}
}

// hivecommons/hive#15689: CloseIssue's gates (here the tracking-issue
// refusal) may keep an issue open; the "this issue is done" note must then
// not be posted. A close that goes through still gets its note.
func TestCloseIssuesAlreadyLandedNotesOnlyClosedIssues(t *testing.T) {
	var mu sync.Mutex
	var comments, closes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/1":
			_ = json.NewEncoder(w).Encode(trackingTestIssue(1, nil, trackingTestBody))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/2":
			_ = json.NewEncoder(w).Encode(&gh.Issue{Number: gh.Ptr(2), State: gh.Ptr("open"), Title: gh.Ptr("Fix the sweep"), User: &gh.User{Login: gh.Ptr("owner")}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			comments = append(comments, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPatch:
			closes = append(closes, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "closed"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	pr := &gh.PullRequest{Number: gh.Ptr(7), Body: gh.Ptr("Closes #1\nCloses #2"), Base: &gh.PullRequestBranch{Ref: gh.Ptr("v5")}}

	got := c.closeIssuesAlreadyLanded(context.Background(), "o", "r", pr, 5, true)
	if !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("closed = %v, want [2]", got)
	}
	if want := []string{"/repos/o/r/issues/2"}; !reflect.DeepEqual(closes, want) {
		t.Fatalf("closes = %v, want %v", closes, want)
	}
	if want := []string{"/repos/o/r/issues/2/comments"}; !reflect.DeepEqual(comments, want) {
		t.Fatalf("done notes = %v, want only on the closed issue %v", comments, want)
	}
}

// alreadyLandedAnyAuthorServer is a GitHub stand-in for the any-author
// already-landed close (hivecommons/hive#15732): PR 15544 by a person,
// compare base...head as given, PR 15545 merged or not.
type alreadyLandedAnyAuthorServer struct {
	mu       sync.Mutex
	compare  map[string]any
	landedOK bool
	closed   bool
	comments []string
	labels   []string
}

func (s *alreadyLandedAnyAuthorServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/compare/"):
		_ = json.NewEncoder(w).Encode(s.compare)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/15545":
		pr := map[string]any{"number": 15545, "state": "closed", "user": map[string]any{"login": "clubanderson"}}
		if s.landedOK {
			pr["merged"] = true
			pr["merged_at"] = "2026-10-20T01:00:00Z"
		}
		_ = json.NewEncoder(w).Encode(pr)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/15544/comments":
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/15544/comments":
		var c struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&c)
		s.comments = append(s.comments, c.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/15544/labels":
		var names []string
		_ = json.NewDecoder(r.Body).Decode(&names)
		s.labels = append(s.labels, names...)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/issues/15544":
		s.closed = true
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 15544, "state": "closed"})
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/labels"):
		_ = json.NewEncoder(w).Encode(map[string]any{"name": SupersededLabel})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/contents/"):
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": "base-blob", "type": "file"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func alreadyLandedContributorPR(login string, changed int) *gh.PullRequest {
	return &gh.PullRequest{Number: gh.Ptr(15544), State: gh.Ptr("open"), Title: gh.Ptr("fix: thing"), Body: gh.Ptr("No closing refs."),
		User: &gh.User{Login: gh.Ptr(login)}, ChangedFiles: gh.Ptr(changed), Additions: gh.Ptr(0), Deletions: gh.Ptr(0),
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("k1k1k1k1k1")}, Base: &gh.PullRequestBranch{Ref: gh.Ptr("v5")},
		CreatedAt: &gh.Timestamp{Time: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}}
}

// TestCloseAlreadyLandedAnyAuthorClosesOnProvenLanding is
// hivecommons/hive#15732: #15544 by a contributor was +0/−0 against v5
// because #15545 (merged) carried the same change, and sat at 085 for 9h.
// With the empty diff and the named PR merged the hive closes it as
// superseded, thanking the author; trust is not consulted.
func TestCloseAlreadyLandedAnyAuthorClosesOnProvenLanding(t *testing.T) {
	for _, login := range []string{"karibhou", "trusted-maintainer"} {
		t.Run(login, func(t *testing.T) {
			api := &alreadyLandedAnyAuthorServer{landedOK: true, compare: map[string]any{"files": []any{}, "commits": []any{}, "status": "identical"}}
			srv := httptest.NewServer(http.HandlerFunc(api.handler))
			defer srv.Close()
			c := testClient(t, srv.URL)
			res, err := c.CloseAlreadyLandedHivePRIfEligible(context.Background(), "o/r", alreadyLandedContributorPR(login, 0), 15545)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Closed || !res.Empty || res.RedundantOf != 15545 || res.Reason != SupersessionActionClosedEmptyDiff {
				t.Fatalf("res = %+v, want closed empty redundant_of=15545", res)
			}
			if !api.closed || !slices.Contains(api.labels, SupersededLabel) {
				t.Fatalf("closed=%v labels=%v", api.closed, api.labels)
			}
			if len(api.comments) != 1 || !strings.Contains(api.comments[0], "Already landed via #15545 — closing as superseded; thank you @"+login+".") || strings.Contains(api.comments[0], "hive-authored") {
				t.Fatalf("comment = %q", api.comments)
			}
		})
	}
}

// TestCloseAlreadyLandedAnyAuthorKeepsRealDiff: a contributor PR whose
// compare lists a file that differs on the base tip is not closed; the
// reviewer's question stands as before.
func TestCloseAlreadyLandedAnyAuthorKeepsRealDiff(t *testing.T) {
	api := &alreadyLandedAnyAuthorServer{landedOK: true, compare: map[string]any{
		"files":   []map[string]any{{"filename": "pkg/a.go", "status": "modified", "sha": "head-blob"}},
		"commits": []map[string]any{{"sha": "k1k1k1k1k1"}},
	}}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()
	c := testClient(t, srv.URL)
	res, err := c.CloseAlreadyLandedHivePRIfEligible(context.Background(), "o/r", alreadyLandedContributorPR("karibhou", 1), 15545)
	if err != nil {
		t.Fatal(err)
	}
	if res.Closed || res.Empty || res.Verdict != hiveEmptyDiffVerdictKeep || res.Reason != hiveEmptyDiffReasonHasDiff {
		t.Fatalf("res = %+v, want kept has-diff", res)
	}
	if api.closed || len(api.comments) != 0 {
		t.Fatalf("a real diff must not be touched: closed=%v comments=%v", api.closed, api.comments)
	}
}

// TestCloseAlreadyLandedAnyAuthorNeedsProof: an empty contributor PR whose
// named PR is not merged (and whose head is not on the base) keeps the
// reviewer's question — the empty compare alone could be a branch
// mid-push. With the head on the base the proof is the compare itself.
func TestCloseAlreadyLandedAnyAuthorNeedsProof(t *testing.T) {
	api := &alreadyLandedAnyAuthorServer{landedOK: false, compare: map[string]any{"files": []any{}, "commits": []map[string]any{{"sha": "k1k1k1k1k1"}}}}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()
	c := testClient(t, srv.URL)
	res, err := c.CloseAlreadyLandedHivePRIfEligible(context.Background(), "o/r", alreadyLandedContributorPR("karibhou", 0), 15545)
	if err != nil {
		t.Fatal(err)
	}
	if res.Closed || !res.Empty || res.Verdict != hiveEmptyDiffVerdictHumanNote || res.Reason != hiveEmptyDiffReasonNotHive {
		t.Fatalf("res = %+v, want human/not-hive (unproven landing)", res)
	}

	api.mu.Lock()
	api.compare = map[string]any{"files": []any{}, "commits": []any{}, "merge_base_commit": map[string]any{"sha": "k1k1k1k1k1"}}
	api.mu.Unlock()
	res, err = c.CloseAlreadyLandedHivePRIfEligible(context.Background(), "o/r", alreadyLandedContributorPR("karibhou", 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Closed || res.RedundantOf != 0 || len(api.comments) != 1 || !strings.Contains(api.comments[0], "Already landed on `v5` — closing as superseded; thank you @karibhou.") {
		t.Fatalf("res = %+v comments=%q, want closed on head-on-base proof", res, api.comments)
	}
}

func TestAlreadyLandedIssueNoteNamesWhosePR(t *testing.T) {
	if got := alreadyLandedIssueNote(15544, 15545, "v5", false); strings.Contains(got, "its own") || !strings.Contains(got, "closed #15544 as superseded") {
		t.Fatalf("contributor note = %q", got)
	}
	if got := alreadyLandedIssueNote(15647, 15506, "v5", true); !strings.Contains(got, "its own #15647") {
		t.Fatalf("hive note = %q", got)
	}
}
