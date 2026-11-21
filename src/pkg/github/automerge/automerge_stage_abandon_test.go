package automerge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// abandonAPI is the GitHub the idle clock talks to: PR acme/widget#7's
// comments and reviews (served as configured) and the writes the hive makes.
type abandonAPI struct {
	mu       sync.Mutex
	comments []map[string]any
	// commentPages, when set, serves the comments one page per entry with
	// a Link rel="next" header, as GitHub pages them.
	commentPages [][]map[string]any
	reviews      []map[string]any
	reads        int
	posted       []string
	closed       bool
	// editFails makes the close (PATCH) fail with a 403.
	editFails bool
	edits     int
	url       string
	// permission is what GET /collaborators/{user}/permission answers
	// (default "read"); permFails answers it with a 500.
	permission string
	permFails  bool
	permReads  int
}

func (a *abandonAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			a.reads++
			if len(a.commentPages) > 0 {
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page < 1 {
					page = 1
				}
				if page < len(a.commentPages) {
					w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widget/issues/7/comments?page=%d>; rel="next"`, a.url, page+1))
				}
				_ = json.NewEncoder(w).Encode(a.commentPages[page-1])
				return
			}
			_ = json.NewEncoder(w).Encode(a.comments)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/collaborators/") && strings.HasSuffix(r.URL.Path, "/permission"):
			a.permReads++
			if a.permFails {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom"})
				return
			}
			perm := a.permission
			if perm == "" {
				perm = "read"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": perm, "role_name": perm})
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
			a.edits++
			if a.editFails {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
				return
			}
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
	a.url = srv.URL
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
	f.store.NoteActivity(f.key, stagelabels.Activity{Head: "aaa", Comments: 2, Reviews: 0, TrackIdle: true})
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

// close runs the auto-close step with the record's own stage as the stage
// the pass wrote.
func (f *abandonFixture) close(t *testing.T, p StageLabelPolicy) (int, bool, error) {
	t.Helper()
	return f.e.closeAbandonedIfDue(context.Background(), f.store, f.key, f.obs, p, f.rec(t).Stage, f.clock.now())
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
	n, _, err := f.close(t, listedPolicy(f))
	if err != nil || n != 1 || len(f.api.posted) != 1 || !strings.HasPrefix(f.api.posted[0], AbandonNoticeMarker) {
		t.Fatalf("notice: n=%d err=%v posted=%q", n, err, f.api.posted)
	}
	if f.api.closed {
		t.Fatal("closed with the notice")
	}
	// Inside the grace: nothing more, and the notice is never repeated.
	f.clock.advance(time.Hour)
	if n, _, _ := f.close(t, listedPolicy(f)); n != 0 || len(f.api.posted) != 1 || f.api.closed {
		t.Fatalf("inside the grace: n=%d posted=%d closed=%v", n, len(f.api.posted), f.api.closed)
	}
	f.clock.advance(25 * time.Hour)
	n, _, err = f.close(t, listedPolicy(f))
	if err != nil || n != 1 || !f.api.closed {
		t.Fatalf("after the grace: n=%d err=%v closed=%v", n, err, f.api.closed)
	}
	last := f.api.posted[len(f.api.posted)-1]
	if !strings.HasPrefix(last, AbandonCloseMarker) || !strings.Contains(last, "Reopen if that's wrong") {
		t.Fatalf("close comment = %q", last)
	}
	// Never twice.
	f.api.closed = false
	if n, _, _ := f.close(t, listedPolicy(f)); n != 0 || f.api.closed {
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
				if n, _, _ := f.close(t, p(f)); n != 0 {
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
				_, _, _ = f.close(t, listedPolicy(f))
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
		_, _, _ = f.close(t, listedPolicy(f))
		f.clock.advance(time.Hour)
		f.store.NoteActivity(f.key, stagelabels.Activity{Head: "bbb", Comments: 3, Reviews: 0, TrackIdle: true})
		f.obs.HeadSHA = "bbb"
		f.e.reconcileAbandoned(ctx, f.store, f.key, f.obs, f.policy(), true, f.clock.now())
		if f.rec(t).Abandoned() {
			t.Fatal("author push did not clear the flag")
		}
		f.clock.advance(48 * time.Hour)
		_, _, _ = f.close(t, listedPolicy(f))
		if f.api.closed || len(f.api.posted) != 1 {
			t.Fatalf("closed=%v posted=%d", f.api.closed, len(f.api.posted))
		}
	})
	t.Run("seen only by the pre-close read", func(t *testing.T) {
		f := abandonFlagged(t)
		ctx := context.Background()
		_, _, _ = f.close(t, listedPolicy(f))
		replied := f.clock.now().Add(2 * time.Hour)
		f.api.comments = []map[string]any{
			{"id": 9, "body": "still on it", "created_at": ghTime(replied), "user": map[string]any{"login": "contributor", "type": "User"}},
		}
		f.clock.advance(48 * time.Hour)
		if n, _, err := f.close(t, listedPolicy(f)); n != 0 || err != nil || f.api.closed {
			t.Fatalf("closed despite the reply: n=%d err=%v closed=%v", n, err, f.api.closed)
		}
		f.e.reconcileAbandoned(ctx, f.store, f.key, f.obs, f.policy(), true, f.clock.now())
		if f.rec(t).Abandoned() {
			t.Fatal("the reply did not clear the flag")
		}
	})
}

// The clock applies to contributor PRs only: a maintainer's PR (an owner,
// an author the trusted-author or merger authorizers admit, a trusted bot,
// an OWNER / MEMBER / COLLABORATOR association), the hive's own and the
// App's are never flagged or closed. An author trusted only through role/
// trusted is a trusted contributor: a contributor, on abandon_after_trusted.
func TestAbandonContributorScope(t *testing.T) {
	f := newAbandonFixture(t)
	f.e.trustedAuthorizer = func(login, _ string) TrustedAuthorDecision {
		return TrustedAuthorDecision{Allowed: login == "authorized-author"}
	}
	f.e.SetMergerAuthorizer(func(login string) bool { return login == "authorized-merger" })
	f.e.trustedBotAuthors = func() map[string]bool { return map[string]bool{"deps-bot[bot]": true} }
	policy := f.policy()
	policy.AbandonAfterTrusted = 2 * abandonAfterTest
	policy.OwnerAuthor = func(login string) bool { return login == "owner" }
	policy.HiveAuthor = func(login string) bool { return login == "hive-agent" }
	for name, tc := range map[string]struct {
		author string
		assoc  string
		kind   authorkind.Kind
		want   bool
	}{
		"contributor":             {"contributor", "CONTRIBUTOR", authorkind.Human, true},
		"untrusted":               {"newbie", "FIRST_TIME_CONTRIBUTOR", authorkind.Human, true},
		"clanker":                 {"someones-agent", "NONE", authorkind.Clanker, true},
		"owner":                   {"owner", "", authorkind.Human, false},
		"trusted-author lane":     {"authorized-author", "CONTRIBUTOR", authorkind.Human, false},
		"merger authorizer":       {"authorized-merger", "CONTRIBUTOR", authorkind.Human, false},
		"trusted bot":             {"deps-bot[bot]", "NONE", authorkind.Unknown, false},
		"collaborator, untrusted": {"maintainer", "COLLABORATOR", authorkind.Human, false},
		"org member, untrusted":   {"maintainer", "member", authorkind.Human, false},
		"repo owner, untrusted":   {"maintainer", "OWNER", authorkind.Human, false},
		"hive kind":               {"anyone", "", authorkind.Hive, false},
		"hive identity":           {"hive-agent", "", authorkind.Human, false},
		"the App":                 {testHiveAppBotLogin, "", authorkind.Unknown, false},
	} {
		obs := f.obs
		obs.Author, obs.AuthorAssociation, obs.AuthorKind = tc.author, tc.assoc, tc.kind
		if got := f.e.abandonContributor(obs, policy, stagelabels.Record{}); got != tc.want {
			t.Errorf("%s: abandonContributor = %v, want %v", name, got, tc.want)
		}
	}
	// Seen as a maintainer once: never a contributor again, whatever a
	// later pass sees.
	if f.e.abandonContributor(f.obs, policy, stagelabels.Record{AbandonMaintainerSeen: true}) {
		t.Error("a PR once seen as a maintainer's read as a contributor's")
	}
	// Seen as a trusted contributor: still a contributor, on the trusted
	// threshold; a record from before the split reads the same way.
	trusted := stagelabels.Record{AbandonTrustedSeen: true}
	if !f.e.abandonContributor(f.obs, policy, trusted) {
		t.Error("a trusted contributor's PR is not a contributor's")
	}
	for name, tc := range map[string]struct {
		policy      StageLabelPolicy
		rec         stagelabels.Record
		contributor bool
		want        time.Duration
	}{
		"contributor":                 {policy, stagelabels.Record{}, true, abandonAfterTest},
		"trusted contributor":         {policy, trusted, true, 2 * abandonAfterTest},
		"trusted timer off":           {f.policy(), trusted, true, 0},
		"both marks: not the trusted": {policy, stagelabels.Record{AbandonTrustedSeen: true, AbandonMaintainerSeen: true}, true, abandonAfterTest},
		"not a contributor":           {policy, trusted, false, 0},
	} {
		if got := abandonThreshold(tc.policy, tc.rec, tc.contributor); got != tc.want {
			t.Errorf("%s: abandonThreshold = %v, want %v", name, got, tc.want)
		}
	}

	// Not a contributor: a flag on record is cleared and nothing is
	// flagged, however long the PR idles.
	g := abandonFlagged(t)
	g.e.reconcileAbandoned(context.Background(), g.store, g.key, g.obs, g.policy(), false, g.clock.now())
	if g.rec(t).Abandoned() {
		t.Fatal("a maintainer's PR kept the flag")
	}
	g.clock.advance(30 * 24 * time.Hour)
	g.e.reconcileAbandoned(context.Background(), g.store, g.key, g.obs, g.policy(), false, g.clock.now())
	if g.rec(t).Abandoned() {
		t.Fatal("a maintainer's PR was flagged")
	}
	if n, closed, _ := g.close(t, listedPolicy(g)); n != 0 || closed || len(g.api.posted) != 0 {
		t.Fatalf("a maintainer's PR was noticed or closed: n=%d closed=%v", n, closed)
	}
}

// The close happens first and the comment after it: a refused close posts
// nothing and backs off; the retry closes and comments exactly once.
func TestCloseAbandonedEditFailureNeverRepostsTheComment(t *testing.T) {
	f := abandonFlagged(t)
	if n, _, _ := f.close(t, listedPolicy(f)); n != 1 {
		t.Fatal("no notice")
	}
	f.clock.advance(25 * time.Hour)
	f.api.editFails = true
	n, closed, err := f.close(t, listedPolicy(f))
	if err == nil || closed || n != 0 || len(f.api.posted) != 1 {
		t.Fatalf("refused close: n=%d closed=%v err=%v posted=%d", n, closed, err, len(f.api.posted))
	}
	// Inside the back-off: no second attempt.
	f.clock.advance(10 * time.Minute)
	if _, _, _ = f.close(t, listedPolicy(f)); f.api.edits != 1 {
		t.Fatalf("retried inside the back-off: %d edits", f.api.edits)
	}
	f.api.editFails = false
	f.clock.advance(stagelabels.AbandonRecheckEvery)
	n, closed, err = f.close(t, listedPolicy(f))
	if err != nil || !closed || n != 1 || !f.api.closed || len(f.api.posted) != 2 || !strings.HasPrefix(f.api.posted[1], AbandonCloseMarker) {
		t.Fatalf("retry: n=%d closed=%v err=%v posted=%q", n, closed, err, f.api.posted)
	}
	if _, closed, _ := f.close(t, listedPolicy(f)); closed || len(f.api.posted) != 2 {
		t.Fatal("closed or commented twice")
	}
}

// The close needs the stage the pass wrote to still be the flagged park.
func TestCloseAbandonedNeedsTheParkStillWritten(t *testing.T) {
	f := abandonFlagged(t)
	_, _, _ = f.close(t, listedPolicy(f))
	f.clock.advance(25 * time.Hour)
	n, closed, _ := f.e.closeAbandonedIfDue(context.Background(), f.store, f.key, f.obs, listedPolicy(f), stagelabels.ReviewPending, f.clock.now())
	if n != 0 || closed || f.api.closed {
		t.Fatalf("closed though the pass wrote another stage: n=%d closed=%v", n, closed)
	}
}

// An 085↔074 flap through the real activity path (stageActivity's digest
// flips with the reason) neither restarts the idle clock nor posts a second
// notice; the close comes once, after the grace.
func TestAbandonFlapThroughStageActivityPostsOneNotice(t *testing.T) {
	f := newAbandonFixture(t)
	f.obs.Detail.CommentsKnown, f.obs.Detail.Comments = true, 2
	f.obs.Detail.ReviewsKnown, f.obs.Detail.Reviews = true, 0
	stalled := stagelabels.LaneReasonAdoptionStalledPrefix + "check-failing:unit"
	pass := func(stage stagelabels.Stage, reason string) {
		obs := f.obs
		obs.Reason = reason
		f.store.ObserveHead(f.key, "aaa", stage, reason)
		act := stageActivity(obs, stage)
		act.TrackIdle = true
		f.store.NoteActivity(f.key, act)
		f.e.reconcileAbandoned(context.Background(), f.store, f.key, obs, f.policy(), true, f.clock.now())
		_, _, _ = f.close(t, listedPolicy(f))
	}
	f.clock.advance(abandonAfterTest + time.Hour)
	pass(stagelabels.NeedsDecision, stalled)
	if !f.rec(t).Abandoned() || len(f.api.posted) != 1 {
		t.Fatalf("fixture: flagged=%v posted=%d", f.rec(t).Abandoned(), len(f.api.posted))
	}
	idle := f.rec(t).AbandonedIdleSince
	for i := 0; i < 3; i++ {
		f.clock.advance(3 * time.Hour)
		pass(stagelabels.AdoptedByHive, "check-failing:unit")
		f.clock.advance(3 * time.Hour)
		pass(stagelabels.NeedsDecision, stalled)
	}
	r := f.rec(t)
	if len(f.api.posted) != 1 || !r.Abandoned() || !r.AbandonedIdleSince.Equal(idle) || !r.IdleSince().Equal(idle) {
		t.Fatalf("after flaps: posted=%d flagged=%v idle since %v (want %v)", len(f.api.posted), r.Abandoned(), r.IdleSince(), idle)
	}
	f.clock.advance(7 * time.Hour)
	pass(stagelabels.NeedsDecision, stalled)
	if !f.api.closed || len(f.api.posted) != 2 {
		t.Fatalf("after the grace: closed=%v posted=%d", f.api.closed, len(f.api.posted))
	}
}

// The confirming read pages through comments, so a crowd of hive and bot
// comments cannot hide the author's reply; a person who is also
// project.ai_author is not excluded.
func TestConfirmingReadPagesComments(t *testing.T) {
	f := newAbandonFixture(t)
	f.clock.advance(abandonAfterTest + time.Hour)
	recent := f.clock.now().Add(-time.Hour)
	var bots []map[string]any
	for i := 0; i < 100; i++ {
		bots = append(bots, map[string]any{"id": i, "body": "status", "created_at": ghTime(recent), "user": map[string]any{"login": testHiveAppBotLogin, "type": "Bot"}})
	}
	f.api.commentPages = [][]map[string]any{bots, {
		{"id": 500, "body": "on it", "created_at": ghTime(recent), "user": map[string]any{"login": "operator-account", "type": "User"}},
	}}
	policy := f.policy()
	policy.HiveAuthor = func(login string) bool { return login == "operator-account" }
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, policy, true, f.clock.now())
	if f.rec(t).Abandoned() {
		t.Fatal("the reply on page 2 was missed")
	}
	if !f.rec(t).IdleSince().Equal(recent) {
		t.Fatalf("idle since = %v, want the reply %v", f.rec(t).IdleSince(), recent)
	}
}

// A hold or keep-open label put on a PR inside the grace cancels the
// countdown: when it is lifted, the hive warns again and waits a full grace
// before closing.
func TestCloseAbandonedHoldCancelsTheCountdown(t *testing.T) {
	for _, label := range []string{"hold", hgithub.SupersessionKeepOpenLabel} {
		t.Run(label, func(t *testing.T) {
			f := abandonFlagged(t)
			if n, _, _ := f.close(t, listedPolicy(f)); n != 1 {
				t.Fatal("no notice")
			}
			f.clock.advance(20 * time.Hour)
			f.obs.Labels = []string{label}
			_, _, _ = f.close(t, listedPolicy(f))
			if r := f.rec(t); !r.AbandonNoticeAt.IsZero() || !r.Abandoned() {
				t.Fatalf("held: notice=%v flagged=%v; want the countdown cancelled, the flag kept", r.AbandonNoticeAt, r.Abandoned())
			}
			// The label comes off past the original grace: a fresh notice,
			// no close.
			f.clock.advance(10 * time.Hour)
			f.obs.Labels = nil
			n, closed, _ := f.close(t, listedPolicy(f))
			if n != 1 || closed || f.api.closed || len(f.api.posted) != 2 || !strings.HasPrefix(f.api.posted[1], AbandonNoticeMarker) {
				t.Fatalf("after the hold: n=%d closed=%v posted=%d", n, closed, len(f.api.posted))
			}
			f.clock.advance(23 * time.Hour)
			if _, closed, _ := f.close(t, listedPolicy(f)); closed {
				t.Fatal("closed inside the new grace")
			}
			f.clock.advance(2 * time.Hour)
			if _, closed, _ := f.close(t, listedPolicy(f)); !closed {
				t.Fatal("not closed after the new grace")
			}
		})
	}
}

// A notice the store forgot (a restart between the post and the store's
// save) is not posted again: the hive's own notice inside this countdown
// restarts the grace from when it was posted. A stranger's copy of the
// marker does not count.
func TestCloseAbandonedNoticeLostFromTheStoreIsNotRepeated(t *testing.T) {
	notice := func(login string, at time.Time) map[string]any {
		return map[string]any{"id": 5, "body": AbandonNoticeMarker + " head=aaa -->\nThis pull request has had no push", "created_at": ghTime(at), "user": map[string]any{"login": login, "type": "Bot"}}
	}
	t.Run("own", func(t *testing.T) {
		f := abandonFlagged(t)
		noticedAt := f.clock.now()
		f.api.comments = []map[string]any{notice(testHiveAppBotLogin, noticedAt)}
		f.clock.advance(time.Hour)
		n, closed, err := f.close(t, listedPolicy(f))
		if err != nil || n != 0 || closed || len(f.api.posted) != 0 {
			t.Fatalf("lost notice: n=%d closed=%v err=%v posted=%q; want it adopted, nothing posted", n, closed, err, f.api.posted)
		}
		if got := f.rec(t).AbandonNoticeAt; !got.Equal(noticedAt) {
			t.Fatalf("notice at = %v, want the comment's %v", got, noticedAt)
		}
		f.clock.advance(24 * time.Hour)
		n, closed, err = f.close(t, listedPolicy(f))
		if err != nil || n != 1 || !closed || len(f.api.posted) != 1 || !strings.HasPrefix(f.api.posted[0], AbandonCloseMarker) {
			t.Fatalf("after the grace: n=%d closed=%v err=%v posted=%q", n, closed, err, f.api.posted)
		}
	})
	t.Run("stranger", func(t *testing.T) {
		f := abandonFlagged(t)
		f.api.comments = []map[string]any{notice("contributor", f.clock.now())}
		f.clock.advance(time.Hour)
		n, _, err := f.close(t, listedPolicy(f))
		if err != nil || n != 1 || len(f.api.posted) != 1 || !strings.HasPrefix(f.api.posted[0], AbandonNoticeMarker) {
			t.Fatalf("stranger's marker: n=%d err=%v posted=%q; want the hive's own notice", n, err, f.api.posted)
		}
	})
	// A notice a hold cancelled belongs to the old countdown: once the
	// hold is lifted the hive warns again.
	t.Run("cancelled", func(t *testing.T) {
		f := abandonFlagged(t)
		noticedAt := f.clock.now()
		if n, _, _ := f.close(t, listedPolicy(f)); n != 1 {
			t.Fatal("no notice")
		}
		f.api.comments = []map[string]any{notice(testHiveAppBotLogin, noticedAt)}
		f.clock.advance(time.Hour)
		f.obs.Labels = []string{"hold"}
		_, _, _ = f.close(t, listedPolicy(f))
		f.clock.advance(time.Hour)
		f.obs.Labels = nil
		n, closed, _ := f.close(t, listedPolicy(f))
		if n != 1 || closed || len(f.api.posted) != 2 || !strings.HasPrefix(f.api.posted[1], AbandonNoticeMarker) {
			t.Fatalf("after the hold: n=%d closed=%v posted=%d; want a fresh notice", n, closed, len(f.api.posted))
		}
	})
}

// Wiring: a full ApplyStageLabels pass in a listed L6 repository never
// flags, warns about or closes an owner's PR, or a trusted contributor's
// while abandon_after_trusted is off, parked at 071 past the threshold,
// while a contributor's PR in the same pass is flagged and warned (so the
// path is live).
func TestApplyStageLabelsAbandonSkipsOwnerAndTrustedPRs(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}, statefulComments: true}
	var closes []string
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/") && strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = w.Write([]byte("[]"))
			return true
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/"):
			closes = append(closes, r.URL.Path)
			_, _ = w.Write([]byte(`{"state":"closed"}`))
			return true
		}
		return false
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.stageLabelPolicy = func() StageLabelPolicy {
		return StageLabelPolicy{
			Enabled: true, StalledRequireQuiet: true, AbandonAfter: abandonAfterTest,
			AbandonClose: func(string) bool { return true },
			OwnerAuthor:  func(login string) bool { return login == "owner-login" },
		}
	}
	type fixture struct {
		number int
		login  string
		role   stagelabels.Role
	}
	prs := []fixture{
		{41, "maintainer-login", stagelabels.RoleTrusted},
		{42, "owner-login", stagelabels.RoleContributor},
		{43, "contributor-login", stagelabels.RoleContributor},
	}
	sweep := func(trustedRole stagelabels.Role) {
		t.Helper()
		for _, p := range prs {
			pr := stagePR(p.number, "author/human")
			pr.Head.SHA = gh.Ptr("aaa")
			pr.User = &gh.User{Login: gh.Ptr(p.login)}
			e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "conflicting", false, nil)
			role := p.role
			if p.number == 41 {
				role = trustedRole
			}
			e.noteStageRole("acme", "widget", p.number, role)
		}
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	sweep(stagelabels.RoleTrusted)
	for i := 0; i < 3; i++ {
		clock.advance(abandonAfterTest/2 + time.Hour)
		// The last passes resolve the trusted author's role as untrusted
		// (no lane reported a tier): the record remembers.
		sweep(stagelabels.RoleUntrusted)
	}
	for _, n := range []int{41, 42} {
		rec, _ := store.Get(stageKey("acme", "widget", n))
		if rec.Stage != stagelabels.NeedsAuthor {
			t.Fatalf("#%d stage = %s, want 071 (fixture)", n, rec.Stage)
		}
		if !rec.AbandonedAt.IsZero() || !rec.AbandonNoticeAt.IsZero() {
			t.Fatalf("#%d was flagged or warned: %+v", n, rec)
		}
		for _, c := range api.posted[n] {
			if b, _ := c["body"].(string); strings.HasPrefix(b, AbandonNoticeMarker) || strings.HasPrefix(b, AbandonCloseMarker) {
				t.Fatalf("#%d got an abandon comment: %q", n, b)
			}
		}
	}
	for _, path := range closes {
		if !strings.HasSuffix(path, "/pulls/43") {
			t.Fatalf("closed %s", path)
		}
	}
	control, _ := store.Get(stageKey("acme", "widget", 43))
	if control.AbandonedAt.IsZero() || control.AbandonNoticeAt.IsZero() {
		t.Fatalf("the contributor's PR was not flagged and warned, so the path did not run: %+v", control)
	}
}

// abandon_after_trusted: a trusted contributor's PR (role/trusted, no
// maintainer signal) is flagged on its own, longer clock, even on passes
// whose role fell back, then warned and closed like anyone's; an owner's,
// a MEMBER's and a COLLABORATOR's PR are never flagged with the setting
// on, nor is a repository maintainer's PR rebuilt from the forge-state
// cache without its association (the permission read catches it); and a
// trusted contributor seen as a maintainer (a MEMBER association) stops the
// countdown for good.
func TestApplyStageLabelsAbandonTrustedContributorTimer(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}, statefulComments: true}
	var closes []string
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/") && strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = w.Write([]byte("[]"))
			return true
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/"):
			closes = append(closes, r.URL.Path)
			_, _ = w.Write([]byte(`{"state":"closed"}`))
			return true
		}
		return false
	}
	permReads := map[string]int{}
	inner := api.extra
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/collaborators/") {
			login := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/collaborators/"), "/permission")
			permReads[login]++
			perm := "read"
			if login == "repo-maintainer" {
				perm = "write"
			}
			_, _ = fmt.Fprintf(w, `{"permission":%q,"role_name":%q}`, perm, perm)
			return true
		}
		return inner(w, r)
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	const trustedAfter = 2 * abandonAfterTest
	e.stageLabelPolicy = func() StageLabelPolicy {
		return StageLabelPolicy{
			Enabled: true, StalledRequireQuiet: true, AbandonAfter: abandonAfterTest, AbandonAfterTrusted: trustedAfter,
			AbandonClose: func(string) bool { return true },
			OwnerAuthor:  func(login string) bool { return login == "owner-login" },
		}
	}
	prs := []struct {
		number       int
		login, assoc string
	}{
		{41, "trusted-contributor", "CONTRIBUTOR"},
		{42, "owner-login", "CONTRIBUTOR"},
		{43, "member-login", "MEMBER"},
		{44, "collaborator-login", "COLLABORATOR"},
		{45, "promoted-contributor", "CONTRIBUTOR"},
		{46, "plain-contributor", "CONTRIBUTOR"},
		// The forge-state cache's shape: no author association. The author
		// can push to the repository (write) but is not in
		// dashboard.authorized_users.
		{47, "repo-maintainer", ""},
	}
	promoted := false
	sweep := func(trustedRole stagelabels.Role) {
		t.Helper()
		for _, p := range prs {
			pr := stagePR(p.number, "author/human")
			pr.Head.SHA = gh.Ptr("aaa")
			pr.User = &gh.User{Login: gh.Ptr(p.login)}
			if p.assoc != "" {
				pr.AuthorAssociation = gh.Ptr(p.assoc)
			}
			if p.number == 45 && promoted {
				pr.AuthorAssociation = gh.Ptr("MEMBER")
			}
			e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "conflicting", false, nil)
			role := trustedRole
			if p.number == 46 {
				role = stagelabels.RoleContributor
			}
			e.noteStageRole("acme", "widget", p.number, role)
		}
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	rec := func(n int) stagelabels.Record {
		r, _ := store.Get(stageKey("acme", "widget", n))
		return r
	}

	sweep(stagelabels.RoleTrusted)
	for _, n := range []int{41, 45, 47} {
		if r := rec(n); !r.AbandonTrustedSeen || r.AbandonMaintainerSeen {
			t.Fatalf("#%d: want the trusted contributor mark only: %+v", n, r)
		}
	}
	for _, n := range []int{42, 43, 44} {
		if !rec(n).AbandonMaintainerSeen {
			t.Fatalf("#%d: maintainer not marked", n)
		}
	}

	// Past abandon_after, with the role fallen back: only the plain
	// contributor's PR is flagged.
	clock.advance(abandonAfterTest + time.Hour)
	sweep(stagelabels.RoleUntrusted)
	if r := rec(46); r.AbandonedAt.IsZero() || r.AbandonNoticeAt.IsZero() {
		t.Fatalf("the contributor's PR was not flagged and warned: %+v", r)
	}
	for _, n := range []int{41, 45, 47} {
		if !rec(n).AbandonedAt.IsZero() {
			t.Fatalf("#%d flagged on abandon_after instead of abandon_after_trusted", n)
		}
	}
	if len(permReads) != 0 {
		t.Fatalf("permission read before the trusted threshold: %v", permReads)
	}

	// Past abandon_after_trusted: both trusted contributors' PRs are
	// flagged and warned.
	clock.advance(trustedAfter - abandonAfterTest)
	sweep(stagelabels.RoleUntrusted)
	for _, n := range []int{41, 45} {
		if r := rec(n); r.AbandonedAt.IsZero() || r.AbandonNoticeAt.IsZero() {
			t.Fatalf("#%d not flagged and warned past abandon_after_trusted: %+v", n, r)
		}
	}
	// The cached-shape PR resolves role/trusted, but the permission read
	// finds write access: never flagged, marked for good.
	if r := rec(47); !r.AbandonedAt.IsZero() || !r.AbandonNoticeAt.IsZero() || !r.AbandonMaintainerSeen {
		t.Fatalf("#47, a repository maintainer's PR, on the trusted timer: %+v", r)
	}

	// #45's author is now seen as a maintainer: the flag and the countdown
	// go, while #41 is closed after the grace.
	promoted = true
	clock.advance(25 * time.Hour)
	sweep(stagelabels.RoleUntrusted)
	if r := rec(45); !r.AbandonMaintainerSeen || !r.AbandonedAt.IsZero() || !r.AbandonNoticeAt.IsZero() {
		t.Fatalf("#45 kept its countdown after its author was seen as a maintainer: %+v", r)
	}
	closed := map[string]bool{}
	for _, path := range closes {
		closed[path] = true
	}
	if !closed["/repos/acme/widget/pulls/41"] || !closed["/repos/acme/widget/pulls/46"] || len(closed) != 2 {
		t.Fatalf("closed %v, want #41 and #46 only", closes)
	}
	for _, n := range []int{42, 43, 44, 47} {
		if r := rec(n); !r.AbandonedAt.IsZero() || !r.AbandonNoticeAt.IsZero() {
			t.Fatalf("#%d was flagged or warned: %+v", n, r)
		}
		for _, c := range api.posted[n] {
			if b, _ := c["body"].(string); strings.HasPrefix(b, AbandonNoticeMarker) || strings.HasPrefix(b, AbandonCloseMarker) {
				t.Fatalf("#%d got an abandon comment: %q", n, b)
			}
		}
	}
}

// The trusted contributor timer reads the author's repository permission
// once its threshold is crossed, and only then: an author with write (or
// maintain, admin) is marked and never flagged or warned; a read-only
// author is flagged; a failed read defers.
func TestAbandonTrustedTimerChecksTheRepoPermission(t *testing.T) {
	policy := func(f *abandonFixture) StageLabelPolicy {
		p := f.policy()
		p.AbandonAfterTrusted = 2 * abandonAfterTest
		return p
	}
	trusted := func(t *testing.T) *abandonFixture {
		f := newAbandonFixture(t)
		f.obs.Detail.CommentsKnown, f.obs.Detail.ReviewsKnown = true, true
		f.store.MarkAbandonTrustedSeen(f.key)
		return f
	}

	f := trusted(t)
	f.api.permission = "write"
	f.clock.advance(abandonAfterTest + time.Hour)
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, policy(f), true, f.clock.now())
	if f.rec(t).Abandoned() || f.api.permReads != 0 {
		t.Fatalf("below the trusted threshold: flagged=%v reads=%d", f.rec(t).Abandoned(), f.api.permReads)
	}
	f.clock.advance(abandonAfterTest)
	f.e.reconcileAbandoned(context.Background(), f.store, f.key, f.obs, policy(f), true, f.clock.now())
	if r := f.rec(t); r.Abandoned() || !r.AbandonMaintainerSeen || f.api.permReads != 1 {
		t.Fatalf("a write author on the trusted timer: %+v reads=%d", r, f.api.permReads)
	}
	if f.e.abandonContributor(f.obs, policy(f), f.rec(t)) {
		t.Fatal("still a contributor after the maintainer mark")
	}

	// A failed read defers; the next good read, read-only access, flags.
	g := trusted(t)
	g.api.permFails = true
	g.clock.advance(2*abandonAfterTest + time.Hour)
	g.e.reconcileAbandoned(context.Background(), g.store, g.key, g.obs, policy(g), true, g.clock.now())
	if r := g.rec(t); r.Abandoned() || r.AbandonMaintainerSeen {
		t.Fatalf("flagged or marked on a failed read: %+v", r)
	}
	g.api.permFails = false
	g.e.reconcileAbandoned(context.Background(), g.store, g.key, g.obs, policy(g), true, g.clock.now())
	if !g.rec(t).Abandoned() {
		t.Fatal("a read-only author was not flagged")
	}

	// Gained maintain after the flag (past the read's cache): no notice,
	// the flag goes.
	g.api.permission = "maintain"
	g.clock.advance(time.Hour)
	p := policy(g)
	p.AbandonClose = func(repo string) bool { return repo == "acme/widget" }
	if n, closed, _ := g.e.closeAbandonedIfDue(context.Background(), g.store, g.key, g.obs, p, g.rec(t).Stage, g.clock.now()); n != 0 || closed || len(g.api.posted) != 0 {
		t.Fatalf("a maintainer's PR was warned or closed: n=%d closed=%v posted=%d", n, closed, len(g.api.posted))
	}
	if r := g.rec(t); r.Abandoned() || !r.AbandonMaintainerSeen {
		t.Fatalf("after the notice check: %+v", r)
	}
}

// noteAbandonTrust marks a trusted registry tier even when the pass's role
// missed it, and marks the hive's own PRs as never the clock's.
func TestNoteAbandonTrustRegistryTierAndHive(t *testing.T) {
	f := newAbandonFixture(t)
	f.e.contributorRegistryTier = func(login string) (string, bool) {
		switch login {
		case "registry-trusted":
			return "trusted", true
		case "registry-contributor":
			return "contributor", true
		}
		return "", false
	}
	f.e.contributorLanePolicy = func() ContributorLanePolicy {
		return ContributorLanePolicy{RequireTiers: map[string]bool{"trusted": true}}
	}
	policy := f.policy()
	policy.HiveAuthor = func(login string) bool { return login == "hive-agent" }
	for name, tc := range map[string]struct {
		author                 string
		kind                   authorkind.Kind
		trusted, maintainerish bool
	}{
		"registry trusted":     {"registry-trusted", authorkind.Human, true, false},
		"registry contributor": {"registry-contributor", authorkind.Human, false, false},
		"no profile":           {"stranger", authorkind.Human, false, false},
		"hive kind":            {"anyone", authorkind.Hive, false, true},
		"hive identity":        {"hive-agent", authorkind.Human, false, true},
		"the App":              {testHiveAppBotLogin, authorkind.Unknown, false, true},
	} {
		store := stagelabels.NewStore(filepath.Join(t.TempDir(), "s.json"), f.clock.now)
		store.ObserveHead(f.key, "aaa", stagelabels.NeedsAuthor, "check-failing:unit")
		obs := f.obs
		obs.Author, obs.AuthorKind = tc.author, tc.kind
		f.e.noteAbandonTrust(store, f.key, obs, stagelabels.RoleUntrusted, policy)
		r, _ := store.Get(f.key)
		if r.AbandonTrustedSeen != tc.trusted || r.AbandonMaintainerSeen != tc.maintainerish {
			t.Errorf("%s: trusted=%v maintainer=%v, want %v / %v", name, r.AbandonTrustedSeen, r.AbandonMaintainerSeen, tc.trusted, tc.maintainerish)
		}
	}
}

// A record from before abandon_after_trusted (abandon_trusted_seen set, no
// maintainer mark) for an OWNER or MEMBER author, already idle past the
// trusted threshold when the setting is turned on: the pass marks the
// maintainer before it picks a threshold, so the PR is never flagged.
func TestApplyStageLabelsAbandonLegacyTrustedSeenMaintainer(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}, statefulComments: true}
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/") && strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = w.Write([]byte("[]"))
			return true
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/"):
			t.Errorf("closed %s", r.URL.Path)
			_, _ = w.Write([]byte(`{"state":"closed"}`))
			return true
		}
		return false
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	on := false
	e.stageLabelPolicy = func() StageLabelPolicy {
		p := StageLabelPolicy{Enabled: true, StalledRequireQuiet: true}
		if on {
			p.AbandonAfter, p.AbandonAfterTrusted = abandonAfterTest, 2*abandonAfterTest
			p.AbandonClose = func(string) bool { return true }
		}
		return p
	}
	prs := map[int]string{51: "OWNER", 52: "MEMBER"}
	sweep := func() {
		t.Helper()
		for n, assoc := range prs {
			pr := stagePR(n, "author/human")
			pr.Head.SHA = gh.Ptr("aaa")
			pr.User = &gh.User{Login: gh.Ptr("maintainer-" + assoc)}
			pr.AuthorAssociation = gh.Ptr(assoc)
			e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "conflicting", false, nil)
			e.noteStageRole("acme", "widget", n, stagelabels.RoleTrusted)
		}
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	sweep()
	for n := range prs {
		key := stageKey("acme", "widget", n)
		if r, _ := store.Get(key); r.AbandonTrustedSeen || r.AbandonMaintainerSeen {
			t.Fatalf("#%d marked with the clock off: %+v", n, r)
		}
		store.MarkAbandonTrustedSeen(key) // the pre-split mark
	}
	clock.advance(3 * abandonAfterTest)
	on = true
	sweep()
	for n := range prs {
		r, _ := store.Get(stageKey("acme", "widget", n))
		if !r.AbandonMaintainerSeen || !r.AbandonedAt.IsZero() || !r.AbandonNoticeAt.IsZero() {
			t.Fatalf("#%d, a maintainer's legacy record: %+v", n, r)
		}
	}
}

// The abandon guard counts write, maintain and admin, from `permission` or
// `role_name`; the adoption rule (repoMaintainerLevel) stays admin or
// maintain only.
func TestRepoWriteLevelLeavesTheAdoptionRuleAlone(t *testing.T) {
	for _, tc := range []struct {
		permission, role  string
		write, maintainer bool
	}{
		{"admin", "admin", true, true},
		{"write", "maintain", true, true},
		{"write", "write", true, false},
		{"write", "", true, false},
		{"read", "write", true, false},
		{"read", "triage", false, false},
		{"read", "read", false, false},
		{"none", "", false, false},
		{"", "", false, false},
	} {
		if got := repoWriteLevel(tc.permission, tc.role); got != tc.write {
			t.Errorf("repoWriteLevel(%q, %q) = %v, want %v", tc.permission, tc.role, got, tc.write)
		}
		if got := repoMaintainerLevel(tc.permission, tc.role); got != tc.maintainer {
			t.Errorf("repoMaintainerLevel(%q, %q) = %v, want %v", tc.permission, tc.role, got, tc.maintainer)
		}
	}
}
