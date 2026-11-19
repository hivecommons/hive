package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// abandonAPI is the GitHub the idle clock talks to: PR acme/widget#7's
// comments and reviews (served as configured) and the writes the hive makes.
type abandonAPI struct {
	mu       sync.Mutex
	comments []map[string]any
	reviews  []map[string]any
	reads    int
	posted   []string
	closed   bool
}

func (a *abandonAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			a.reads++
			_ = json.NewEncoder(w).Encode(a.comments)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			a.reads++
			_ = json.NewEncoder(w).Encode(a.reviews)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.posted = append(a.posted, body["body"])
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(a.posted), "body": body["body"]})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/widget/pulls/7":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.closed = body["state"] == "closed"
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": body["state"]})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type abandonFixture struct {
	clock *contributorLaneClock
	api   *abandonAPI
	e     *Engine
	store *stagelabels.Store
	key   string
	obs   stageObservation
}

const abandonAfterTest = 7 * 24 * time.Hour

// newAbandonFixture parks acme/widget#7 at an adoption-stalled 085 with its
// activity noted at the clock's start.
func newAbandonFixture(t *testing.T) *abandonFixture {
	t.Helper()
	f := &abandonFixture{
		clock: &contributorLaneClock{at: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)},
		api:   &abandonAPI{},
		key:   stageKey("acme", "widget", 7),
	}
	srv := f.api.server(t)
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	f.store = stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), f.clock.now)
	f.e = New(client, Options{Now: f.clock.now, StageStore: f.store, StageTracker: NewStageTracker()})
	f.obs = stageObservation{Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 7, HeadSHA: "aaa", Author: "contributor"}
	f.store.ObserveHead(f.key, "aaa", stagelabels.NeedsDecision, stagelabels.LaneReasonAdoptionStalledPrefix+"check-failing:unit")
	f.store.NoteActivity(f.key, stagelabels.Activity{Head: "aaa", Comments: 2, Reviews: 0})
	return f
}

func (f *abandonFixture) rec(t *testing.T) stagelabels.Record {
	t.Helper()
	r, ok := f.store.Get(f.key)
	if !ok {
		t.Fatal("record missing")
	}
	return r
}

func (f *abandonFixture) policy() StageLabelPolicy {
	return StageLabelPolicy{Enabled: true, AbandonAfter: abandonAfterTest}
}

func ghTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// abandon_after off: nothing is flagged and nothing is read, however long
// the PR idles.
func TestReconcileAbandonedOff(t *testing.T) {
	f := newAbandonFixture(t)
	f.clock.advance(365 * 24 * time.Hour)
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, StageLabelPolicy{Enabled: true}, true, f.clock.now())
	if r := f.rec(t); r.Abandoned() || !r.AbandonedAt.IsZero() {
		t.Fatalf("flag set with the setting off: %+v", r)
	}
	if f.api.reads != 0 {
		t.Fatalf("%d reads with the setting off", f.api.reads)
	}
}

// Past the threshold with counts fetched this cycle: flagged with no read.
// Below it: unchanged.
func TestReconcileAbandonedThreshold(t *testing.T) {
	f := newAbandonFixture(t)
	f.obs.Detail.CommentsKnown, f.obs.Detail.ReviewsKnown = true, true
	f.clock.advance(abandonAfterTest - time.Hour)
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, f.policy(), true, f.clock.now())
	if f.rec(t).Abandoned() {
		t.Fatal("flagged below the threshold")
	}
	f.clock.advance(2 * time.Hour)
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, f.policy(), true, f.clock.now())
	r := f.rec(t)
	if !r.Abandoned() || r.Stage != stagelabels.NeedsDecision || !strings.HasPrefix(r.Reason, stagelabels.LaneReasonAdoptionStalledPrefix) {
		t.Fatalf("want flagged with stage and reason unchanged: %+v", r)
	}
	if got := r.AbandonedReason(f.clock.now()); got != "abandoned 7d: check-failing:unit" {
		t.Fatalf("reason = %q", got)
	}
	if f.api.reads != 0 {
		t.Fatalf("%d reads with the counts known; want none", f.api.reads)
	}

	// A hive PR never keeps the flag.
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, f.policy(), false, f.clock.now())
	if f.rec(t).Abandoned() {
		t.Fatal("a hive-authored PR kept the flag")
	}
}

// Crossing the threshold on a cycle without counts: one confirming read
// (comments + reviews). The hive's own comment and a bot's do not count;
// an author's comment restarts the clock instead of flagging.
func TestReconcileAbandonedConfirmingRead(t *testing.T) {
	f := newAbandonFixture(t)
	f.clock.advance(abandonAfterTest + time.Hour)
	recent := f.clock.now().Add(-time.Hour)
	f.api.comments = []map[string]any{
		{"id": 1, "body": "status", "created_at": ghTime(recent), "user": map[string]any{"login": testHiveAppBotLogin, "type": "Bot"}},
		{"id": 2, "body": "ci", "created_at": ghTime(recent), "user": map[string]any{"login": "ci-helper[bot]", "type": "Bot"}},
	}
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, f.policy(), true, f.clock.now())
	if f.api.reads != 2 {
		t.Fatalf("reads = %d, want 2 (comments + reviews)", f.api.reads)
	}
	if !f.rec(t).Abandoned() {
		t.Fatal("hive and bot comments kept the PR from being flagged")
	}

	// Same picture on a second PR, but the author replied two days ago.
	g := newAbandonFixture(t)
	g.clock.advance(abandonAfterTest + time.Hour)
	replied := g.clock.now().Add(-48 * time.Hour)
	g.api.reviews = []map[string]any{
		{"id": 3, "state": "COMMENTED", "submitted_at": ghTime(replied), "user": map[string]any{"login": "contributor", "type": "User"}},
	}
	g.e.reconcileAbandoned(context.Background(), g.store, g.key, g.obs, g.policy(), true, g.clock.now())
	r := g.rec(t)
	if r.Abandoned() {
		t.Fatal("flagged despite the author's review")
	}
	if !r.IdleSince().Equal(replied) {
		t.Fatalf("idle since = %v, want the reply %v", r.IdleSince(), replied)
	}
	// The next sweep does not read again: the clock restarted.
	g.e.reconcileAbandoned(context.Background(), g.store, g.key, g.obs, g.policy(), true, g.clock.now())
	if g.api.reads != 2 {
		t.Fatalf("reads = %d after a second sweep, want still 2", g.api.reads)
	}
}

// abandonFlagged returns a fixture whose PR is flagged abandoned.
func abandonFlagged(t *testing.T) *abandonFixture {
	t.Helper()
	f := newAbandonFixture(t)
	f.obs.Detail.CommentsKnown, f.obs.Detail.ReviewsKnown = true, true
	f.clock.advance(abandonAfterTest + time.Hour)
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, f.policy(), true, f.clock.now())
	if !f.rec(t).Abandoned() {
		t.Fatal("fixture: not flagged")
	}
	return f
}

func listedPolicy(f *abandonFixture) StageLabelPolicy {
	p := f.policy()
	p.AbandonClose = func(repo string) bool { return repo == "acme/widget" }
	return p
}

// A listed repository at L6 (AbandonClose true): one notice, then after
// the grace with nobody acting, a close with a comment that says how to
// reopen.
func TestCloseAbandonedAfterNoticeAndGrace(t *testing.T) {
	f := abandonFlagged(t)
	ctx := context.Background()
	n, err := f.e.closeAbandonedIfDue(ctx, f.store, f.key, f.obs, listedPolicy(f), f.clock.now())
	if err != nil || n != 1 || len(f.api.posted) != 1 || !strings.HasPrefix(f.api.posted[0], AbandonNoticeMarker) {
		t.Fatalf("notice: n=%d err=%v posted=%q", n, err, f.api.posted)
	}
	if f.api.closed {
		t.Fatal("closed with the notice")
	}
	// Inside the grace: nothing more, and the notice is never repeated.
	f.clock.advance(time.Hour)
	if n, _ := f.e.closeAbandonedIfDue(ctx, f.store, f.key, f.obs, listedPolicy(f), f.clock.now()); n != 0 || len(f.api.posted) != 1 || f.api.closed {
		t.Fatalf("inside the grace: n=%d posted=%d closed=%v", n, len(f.api.posted), f.api.closed)
	}
	f.clock.advance(25 * time.Hour)
	n, err = f.e.closeAbandonedIfDue(ctx, f.store, f.key, f.obs, listedPolicy(f), f.clock.now())
	if err != nil || n != 1 || !f.api.closed {
		t.Fatalf("after the grace: n=%d err=%v closed=%v", n, err, f.api.closed)
	}
	last := f.api.posted[len(f.api.posted)-1]
	if !strings.HasPrefix(last, AbandonCloseMarker) || !strings.Contains(last, "Reopen if that's wrong") {
		t.Fatalf("close comment = %q", last)
	}
	// Never twice.
	f.api.closed = false
	if n, _ := f.e.closeAbandonedIfDue(ctx, f.store, f.key, f.obs, listedPolicy(f), f.clock.now()); n != 0 || f.api.closed {
		t.Fatal("closed twice")
	}
}

// Not listed (or below L6 — config.AbandonCloseAllowed answers both): no
// notice, no close.
func TestCloseAbandonedNeedsTheRepoOptIn(t *testing.T) {
	for name, p := range map[string]func(*abandonFixture) StageLabelPolicy{
		"no opt-in": func(f *abandonFixture) StageLabelPolicy { return f.policy() },
		"unlisted/below": func(f *abandonFixture) StageLabelPolicy {
			q := f.policy()
			q.AbandonClose = func(string) bool { return false }
			return q
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := abandonFlagged(t)
			for i := 0; i < 3; i++ {
				f.clock.advance(48 * time.Hour)
				if n, _ := f.e.closeAbandonedIfDue(context.Background(), f.store, f.key, f.obs, p(f), f.clock.now()); n != 0 {
					t.Fatalf("posted %d", n)
				}
			}
			if len(f.api.posted) != 0 || f.api.closed {
				t.Fatalf("posted=%q closed=%v", f.api.posted, f.api.closed)
			}
		})
	}
}

// A hold or a keep-open label stops the close (and the notice).
func TestCloseAbandonedHonoursHoldAndKeepOpen(t *testing.T) {
	for _, label := range []string{"hold", hgithub.SupersessionKeepOpenLabel} {
		t.Run(label, func(t *testing.T) {
			f := abandonFlagged(t)
			f.obs.Labels = []string{label}
			for i := 0; i < 3; i++ {
				f.clock.advance(48 * time.Hour)
				_, _ = f.e.closeAbandonedIfDue(context.Background(), f.store, f.key, f.obs, listedPolicy(f), f.clock.now())
			}
			if len(f.api.posted) != 0 || f.api.closed {
				t.Fatalf("posted=%q closed=%v", f.api.posted, f.api.closed)
			}
		})
	}
}

// Activity after the notice cancels the close: a reply the lane saw clears
// the flag on the next pass; one only the pre-close read sees keeps the PR
// open too.
func TestCloseAbandonedCancelledByActivityAfterNotice(t *testing.T) {
	t.Run("seen by the lane", func(t *testing.T) {
		f := abandonFlagged(t)
		ctx := context.Background()
		_, _ = f.e.closeAbandonedIfDue(ctx, f.store, f.key, f.obs, listedPolicy(f), f.clock.now())
		f.clock.advance(time.Hour)
		f.store.NoteActivity(f.key, stagelabels.Activity{Head: "bbb", Comments: 3, Reviews: 0})
		f.obs.HeadSHA = "bbb"
		f.e.reconcileAbandoned(ctx, f.store, f.key, f.obs, f.policy(), true, f.clock.now())
		if f.rec(t).Abandoned() {
			t.Fatal("author push did not clear the flag")
		}
		f.clock.advance(48 * time.Hour)
		_, _ = f.e.closeAbandonedIfDue(ctx, f.store, f.key, f.obs, listedPolicy(f), f.clock.now())
		if f.api.closed || len(f.api.posted) != 1 {
			t.Fatalf("closed=%v posted=%d", f.api.closed, len(f.api.posted))
		}
	})
	t.Run("seen only by the pre-close read", func(t *testing.T) {
		f := abandonFlagged(t)
		ctx := context.Background()
		_, _ = f.e.closeAbandonedIfDue(ctx, f.store, f.key, f.obs, listedPolicy(f), f.clock.now())
		replied := f.clock.now().Add(2 * time.Hour)
		f.api.comments = []map[string]any{
			{"id": 9, "body": "still on it", "created_at": ghTime(replied), "user": map[string]any{"login": "contributor", "type": "User"}},
		}
		f.clock.advance(48 * time.Hour)
		if n, err := f.e.closeAbandonedIfDue(ctx, f.store, f.key, f.obs, listedPolicy(f), f.clock.now()); n != 0 || err != nil || f.api.closed {
			t.Fatalf("closed despite the reply: n=%d err=%v closed=%v", n, err, f.api.closed)
		}
		f.e.reconcileAbandoned(ctx, f.store, f.key, f.obs, f.policy(), true, f.clock.now())
		if f.rec(t).Abandoned() {
			t.Fatal("the reply did not clear the flag")
		}
	})
}
