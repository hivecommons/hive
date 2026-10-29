package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestDelegatedAdoptionCannotTakeHumanOrOwnerPR(t *testing.T) {
	for _, tc := range []struct {
		name         string
		human, owner bool
		want         string
	}{
		{"human hand-authored", true, false, stagelabels.AdoptBlockedHumanAuthor},
		{"human label", true, false, stagelabels.AdoptBlockedHumanAuthor},
		{"owner clanker", false, true, stagelabels.AdoptBlockedOwnerAuthor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			// Default maintainer policy and an explicit request cannot override this rail.
			p.OwnerAuthor = func(login string) bool { return tc.owner && login == "mallory" }
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, p)
			defer done()
			labels := []string{"stage/085-needs-decision", delegatedecision.LabelAdoptSupersede}
			if tc.name == "human label" {
				labels = append(labels, "author/human")
			}
			pr := adoptPR(7, "aaa", labels...)
			if tc.name == "human hand-authored" {
				pr.Body = gh.Ptr("Hand-written fix")
			}
			h.observe(pr, contributorLaneReasonNoReviewer)
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if rec.AdoptedHead != "" || h.adopt.refAttempts != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.prEdits) != 0 {
				t.Fatalf("protected PR adopted: %+v", rec)
			}
			if rec.AdoptionBlocked("aaa") != tc.want || !rec.AdoptionRequestConsumed("aaa") {
				t.Fatalf("missing terminal blocked reason: %+v", rec)
			}
			found := false
			for _, l := range h.api.removed {
				if l == delegatedecision.LabelAdoptSupersede {
					found = true
				}
			}
			if !found {
				t.Fatalf("request not consumed: %v", h.api.removed)
			}
			// Direct in-place re-home paths use the same guard before any branch writes.
			kind := authorkind.Clanker
			if tc.human {
				kind = authorkind.Human
			}
			obs := stageObservation{Owner: "acme", Repo: "widget", Author: "mallory", Number: 7, HeadSHA: "aaa", BaseRef: "v5", AuthorKind: kind}
			if _, err := h.e.supersedeAdoptedPR(context.Background(), h.e.stageStoreOrShared(), stageKey("acme", "widget", 7), obs, rec, p, AdoptReasonDCORehome, time.Hour, false, true, h.clock.at); err != nil {
				t.Fatal(err)
			}
			if h.adopt.refAttempts != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.prEdits) != 0 {
				t.Fatal("re-home mutated protected PR")
			}
		})
	}
}

func TestHumanRequiresHumanNotifiesAuthorAt071(t *testing.T) {
	for _, verdict := range []string{"requires_human", "changes_requested"} {
		t.Run(verdict, func(t *testing.T) {
			api, done := seededStageAPI(t)
			defer done()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}
			v := &ContributorLaneVerdict{Configured: true, Verdict: verdict, DecisionQuestion: "Should the retry budget change?"}
			e, store := humanStageEngine(t, api.url, clock, v, 0)
			pr := stagePR(17, "author/human", "stage/085-needs-decision")
			pr.User = &gh.User{Login: gh.Ptr("alice")}
			for i := 0; i < 2; i++ {
				api.reset()
				e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
				e.noteStageVerdict("acme", "widget", 17, *v)
				e.noteStageRole("acme", "widget", 17, stagelabels.RoleContributor)
				if _, err := e.ApplyStageLabels(context.Background()); err != nil {
					t.Fatal(err)
				}
				if got := stageOf(t, store, 17); got != stagelabels.NeedsAuthor {
					t.Fatalf("stage %v, want 071", got)
				}
				if i == 0 {
					if len(api.comments) != 1 || !strings.Contains(api.comments[0], "@alice") || !strings.Contains(api.comments[0], v.DecisionQuestion) {
						t.Fatalf("missing author notification: %v", api.comments)
					}
				} else if len(api.comments) != 0 {
					t.Fatalf("repeated notification: %v", api.comments)
				}
			}
		})
	}
}
