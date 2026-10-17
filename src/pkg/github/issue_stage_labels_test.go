package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// issueStageServer is a fake GitHub that records label writes and comments
// and answers the label-family provisioning list with everything present.
type issueStageServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	added    map[int][]string
	removed  map[int][]string
	comments map[int]int
	lists    int
	// truth is what GET /issues/N reports — the fresh read Enforce takes
	// before writing. Tests seed it from the enumeration payload (seed) or
	// set it to a different label set to simulate another writer.
	truth map[int][]string
	gets  map[int]int
}

func newIssueStageServer(t *testing.T) *issueStageServer {
	t.Helper()
	s := &issueStageServer{added: map[int][]string{}, removed: map[int][]string{}, comments: map[int]int{}, truth: map[int][]string{}, gets: map[int]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path, _ := url.PathUnescape(r.URL.Path)
		const prefix = "/repos/acme/widgets/"
		if !strings.HasPrefix(path, prefix) {
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rest := strings.TrimPrefix(path, prefix)
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && rest == "labels":
			s.lists++
			var out []map[string]string
			for _, d := range stagelabels.AllLabelDefs() {
				out = append(out, map[string]string{"name": d.Name, "color": d.Color})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasPrefix(rest, "issues/") && !strings.Contains(strings.TrimPrefix(rest, "issues/"), "/"):
			var n int
			_, _ = sscanIssue(rest, &n)
			s.gets[n]++
			labels := make([]map[string]string, 0, len(s.truth[n]))
			for _, l := range s.truth[n] {
				labels = append(labels, map[string]string{"name": l})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "labels": labels})
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/labels"):
			var n int
			_, _ = sscanIssue(rest, &n)
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.added[n] = append(s.added[n], body...)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.Contains(rest, "/labels/"):
			var n int
			tail, _ := sscanIssue(rest, &n)
			s.removed[n] = append(s.removed[n], strings.TrimPrefix(tail, "labels/"))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/comments"):
			var n int
			_, _ = sscanIssue(rest, &n)
			s.comments[n]++
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// sscanIssue parses "issues/<n>/<tail>" and returns tail.
func sscanIssue(rest string, n *int) (string, bool) {
	rest = strings.TrimPrefix(rest, "issues/")
	num, tail, _ := strings.Cut(rest, "/")
	v := 0
	for _, r := range num {
		if r < '0' || r > '9' {
			return "", false
		}
		v = v*10 + int(r-'0')
	}
	*n = v
	return tail, true
}

// seed makes the fresh read agree with the enumeration payload.
func (s *issueStageServer) seed(result *ActionableResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range IssueStageInputs(result) {
		s.truth[in.Number] = append([]string(nil), in.Labels...)
	}
}

func (s *issueStageServer) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.added {
		if len(a) > 0 {
			n++
		}
	}
	for _, r := range s.removed {
		n += len(r)
	}
	return n
}

// syncSeeded runs the reconciler against a payload the fresh read agrees with.
func syncSeeded(fake *issueStageServer, c *Client, result *ActionableResult, store *stagelabels.Store, now time.Time) IssueStageResult {
	fake.seed(result)
	return SyncIssueStageLabels(context.Background(), c, result, store, now, testLogger())
}

func issueStageTestClient(t *testing.T, srv *httptest.Server, enabled bool) *Client {
	t.Helper()
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	c.SetIssueStageLabels(func() IssueStagePolicy { return IssueStagePolicy{Enabled: enabled} })
	return c
}

func stageNow() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }

func TestIssueStageFor(t *testing.T) {
	prStage := func(repo string, number int) stagelabels.Stage {
		if repo == "acme/widgets" && number == 99 {
			return stagelabels.MergeQueue
		}
		return stagelabels.None
	}
	cases := []struct {
		name    string
		in      IssueStageInput
		want    stagelabels.Stage
		covered bool
	}{
		{"actionable", IssueStageInput{}, stagelabels.Actionable, false},
		{"live human claim", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: "human"}, stagelabels.Claimed, false},
		{"live contributor claim", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: "contributor"}, stagelabels.Claimed, false},
		{"live agent claim", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent)}, stagelabels.PRInProgress, false},
		{"in-progress sweep", IssueStageInput{Bucket: inProgressWorkBucket}, stagelabels.PRInProgress, false},
		{"covered, PR not yet staged", IssueStageInput{Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 5}}, stagelabels.CoveredByPR, false},
		// hivecommons/hive#12451: the PR's stage is narrated, never mirrored
		// as the issue's label — 050+ are PR-only stages.
		{"covered, PR staged: stays 040", IssueStageInput{Repo: "acme/widgets", Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 99}}, stagelabels.CoveredByPR, true},
		{"covered, cross-repo PR staged: stays 040", IssueStageInput{Repo: "acme/other", Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 99, PRRepo: "acme/widgets"}}, stagelabels.CoveredByPR, true},
		{"likely-done is no stage", IssueStageInput{Labels: []string{LikelyDoneLabel}}, stagelabels.None, false},
		{"filtered out", IssueStageInput{Bucket: "filtered"}, stagelabels.None, false},
		{"parked by needs-direction (#12481)", IssueStageInput{Bucket: "filtered", Reason: "needs-direction", Labels: []string{"help wanted", "needs-direction", stagelabels.PRInProgress.Label()}}, stagelabels.NeedsDecision, false},
		{"parked by needs-decision", IssueStageInput{Bucket: "filtered", Reason: "needs-decision", Labels: []string{"needs-decision"}}, stagelabels.NeedsDecision, false},
		{"parked by renamed label, bucket reason canonical", IssueStageInput{Bucket: "filtered", Reason: "needs-human", Labels: []string{"triage/needs-maintainer"}}, stagelabels.NeedsDecision, false},
		{"parked label still on a claimed issue", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent), Labels: []string{"needs-spec"}}, stagelabels.NeedsDecision, false},
		{"exempt label is not parked", IssueStageInput{Bucket: "filtered", Reason: "Exempt label", Labels: []string{"wontfix"}}, stagelabels.None, false},
		{"reporter triage", IssueStageInput{Bucket: "reporter_triage"}, stagelabels.None, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, covered, reason := IssueStageFor(tc.in, prStage)
			if got != tc.want || covered != tc.covered {
				t.Fatalf("IssueStageFor = %s/%v, want %s/%v", got, covered, tc.want, tc.covered)
			}
			if covered && !strings.Contains(reason, "PR #99 covers it (PR at 100-merge-queue)") {
				t.Fatalf("reason %q must name the covering PR and its stage", reason)
			}
			if got != stagelabels.None && !got.Applies(stagelabels.IssueItem) {
				t.Fatalf("%s is not an issue stage", got)
			}
		})
	}
}

func issueStageResult() *ActionableResult {
	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{
		{Repo: "acme/widgets", Number: 1, Labels: []string{"bug"}},
	})
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 2, Labels: []string{claims.LegacyLabelClaimed}, ClaimKind: "human"}, workBucketClaimed, "live claim")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 3, Labels: []string{claims.LegacyLabelInProgress}, ClaimKind: string(claims.KindAgent)}, workBucketClaimed, "live claim")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 4, Labels: []string{stagelabels.LegacyCoveredByPR}, ClaimContext: &IssueClaimContext{PRNumber: 40}}, workBucketClaimedByPR, "open PR")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 5, Labels: []string{stagelabels.Actionable.Label()}}, "filtered", "Exempt label")
	return result
}

func TestSyncIssueStageLabels_MapsBucketsAndRetiresLegacy(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	store.Observe(stagelabels.Key("acme/widgets", 40), stagelabels.ReviewPending, "contributor-lane:no-verdict")

	res := syncSeeded(fake, c, issueStageResult(), store, stageNow())
	if res.Items != 5 {
		t.Fatalf("items = %d, want 5", res.Items)
	}
	want := map[int]struct{ add, remove []string }{
		1: {add: []string{stagelabels.Actionable.Label()}},
		2: {add: []string{stagelabels.Claimed.Label()}, remove: []string{claims.LegacyLabelClaimed}},
		3: {add: []string{stagelabels.PRInProgress.Label()}, remove: []string{claims.LegacyLabelInProgress}},
		4: {add: []string{stagelabels.CoveredByPR.Label()}, remove: []string{stagelabels.LegacyCoveredByPR}}, // PR staged 050, issue stays 040 (hivecommons/hive#12451)
		5: {remove: []string{stagelabels.Actionable.Label()}},
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for n, w := range want {
		if got := fake.added[n]; strings.Join(got, ",") != strings.Join(w.add, ",") {
			t.Errorf("issue %d added %v, want %v", n, got, w.add)
		}
		if got := fake.removed[n]; strings.Join(got, ",") != strings.Join(w.remove, ",") {
			t.Errorf("issue %d removed %v, want %v", n, got, w.remove)
		}
	}
	if fake.lists != 1 {
		t.Fatalf("label family listed %d times, want once per repo per process", fake.lists)
	}
	if len(fake.comments) != 0 {
		t.Fatalf("unexpected comments: %v", fake.comments)
	}
}

func TestSyncIssueStageLabels_IdempotentAndDisabled(t *testing.T) {
	fake := newIssueStageServer(t)
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 1, Labels: []string{"bug", stagelabels.Actionable.Label()}}})
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 2, Labels: []string{stagelabels.Claimed.Label(), "hive/claimed-by-bob"}, ClaimKind: "human"}, workBucketClaimed, "live claim")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 4, Labels: []string{stagelabels.CoveredByPR.Label()}, ClaimContext: &IssueClaimContext{PRNumber: 40}}, workBucketClaimedByPR, "open PR")

	c := issueStageTestClient(t, fake.srv, true)
	res := syncSeeded(fake, c, result, store, stageNow())
	if res.Writes != 0 || fake.writes() != 0 {
		t.Fatalf("unchanged state produced %d writes (%v / %v)", fake.writes(), fake.added, fake.removed)
	}
	if fake.lists != 0 {
		t.Fatal("no writes planned must not provision the label family")
	}

	off := issueStageTestClient(t, fake.srv, false)
	result.Issues.Items[0].Labels = []string{"bug"}
	if res := SyncIssueStageLabels(context.Background(), off, result, store, stageNow(), testLogger()); res.Items != 0 || fake.writes() != 0 {
		t.Fatalf("disabled policy must be a no-op, got %+v", res)
	}
}

// hivecommons/hive#12451: issue #12451 carried 010 + 040 + 050 (050 mirrored
// from PR #12495 by the pre-fix reconciler). Covered by a staged PR, the
// issue is written exactly 040; the PR's stage lives in the reason, and the
// issue's store record never holds a PR stage (so a PR falling 100 → 060
// does not read as the issue regressing).
func TestSyncIssueStageLabels_CoveredIssueStaysAt040(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	store.Observe(stagelabels.Key("acme/widgets", 40), stagelabels.MergeQueue, "soaking")
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 4, Labels: []string{stagelabels.Actionable.Label(), stagelabels.CoveredByPR.Label(), stagelabels.ReviewPending.Label(), stagelabels.StalledLabel}, ClaimContext: &IssueClaimContext{PRNumber: 40}}, workBucketClaimedByPR, "open PR")

	syncSeeded(fake, c, result, store, stageNow())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := fake.added[4]; len(got) != 0 {
		t.Fatalf("added %v, want nothing: 040 is already there and 100 is a PR stage", got)
	}
	if got := strings.Join(fake.removed[4], ","); got != strings.Join([]string{stagelabels.Actionable.Label(), stagelabels.ReviewPending.Label(), stagelabels.StalledLabel}, ",") {
		t.Fatalf("removed %v: every other owned label must come off (a covered issue carries no health)", fake.removed[4])
	}
	rec, ok := store.Get(stagelabels.Key("acme/widgets", 4))
	if !ok || rec.Stage != stagelabels.CoveredByPR || !strings.Contains(rec.Reason, "PR #40 covers it (PR at 100-merge-queue)") {
		t.Fatalf("issue record %+v: must hold 040 with the PR's stage in the reason", rec)
	}
}

func TestSyncIssueStageLabels_LikelyDoneAndHealth(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	now := stageNow()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	key := stagelabels.Key("acme/widgets", 7)

	// Cycle 1: an agent claim → 030.
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 7, ClaimKind: string(claims.KindAgent)}, workBucketClaimed, "live claim")
	syncSeeded(fake, c, result, store, clock)
	if rec, ok := store.Get(key); !ok || rec.Stage != stagelabels.PRInProgress {
		t.Fatalf("record after cycle 1 = %+v", rec)
	}

	// Cycle 2, two hours on and no PR: the claim lapsed, the issue is
	// actionable again → 010 and one explanatory comment. The backward
	// move is recorded in the store's history, not as a label
	// (stage/regressed is retired, hivecommons/hive#12534).
	clock = now.Add(2 * time.Hour)
	result = &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{stagelabels.PRInProgress.Label()}}})
	syncSeeded(fake, c, result, store, clock)
	fake.mu.Lock()
	if got := fake.added[7]; strings.Join(got, ",") != stagelabels.PRInProgress.Label()+","+stagelabels.Actionable.Label() {
		t.Fatalf("adds = %v", got)
	}
	if fake.comments[7] != 1 {
		t.Fatalf("regression comments = %d, want 1", fake.comments[7])
	}
	fake.mu.Unlock()
	if rec, _ := store.Get(key); len(rec.History) != 2 || !rec.History[1].Regressed || rec.History[1].From != stagelabels.PRInProgress {
		t.Fatalf("history = %+v", rec.History)
	}

	// Cycle 3, a day later still actionable: the actionable dwell (24h) is
	// exceeded → stalled; a stale stage/regressed left by an older hive is
	// stripped by the sweep.
	clock = now.Add(2*time.Hour + 25*time.Hour)
	result = &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{stagelabels.Actionable.Label(), stagelabels.RegressedLabel}}})
	syncSeeded(fake, c, result, store, clock)
	fake.mu.Lock()
	if fake.comments[7] != 1 {
		t.Fatalf("regression comment must not repeat, got %d", fake.comments[7])
	}
	if added := fake.added[7]; added[len(added)-1] != stagelabels.StalledLabel || len(added) != 3 {
		t.Fatalf("stalled expected after the dwell: adds = %v", added)
	}
	if removed := fake.removed[7]; removed[len(removed)-1] != stagelabels.RegressedLabel {
		t.Fatalf("retired stage/regressed must be stripped: removed = %v", removed)
	}
	fake.mu.Unlock()

	// Cycle 4: a PR merged; the issue stays open as likely-done → no stage,
	// every owned label off, record forgotten.
	result = &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{stagelabels.Actionable.Label(), stagelabels.StalledLabel, LikelyDoneLabel}, ClaimContext: &IssueClaimContext{PRNumber: 70, MergedPR: true}}})
	syncSeeded(fake, c, result, store, clock)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.removed[7], ","); got != stagelabels.PRInProgress.Label()+","+stagelabels.RegressedLabel+","+stagelabels.Actionable.Label()+","+stagelabels.StalledLabel {
		t.Fatalf("removed = %v", fake.removed[7])
	}
	if _, ok := store.Get(key); ok {
		t.Fatal("likely-done issue must be forgotten by the store")
	}
}

func TestSweepInProgressIssues_TriggersOnStageLabels(t *testing.T) {
	for _, trigger := range []string{claims.LabelPRInProgress, claims.LabelClaimed, claims.LegacyLabelInProgress} {
		t.Run(trigger, func(t *testing.T) {
			server, removed := sweepServer(t, []string{InProgressCommentBody("scanner", "hive-b", inProgressNow, inProgressNow.Add(time.Hour))})
			c := newTestClient(t, server, "acme", []string{"widgets"})
			enableInProgress(c, "hive-a", 2*time.Hour)
			issue := inProgressIssue()
			issue.Labels = []string{"bug", trigger}
			result := &ActionableResult{Issues: IssueResultFromItems([]Issue{issue})}
			if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, nil); withheld != 1 {
				t.Fatalf("withheld = %d, want 1: %s must trigger the marker read", withheld, trigger)
			}
			if len(*removed) != 0 {
				t.Fatalf("live marker must keep the label, removed %v", *removed)
			}
			// Lapsed marker: the trigger label comes off, whichever spelling.
			server2, removed2 := sweepServer(t, []string{InProgressCommentBody("scanner", "hive-b", inProgressNow.Add(-3*time.Hour), inProgressNow.Add(-time.Hour))})
			c2 := newTestClient(t, server2, "acme", []string{"widgets"})
			enableInProgress(c2, "hive-a", 2*time.Hour)
			issue2 := inProgressIssue()
			issue2.Labels = []string{"bug", trigger}
			result2 := &ActionableResult{Issues: IssueResultFromItems([]Issue{issue2})}
			if withheld := SweepInProgressIssues(context.Background(), c2, result2, inProgressNow, nil); withheld != 0 {
				t.Fatalf("lapsed marker withheld %d", withheld)
			}
			if len(*removed2) != 1 || !strings.HasSuffix((*removed2)[0], url.PathEscape(trigger)) && !strings.HasSuffix((*removed2)[0], trigger) {
				t.Fatalf("removed = %v, want %s", *removed2, trigger)
			}
			if got := result2.Issues.Items[0].Labels; len(got) != 1 || got[0] != "bug" {
				t.Fatalf("labels after sweep = %v", got)
			}
		})
	}
}

// hivecommons/hive#12512: the list payload said stage/010 and the reconciler
// still wants 010, but between the list and the write a claim hook put
// stage/020 on the issue. Planning from the payload alone would see nothing
// to do; Enforce re-reads only when a write is due, so the pass that does
// write (here: the legacy label coming off) finds both stages and leaves
// exactly the wanted one, counting the heal.
func TestSyncIssueStageLabels_HealsDuplicateStageLabels(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)

	// Payload: 010 + 020 both present (the duplicate was visible to the
	// enumeration), ledger says claimed → want 020 only.
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 12, Labels: []string{"bug", stagelabels.Actionable.Label(), stagelabels.Claimed.Label(), "hive/claimed-by-ann"}, ClaimKind: "contributor"}, workBucketClaimed, "live claim")
	fake.seed(result)
	// Truth drifted further: a third stage landed since the list.
	fake.mu.Lock()
	fake.truth[12] = append(fake.truth[12], stagelabels.PRInProgress.Label())
	fake.mu.Unlock()
	res := SyncIssueStageLabels(context.Background(), c, result, store, stageNow(), testLogger())
	fake.mu.Lock()
	if got := strings.Join(fake.removed[12], ","); got != stagelabels.Actionable.Label()+","+stagelabels.PRInProgress.Label() {
		t.Fatalf("removed %v, want 010 and 030 (planned from the fresh read, not the payload)", fake.removed[12])
	}
	if len(fake.added[12]) != 0 {
		t.Fatalf("020 already present, nothing to add; added %v", fake.added[12])
	}
	if fake.gets[12] != 1 {
		t.Fatalf("fresh reads = %d, want exactly one", fake.gets[12])
	}
	fake.mu.Unlock()
	if res.Healed != 1 || res.Writes != 2 {
		t.Fatalf("result = %+v, want Healed=1 Writes=2", res)
	}

	// Payload says 010 only and the issue is actionable: no write is due, so
	// no read either — an unchanged issue stays free.
	result = &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 13, Labels: []string{stagelabels.Actionable.Label()}}})
	fake.seed(result)
	fake.mu.Lock()
	fake.truth[13] = append(fake.truth[13], stagelabels.Claimed.Label())
	fake.mu.Unlock()
	SyncIssueStageLabels(context.Background(), c, result, store, stageNow(), testLogger())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.gets[13] != 0 || len(fake.removed[13]) != 0 {
		t.Fatalf("unchanged payload must not read or write (gets=%d removed=%v); the next cycle's payload carries the duplicate", fake.gets[13], fake.removed[13])
	}
}
