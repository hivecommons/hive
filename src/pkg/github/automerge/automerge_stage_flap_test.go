package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah-packages, 11-05 21:10–21:29Z: once stage labels were
// written on contributor PRs, three fork PRs flipped stage on every pass with
// the head, checks and labels unchanged. Two sweeps feed the one stage
// tracker and each reconciles what its own pass saw:
//
//   - the engine's tick pass (self-authored + trusted + contributor lanes) and
//     the queued sweep (queued + trusted + contributor, no self-authored lane);
//   - when one sweep held the destination-merge lock, the other's contributor
//     lane recorded base-refresh:merge-guard ("not evaluated this pass"),
//     which read as an owned 080 (#869 080↔085, #873 080↔090);
//   - #878's self-authored lane (trusted-bot author) ran only in the tick
//     pass, so its 070 alternated with the contributor lane's 072.

// stageLabelsAfter applies one pass's label writes to labels.
func stageLabelsAfter(labels []string, api *stageAPI) []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	removed := map[string]bool{}
	for _, l := range api.removed {
		removed[strings.ToLower(l)] = true
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if !removed[strings.ToLower(l)] {
			out = append(out, l)
		}
	}
	for _, batch := range api.added {
		for _, l := range batch {
			if !hasLabelFold(out, l) {
				out = append(out, l)
			}
		}
	}
	return out
}

func stageLabelOf(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, "stage/") {
			return l
		}
	}
	return ""
}

func TestMergeGuardIsNoStageOpinion(t *testing.T) {
	if got := basePreparationPrefix + "merge-guard"; got != prstage.StageReasonBaseRefreshMergeGuard {
		t.Fatalf("lanes emit %q, the classifier knows %q", got, prstage.StageReasonBaseRefreshMergeGuard)
	}
	if v := classifyStage(stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: prstage.StageReasonBaseRefreshMergeGuard}); v.Rank != stageRankNone {
		t.Fatalf("merge-guard classified %+v, want no opinion", v)
	}
	// Base preparation proper still owns its stage.
	if v := classifyStage(stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: basePreparationReady}); v.Rank != stageRankOwned {
		t.Fatalf("base-refresh:preparing classified %+v, want an owned stage", v)
	}
}

// #869/#873: a merge-guard pass between two evaluated passes writes nothing.
func TestMergeGuardPassKeepsTheStage(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 5, 21, 10, 0, 0, time.UTC)}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, &fakeNudgeSink{}, StageLabelPolicy{Enabled: true})
	labels := []string{"role/contributor", "author/clanker"}
	pass := func(reason string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", nudgePR(7, "aaa", labels...), "not-app-authored", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", nudgePR(7, "aaa", labels...), reason, false, nil)
		e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		labels = stageLabelsAfter(labels, api)
		clock.at = clock.at.Add(70 * time.Second)
	}
	pass("required-check-failing:Canary")
	settled := stageLabelOf(labels)
	if settled == "" {
		t.Fatalf("first pass wrote no stage: %v", labels)
	}
	for i := 0; i < 6; i++ {
		reason := "required-check-failing:Canary"
		if i%2 == 0 {
			reason = prstage.StageReasonBaseRefreshMergeGuard
		}
		pass(reason)
		if got := stageLabelOf(labels); got != settled || api.writes() != 0 {
			t.Fatalf("pass %d (%s): stage %s → %s, added=%v removed=%v", i+2, reason, settled, got, api.added, api.removed)
		}
	}
}

// #878: the lane that wrote the stage (self-authored, tick pass only) is
// absent from the queued sweep's pass; the contributor lane's different
// stage for the same head waits for it, within the grace. A new head, or the
// owning lane staying away past the grace, lets it through.
func TestAbsentOwningLaneKeepsTheStage(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 5, 21, 10, 0, 0, time.UTC)}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, &fakeNudgeSink{}, StageLabelPolicy{Enabled: true})
	labels := []string{"role/trusted", "author/clanker"}
	head := "95b9d22"
	pass := func(tick bool) {
		t.Helper()
		api.reset()
		pr := nudgePR(7, head, labels...)
		if tick {
			e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "check-failing:Canary", false, nil)
		}
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "required-check-failing:Canary", false, nil)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		labels = stageLabelsAfter(labels, api)
	}
	const owned, contributor = "stage/070-fixing", "stage/072-returned-to-contributor"
	pass(true)
	if got := stageLabelOf(labels); got != owned {
		t.Fatalf("tick pass wrote %s, want %s", got, owned)
	}
	for i := 0; i < 8; i++ {
		clock.at = clock.at.Add(65 * time.Second)
		pass(i%2 == 1)
		if got := stageLabelOf(labels); got != owned {
			t.Fatalf("pass %d (tick=%v): %s → %s with head and checks unchanged", i+1, i%2 == 1, owned, got)
		}
	}
	// The owning lane stays away past the grace: the other lane's stage applies.
	clock.at = clock.at.Add(stageOwnerAbsenceGrace + time.Minute)
	pass(false)
	if got := stageLabelOf(labels); got != contributor {
		t.Fatalf("past the grace: %s, want %s", got, contributor)
	}
	// A new head is new input: no hold.
	labels = []string{"role/trusted", "author/clanker", owned}
	pass(true)
	head = "08b9e69"
	clock.at = clock.at.Add(time.Minute)
	pass(false)
	if got := stageLabelOf(labels); got != contributor {
		t.Fatalf("new head: %s, want %s", got, contributor)
	}
}

func TestStageOwnerHolds(t *testing.T) {
	now := time.Date(2026, 11, 5, 21, 20, 0, 0, time.UTC)
	owner := stageOwner{Lane: stageLaneSelf, Head: "aaa", Stage: stagelabels.Fixing, At: now.Add(-time.Minute)}
	contrib := stageObservation{Lane: stageLaneContributor, HeadSHA: "aaa"}
	only := []stageObservation{contrib}
	want072 := stageVerdict{Stage: stagelabels.ReturnedToContributor, Rank: stageRankOwned}
	for _, tc := range []struct {
		name    string
		owner   stageOwner
		known   bool
		list    []stageObservation
		obs     stageObservation
		verdict stageVerdict
		current stagelabels.Stage
		had     bool
		want    bool
	}{
		{name: "owning lane absent, same head", owner: owner, known: true, list: only, obs: contrib, verdict: want072, current: stagelabels.Fixing, had: true, want: true},
		{name: "no owner recorded", owner: owner, known: false, list: only, obs: contrib, verdict: want072, current: stagelabels.Fixing, had: true},
		{name: "no current stage", owner: owner, known: true, list: only, obs: contrib, verdict: want072, had: false},
		{name: "terminal", owner: owner, known: true, list: only, obs: contrib, verdict: stageVerdict{Terminal: true, Rank: stageRankOwned}, current: stagelabels.Fixing, had: true},
		{name: "new head", owner: owner, known: true, list: only, obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "bbb"}, verdict: want072, current: stagelabels.Fixing, had: true},
		{name: "another writer moved the stage", owner: owner, known: true, list: only, obs: contrib, verdict: want072, current: stagelabels.NeedsDecision, had: true},
		{name: "same stage", owner: owner, known: true, list: only, obs: contrib, verdict: stageVerdict{Stage: stagelabels.Fixing, Rank: stageRankOwned}, current: stagelabels.Fixing, had: true},
		{name: "owning lane reported", owner: owner, known: true, list: []stageObservation{{Lane: stageLaneSelf, HeadSHA: "aaa"}, contrib}, obs: contrib, verdict: want072, current: stagelabels.Fixing, had: true},
		{name: "owner does not outrank", owner: stageOwner{Lane: stageLaneContributor, Head: "aaa", Stage: stagelabels.Fixing, At: owner.At}, known: true, list: []stageObservation{{Lane: stageLaneSelf, HeadSHA: "aaa"}}, obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "aaa"}, verdict: want072, current: stagelabels.Fixing, had: true},
		{name: "lanes without precedence never hold", owner: stageOwner{Lane: "queued", Head: "aaa", Stage: stagelabels.Fixing, At: owner.At}, known: true, list: []stageObservation{{Lane: "other", HeadSHA: "aaa"}}, obs: stageObservation{Lane: "other", HeadSHA: "aaa"}, verdict: want072, current: stagelabels.Fixing, had: true},
		{name: "past the grace", owner: stageOwner{Lane: stageLaneSelf, Head: "aaa", Stage: stagelabels.Fixing, At: now.Add(-stageOwnerAbsenceGrace - time.Second)}, known: true, list: only, obs: contrib, verdict: want072, current: stagelabels.Fixing, had: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageOwnerHolds(tc.owner, tc.known, tc.list, tc.obs, tc.verdict, tc.current, tc.had, now); got != tc.want {
				t.Fatalf("stageOwnerHolds = %v, want %v", got, tc.want)
			}
		})
	}
}
