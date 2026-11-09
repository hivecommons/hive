package automerge

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// newNudgeEngineAt is newNudgeEngine with the stage store at storePath so a
// second engine can be booted on the same file (a restart).
func newNudgeEngineAt(t *testing.T, apiURL, storePath string, clock *contributorLaneClock, sink ContributorNudgeSink, policy StageLabelPolicy) *Engine {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	store := stagelabels.NewStore(storePath, clock.now)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	return New(client, Options{
		Now:              clock.now,
		StageLabelPolicy: func() StageLabelPolicy { return policy },
		StageTracker:     NewStageTracker(),
		StageStore:       store,
		ContributorNudge: sink,
	})
}

// CI-failure nudges are keyed on the failing-check set (hivecommons/hive#17328):
// the reason prefix flapping between check-failing / optional- / required-
// on one head is one class; a check joining the set on the same head folds
// into the comment without a relay re-dispatch; the same set on a new head
// bumps the attempt count; a changed set on a new head inside the cooldown
// folds, after it counts a fresh nudge. All of it survives a restart.
func TestContributorNudgeCIFailureSetCooldown(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	api.statefulComments = true
	clock := &contributorLaneClock{at: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "preempt", Online: true}}
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, RenudgeInterval: 72 * time.Hour, CIFailureCooldown: 6 * time.Hour}
	storePath := filepath.Join(t.TempDir(), "stage.json")
	e := newNudgeEngineAt(t, apiURLOf(t, api), storePath, clock, sink, policy)
	ctx := context.Background()
	observe := func(e *Engine, pr *gh.PullRequest, reason string, red ...string) StageLabelResult {
		if len(red) > 0 {
			e.noteStageRedChecks("acme", "widget", 7, pr.GetHead().GetSHA(), red)
		}
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
		res, err := e.ApplyStageLabels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	lastEdit := func() string {
		if len(api.commentEdits) == 0 {
			return ""
		}
		return api.commentEdits[len(api.commentEdits)-1]
	}
	in072 := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	key := stageKey("acme", "widget", 7)

	// First red head: one comment, attempt 1, relay dispatched.
	res := observe(e, nudgePR(7, "aaa"), "check-failing:changelog-fragment-guard")
	if got := nudgeComments(api); len(got) != 1 || res.Comments != 1 {
		t.Fatalf("first nudge: res=%+v comments=%v", res, api.comments)
	} else if !strings.Contains(got[0], "reason=check-failing head=aaa n=1 mode=preempt checks=") || !strings.Contains(got[0], "attempt=1") {
		t.Fatalf("marker: %s", got[0])
	}
	if len(sink.nudges) != 1 {
		t.Fatalf("sink=%d", len(sink.nudges))
	}

	// Same head, the lane's prefix flips to optional-check-failing and a
	// second check is red: same class, the set grows → the comment is
	// edited (both checks listed), the relay is NOT dispatched again, the
	// class count stays at 1 and the cooldown clock does not move.
	api.reset()
	clock.advance(4 * time.Minute)
	res = observe(e, nudgePR(7, "aaa", in072...), "optional-check-failing:native-queue-policy", "changelog-fragment-guard", "native-queue-policy")
	if res.Comments != 0 || len(nudgeComments(api)) != 0 || len(api.commentEdits) != 1 {
		t.Fatalf("set grew on same head: res=%+v comments=%v edits=%d", res, api.comments, len(api.commentEdits))
	}
	for _, want := range []string{"reason=check-failing head=aaa n=1 mode=preempt", "attempt=1", "`changelog-fragment-guard`", "`native-queue-policy`"} {
		if !strings.Contains(lastEdit(), want) {
			t.Fatalf("fold missing %q:\n%s", want, lastEdit())
		}
	}
	if len(sink.nudges) != 1 {
		t.Fatalf("relay re-dispatched on the same head: %d", len(sink.nudges))
	}
	rec, _ := e.stageStore.Get(key)
	if rec.NudgeReasonCount != 1 || !reflect.DeepEqual(rec.NudgeCIChecks, []string{"changelog-fragment-guard", "native-queue-policy"}) || !rec.NudgeCIAt.Equal(clock.at.Add(-4*time.Minute)) {
		t.Fatalf("ledger after fold: %+v", rec)
	}

	// Same head again, prefix flips back to required- naming one check:
	// the set is a subset of what the comment lists → nothing at all.
	api.reset()
	clock.advance(time.Minute)
	observe(e, nudgePR(7, "aaa", in072...), "required-check-failing:changelog-fragment-guard")
	if api.writes() != 0 || len(api.commentEdits) != 1 || len(sink.nudges) != 1 {
		t.Fatalf("subset flap wrote: comments=%v edits=%d sink=%d", api.comments, len(api.commentEdits), len(sink.nudges))
	}

	// New head, same failing set: edited in place with attempt 2, the
	// relay is told (the author pushed), still no new comment.
	api.reset()
	clock.advance(10 * time.Minute)
	res = observe(e, nudgePR(7, "bbb", in072...), "optional-check-failing:changelog-fragment-guard", "changelog-fragment-guard", "native-queue-policy")
	if res.Comments != 0 || len(nudgeComments(api)) != 0 || len(api.commentEdits) != 2 || len(sink.nudges) != 2 {
		t.Fatalf("same set new head: res=%+v comments=%v edits=%d sink=%d", res, api.comments, len(api.commentEdits), len(sink.nudges))
	}
	for _, want := range []string{"head=bbb n=1", "attempt=2", "failed on 2 heads so far (latest `bbb`)"} {
		if !strings.Contains(lastEdit(), want) {
			t.Fatalf("attempt rewrite missing %q:\n%s", want, lastEdit())
		}
	}

	// Restart: a fresh engine on the same store file sees the ledger and
	// the same head + set is still nothing.
	if err := e.stageStore.Save(); err != nil {
		t.Fatal(err)
	}
	api.reset()
	e = newNudgeEngineAt(t, apiURLOf(t, api), storePath, clock, sink, policy)
	clock.advance(time.Minute)
	observe(e, nudgePR(7, "bbb", in072...), "check-failing:native-queue-policy")
	if api.writes() != 0 || len(api.commentEdits) != 2 || len(sink.nudges) != 2 {
		t.Fatalf("restart re-nudged: comments=%v edits=%d sink=%d", api.comments, len(api.commentEdits), len(sink.nudges))
	}
	rec, _ = e.stageStore.Get(key)
	if rec.NudgeCIAttempts != 2 || rec.NudgeCIHead != "bbb" || rec.NudgeMode != "preempt" {
		t.Fatalf("ledger not persisted: %+v", rec)
	}

	// New head, a different failing set, 15 minutes into the 6h cooldown:
	// folded into the comment (attempt restarts at 1 for the new set) and
	// the class count does not restart.
	api.reset()
	res = observe(e, nudgePR(7, "ccc", in072...), "check-failing:lint")
	if res.Comments != 0 || len(nudgeComments(api)) != 0 || len(api.commentEdits) != 3 || len(sink.nudges) != 3 {
		t.Fatalf("changed set inside cooldown: res=%+v comments=%v edits=%d sink=%d", res, api.comments, len(api.commentEdits), len(sink.nudges))
	}
	if !strings.Contains(lastEdit(), "head=ccc n=1") || !strings.Contains(lastEdit(), "attempt=1") || !strings.Contains(lastEdit(), "`lint`") || strings.Contains(lastEdit(), "native-queue-policy") {
		t.Fatalf("fold rewrite:\n%s", lastEdit())
	}
	rec, _ = e.stageStore.Get(key)
	if rec.NudgeReasonCount != 1 || !rec.NudgeCIAt.Equal(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)) || !reflect.DeepEqual(rec.NudgeCIChecks, []string{"lint"}) {
		t.Fatalf("ledger after cooldown fold: %+v", rec)
	}

	// Past the cooldown, yet another set on a new head: a fresh nudge
	// (count restarts, cooldown clock moves) — still the one comment.
	api.reset()
	clock.advance(7 * time.Hour)
	res = observe(e, nudgePR(7, "ddd", in072...), "check-failing:vet")
	if res.Comments != 0 || len(nudgeComments(api)) != 0 || len(api.commentEdits) != 4 || len(sink.nudges) != 4 || sink.nudges[3].Nudge != 1 {
		t.Fatalf("changed set after cooldown: res=%+v comments=%v edits=%d sink=%+v", res, api.comments, len(api.commentEdits), sink.nudges)
	}
	rec, _ = e.stageStore.Get(key)
	if rec.NudgeReasonCount != 1 || !rec.NudgeCIAt.Equal(clock.at) || rec.NudgeCIAttempts != 1 {
		t.Fatalf("ledger after fresh nudge: %+v", rec)
	}
	if len(api.comments) != 0 {
		t.Fatalf("a second nudge comment was posted: %v", api.comments)
	}
}

func TestCINudgeDue(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	rec := stagelabels.Record{NudgeHead: "a", NudgeReason: stagelabels.NudgeReasonCIFailure, NudgeReasonCount: 1, NudgeReasonAt: now.Add(-time.Hour), NudgeCIChecks: []string{"lint", "unit"}, NudgeCIAt: now.Add(-time.Hour)}
	legacy := stagelabels.Record{NudgeHead: "a", NudgeReason: stagelabels.NudgeReasonCIFailure, NudgeReasonCount: 1, NudgeReasonAt: now.Add(-time.Hour)}
	cases := []struct {
		name     string
		rec      stagelabels.Record
		head     string
		checks   []string
		cooldown time.Duration
		ordinal  int
		ok       bool
	}{
		{name: "empty head", rec: rec, head: "", checks: []string{"unit"}, cooldown: 6 * time.Hour},
		{name: "new class", rec: stagelabels.Record{NudgeReason: "conflicting"}, head: "a", checks: []string{"unit"}, cooldown: 6 * time.Hour, ordinal: 1, ok: true},
		{name: "same head same set inside repeat window", rec: rec, head: "a", checks: []string{"unit", "lint"}, cooldown: 6 * time.Hour},
		{name: "same head set changed folds", rec: rec, head: "a", checks: []string{"unit", "lint", "vet"}, cooldown: 6 * time.Hour, ordinal: 0, ok: true},
		{name: "new head same set folds", rec: rec, head: "b", checks: []string{"lint", "unit"}, cooldown: 6 * time.Hour, ordinal: 0, ok: true},
		{name: "new head changed set inside cooldown folds", rec: rec, head: "b", checks: []string{"vet"}, cooldown: 6 * time.Hour, ordinal: 0, ok: true},
		{name: "new head changed set after cooldown counts", rec: rec, head: "b", checks: []string{"vet"}, cooldown: 30 * time.Minute, ordinal: 1, ok: true},
		{name: "zero cooldown counts", rec: rec, head: "b", checks: []string{"vet"}, cooldown: 0, ordinal: 1, ok: true},
		{name: "legacy record counts from the class", rec: legacy, head: "b", checks: []string{"vet"}, cooldown: 6 * time.Hour, ordinal: 0, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := ciNudgeDue(tc.rec, tc.head, tc.checks, 72*time.Hour, tc.cooldown, now)
			if n != tc.ordinal || ok != tc.ok {
				t.Fatalf("ciNudgeDue = (%d, %v), want (%d, %v)", n, ok, tc.ordinal, tc.ok)
			}
		})
	}
	if ciFailureSetDigest([]string{"unit", "lint"}) != ciFailureSetDigest([]string{"lint", "unit"}) || len(ciFailureSetDigest([]string{"unit"})) != 12 {
		t.Fatal("digest must be order-independent and 12 hex chars")
	}
	for in, want := range map[string]bool{"check-failing:unit": true, "optional-check-failing:links": true, "required-check-failing:lint": true, "conflicting": false, "status-failure": false} {
		if got := isCIFailureNudgeReason(in); got != want {
			t.Errorf("isCIFailureNudgeReason(%q) = %v", in, got)
		}
	}
}
