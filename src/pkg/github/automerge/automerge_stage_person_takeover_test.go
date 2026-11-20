package automerge

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// The opt-in timer for a person's sent-back PR
// (governor.labels.stage.adoption.person_after, hivecommons/hive#19395).

const personAfterTest = 24 * time.Hour

// newPersonFixture parks acme/widget#7 at 072 for a failed check, written
// by the hive owner "owner" (an owner's agent PR, author/clanker) or, with
// human, by "someone" by hand (author/human), with its activity noted at
// the clock's start. It reuses the abandoned-PR fixture's GitHub.
func newPersonFixture(t *testing.T, human bool) *abandonFixture {
	t.Helper()
	f := &abandonFixture{
		clock: &contributorLaneClock{at: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)},
		api:   &abandonAPI{},
		key:   stageKey("acme", "widget", 7),
	}
	srv := f.api.server(t)
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	f.store = stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), f.clock.now)
	f.e = New(client, Options{Now: f.clock.now, StageStore: f.store, StageTracker: NewStageTracker()})
	f.obs = stageObservation{Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 7, HeadSHA: "aaa", Author: "owner", AuthorKind: authorkind.Clanker, Reason: "check-failing:unit"}
	stage := stagelabels.ReturnedToContributor
	if human {
		f.obs.Author, f.obs.AuthorKind = "someone", authorkind.Human
		stage = stagelabels.NeedsAuthor
	}
	f.store.ObserveHead(f.key, "aaa", stage, "check-failing:unit")
	f.store.NoteActivity(f.key, stagelabels.Activity{Head: "aaa", Comments: 2, Reviews: 0, TrackIdle: true})
	return f
}

// personPolicy turns the timer on for acme/widget for the PR kind human
// names only, with "owner" as this hive's owner.
func personPolicy(human bool) StageLabelPolicy {
	return StageLabelPolicy{
		Enabled: true, Adoption: true, PersonAfter: personAfterTest,
		PersonTakeover: func(repo string, h bool) bool { return repo == "acme/widget" && h == human },
		OwnerAuthor:    func(login string) bool { return login == "owner" },
	}
}

// takeover runs the timer with the record's own stage as the stage the
// pass wrote.
func (f *abandonFixture) takeover(t *testing.T, p StageLabelPolicy) (int, bool, error) {
	t.Helper()
	return f.e.takeOverPersonPRIfDue(context.Background(), f.store, f.key, f.obs, p, f.rec(t).Stage, f.clock.now())
}

// What a person's PR was sent back for decides whether it is work the hive
// may take over: conflicts, failed checks and the swarm's or a review
// bot's change request are; a maintainer's change request, a question for
// the author and anything only a person can do are not.
func TestPersonTakeoverWork(t *testing.T) {
	route := func(reasons ...string) *stagelabels.Route { return &stagelabels.Route{Reasons: reasons} }
	for _, tc := range []struct {
		reason string
		route  *stagelabels.Route
		want   string
	}{
		{"conflicting", nil, personWorkConflict},
		{"label:needs-rebase", nil, personWorkConflict},
		{"check-failing:unit", nil, personWorkCheck},
		{"required-check-failing:build", nil, personWorkCheck},
		{stageReasonOptionalCheckFailing + "lint", nil, personWorkCheck},
		{stagelabels.LaneReasonChangesRequested, nil, personWorkChanges},
		{contributorLaneReasonChangesRequested, nil, personWorkChanges},
		{"changes-requested", nil, personWorkChanges},
		{autoMergeReasonReviewGatePrefix + ReviewGateOutcomeChangesRequested + ":security", nil, personWorkChanges},
		{autoMergeReasonHumanChangesRequested, nil, ""},
		{stageReasonDecisionAnswered, nil, ""},
		{stagelabels.LaneReasonReviewerQuestion, nil, ""},
		{stagelabels.LaneReasonDCOMissing, nil, ""},
		{stagelabels.LaneReasonHiveSignoffAttribution, nil, ""},
		{"", nil, ""},
		// A lane that stopped at a gate: the human route's evidence decides,
		// and every reason on it must be work the hive can do.
		{contributorLaneReasonNoReviewer, route("red checks: unit, lint"), personWorkCheck},
		{contributorLaneReasonNoReviewer, route("merge conflict with the base branch"), personWorkConflict},
		{contributorLaneReasonNoReviewer, route("red checks: unit", "DCO sign-off missing"), ""},
		{contributorLaneReasonNoReviewer, nil, ""},
	} {
		obs := stageObservation{Reason: tc.reason, Route: tc.route}
		if got := personTakeoverWork(obs); got != tc.want {
			t.Errorf("personTakeoverWork(%q, %v) = %q, want %q", tc.reason, tc.route, got, tc.want)
		}
	}
}

// The timer applies to a person's PR only, in a repository listed for its
// kind, with adoption on: owners' agent PRs ask person_repos (human false),
// author/human PRs ask person_human_repos (human true).
func TestPersonTakeoverApplies(t *testing.T) {
	owner := stageObservation{Owner: "acme", Repo: "widget", Author: "owner", AuthorKind: authorkind.Clanker}
	human := stageObservation{Owner: "acme", Repo: "widget", Author: "someone", AuthorKind: authorkind.Human}
	contributor := stageObservation{Owner: "acme", Repo: "widget", Author: "contributor", AuthorKind: authorkind.Clanker}
	if h, ok := personTakeoverApplies(owner, personPolicy(false)); !ok || h {
		t.Fatalf("owner's agent PR in person_repos: human=%v ok=%v", h, ok)
	}
	if _, ok := personTakeoverApplies(owner, personPolicy(true)); ok {
		t.Fatal("owner's agent PR applied under person_human_repos only")
	}
	if h, ok := personTakeoverApplies(human, personPolicy(true)); !ok || !h {
		t.Fatalf("author/human PR in person_human_repos: human=%v ok=%v", h, ok)
	}
	if _, ok := personTakeoverApplies(human, personPolicy(false)); ok {
		t.Fatal("author/human PR applied under person_repos only")
	}
	if _, ok := personTakeoverApplies(contributor, personPolicy(false)); ok {
		t.Fatal("a contributor's PR is not a person's: the ordinary adoption owns it")
	}
	off := personPolicy(false)
	off.PersonAfter = 0
	noFunc := personPolicy(false)
	noFunc.PersonTakeover = nil
	noAdoption := personPolicy(false)
	noAdoption.Adoption = false
	for name, p := range map[string]StageLabelPolicy{"person_after off": off, "no repo check": noFunc, "adoption off": noAdoption} {
		if _, ok := personTakeoverApplies(owner, p); ok {
			t.Errorf("%s: applied", name)
		}
	}
}

// Idle past person_after: one notice, nothing inside the grace, then after
// the grace and a fresh read the Adopt now mark is recorded on the timer's
// behalf, for both kinds of person PR.
func TestPersonTakeoverNoticeGraceThenAdoptNowMark(t *testing.T) {
	for _, human := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner agent PR", true: "author/human PR"}[human], func(t *testing.T) {
			f := newPersonFixture(t, human)
			p := personPolicy(human)
			f.clock.advance(personAfterTest - time.Hour)
			if n, takeover, err := f.takeover(t, p); n != 0 || takeover || err != nil || f.api.reads != 0 || len(f.api.posted) != 0 {
				t.Fatalf("below person_after: n=%d takeover=%v err=%v reads=%d posted=%q", n, takeover, err, f.api.reads, f.api.posted)
			}
			f.clock.advance(2 * time.Hour)
			n, takeover, err := f.takeover(t, p)
			if err != nil || n != 1 || takeover || len(f.api.posted) != 1 {
				t.Fatalf("notice: n=%d takeover=%v err=%v posted=%q", n, takeover, err, f.api.posted)
			}
			notice := f.api.posted[0]
			for _, want := range []string{PersonTakeoverNoticeMarker + " head=aaa -->", "no push, comment or review for 1 day", "a failed check (`check-failing:unit`)", "once 24 hours have passed since this notice", "adds a `hold` label", "keeps your commits and sign-off"} {
				if !strings.Contains(notice, want) {
					t.Fatalf("notice lacks %q:\n%s", want, notice)
				}
			}
			noticedAt := f.clock.now()
			if r := f.rec(t); !r.PersonNoticeAt.Equal(noticedAt) || r.PersonTakeoverPending() {
				t.Fatalf("after the notice: %+v", r)
			}

			// Inside the grace: nothing, and the notice is never repeated.
			f.clock.advance(23 * time.Hour)
			if n, takeover, _ := f.takeover(t, p); n != 0 || takeover || len(f.api.posted) != 1 {
				t.Fatalf("inside the grace: n=%d takeover=%v posted=%d", n, takeover, len(f.api.posted))
			}
			f.clock.advance(2 * time.Hour)
			reads := f.api.reads
			n, takeover, err = f.takeover(t, p)
			if err != nil || n != 0 || !takeover {
				t.Fatalf("after the grace: n=%d takeover=%v err=%v", n, takeover, err)
			}
			if f.api.reads <= reads {
				t.Fatal("no fresh read before the takeover")
			}
			r := f.rec(t)
			if r.AdoptForcedBy != stagelabels.AdoptForcedByPersonTimer || !r.AdoptForcedAt.Equal(f.clock.now()) || !r.PersonTakeoverAt.Equal(f.clock.now()) || !r.PersonTakeoverPending() {
				t.Fatalf("Adopt now mark: %+v", r)
			}
			if !r.PersonNoticeAt.IsZero() {
				t.Fatalf("the spent countdown kept its notice: %+v", r)
			}
			// A deferred adoption is handed over again, with no new notice.
			if n, takeover, _ := f.takeover(t, p); n != 0 || !takeover || len(f.api.posted) != 1 {
				t.Fatalf("pending takeover: n=%d takeover=%v posted=%d", n, takeover, len(f.api.posted))
			}
		})
	}
}

// person_grace sets the wait after the notice and the notice says it; zero
// reads as the 24h default.
func TestPersonTakeoverGraceSetting(t *testing.T) {
	f := newPersonFixture(t, false)
	p := personPolicy(false)
	p.PersonGrace = 6 * time.Hour
	f.clock.advance(personAfterTest + time.Minute)
	if n, _, _ := f.takeover(t, p); n != 1 || !strings.Contains(f.api.posted[0], "once 6 hours have passed since this notice") {
		t.Fatalf("notice n=%d posted=%q", n, f.api.posted)
	}
	f.clock.advance(5 * time.Hour)
	if _, takeover, _ := f.takeover(t, p); takeover {
		t.Fatal("taken over before person_grace")
	}
	f.clock.advance(time.Hour)
	if _, takeover, _ := f.takeover(t, p); !takeover {
		t.Fatal("not taken over at person_grace")
	}
	if got := (StageLabelPolicy{}).personGrace(); got != 24*time.Hour {
		t.Fatalf("zero person_grace = %v, want the 24h default", got)
	}
}

// Activity after the notice cancels the countdown: a push the lane saw, or
// a reply only the fresh read before the takeover sees. The idle clock
// restarts, so the next notice waits a full person_after.
func TestPersonTakeoverActivityDuringGraceCancels(t *testing.T) {
	t.Run("seen by the lane", func(t *testing.T) {
		f := newPersonFixture(t, false)
		p := personPolicy(false)
		f.clock.advance(personAfterTest + time.Minute)
		_, _, _ = f.takeover(t, p)
		f.clock.advance(time.Hour)
		f.store.NoteActivity(f.key, stagelabels.Activity{Head: "bbb", Comments: 2, Reviews: 0, TrackIdle: true})
		f.obs.HeadSHA = "bbb"
		if _, takeover, _ := f.takeover(t, p); takeover {
			t.Fatal("taken over after the author pushed")
		}
		if r := f.rec(t); !r.PersonNoticeAt.IsZero() || r.PersonNoticeResetAt.IsZero() {
			t.Fatalf("countdown not cancelled: %+v", r)
		}
		f.clock.advance(23 * time.Hour)
		if n, takeover, _ := f.takeover(t, p); takeover || n != 0 || len(f.api.posted) != 1 {
			t.Fatalf("23h after the push: n=%d takeover=%v posted=%d (the clock restarted at the push)", n, takeover, len(f.api.posted))
		}
		f.clock.advance(2 * time.Hour)
		if n, takeover, _ := f.takeover(t, p); takeover || n != 1 || len(f.api.posted) != 2 {
			t.Fatalf("person_after after the push: n=%d takeover=%v posted=%d, want a fresh notice", n, takeover, len(f.api.posted))
		}
	})
	t.Run("seen only by the fresh read", func(t *testing.T) {
		f := newPersonFixture(t, true)
		p := personPolicy(true)
		f.clock.advance(personAfterTest + time.Minute)
		_, _, _ = f.takeover(t, p)
		replied := f.clock.now().Add(2 * time.Hour)
		f.api.comments = []map[string]any{
			{"id": 9, "body": "back on it tomorrow", "created_at": ghTime(replied), "user": map[string]any{"login": "someone", "type": "User"}},
		}
		f.clock.advance(25 * time.Hour)
		if n, takeover, err := f.takeover(t, p); n != 0 || takeover || err != nil {
			t.Fatalf("taken over despite the reply: n=%d takeover=%v err=%v", n, takeover, err)
		}
		r := f.rec(t)
		if r.PersonTakeoverPending() || !r.PersonNoticeAt.IsZero() || !r.AbandonActivityAt.Equal(replied) {
			t.Fatalf("after the reply: %+v", r)
		}
	})
}

// Stop labels, a park that is not 071 / 072, a fork head, an operator's
// reroute, a person's change request and work only a person can do keep the
// PR with its author: no notice,
// and a pending countdown is cancelled.
func TestPersonTakeoverStops(t *testing.T) {
	// Each case changes the fixture and returns the stage the pass wrote.
	cases := map[string]func(f *abandonFixture) stagelabels.Stage{
		"hold": func(f *abandonFixture) stagelabels.Stage {
			f.obs.Labels = []string{"hold"}
			return stagelabels.ReturnedToContributor
		},
		"do-not-merge": func(f *abandonFixture) stagelabels.Stage {
			f.obs.Labels = []string{"do-not-merge"}
			return stagelabels.ReturnedToContributor
		},
		"needs-human": func(f *abandonFixture) stagelabels.Stage {
			f.obs.Labels = []string{"needs-human"}
			return stagelabels.ReturnedToContributor
		},
		"keep-open": func(f *abandonFixture) stagelabels.Stage {
			f.obs.Labels = []string{hgithub.SupersessionKeepOpenLabel}
			return stagelabels.ReturnedToContributor
		},
		"maintainer's request": func(f *abandonFixture) stagelabels.Stage {
			f.obs.Detail.HumanChangeRequester = "maintainer"
			return stagelabels.ReturnedToContributor
		},
		"human-changes-reason": func(f *abandonFixture) stagelabels.Stage {
			f.obs.Reason = autoMergeReasonHumanChangesRequested
			return stagelabels.ReturnedToContributor
		},
		"question for author": func(f *abandonFixture) stagelabels.Stage {
			f.obs.Reason = stagelabels.LaneReasonReviewerQuestion
			return stagelabels.ReturnedToContributor
		},
		"fork head": func(f *abandonFixture) stagelabels.Stage {
			f.obs.HeadRepo = "someone/widget"
			return stagelabels.ReturnedToContributor
		},
		"operator reroute": func(f *abandonFixture) stagelabels.Stage {
			f.store.NoteReroute(f.key, "aaa", "other-clanker", "operator", "give it to another clanker", f.clock.now())
			return stagelabels.ReturnedToContributor
		},
		"parked at 085": func(f *abandonFixture) stagelabels.Stage {
			return stagelabels.NeedsDecision
		},
		"operator Adopt now": func(f *abandonFixture) stagelabels.Stage {
			f.store.NoteAdoptionForced(f.key, "operator", f.clock.now())
			return stagelabels.ReturnedToContributor
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPersonFixture(t, false)
			p := personPolicy(false)
			f.clock.advance(personAfterTest + time.Minute)
			written := setup(f)
			for i := 0; i < 3; i++ {
				f.clock.advance(48 * time.Hour)
				if _, takeover, _ := f.e.takeOverPersonPRIfDue(context.Background(), f.store, f.key, f.obs, p, written, f.clock.now()); takeover {
					t.Fatal("taken over")
				}
			}
			if len(f.api.posted) != 0 {
				t.Fatalf("posted %q", f.api.posted)
			}
			if r := f.rec(t); r.PersonTakeoverPending() {
				t.Fatalf("timer mark recorded: %+v", r)
			}
		})
	}

	// A stop label added during the grace cancels the countdown; once it
	// is lifted the hive warns again and waits a full grace.
	f := newPersonFixture(t, false)
	p := personPolicy(false)
	f.clock.advance(personAfterTest + time.Minute)
	_, _, _ = f.takeover(t, p)
	f.clock.advance(time.Hour)
	f.obs.Labels = []string{"hold"}
	_, _, _ = f.takeover(t, p)
	if r := f.rec(t); !r.PersonNoticeAt.IsZero() {
		t.Fatalf("hold did not cancel the countdown: %+v", r)
	}
	f.obs.Labels = nil
	f.clock.advance(time.Hour)
	if n, takeover, _ := f.takeover(t, p); n != 1 || takeover || len(f.api.posted) != 2 {
		t.Fatalf("after the hold: n=%d takeover=%v posted=%d, want a fresh notice", n, takeover, len(f.api.posted))
	}
	f.clock.advance(23 * time.Hour)
	if _, takeover, _ := f.takeover(t, p); takeover {
		t.Fatal("taken over before a full grace after the fresh notice")
	}
}

// A change request from a person (not a bot, not the App) means a person is
// engaged: no notice and no takeover. The review swarm's and review bots'
// change requests are work the hive may pick up, and a person's later
// approval replaces their change request.
func TestPersonTakeoverChangeRequests(t *testing.T) {
	review := func(id int, login, typ, state string) map[string]any {
		return map[string]any{"id": id, "state": state, "submitted_at": ghTime(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)), "user": map[string]any{"login": login, "type": typ}}
	}
	for _, tc := range []struct {
		name    string
		reviews []map[string]any
		notice  bool
	}{
		{"maintainer's change request", []map[string]any{review(1, "maintainer", "User", "CHANGES_REQUESTED")}, false},
		{"review bot's change request", []map[string]any{review(1, "copilot-pull-request-reviewer[bot]", "Bot", "CHANGES_REQUESTED")}, true},
		{"the App's change request", []map[string]any{review(1, testHiveAppBotLogin, "Bot", "CHANGES_REQUESTED")}, true},
		{"change request later approved", []map[string]any{review(1, "maintainer", "User", "CHANGES_REQUESTED"), review(2, "maintainer", "User", "APPROVED")}, true},
		{"a comment-only review", []map[string]any{review(1, "maintainer", "User", "COMMENTED")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPersonFixture(t, false)
			f.obs.Reason = contributorLaneReasonChangesRequested
			f.api.reviews = tc.reviews
			p := personPolicy(false)
			f.clock.advance(personAfterTest + time.Minute)
			n, _, err := f.takeover(t, p)
			if err != nil || (n == 1) != tc.notice || (len(f.api.posted) == 1) != tc.notice {
				t.Fatalf("n=%d err=%v posted=%q, want notice=%v", n, err, f.api.posted, tc.notice)
			}
			if !tc.notice {
				// Not read again every sweep: an hour's back-off.
				reads := f.api.reads
				f.clock.advance(30 * time.Minute)
				_, _, _ = f.takeover(t, p)
				if f.api.reads != reads {
					t.Fatalf("re-read inside the back-off: %d -> %d", reads, f.api.reads)
				}
				f.clock.advance(31 * time.Minute)
				_, _, _ = f.takeover(t, p)
				if f.api.reads == reads {
					t.Fatal("never read again after the back-off")
				}
			}
		})
	}

	// A person's change request that lands during the grace stops the
	// takeover at the final read.
	f := newPersonFixture(t, false)
	f.obs.Reason = contributorLaneReasonChangesRequested
	p := personPolicy(false)
	f.clock.advance(personAfterTest + time.Minute)
	if n, _, _ := f.takeover(t, p); n != 1 {
		t.Fatal("no notice")
	}
	f.api.reviews = []map[string]any{review(1, "maintainer", "User", "CHANGES_REQUESTED")}
	f.clock.advance(25 * time.Hour)
	if _, takeover, _ := f.takeover(t, p); takeover {
		t.Fatal("taken over over a person's change request")
	}
	if r := f.rec(t); r.PersonTakeoverPending() || !r.PersonNoticeAt.IsZero() {
		t.Fatalf("after the change request: %+v", r)
	}
}

// With the timer off, unlisted, or for a PR that is not a person's, nothing
// is read or posted however long the PR idles, and a countdown left from
// before is withdrawn.
func TestPersonTakeoverOff(t *testing.T) {
	off := personPolicy(false)
	off.PersonAfter = 0
	unlisted := personPolicy(false)
	unlisted.PersonTakeover = func(string, bool) bool { return false }
	for name, p := range map[string]StageLabelPolicy{"timer off": off, "unlisted": unlisted, "zero policy": {Enabled: true}} {
		t.Run(name, func(t *testing.T) {
			f := newPersonFixture(t, false)
			f.clock.advance(365 * 24 * time.Hour)
			if n, takeover, err := f.takeover(t, p); n != 0 || takeover || err != nil || f.api.reads != 0 || len(f.api.posted) != 0 {
				t.Fatalf("n=%d takeover=%v err=%v reads=%d posted=%q", n, takeover, err, f.api.reads, f.api.posted)
			}
		})
	}
	t.Run("a contributor's PR", func(t *testing.T) {
		f := newPersonFixture(t, false)
		f.obs.Author = "contributor"
		f.clock.advance(365 * 24 * time.Hour)
		if n, takeover, _ := f.takeover(t, personPolicy(false)); n != 0 || takeover || f.api.reads != 0 {
			t.Fatalf("n=%d takeover=%v reads=%d", n, takeover, f.api.reads)
		}
	})
	t.Run("turned off mid-countdown", func(t *testing.T) {
		f := newPersonFixture(t, false)
		f.clock.advance(personAfterTest + time.Minute)
		_, _, _ = f.takeover(t, personPolicy(false))
		f.clock.advance(48 * time.Hour)
		if _, takeover, _ := f.takeover(t, off); takeover {
			t.Fatal("taken over with the timer off")
		}
		if r := f.rec(t); !r.PersonNoticeAt.IsZero() {
			t.Fatalf("countdown kept with the timer off: %+v", r)
		}
	})
}

// The store saves after the pass: a notice of the hive's own already on the
// PR inside this countdown is taken as the notice, not posted again.
func TestPersonTakeoverNoticeLostFromTheStoreIsNotRepeated(t *testing.T) {
	f := newPersonFixture(t, false)
	p := personPolicy(false)
	f.clock.advance(personAfterTest + time.Hour)
	posted := f.clock.now().Add(-10 * time.Minute)
	f.api.comments = []map[string]any{
		{"id": 5, "body": PersonTakeoverNoticeMarker + " head=aaa -->\nnotice", "created_at": ghTime(posted), "user": map[string]any{"login": testHiveAppBotLogin, "type": "Bot"}},
	}
	if n, _, err := f.takeover(t, p); n != 0 || err != nil || len(f.api.posted) != 0 {
		t.Fatalf("n=%d err=%v posted=%q", n, err, f.api.posted)
	}
	if r := f.rec(t); !r.PersonNoticeAt.Equal(posted) {
		t.Fatalf("PersonNoticeAt = %v, want the found notice's %v", r.PersonNoticeAt, posted)
	}
}

// personHarness is the adoption harness with GET pulls/7/reviews served
// (the timer's change-request read), empty.
func personHarness(t *testing.T, policy StageLabelPolicy) (*adoptHarness, func()) {
	t.Helper()
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, policy)
	inner := h.api.extra
	h.api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews" {
			_, _ = w.Write([]byte(`[]`))
			return true
		}
		return inner(w, r)
	}
	return h, done
}

func personTakeoverNotices(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, PersonTakeoverNoticeMarker) {
			out = append(out, c)
		}
	}
	return out
}

// A full stage pass: an owner's agent PR at 072 for a failed check, idle
// past person_after, gets the notice; after the grace the same pass hands
// it to the Adopt now path, which supersedes it under the timer's reason.
// With the timer off the PR stays refused as person-authored.
func TestApplyStageLabelsPersonTimerTakesOverOwnerPR(t *testing.T) {
	run := func(t *testing.T, timer bool) (*adoptHarness, func()) {
		policy := adoptPolicy()
		// close_first is on by default; the timer's copy must still leave
		// the person's original open until the copy merges.
		policy.AdoptSupersedeCloseFirst = true
		policy.OwnerAuthor = func(login string) bool { return login == "mallory" }
		if timer {
			policy.PersonAfter = personAfterTest
			policy.PersonTakeover = func(repo string, human bool) bool { return repo == "acme/widget" && !human }
		}
		h, done := personHarness(t, policy)
		h.sink.presence = &ContributorPresence{Registered: true, Online: false}
		labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
		h.observe(adoptPR(7, "aaa"), "check-failing:unit")
		h.clock.advance(personAfterTest + time.Hour)
		h.api.reset()
		h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
		if got := personTakeoverNotices(h.api); (len(got) == 1) != timer || len(h.adopt.pulls) != 0 {
			t.Fatalf("notice pass: notices=%q pulls=%v (timer=%v)", got, h.adopt.pulls, timer)
		}
		h.clock.advance(config.DefaultPersonTakeoverGrace + time.Hour)
		h.api.reset()
		h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
		return h, done
	}

	t.Run("timer on", func(t *testing.T) {
		h, done := run(t, true)
		defer done()
		if len(h.adopt.pulls) != 1 {
			t.Fatalf("not taken over: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
		}
		comments := adoptionComments(h.api)
		if len(comments) != 1 {
			t.Fatalf("adoption comments = %v", h.api.comments)
		}
		for _, want := range []string{"by=acme-hive reason=person-timer mode=supersede superseded_by=90 head=aaa", "@mallory", "past this repository's takeover timer"} {
			if !strings.Contains(comments[0], want) {
				t.Fatalf("adoption comment lacks %q:\n%s", want, comments[0])
			}
		}
		if strings.Contains(comments[0], "a maintainer asked") || strings.Contains(comments[0], "by=@") {
			t.Fatalf("the timer's adoption reads as an operator's:\n%s", comments[0])
		}
		if len(personTakeoverNotices(h.api)) != 0 {
			t.Fatalf("notice repeated on the takeover pass: %v", h.api.comments)
		}
		rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
		if rec.AdoptedBy != 90 || rec.AdoptReason != AdoptReasonPersonTimer || rec.PersonTakeoverAt.IsZero() {
			t.Fatalf("record after the takeover: %+v", rec)
		}
		if _, forced := rec.AdoptionForced(); forced {
			t.Fatalf("the timer's mark was not consumed: %+v", rec)
		}
		if len(h.adopt.prEdits) != 0 {
			t.Fatalf("the person's original was closed when its copy opened: %v", h.adopt.prEdits)
		}
	})
	t.Run("timer off", func(t *testing.T) {
		h, done := run(t, false)
		defer done()
		if len(h.adopt.pulls) != 0 || len(personTakeoverNotices(h.api)) != 0 {
			t.Fatalf("timer off: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
		}
		rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
		if !rec.PersonTakeoverAt.IsZero() || rec.PersonTakeoverPending() || !rec.PersonNoticeAt.IsZero() {
			t.Fatalf("timer off: %+v", rec)
		}
	})
}

// "Let hive decide" stays out of it: with the timer on, an adoption/supersede
// request on a person's PR is still refused as person-authored
// (hivecommons/hive#15202); only the timer's own mark takes it over.
func TestPersonTimerLeavesTheDelegateRefusalInPlace(t *testing.T) {
	policy := adoptPolicy()
	policy.OwnerAuthor = func(login string) bool { return login == "mallory" }
	policy.PersonAfter = personAfterTest
	policy.PersonTakeover = func(repo string, human bool) bool { return repo == "acme/widget" }
	h, done := personHarness(t, policy)
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker", delegatedecision.LabelAdoptSupersede), "check-failing:unit")
	if len(h.adopt.pulls) != 0 {
		t.Fatalf("a delegated request adopted a person's PR: pulls=%v", h.adopt.pulls)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptionBlocked("aaa") != stagelabels.AdoptBlockedAuthorPerson || rec.PersonTakeoverPending() {
		t.Fatalf("refusal not recorded, or the timer fired early: %+v", rec)
	}
	out := delegatedecision.ApplyRails(delegatedecision.Decision{Verdict: delegatedecision.VerdictAdoptSupersede, Actions: []delegatedecision.Action{{Kind: delegatedecision.ActionAdoptSupersede}}}, delegatedecision.Rails{PersonAuthored: true})
	refused := false
	for _, r := range out.Refusals {
		refused = refused || r == delegatedecision.RefusalAdoptPersonAuthored
	}
	for _, a := range out.Actions {
		if a.Kind == delegatedecision.ActionAdoptSupersede {
			refused = false
		}
	}
	if !refused {
		t.Fatalf("Rails.PersonAuthored no longer refuses adopt-supersede: %+v", out)
	}
}

// The adoption comment and reasons name the timer, never an operator.
func TestAdoptionPersonTimerWording(t *testing.T) {
	obs := stageObservation{Author: "mallory", HeadSHA: "aaa"}
	body := AdoptionPersonTimerComment(obs, "acme-hive", 90)
	if !strings.HasPrefix(body, AdoptionMarkerPrefix+" by=acme-hive reason=person-timer mode=supersede superseded_by=90 head=aaa -->") {
		t.Fatalf("marker: %s", body)
	}
	if m, ok := ParseAdoptionMarker(body); !ok || m.SupersededBy != 90 || m.Head != "aaa" {
		t.Fatalf("the marker scan cannot read the timer's adoption: %+v %v", m, ok)
	}
	if forcedAdoptReason(stagelabels.AdoptForcedByPersonTimer) != AdoptReasonPersonTimer || forcedAdoptReason("clubanderson") != AdoptReasonOperatorForced {
		t.Fatal("forcedAdoptReason")
	}
	if !adoptReasonForced(AdoptReasonPersonTimer) || !adoptReasonForced(AdoptReasonOperatorForced) || adoptReasonForced(AdoptReasonOperator) {
		t.Fatal("adoptReasonForced")
	}
	if adoptionTrigger(AdoptReasonPersonTimer, "check-failing:unit") != AdoptTriggerPersonTimer {
		t.Fatal("trigger")
	}
	for _, s := range []string{adoptionBodyCause(AdoptReasonPersonTimer, adoptionWait{}), adoptionCloseFirstCause(AdoptReasonPersonTimer)} {
		if !strings.Contains(s, "takeover timer") {
			t.Fatalf("cause %q does not name the timer", s)
		}
	}
}

// A notice the write-back gate refuses costs no reads: the gate is asked
// first, and a refusal backs off an hour instead of reading comments and
// reviews on every sweep.
func TestPersonTakeoverNoticeRefusedByTheGateReadsNothing(t *testing.T) {
	f := newPersonFixture(t, false)
	gate := writeback.New(writeback.Config{}, writeback.Deps{
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
			return writeback.RepoPauseInfo{By: "owner", Reason: "freeze"}, true
		},
	}, "")
	f.e.transport.(*hgithub.Client).SetWritebackGate(gate)
	p := personPolicy(false)
	f.clock.advance(personAfterTest + time.Minute)
	for i := 0; i < 3; i++ {
		if n, takeover, err := f.takeover(t, p); n != 0 || takeover || err != nil {
			t.Fatalf("pass %d: n=%d takeover=%v err=%v", i, n, takeover, err)
		}
		f.clock.advance(10 * time.Minute)
	}
	if f.api.reads != 0 || len(f.api.posted) != 0 {
		t.Fatalf("refused notice: reads=%d posted=%q", f.api.reads, f.api.posted)
	}
	r := f.rec(t)
	if !r.PersonNoticeAt.IsZero() || !r.PersonCheckBackoff(f.clock.now()) {
		t.Fatalf("refused notice must back off without a countdown: %+v", r)
	}
	if suppressed := gate.Recent(); len(suppressed) == 0 || suppressed[0].Kind != writeback.KindSupersessionNotice {
		t.Fatalf("suppressed=%+v", suppressed)
	}
}
