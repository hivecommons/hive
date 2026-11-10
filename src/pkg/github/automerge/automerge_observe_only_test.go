package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
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
