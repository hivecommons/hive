package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// observeOnlyLaneAPI serves one open, approved, green PR (#7 by alice, head
// sha7) on acme/widget to the merge lanes, on top of the stage API the
// stage writer uses, and records every request that is not a read.
type observeOnlyLaneAPI struct {
	writes  []string
	merges  int
	updates int
	graphql int
}

func (a *observeOnlyLaneAPI) serve(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		a.writes = append(a.writes, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
			a.merges++
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "merge7"})
			return true
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
			a.updates++
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Updating pull request branch."})
			return true
		case r.URL.Path == "/graphql" || r.URL.Path == "/api/graphql":
			a.graphql++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
			return true
		}
		return false
	}
	if serveUnprotectedSweepBase(w, r) {
		return true
	}
	pull := map[string]any{
		"number":          7,
		"state":           "open",
		"mergeable_state": "clean",
		"mergeable":       true,
		"user":            map[string]string{"login": "alice"},
		"head":            map[string]any{"sha": "sha7", "ref": "feature", "repo": map[string]string{"full_name": "acme/widget"}},
		"base":            map[string]any{"ref": "main", "sha": "base1", "repo": map[string]string{"full_name": "acme/widget"}},
		"labels":          []map[string]string{},
	}
	switch r.URL.Path {
	case "/repos/acme/widget/pulls":
		_ = json.NewEncoder(w).Encode([]map[string]any{pull})
	case "/repos/acme/widget/pulls/7":
		_ = json.NewEncoder(w).Encode(pull)
	case "/repos/acme/widget/pulls/7/reviews":
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	case "/repos/acme/widget/commits/sha7/status":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":       "success",
			"total_count": 1,
			"statuses":    []map[string]string{{"context": "ci/build", "state": "success"}},
		})
	case "/repos/acme/widget/commits/sha7/check-runs":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"check_runs":  []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}},
		})
	default:
		return false
	}
	return true
}

// TestObserveOnlyPassStagesAutoMergeOffRepoWithoutWrites: on a repo whose
// auto-merge is off the lanes used to skip the repo before recording any
// stage evidence, so an approved, green PR there never got a stage
// (hivecommons/hive#17566). Each lane now observes it: the drain holds a
// repo-auto-merge-disabled observation, the stage writer parks it at 090
// "auto-merge off", and the lanes send GitHub no write at all — no merge,
// no branch update, no native-queue enqueue. Only a repo an owner switched
// off at L6 is observed (hivecommons/hive#17552 decision A): on an ACMM L5
// hive, where auto-merge is off everywhere and RepoAutoMergeSwitchedOff
// names no repo, the lanes skip the repo as before and stage nothing.
func TestObserveOnlyPassStagesAutoMergeOffRepoWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name        string
		acmmAllowed bool // ACMM L6 (self-authored lane runs, repo switched off) vs L5
	}{
		{name: "acmm L6", acmmAllowed: true},
		{name: "acmm L5", acmmAllowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lane := &observeOnlyLaneAPI{}
			api := &stageAPI{labels: map[string]string{}, extra: lane.serve}
			for _, d := range stagelabels.AllLabelDefs() {
				api.labels[d.Name] = d.Color
			}
			srv := newStageAPI(t, api)
			defer srv.Close()
			setStagePRTruth(7)

			clock := &contributorLaneClock{at: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			tracker := NewStageTracker()
			store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
			e := New(client, Options{
				Now:                  clock.now,
				StageLabelPolicy:     func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
				StageTracker:         tracker,
				StageStore:           store,
				CheckSummary:         greenChecks,
				BasePreparation:      NewBasePreparationStore(""),
				RepoAutoMergeEnabled: func(string) bool { return false },
				RepoAutoMergeSwitchedOff: func(repo string) bool {
					// The lanes pass the transport's display name (here
					// the short "widget"); config.RepoPolicyFor accepts both.
					return tc.acmmAllowed && (strings.EqualFold(repo, "widget") || strings.EqualFold(repo, "acme/widget"))
				},
				TrustedAuthorPolicy: func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true} },
				TrustedAuthorizer: func(login, requireRole string) TrustedAuthorDecision {
					return TrustedAuthorDecision{Allowed: login == "alice", Role: requireRole}
				},
				ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
					return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
				},
			})
			ctx := context.Background()

			// The lanes sweepPass runs for this ACMM level.
			if tc.acmmAllowed {
				res, err := e.SweepSelfAuthoredAutoMerges(ctx, AutoMergeSweepOptions{})
				if err != nil || len(res.Merged) != 0 {
					t.Fatalf("self-authored lane: result=%+v err=%v", res, err)
				}
			}
			res, err := e.SweepTrustedAuthorAutoMerges(ctx, AutoMergeSweepOptions{SelfAuthoredLaneActive: tc.acmmAllowed})
			if err != nil || len(res.Merged) != 0 {
				t.Fatalf("trusted-author lane: result=%+v err=%v", res, err)
			}
			if len(lane.writes) != 0 {
				t.Fatalf("lanes wrote to GitHub on an auto-merge-off repo: %v", lane.writes)
			}

			tracker.mu.Lock()
			obs, ok := tracker.obs[stageKey("acme", "widget", 7)][stageLaneTrusted]
			tracker.mu.Unlock()
			if !tc.acmmAllowed {
				if ok {
					t.Fatalf("below L6 the lanes must not observe the repo, got %+v", obs)
				}
				if _, err := e.ApplyStageLabels(ctx); err != nil {
					t.Fatalf("ApplyStageLabels: %v", err)
				}
				if rec, ok := store.Get(stageKey("acme", "widget", 7)); ok {
					t.Fatalf("below L6 the PR must have no stage record, got %+v", rec)
				}
				if len(api.added) != 0 || lane.merges != 0 || lane.updates != 0 || lane.graphql != 0 {
					t.Fatalf("below L6 the lanes wrote labels=%v merges=%d update-branch=%d graphql=%d, want none", api.added, lane.merges, lane.updates, lane.graphql)
				}
				return
			}
			if !ok {
				t.Fatal("trusted-author lane recorded no observation for the auto-merge-off repo's PR")
			}
			if obs.Reason != observeOnlyReasonRepoAutoMergeDisabled || obs.Merged || obs.Err || obs.HeadSHA != "sha7" {
				t.Fatalf("observation = reason %q merged %v err %v head %q, want %q at sha7", obs.Reason, obs.Merged, obs.Err, obs.HeadSHA, observeOnlyReasonRepoAutoMergeDisabled)
			}

			if _, err := e.ApplyStageLabels(ctx); err != nil {
				t.Fatalf("ApplyStageLabels: %v", err)
			}
			rec, ok := store.Get(stageKey("acme", "widget", 7))
			if !ok || rec.Stage != stagelabels.ReadyForHumanMerge {
				t.Fatalf("stage record = %+v (ok %v), want %s", rec, ok, stagelabels.ReadyForHumanMerge)
			}
			if got := prstage.HumanMergeReason(rec.Reason); got != prstage.HumanMergeReasonAutoMergeOff {
				t.Fatalf("human merge reason of %q = %q, want %q", rec.Reason, got, prstage.HumanMergeReasonAutoMergeOff)
			}
			if !slices.Contains(slices.Concat(api.added...), stagelabels.ReadyForHumanMerge.Label()) {
				t.Fatalf("stage labels added = %v, want %s", api.added, stagelabels.ReadyForHumanMerge.Label())
			}
			if lane.merges != 0 || lane.updates != 0 || lane.graphql != 0 {
				t.Fatalf("merges=%d update-branch=%d graphql (enqueue)=%d, want none", lane.merges, lane.updates, lane.graphql)
			}
		})
	}
}

// The observe-only pass keeps the head's own evidence and replaces only the
// reasons that would stage a PR nothing will merge as merging.
func TestObserveOnlyStageReason(t *testing.T) {
	for reason, want := range map[string]string{
		"":                             observeOnlyReasonRepoAutoMergeDisabled,
		integrationEligibilityCaptured: observeOnlyReasonRepoAutoMergeDisabled,
		"behind":                       observeOnlyReasonRepoAutoMergeDisabled,
		"update-branch":                observeOnlyReasonRepoAutoMergeDisabled,
		contributorLaneReasonSoaking:   observeOnlyReasonRepoAutoMergeDisabled,
		"conflicting":                  "conflicting",
		"label:hold":                   "label:hold",
		"draft":                        "draft",
		"not-app-authored":             "not-app-authored",
		"untrusted-author-role":        "untrusted-author-role",
		"required-check-failing:build": "required-check-failing:build",
		"check-pending":                "check-pending",
	} {
		if got := observeOnlyStageReason(stageLaneTrusted, reason); got != want {
			t.Errorf("observeOnlyStageReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

// The engine an observe-only pass runs on refuses every write path the
// lanes reach, whatever the live engine allows.
func TestObserveOnlyEngineRefusesWrites(t *testing.T) {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:1")
	client.SetAppBotLogin(testHiveAppBotLogin)
	e := New(client, Options{StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }})
	oc := e.observeOnlyEngine()
	if e.observeOnly || !oc.observeOnly {
		t.Fatalf("observeOnly live=%v observe=%v, want false/true", e.observeOnly, oc.observeOnly)
	}
	if oc.stageTrackerOrShared() != e.stageTrackerOrShared() {
		t.Fatal("observe-only engine must record into the live engine's stage tracker")
	}
	if d := oc.writebackAllowed("acme/widget", writebackItemForObservation(stageObservation{Number: 7}), "", ""); d.Allowed {
		t.Fatalf("write-back gate allowed a write in an observe-only pass: %+v", d)
	}
	if _, reason, err := oc.routeIntegrationMerge(context.Background(), nil, effects.Claim{Repo: "acme/widget", Kind: effects.KindPullRequestMerge}); reason != observeOnlyReasonRepoAutoMergeDisabled || err != nil {
		t.Fatalf("routeIntegrationMerge = %q, %v; want the merge refused as %q", reason, err, observeOnlyReasonRepoAutoMergeDisabled)
	}
}

// projectbluefin/server#699: an approved trusted-author head on an
// auto-merge-off repo flipped 090 ↔ 050 every ~25 minutes, reason
// repo-auto-merge-disabled both ways; the 050 pass resolved the PR with no
// verdict. The engine ticker and the queued sweep drain one shared tracker
// (hivecommons/hive#12573), so a drain between a lane's noteStageVerdict and
// its recordStage can take the verdict, and when no later lane in the pass
// notes it again the observation classifies as 050. The stage pass now reads
// the head's verdict from the review-verdicts record the lanes read when the
// drain carries none: for the trusted-author, contributor and self-authored
// lanes. A trusted bot's PR is covered by
// TestFoldObserveOnlyVerdictTrustedBotMatchesLane.
func TestFoldObserveOnlyVerdictSurvivesSplitDrain(t *testing.T) {
	const approvedHead, newHead = "7db5a9e69fec7752953f832862d45e67555acd4e", "beefbeefbeefbeefbeefbeefbeefbeefbeefbeef"
	approve := ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	verdicts := func(repo string, number int, head string) ContributorLaneVerdict {
		if repo == "acme/widget" && number == 699 && head == approvedHead {
			return approve
		}
		// Reviewed nothing for this head.
		return ContributorLaneVerdict{Configured: true}
	}
	newEngine := func(fn ContributorLaneVerdictFunc) *Engine {
		return &Engine{
			transport:              stubTransport{},
			stageTracker:           NewStageTracker(),
			stageLabelPolicy:       func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
			contributorLaneVerdict: fn,
		}
	}
	observe := func(lane, head string) stageObservation {
		author := "castrojo"
		if lane == stageLaneSelf {
			author = testHiveAppBotLogin
		}
		return stageObservation{Lane: lane, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 699, HeadSHA: head, Author: author, Reason: observeOnlyReasonRepoAutoMergeDisabled}
	}
	// split drives the race: the lane notes the verdict, the other
	// driver's drain takes it, then the lane records its observation.
	split := func(t *testing.T, c *Engine, lane, head string) []stageObservation {
		t.Helper()
		c.noteStageVerdict("acme", "widget", 699, c.contributorLaneVerdictFor("acme/widget", 699, head))
		c.stageTracker.drain()
		c.stageTracker.record(observe(lane, head))
		lists := c.stageTracker.drain()
		if len(lists) != 1 || len(lists[0]) != 1 {
			t.Fatalf("drain = %+v, want one observation", lists)
		}
		if lists[0][0].Detail.VerdictKnown {
			t.Fatalf("the split drain kept the verdict; the race is not reproduced: %+v", lists[0][0].Detail)
		}
		return lists[0]
	}

	for _, lane := range []string{stageLaneTrusted, stageLaneContributor, stageLaneSelf} {
		t.Run(lane+": approved head stays 090", func(t *testing.T) {
			c := newEngine(verdicts)
			list := split(t, c, lane, approvedHead)
			if got, _, _ := resolveStage(list); got.Stage != stagelabels.ReviewPending {
				t.Fatalf("before the fold: stage %s, want 050 (the flap's other half)", got.Stage)
			}
			c.foldObserveOnlyVerdict(list)
			got, _, obs := resolveStage(list)
			if got.Stage != stagelabels.ReadyForHumanMerge || !got.Human {
				t.Fatalf("stage %s human=%v (lane %s), want 090 human", got.Stage, got.Human, obs.Lane)
			}
			if d := stageengine.DerivePR(factsFrom(list)); d.Stage != stagelabels.ReadyForHumanMerge {
				t.Fatalf("reconciler derivation = %s, want 090", d.Stage)
			}
		})
		t.Run(lane+": unreviewed head is still 050", func(t *testing.T) {
			c := newEngine(func(string, int, string) ContributorLaneVerdict { return ContributorLaneVerdict{Configured: true} })
			list := split(t, c, lane, approvedHead)
			c.foldObserveOnlyVerdict(list)
			if !list[0].Detail.VerdictKnown || list[0].Detail.Verdict.Approved {
				t.Fatalf("detail = %+v, want a known, unapproved verdict", list[0].Detail)
			}
			if got, _, _ := resolveStage(list); got.Stage != stagelabels.ReviewPending {
				t.Fatalf("stage %s, want 050", got.Stage)
			}
			if d := stageengine.DerivePR(factsFrom(list)); d.Stage != stagelabels.ReviewPending {
				t.Fatalf("reconciler derivation = %s, want 050", d.Stage)
			}
		})
		t.Run(lane+": a new head does not keep the old head's verdict", func(t *testing.T) {
			c := newEngine(verdicts)
			list := split(t, c, lane, newHead)
			c.foldObserveOnlyVerdict(list)
			if list[0].Detail.Verdict.Approved {
				t.Fatalf("the old head's approve carried onto %s: %+v", newHead, list[0].Detail)
			}
			if got, _, _ := resolveStage(list); got.Stage != stagelabels.ReviewPending {
				t.Fatalf("stage %s, want 050", got.Stage)
			}
		})
	}

	t.Run("a verdict the drain carried is left alone", func(t *testing.T) {
		c := newEngine(verdicts)
		o := observe(stageLaneContributor, approvedHead)
		o.Detail = stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
		list := []stageObservation{o}
		c.foldObserveOnlyVerdict(list)
		if list[0].Detail.Verdict.Approved || list[0].Detail.Verdict.Verdict != "changes_requested" {
			t.Fatalf("noted verdict overwritten: %+v", list[0].Detail)
		}
	})
	t.Run("an author in no sweep lane is left alone", func(t *testing.T) {
		c := newEngine(verdicts)
		o := observe(stageLaneSelf, approvedHead)
		o.Author = "mallory"
		list := []stageObservation{o}
		c.foldObserveOnlyVerdict(list)
		if list[0].Detail.VerdictKnown {
			t.Fatalf("verdict folded onto an author in no sweep lane: %+v", list[0].Detail)
		}
	})
	t.Run("other lane reasons are left alone", func(t *testing.T) {
		c := newEngine(verdicts)
		o := observe(stageLaneContributor, approvedHead)
		o.Reason = contributorLaneReasonSentinelBlocked
		list := []stageObservation{o}
		c.foldObserveOnlyVerdict(list)
		if list[0].Detail.VerdictKnown {
			t.Fatalf("verdict folded onto reason %q: %+v", o.Reason, list[0].Detail)
		}
	})
}

// TestFoldObserveOnlyVerdictTrustedBotMatchesLane: a trusted bot's PR (the
// self-authored sweep's trusted-bot lane, e.g. mergeraptor) on a repo whose
// auto-merge is off flipped 050 <-> 060 for days. The fold skipped it, so a
// drain that split the lane's verdict note from its observation classified
// repo-auto-merge-disabled with no verdict as 050, while a drain that kept
// the note routed the approved verdict. The fold now gives the observation
// the verdict the lane notes on the observe-only engine (trustedBotLaneVerdict
// over the raw record, which that engine reads without recorded decisions),
// and foldRecordedDecision applies a person's answer on both drains, so both
// reach the same stage and reason for the same head, verdict and decision.
func TestFoldObserveOnlyVerdictTrustedBotMatchesLane(t *testing.T) {
	const head, otherHead = "7db5a9e69fec7752953f832862d45e67555acd4e", "beefbeefbeefbeefbeefbeefbeefbeefbeefbeef"
	const bot = "mergeraptor[bot]"
	cases := []struct {
		name       string
		verdict    ContributorLaneVerdict
		skipReview bool
		decided    bool              // a person's decision is recorded on the head
		decision   string            // its kind (review.HumanChangeRequestKind*)
		want       stagelabels.Stage // None: only require both drains to agree
	}{
		{name: "approved", verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, skipReview: true, want: stagelabels.ReadyForHumanMerge},
		{name: "no verdict", verdict: ContributorLaneVerdict{Configured: true}, skipReview: true, want: stagelabels.ReviewPending},
		{name: "changes requested", verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}, skipReview: true},
		{name: "requires human", verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}, skipReview: true},
		{name: "changes requested, trusted_bot_skip_review off", verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}},
		{name: "requires human, trusted_bot_skip_review off", verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}, want: stagelabels.NeedsDecision},
		{name: "requires human answered approve", verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}, skipReview: true, decided: true, decision: review.HumanChangeRequestKindApprove},
		{name: "requires human answered with changes", verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}, skipReview: true, decided: true, decision: review.HumanChangeRequestKindChanges},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip := tc.skipReview
			c := &Engine{
				transport:            stubTransport{},
				trustedBotAuthors:    func() map[string]bool { return map[string]bool{bot: true} },
				trustedBotSkipReview: func() bool { return skip },
				stageTracker:         NewStageTracker(),
				stageLabelPolicy:     func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
				contributorLaneVerdict: func(repo string, number int, h string) ContributorLaneVerdict {
					if repo == "acme/widget" && number == 1721 && h == head {
						return tc.verdict
					}
					return ContributorLaneVerdict{Configured: true}
				},
			}
			if tc.decided {
				body := "approve"
				if tc.decision != review.HumanChangeRequestKindApprove {
					body = "Pin the bumped dependency to the previous minor."
				}
				store := review.NewHumanChangeRequests("")
				if err := store.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 1721, HeadSHA: head, Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "Danathar", Body: body, Kind: tc.decision}); err != nil {
					t.Fatal(err)
				}
				c.humanChangeRequests = store
			}
			if lane, ok := c.sweepLaneForAuthor(bot); !ok || lane != laneTrustedBot {
				t.Fatalf("sweepLaneForAuthor(%s) = %q, %v; want the trusted-bot lane", bot, lane, ok)
			}
			observe := func(h string) stageObservation {
				return stageObservation{Lane: stageLaneSelf, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 1721, HeadSHA: h, Author: bot, Reason: observeOnlyReasonRepoAutoMergeDisabled}
			}
			// laneNote is what trySweepSelfAuthoredPR notes for the head on
			// the observe-only engine, which has no recorded decisions
			// (integrationReadOnlyOptions): the raw verdict, downgraded.
			laneNote := func(h string) {
				v := normalizeContributorLaneVerdict(c.contributorLaneVerdict("acme/widget", 1721, h))
				c.noteStageVerdict("acme", "widget", 1721, c.trustedBotLaneVerdict("acme/widget", 1721, h, laneTrustedBot, v))
			}
			// stagePass runs the folds in ApplyStageLabels' order.
			stagePass := func(list []stageObservation) stageVerdict {
				c.foldObserveOnlyVerdict(list)
				c.foldRecordedDecision(context.Background(), list)
				got, _, _ := resolveStage(list)
				return got
			}
			drainOne := func() []stageObservation {
				t.Helper()
				lists := c.stageTracker.drain()
				if len(lists) != 1 || len(lists[0]) != 1 {
					t.Fatalf("drain = %+v, want one observation", lists)
				}
				return lists[0]
			}

			// The drain that kept the lane's note.
			laneNote(head)
			c.stageTracker.record(observe(head))
			whole := drainOne()
			if !whole[0].Detail.VerdictKnown {
				t.Fatalf("the whole drain lost the lane's verdict: %+v", whole[0].Detail)
			}
			wholeStage := stagePass(whole)

			// The drain the other driver split: no verdict until the fold.
			laneNote(head)
			c.stageTracker.drain()
			c.stageTracker.record(observe(head))
			split := drainOne()
			if split[0].Detail.VerdictKnown {
				t.Fatalf("the split drain kept the verdict; the race is not reproduced: %+v", split[0].Detail)
			}
			splitStage := stagePass(split)
			if !split[0].Detail.VerdictKnown {
				t.Fatalf("the fold left the trusted bot's observation without a verdict: %+v", split[0].Detail)
			}

			if splitStage.Stage != wholeStage.Stage || splitStage.Reason != wholeStage.Reason {
				t.Fatalf("split drain stage %s (%s), whole drain stage %s (%s): the two drains disagree", splitStage.Stage, splitStage.Reason, wholeStage.Stage, wholeStage.Reason)
			}
			if !reflect.DeepEqual(split[0].Detail.Verdict, whole[0].Detail.Verdict) {
				t.Fatalf("folded verdict %+v, lane's noted verdict %+v", split[0].Detail.Verdict, whole[0].Detail.Verdict)
			}
			if ds, dw := stageengine.DerivePR(factsFrom(split)), stageengine.DerivePR(factsFrom(whole)); ds.Stage != dw.Stage {
				t.Fatalf("reconciler derivation: split %s, whole %s", ds.Stage, dw.Stage)
			}
			if tc.want != stagelabels.None && splitStage.Stage != tc.want {
				t.Fatalf("stage %s, want %s", splitStage.Stage, tc.want)
			}

			// A new head never inherits the old head's verdict.
			c.stageTracker.record(observe(otherHead))
			fresh := drainOne()
			if got := stagePass(fresh); fresh[0].Detail.Verdict.Approved || fresh[0].Detail.Verdict.DecisionAnswered {
				t.Fatalf("the old head's verdict or decision carried onto %s: %+v", otherHead, fresh[0].Detail)
			} else if got.Stage != stagelabels.ReviewPending {
				t.Fatalf("new head stage %s, want 050", got.Stage)
			}
		})
	}
}
