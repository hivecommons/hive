package automerge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15307: a returned-to-contributor PR whose clanker is
// offline must be adopted even when the contributor head conflicts with
// the base. These tests cover the admission (adoptionDue with an attempt
// that opened nothing), the supersede route (clean head → copy; dirty head
// → copy + fixer rebase-resolve episode), the second-failure park (085
// rebase-failed-twice on the copy and on the original) and the retry after
// an earlier attempt deleted the hive branch.

func TestAdoptionRetryAdmissionAfterAttemptOpenedNothing(t *testing.T) {
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	attempted := stagelabels.Record{
		NudgeHead: "aaa", NudgeCount: 1, FirstNudgeAt: at.Add(-time.Hour),
		AdoptedHead: "aaa", AdoptedBy: 0, AdoptedAt: at, AdoptMode: stagelabels.AdoptModeSupersede,
	}
	copyOpen := attempted
	copyOpen.AdoptedBy = 90
	policy := adoptPolicy()
	short := adoptPolicy()
	short.AdoptOfflineGrace = 10 * time.Minute
	online := ContributorPresence{Registered: true, Online: true}
	offline := ContributorPresence{Registered: true}
	seenLater := ContributorPresence{Registered: true, LastSeen: at.Add(20 * time.Minute)}
	cases := []struct {
		name     string
		rec      stagelabels.Record
		policy   StageLabelPolicy
		now      time.Time
		presence ContributorPresence
		has      bool
		due      bool
		reason   string
	}{
		{"online clanker waits", attempted, policy, at.Add(3 * time.Hour), online, true, false, ""},
		{"offline inside grace waits", attempted, policy, at.Add(29 * time.Minute), offline, true, false, ""},
		{"offline past grace re-enters", attempted, policy, at.Add(30 * time.Minute), offline, true, true, AdoptReasonOffline},
		{"offline grace counts from the last heartbeat", attempted, policy, at.Add(49 * time.Minute), seenLater, true, false, ""},
		{"offline heartbeat then grace re-enters", attempted, policy, at.Add(50 * time.Minute), seenLater, true, true, AdoptReasonOffline},
		{"unregistered past grace re-enters", attempted, policy, at.Add(30 * time.Minute), ContributorPresence{}, true, true, AdoptReasonUnregistered},
		{"no presence past grace re-enters", attempted, policy, at.Add(30 * time.Minute), ContributorPresence{}, false, true, AdoptReasonGrace},
		{"configured grace", attempted, short, at.Add(10 * time.Minute), offline, true, true, AdoptReasonOffline},
		{"copy open is live, not retried", copyOpen, policy, at.Add(3 * time.Hour), offline, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			due, reason := adoptionDue(tc.rec, "aaa", tc.policy, tc.now, tc.presence, tc.has)
			if due != tc.due || (due && reason != tc.reason) {
				t.Fatalf("adoptionDue = %v %q, want %v %q", due, reason, tc.due, tc.reason)
			}
		})
	}
}

// offlineAdoptHarness is the adopt harness with the author's clanker
// offline (registered, no heartbeat) and a recording RebaseResolver.
type offlineAdoptHarness struct {
	*adoptHarness
	requests []RebaseRequest
	episode  map[int]RebaseEpisode
}

func newOfflineAdoptHarness(t *testing.T, withResolver bool) (*offlineAdoptHarness, func()) {
	t.Helper()
	fake := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "comment"}, presence: &ContributorPresence{Registered: true}}
	h, done := newAdoptHarnessWith(t, fake, fake, adoptPolicy())
	o := &offlineAdoptHarness{adoptHarness: h, episode: map[int]RebaseEpisode{}}
	if withResolver {
		o.wireResolver()
	}
	return o, done
}

func (o *offlineAdoptHarness) wireResolver() {
	o.e.rebaseResolver = RebaseResolverFunc(func(_ context.Context, req RebaseRequest) error {
		o.requests = append(o.requests, req)
		return nil
	})
	o.e.rebaseEpisode = func(_ string, number int) (RebaseEpisode, bool) {
		ep, ok := o.episode[number]
		return ep, ok
	}
}

var offlineReturnedLabels = []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}

// returnThenWaitOffline nudges once and advances past the offline wait
// (15m after the nudge) without sweeping again.
func (o *offlineAdoptHarness) returnThenWaitOffline(reason string) {
	o.t.Helper()
	o.observe(adoptPR(7, "aaa"), reason)
	o.api.reset()
	o.clock.advance(15 * time.Minute)
}

func (o *offlineAdoptHarness) dirtyPR(labels ...string) *gh.PullRequest {
	return adoptPR(7, "aaa", append(append([]string{}, labels...), "needs-rebase")...)
}

func (o *offlineAdoptHarness) record() stagelabels.Record {
	rec, _ := o.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	return rec
}

func (o *offlineAdoptHarness) supersedeConflictNotes() int {
	n := 0
	for _, c := range o.api.comments {
		if strings.HasPrefix(c, AdoptionSupersedeConflictMarker) {
			n++
		}
	}
	return n
}

func TestAdoptionOfflineClankerSupersedeRoutes(t *testing.T) {
	cases := []struct {
		name         string
		online       bool
		dirty        bool
		replayStatus int
		wantPulls    int
		wantReplays  int
		wantFixer    bool
	}{
		{name: "online clanker waits", online: true, dirty: true, replayStatus: 409},
		{name: "offline clean head opens the copy", wantPulls: 1},
		{name: "offline dirty head replays then opens the copy", dirty: true, wantPulls: 1, wantReplays: 1},
		{name: "offline dirty head conflicting replay opens the copy for the fixer", dirty: true, replayStatus: 409, wantPulls: 1, wantReplays: 1, wantFixer: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, done := newOfflineAdoptHarness(t, true)
			defer done()
			if tc.online {
				o.sink.presence = &ContributorPresence{Registered: true, Online: true}
			}
			o.adopt.mergeStatus = tc.replayStatus
			reason, pr := "check-failing:unit", adoptPR(7, "aaa", offlineReturnedLabels...)
			if tc.dirty {
				reason, pr = "conflicting", o.dirtyPR(offlineReturnedLabels...)
			}
			o.returnThenWaitOffline(reason)
			o.observe(pr, reason)
			if len(o.adopt.pulls) != tc.wantPulls || len(o.adopt.merges) != tc.wantReplays {
				t.Fatalf("pulls=%v merges=%v, want %d/%d", o.adopt.pulls, o.adopt.merges, tc.wantPulls, tc.wantReplays)
			}
			rec := o.record()
			if !tc.wantFixer {
				if len(o.requests) != 0 || rec.AdoptCopyConflicting {
					t.Fatalf("fixer episode requested without a conflict: %+v rec=%+v", o.requests, rec)
				}
				return
			}
			if _, kept := o.adopt.refs["refs/heads/hive/adopt-7"]; !kept || slices.Contains(o.adopt.deletedRefs, "refs/heads/hive/adopt-7") {
				t.Fatalf("conflicting hive branch not kept for the fixer: refs=%v deleted=%v", o.adopt.refs, o.adopt.deletedRefs)
			}
			if o.supersedeConflictNotes() != 0 {
				t.Fatalf("give-up note posted although the fixer takes the conflict: %v", o.api.comments)
			}
			if len(o.requests) != 1 {
				t.Fatalf("rebase requests = %+v, want one", o.requests)
			}
			req := o.requests[0]
			if req.RequestedBy != AdoptionRebaseRequester || req.Number != 90 || req.Head != "hive/adopt-7" || req.Base != "v5" || req.Attempt != 1 || req.MaxAttempts != DefaultMergeTrainPolicy.MaxRebaseAttempts || req.Repo != "acme/widget" {
				t.Fatalf("rebase request = %+v", req)
			}
			if !rec.AdoptCopyConflicting || rec.AdoptedBy != 90 || !rec.AdoptionLive("aaa") {
				t.Fatalf("record after adoption = %+v", rec)
			}
			body, _ := o.adopt.pulls[0]["body"].(string)
			if !strings.Contains(body, "fixer is resolving the conflicts") {
				t.Fatalf("copy body does not name the conflict: %q", body)
			}
			// Next sweep: 074 while the fixer works, nothing re-requested.
			o.api.reset()
			o.clock.advance(time.Minute)
			o.observe(o.dirtyPR(offlineReturnedLabels...), "conflicting")
			if added := o.addedLabels(); !slices.Contains(added, "stage/074-adopted-by-hive") {
				t.Fatalf("074 not written while the fixer resolves: %v", o.api.added)
			}
			if len(o.requests) != 1 || len(o.adopt.pulls) != 1 {
				t.Fatalf("re-requested or re-opened: requests=%d pulls=%d", len(o.requests), len(o.adopt.pulls))
			}
		})
	}
}

// The original parks at 085 adoption-stalled:rebase-failed-twice once the
// copy's episode is exhausted and the copy still reads dirty; a copy that
// reads mergeable clears the mark instead.
func TestAdoptionCopyRebaseExhaustedParksOriginal(t *testing.T) {
	o, done := newOfflineAdoptHarness(t, true)
	defer done()
	o.adopt.mergeStatus = 409
	o.returnThenWaitOffline("conflicting")
	o.observe(o.dirtyPR(offlineReturnedLabels...), "conflicting")
	if len(o.requests) != 1 || !o.record().AdoptCopyConflicting {
		t.Fatalf("setup: requests=%d rec=%+v", len(o.requests), o.record())
	}
	labels074 := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}

	// First attempt used, second running: still 074.
	o.episode[90] = RebaseEpisode{Attempt: 2, MaxAttempts: 2, RequestedBy: AdoptionRebaseRequester}
	o.api.reset()
	o.clock.advance(time.Minute)
	o.observe(o.dirtyPR(labels074...), "conflicting")
	if rec := o.record(); rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("stage while the second attempt runs = %s (%q)", rec.Stage, rec.Reason)
	}

	// Both attempts failed: 085 with the reason, one note naming the options.
	o.episode[90] = RebaseEpisode{Attempt: 2, MaxAttempts: 2, Exhausted: true, RequestedBy: AdoptionRebaseRequester}
	o.api.reset()
	o.clock.advance(time.Minute)
	o.observe(o.dirtyPR(labels074...), "conflicting")
	want := stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonRebaseFailedTwice
	rec := o.record()
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != want {
		t.Fatalf("stage after exhaustion = %s (%q), want %s (%q)", rec.Stage, rec.Reason, stagelabels.NeedsDecision, want)
	}
	if !slices.Contains(o.addedLabels(), "stage/085-needs-decision") {
		t.Fatalf("085 not written: %v", o.api.added)
	}
	notes := 0
	for _, c := range o.api.comments {
		if strings.HasPrefix(c, AdoptionStalledMarker) {
			notes++
			if !strings.Contains(c, "#90") || !strings.Contains(c, "fresh branch") || !strings.Contains(c, "close this PR as superseded") {
				t.Fatalf("stall note does not name the copy and the options: %q", c)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("stall notes = %d: %v", notes, o.api.comments)
	}
	o.api.reset()
	o.clock.advance(time.Minute)
	o.observe(o.dirtyPR("stage/085-needs-decision", "role/contributor", "author/clanker"), "conflicting")
	if len(o.api.comments) != 0 {
		t.Fatalf("stall note repeated: %v", o.api.comments)
	}

	// The copy reads mergeable (a later push took): the mark clears and the
	// PR is the hive's again.
	o.adopt.mu.Lock()
	o.adopt.superseders[90] = map[string]any{"number": 90, "state": "open", "merged": false, "mergeable": true, "mergeable_state": "clean"}
	o.adopt.mu.Unlock()
	o.api.reset()
	o.clock.advance(adoptCheckInterval)
	o.observe(o.dirtyPR("stage/085-needs-decision", "role/contributor", "author/clanker"), "conflicting")
	rec = o.record()
	if rec.AdoptCopyConflicting {
		t.Fatalf("mergeable copy left the conflicting mark: %+v", rec)
	}
	o.clock.advance(time.Minute)
	o.observe(o.dirtyPR("stage/085-needs-decision", "role/contributor", "author/clanker"), "conflicting")
	if rec = o.record(); rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("stage after the copy went mergeable = %s (%q)", rec.Stage, rec.Reason)
	}
}

// The hive's own copy (a hive/adopt-* branch) parks at 085
// rebase-failed-twice when its episode is exhausted and it still conflicts.
func TestAdoptionCopyRebaseExhaustedParksCopy(t *testing.T) {
	o, done := newOfflineAdoptHarness(t, true)
	defer done()
	copy := func(labels ...string) *gh.PullRequest {
		pr := adoptPR(90, "ccc", labels...)
		pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin), Type: gh.Ptr("Bot")}
		pr.Head.Ref = gh.Ptr("hive/adopt-7")
		pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("acme/widget")}
		return pr
	}
	o.observeSelf(copy("stage/070-fixing", "needs-rebase"), "conflicting")
	key := stageKey("acme", "widget", 90)
	if rec, _ := o.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.Fixing {
		t.Fatalf("copy with attempts left = %s", rec.Stage)
	}
	o.episode[90] = RebaseEpisode{Attempt: 2, MaxAttempts: 2, Exhausted: true, RequestedBy: AdoptionRebaseRequester}
	o.api.reset()
	o.clock.advance(time.Minute)
	o.observeSelf(copy("stage/070-fixing", "needs-rebase"), "conflicting")
	rec, _ := o.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonRebaseFailedTwice {
		t.Fatalf("copy after exhaustion = %s (%q)", rec.Stage, rec.Reason)
	}
	// A different branch of the hive's is not an adoption copy.
	other := copy("stage/070-fixing", "needs-rebase")
	other.Number = gh.Ptr(91)
	other.Head.Ref = gh.Ptr("hive/fix-91")
	o.episode[91] = RebaseEpisode{Exhausted: true}
	o.observeSelf(other, "conflicting")
	if rec, _ := o.e.stageStoreOrShared().Get(stageKey("acme", "widget", 91)); rec.Stage == stagelabels.NeedsDecision {
		t.Fatalf("non-adoption branch parked at 085: %+v", rec)
	}
}

// Without a resolver the old give-up stands (one note, branch deleted), but
// the offline clanker's PR is re-admitted after the grace, and under a
// journaling boundary the re-admitted push is a new claim: the branch the
// first attempt deleted is recreated instead of the journal answering
// "already applied" and the replay merging into nothing (the 07:15Z
// "hive has nothing it can push" on hivecommons/hive#15307).
func TestAdoptionOfflineRetryRecreatesDeletedBranch(t *testing.T) {
	o, done := newOfflineAdoptHarness(t, false)
	defer done()
	jb := &journalBoundary{applied: map[string]string{}}
	o.e.mutation = jb
	o.adopt.mergeStatus = 409
	o.returnThenWaitOffline("conflicting")
	o.observe(o.dirtyPR(offlineReturnedLabels...), "conflicting")
	if o.supersedeConflictNotes() != 1 || len(o.adopt.pulls) != 0 || !slices.Contains(o.adopt.deletedRefs, "refs/heads/hive/adopt-7") {
		t.Fatalf("give-up path: notes=%d pulls=%d deleted=%v", o.supersedeConflictNotes(), len(o.adopt.pulls), o.adopt.deletedRefs)
	}
	delete(o.adopt.refs, "refs/heads/hive/adopt-7")

	// The fixer is wired now (an upgrade); the clanker is still offline.
	o.wireResolver()
	o.api.reset()
	o.clock.advance(adoptionOfflineGrace(adoptPolicy()))
	o.observe(o.dirtyPR(offlineReturnedLabels...), "conflicting")
	if sha, ok := o.adopt.refs["refs/heads/hive/adopt-7"]; !ok || sha != "aaa" {
		t.Fatalf("branch not recreated on the retry: refs=%v replays=%v", o.adopt.refs, jb.replays)
	}
	if len(o.adopt.pulls) != 1 || len(o.requests) != 1 || !o.record().AdoptCopyConflicting {
		t.Fatalf("retry did not open the copy for the fixer: pulls=%d requests=%d rec=%+v", len(o.adopt.pulls), len(o.requests), o.record())
	}
}
