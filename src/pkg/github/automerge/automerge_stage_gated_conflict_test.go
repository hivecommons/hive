package automerge

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// gatedConflictTransport is the hive client with its PR detail cache
// replaced by a fixed map, keyed "owner/repo#N@head": the mergeable_state
// the enumeration read for a held PR's head.
type gatedConflictTransport struct {
	*hgithub.Client
	states map[string]string
}

func (g gatedConflictTransport) CachedPRMergeableState(repo string, number int, head string) (string, bool) {
	s, ok := g.states[fmt.Sprintf("%s#%d@%s", repo, number, head)]
	return s, ok
}

// gatedConflictEngine is a stage engine whose heads are green and approved
// (so only the gate and the conflict decide the route), with the held PR's
// cached mergeable_state and the contributor nudge wired.
func gatedConflictEngine(t *testing.T, apiURL string, clock *contributorLaneClock, states map[string]string, sink ContributorNudgeSink) (*Engine, *StageTracker, *stagelabels.Store) {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	tracker := NewStageTracker()
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
	e := New(gatedConflictTransport{Client: client, states: states}, Options{
		Now: clock.now,
		StageLabelPolicy: func() StageLabelPolicy {
			return StageLabelPolicy{Enabled: true, ContributorNudge: true, RenudgeInterval: 72 * time.Hour}
		},
		StageTracker:    tracker,
		StageStore:      store,
		CheckSummary:    greenChecks,
		ContributorSoak: NewContributorSoakTracker(),
		ContributorLanePolicy: func() ContributorLanePolicy {
			return ContributorLanePolicy{Enabled: true, RequireTiers: map[string]bool{"trusted": true}}
		},
		ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
			return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
		},
		ContributorNudge: sink,
	})
	return e, tracker, store
}

// TestGatedConflictRoutesToAuthor: the lane stopped at the gate, so it never
// read mergeability; the enumeration's read of the held PR's head decides.
// A dirty current head goes back to its author like any conflict, with the
// gate riding as Gates. An older head's state, an uncomputed state, or no
// state at all leaves the route as it was: a green, reviewed head at 090,
// held by the gate.
func TestGatedConflictRoutesToAuthor(t *testing.T) {
	const sentinelGate = "a sentinel alert is open on this PR"
	const holdGate = "the `hold` label is set"
	cases := []struct {
		name   string
		kind   authorkind.Kind
		labels []string
		reason string
		states map[string]string
		want   stagelabels.Stage
		gate   string
	}{
		{"sentinel, dirty head", authorkind.Clanker, []string{"sentinel-alert"}, contributorLaneReasonSentinelBlocked,
			map[string]string{"acme/widget#9@sha": "dirty"}, stagelabels.ReturnedToContributor, sentinelGate},
		{"hold label, dirty head", authorkind.Clanker, []string{"hold"}, "label:hold",
			map[string]string{"acme/widget#9@sha": "dirty"}, stagelabels.ReturnedToContributor, holdGate},
		{"person's own PR, dirty head", authorkind.Human, []string{"sentinel-alert"}, contributorLaneReasonSentinelBlocked,
			map[string]string{"acme/widget#9@sha": "dirty"}, stagelabels.NeedsAuthor, sentinelGate},
		{"dirty only for an older head", authorkind.Clanker, []string{"sentinel-alert"}, contributorLaneReasonSentinelBlocked,
			map[string]string{"acme/widget#9@old": "dirty"}, stagelabels.ReadyForHumanMerge, sentinelGate},
		{"mergeability not computed", authorkind.Clanker, []string{"sentinel-alert"}, contributorLaneReasonSentinelBlocked,
			map[string]string{"acme/widget#9@sha": "unknown"}, stagelabels.ReadyForHumanMerge, sentinelGate},
		{"clean head", authorkind.Clanker, []string{"sentinel-alert"}, contributorLaneReasonSentinelBlocked,
			map[string]string{"acme/widget#9@sha": "clean"}, stagelabels.ReadyForHumanMerge, sentinelGate},
		{"nothing cached", authorkind.Clanker, []string{"hold"}, "label:hold",
			nil, stagelabels.ReadyForHumanMerge, holdGate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, closeAPI := seededStageAPI(t)
			defer closeAPI()
			clock := &contributorLaneClock{at: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
			e, tracker, _ := gatedConflictEngine(t, api.url, clock, tc.states, nil)
			obs := stageObservation{
				Lane: stageLaneContributor, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 9,
				HeadSHA: "sha", Labels: tc.labels, Reason: tc.reason, LaneReason: tc.reason, AuthorKind: tc.kind,
			}
			route, ev := e.routeHuman(context.Background(), tracker, stageKey("acme", "widget", 9), obs, stagelabels.ReadyForHumanMerge, clock.now())
			if route.Stage != tc.want {
				t.Fatalf("stage = %v, want %v (route %+v)", route.Stage, tc.want, route)
			}
			if !slices.Contains(route.Gates, tc.gate) {
				t.Fatalf("gates = %v, want the gate %q kept", route.Gates, tc.gate)
			}
			conflict := tc.want != stagelabels.ReadyForHumanMerge
			if ev.Conflict != conflict || ev.HeadDirty != conflict {
				t.Fatalf("evidence conflict=%v dirty=%v, want %v", ev.Conflict, ev.HeadDirty, conflict)
			}
			if conflict && (route.LaneReason != stagelabels.LaneReasonConflicting || !slices.Contains(route.Reasons, "merge conflict with the base branch")) {
				t.Fatalf("route = %+v, want the conflict as the author's instruction", route)
			}
			if !conflict && route.LaneReason == stagelabels.LaneReasonConflicting {
				t.Fatalf("route = %+v: no conflict on this head", route)
			}
		})
	}
}

// TestGatedConflictUsesTheLaneReadFirst: a lane that fetched exactly this
// head (noteStagePR) answers for it; the cached enumeration read is the
// fallback only.
func TestGatedConflictUsesTheLaneReadFirst(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	e, _, _ := gatedConflictEngine(t, api.url, clock, map[string]string{"acme/widget#9@sha": "dirty"}, nil)
	obs := stageObservation{Owner: "acme", Repo: "widget", Number: 9, HeadSHA: "sha", Reason: contributorLaneReasonSentinelBlocked, LaneReason: contributorLaneReasonSentinelBlocked}
	obs.Detail.MergeableState, obs.Detail.MergeableStateHead = "clean", "sha"
	if e.stageHeadDirty(stageKey("acme", "widget", 9), obs) {
		t.Fatal("the lane's own clean read of this head must win over the cache")
	}
	obs.Detail.MergeableState = "unknown"
	if !e.stageHeadDirty(stageKey("acme", "widget", 9), obs) {
		t.Fatal("an uncomputed lane read must fall back to the cached read")
	}
	// An ungated PR's lane reads mergeability itself: the cache is not
	// consulted for it.
	obs.Reason, obs.LaneReason = "contributor-lane:no-claim", "contributor-lane:no-claim"
	if e.stageHeadDirty(stageKey("acme", "widget", 9), obs) {
		t.Fatal("an ungated PR must not read the gated fallback")
	}
}

// TestGatedConflictCarriesAcrossAnUnreadPass: once the stage sent a gated
// head back for its conflict, a pass with no computed state for it (the
// cached detail expired, a webhook dropped it) keeps the conflict instead
// of flapping to 090 and back; a new head, or a computed state, does not.
func TestGatedConflictCarriesAcrossAnUnreadPass(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	states := map[string]string{}
	e, _, store := gatedConflictEngine(t, api.url, clock, states, nil)
	key := stageKey("acme", "widget", 9)
	store.ObserveHead(key, "sha", stagelabels.ReturnedToContributor, stagelabels.LaneReasonConflicting)
	store.NoteActivity(key, stagelabels.Activity{Head: "sha", Comments: -1, Reviews: -1})
	obs := stageObservation{Owner: "acme", Repo: "widget", Number: 9, HeadSHA: "sha", Reason: contributorLaneReasonSentinelBlocked, LaneReason: contributorLaneReasonSentinelBlocked}
	if !e.stageHeadDirty(key, obs) {
		t.Fatal("an unread pass on the same head must keep the recorded conflict")
	}
	states["acme/widget#9@sha"] = "clean"
	if e.stageHeadDirty(key, obs) {
		t.Fatal("a computed clean state must win over the recorded conflict")
	}
	delete(states, "acme/widget#9@sha")
	obs.HeadSHA = "new"
	if e.stageHeadDirty(key, obs) {
		t.Fatal("a new head must not inherit the old head's conflict")
	}
}

// TestStageGatedConflictFollowsTheConflictRule: the gate reason stands
// where "conflicting" would have, by the same rule humanConflict applies:
// the stage reason for any author, the lane's reason too for a person's own
// PR. A decision the classifier chose over the gate keeps outranking the
// conflict on a contributor's PR (hivecommons/hive#14607).
func TestStageGatedConflictFollowsTheConflictRule(t *testing.T) {
	decided := stageObservation{AuthorKind: authorkind.Clanker, Reason: "reviewer-verdict:requires_human", LaneReason: contributorLaneReasonSentinelBlocked}
	cases := []struct {
		name  string
		obs   stageObservation
		dirty bool
		want  bool
	}{
		{"gated and dirty", stageObservation{AuthorKind: authorkind.Clanker, Reason: contributorLaneReasonSentinelBlocked, LaneReason: contributorLaneReasonSentinelBlocked}, true, true},
		{"hold gate and dirty", stageObservation{AuthorKind: authorkind.Clanker, Reason: "label:hold", LaneReason: "label:hold"}, true, true},
		{"gated, clean", stageObservation{AuthorKind: authorkind.Clanker, Reason: contributorLaneReasonSentinelBlocked, LaneReason: contributorLaneReasonSentinelBlocked}, false, false},
		{"not gated", stageObservation{AuthorKind: authorkind.Clanker, Reason: "contributor-lane:no-claim", LaneReason: "contributor-lane:no-claim"}, true, false},
		{"decision over the gate, contributor", decided, true, false},
		{"decision over the gate, person's own PR", stageObservation{AuthorKind: authorkind.Human, Reason: decided.Reason, LaneReason: decided.LaneReason}, true, true},
	}
	for _, tc := range cases {
		if got := stageGatedConflict(tc.obs, tc.dirty); got != tc.want {
			t.Errorf("%s: stageGatedConflict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGatedConflictNudgesTheContributor end to end: a sentinel-gated
// contributor PR whose head GitHub reports dirty is written to 072, the
// contributor is nudged for the conflict (the fix queue is the nudge
// sink), and no 090 "ready for a human merge" comment goes out.
func TestGatedConflictNudgesTheContributor(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	api.statefulComments = true
	clock := &contributorLaneClock{at: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "spawn", Online: true}}
	e, _, store := gatedConflictEngine(t, api.url, clock, map[string]string{"acme/widget#7@aaa": "dirty"}, sink)

	pr := nudgePR(7, "aaa", "role/contributor", "author/clanker", "stage/090-ready-for-human-merge", "sentinel-alert")
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonSentinelBlocked, false, nil)
	e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 7))
	if !ok || rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != stagelabels.LaneReasonConflicting {
		t.Fatalf("record = %+v ok=%v, want 072 for the conflict", rec, ok)
	}
	if len(sink.nudges) != 1 || sink.nudges[0].Reason != stagelabels.LaneReasonConflicting {
		t.Fatalf("nudges = %+v, want one conflict nudge", sink.nudges)
	}
	nudges := nudgeComments(api)
	if len(nudges) != 1 || !strings.Contains(nudges[0], "conflicts with the base") {
		t.Fatalf("nudge comments = %q", nudges)
	}
	for _, c := range api.comments {
		if strings.HasPrefix(c, stagelabels.ReadyCommentMarker) {
			t.Fatalf("a dirty head got the ready-for-merge comment: %q", c)
		}
	}
}
