package automerge

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestApprovedFreshHeadStaysReviewedAcrossCheckReads: a sentinel-blocked
// contributor PR whose head the reviewer approved, while its workflow runs
// wait for a maintainer's "Approve and run", so no check or status has
// reported on it. One pass's check read is refused (GitHub pacing shed the
// request); the next, past the digest cache, reads the empty head. Both are
// 060 with the sentinel as the gate. The route used to read the empty head
// as 050 "no reviewer verdict", so the same head flipped 050 and 060 on
// every refused read. A new head with no verdict yet is still 050.
func TestApprovedFreshHeadStaysReviewedAcrossCheckReads(t *testing.T) {
	const sentinelGate = "a sentinel alert is open on this PR"
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 28, 11, 19, 0, 0, time.UTC)}
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.url)
	client.SetAppBotLogin(testHiveAppBotLogin)
	tracker := NewStageTracker()
	reads := 0
	e := New(client, Options{
		Now: clock.now,
		StageLabelPolicy: func() StageLabelPolicy {
			return StageLabelPolicy{Enabled: true}
		},
		StageTracker: tracker,
		StageStore:   stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now),
		CheckSummary: func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
			reads++
			if reads == 1 {
				return hgithub.CommitChecksSummary{}, errors.New("github pacing: request shed")
			}
			return hgithub.CommitChecksSummary{MissingRequired: []string{"Unit tests"}}, nil
		},
		ContributorSoak: NewContributorSoakTracker(),
		ContributorLanePolicy: func() ContributorLanePolicy {
			return ContributorLanePolicy{Enabled: true, RequireTiers: map[string]bool{"trusted": true}}
		},
		// The verdict is keyed by head, as review-verdicts.json is: the
		// approval is for "sha" only.
		ContributorLaneVerdict: func(_ string, _ int, head string) ContributorLaneVerdict {
			if head != "sha" {
				return ContributorLaneVerdict{Configured: true}
			}
			return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
		},
	})
	key := stageKey("acme", "widget", 9)
	obs := stageObservation{
		Lane: stageLaneContributor, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 9,
		HeadSHA: "sha", Labels: []string{"sentinel-alert"}, Reason: contributorLaneReasonSentinelBlocked,
		LaneReason: contributorLaneReasonSentinelBlocked, AuthorKind: authorkind.Clanker,
	}
	route := func(o stageObservation, wantFresh bool, want stagelabels.Stage, reason string) {
		t.Helper()
		r, ev := e.routeHuman(context.Background(), tracker, key, o, stagelabels.ReadyForHumanMerge, clock.now())
		if ev.HeadFresh != wantFresh {
			t.Fatalf("head %s: evidence HeadFresh = %v, want %v (checks %+v)", o.HeadSHA, ev.HeadFresh, wantFresh, ev.Checks)
		}
		if r.Stage != want {
			t.Fatalf("head %s: stage = %v, want %v (route %+v)", o.HeadSHA, r.Stage, want, r)
		}
		if !slices.Contains(r.Reasons, reason) {
			t.Fatalf("head %s: reasons = %v, want %q", o.HeadSHA, r.Reasons, reason)
		}
		if !slices.Contains(r.Gates, sentinelGate) {
			t.Fatalf("head %s: gates = %v, want the sentinel gate kept", o.HeadSHA, r.Gates)
		}
	}
	// The refused read: the checks are unknown, and the approved head waits
	// at 060 for them.
	route(obs, false, stagelabels.Reviewed, "checks could not be read")
	// Past the digest cache the read succeeds and finds nothing on the
	// head: still 060, now waiting for the checks to report.
	clock.advance(humanChecksTTL + time.Minute)
	route(obs, true, stagelabels.Reviewed, "no check has reported on this head yet")
	// A new head has no verdict for it yet: 050, as before.
	pushed := obs
	pushed.HeadSHA = "newhead"
	route(pushed, true, stagelabels.ReviewPending, "no reviewer verdict for this head yet")
}
