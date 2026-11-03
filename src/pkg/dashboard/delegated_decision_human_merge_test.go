package dashboard

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// "Let hive decide" on an untrusted author's 090 PR takes the decision
// (hivecommons/hive#15173): a reviewed, green, mergeable head is approved
// for the merge sweep under its own number — no adoption — and only a red,
// conflicting or unreviewed head falls back to supersede adoption. The
// chosen path and its reason reach the tracker's ✓ hover.
func TestDelegatedDecisionUntrustedReadyForHumanMergeDecides(t *testing.T) {
	const reviewLabel = "review/changes-requested"
	cases := []struct {
		name       string
		mock       *repoItemDecisionMock
		reviewBad  bool
		wantRule   string
		wantDetail string
		wantAdopt  bool
	}{
		{name: "good review, green, mergeable approves", mock: &repoItemDecisionMock{},
			wantRule: delegatedHumanMergeApproveRule, wantDetail: "hive decided: approve · review passed, CI green"},
		{name: "red CI adopts", mock: &repoItemDecisionMock{prMergeState: "blocked", prRedCI: true},
			wantRule: delegatedHumanMergeAdoptCIRedRule, wantDetail: "hive decided: adopt · CI red", wantAdopt: true},
		{name: "conflicting adopts to rebase", mock: &repoItemDecisionMock{prMergeState: "dirty"},
			wantRule: delegatedHumanMergeAdoptRebaseRule, wantDetail: "hive decided: adopt · conflicting, needs rebase", wantAdopt: true},
		{name: "review not passed adopts", mock: &repoItemDecisionMock{}, reviewBad: true,
			wantRule: delegatedHumanMergeAdoptReviewRule, wantDetail: "hive decided: adopt · review not passed", wantAdopt: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := tc.mock
			mock.prForeign = true
			mock.prLabels = []string{"stage/090-ready-for-human-merge", "role/newcomer"}
			srv := newDelegatedServer(t, mock)
			srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
			srv.operatorActionStore = newOperatorActionStore("")
			store := seedAdmissionRecord(t, srv, stagelabels.ReadyForHumanMerge)
			if tc.reviewBad {
				srv.deps.Config.Review.VerdictLabels.ChangesRequested = reviewLabel
				mock.prLabels = append(mock.prLabels, reviewLabel)
			}
			body := `{"type":"pr","reason":"contributor-lane:author-association","caption":"Ready for human merge · untrusted author"}`
			w := postDelegated(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			job := decodeDelegatedJob(t, w)
			if job.Status != delegatedJobStatusDone || job.Decision == nil || job.Result == nil {
				t.Fatalf("job = %+v", job)
			}
			if job.Decision.Rule != tc.wantRule || job.Result.Action == rowActionNoEffect {
				t.Fatalf("rule=%q action=%q, want rule %q and a decision taken", job.Decision.Rule, job.Result.Action, tc.wantRule)
			}
			if strings.Contains(job.Result.Outcome, "I can't merge this") {
				t.Fatalf("outcome still refuses: %q", job.Result.Outcome)
			}
			added := strings.Join(mock.added, ",")
			adopted := strings.Contains(added, delegatedecision.LabelAdoptSupersede)
			if adopted != tc.wantAdopt {
				t.Fatalf("adoption queued=%v, want %v (labels added: %s)", adopted, tc.wantAdopt, added)
			}
			if tc.wantAdopt {
				if job.Decision.Verdict != delegatedecision.VerdictAdoptSupersede || len(mock.reviews) != 0 {
					t.Fatalf("verdict=%q reviews=%v", job.Decision.Verdict, mock.reviews)
				}
			} else {
				if job.Decision.Verdict != delegatedecision.VerdictApprove {
					t.Fatalf("verdict = %q", job.Decision.Verdict)
				}
				if len(mock.reviews) != 1 || mock.reviews[0]["event"] != "APPROVE" {
					t.Fatalf("want one APPROVE review on the head, got %v", mock.reviews)
				}
				if !strings.Contains(added, srv.deps.GHClient.AutoMergeLabel()) {
					t.Fatalf("approval must queue the PR for the merge sweep; labels added: %s", added)
				}
			}
			admitted := ""
			if rec, ok := store.Get(stagelabels.Key("testorg/testrepo", 8)); ok {
				admitted = rec.Admission(admissionTestHead)
			}
			if want := map[bool]string{false: "operator-approval:writer", true: ""}[tc.wantAdopt]; admitted != want {
				t.Fatalf("head admission = %q, want %q", admitted, want)
			}
			if len(mock.merges) != 0 {
				t.Fatalf("the dashboard merges nothing itself: %v", mock.merges)
			}
			if len(mock.comments) == 0 || !strings.Contains(mock.comments[0], tc.wantRule) {
				t.Fatalf("decision comment must carry rule %q: %v", tc.wantRule, mock.comments)
			}

			// The ✓ hover reads the ledger detail: recorded from the response
			// on a synchronous run, settled from the job on an async one.
			if err := srv.operatorActions().Record("testorg/testrepo", 8, OperatorAction{Action: operatorActionLetHiveDecide, Actor: "writer", At: time.Now().UTC(), Outcome: operatorActionOutcomePending}); err != nil {
				t.Fatal(err)
			}
			srv.settleOperatorAction("testorg/testrepo", 8, &job)
			got := srv.operatorActions().List("testorg/testrepo", 8)
			if len(got) != 2 {
				t.Fatalf("ledger = %+v, want the sync record and the settled one", got)
			}
			for _, a := range got {
				if a.Action != operatorActionLetHiveDecide || a.Detail != tc.wantDetail {
					t.Errorf("✓ detail = %+v, want %q", a, tc.wantDetail)
				}
			}
		})
	}
}

// hivecommons/hive#15202: a person's 090 PR (owner or author/human) is never
// adopted. A conflicting, red or unsigned head is returned to its author with
// the fix spelled out; a clean head is still approved; and a DCO-less clanker
// head still adopts (the one branch folded from hivecommons/hive#15191).
func TestDelegatedHumanMergeDecisionPersonAuthoredNeverAdopts(t *testing.T) {
	srv := newDelegatedServer(t, &repoItemDecisionMock{})
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	base := delegatedecision.Input{
		Repo: "testorg/testrepo", Number: 15157, Kind: delegatedecision.KindPR, Author: "clubanderson",
		Labels: []string{stagelabels.ReadyForHumanMerge.Label(), "role/newcomer"}, CIGreen: true,
	}
	req := delegatedDecisionRequest{Type: "pr", Reason: "contributor-lane:author-association"}
	cases := []struct {
		name     string
		mutate   func(in *delegatedecision.Input)
		wantRule string
		wantNote string
		wantText string
	}{
		{name: "owner conflicting returns to author", mutate: func(in *delegatedecision.Input) { in.OwnerAuthor = true; in.Conflicting = true },
			wantRule: delegatedecision.RuleReturnToAuthor, wantNote: "hive decided: return to author · the branch conflicts with the base and needs a rebase",
			wantText: "@clubanderson: rebase onto the base branch on #15157; then it merges"},
		{name: "author/human red CI returns to author", mutate: func(in *delegatedecision.Input) { in.HumanAuthor = true; in.CIGreen = false },
			wantRule: delegatedecision.RuleReturnToAuthor, wantNote: "hive decided: return to author · CI is not green on its head",
			wantText: "make CI green"},
		{name: "owner DCO missing returns to author", mutate: func(in *delegatedecision.Input) { in.OwnerAuthor = true; in.DCOMissing = true },
			wantRule: delegatedecision.RuleReturnToAuthor, wantNote: "hive decided: return to author · the DCO sign-off is missing from its commits",
			wantText: "add the DCO sign-off"},
		{name: "owner clean head approves", mutate: func(in *delegatedecision.Input) { in.OwnerAuthor = true },
			wantRule: delegatedHumanMergeApproveRule, wantNote: "hive decided: approve · review passed, CI green"},
		{name: "clanker DCO missing adopts", mutate: func(in *delegatedecision.Input) { in.DCOMissing = true },
			wantRule: delegatedHumanMergeAdoptDCORule, wantNote: "hive decided: adopt · DCO sign-off missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.Labels = append([]string(nil), base.Labels...)
			tc.mutate(&in)
			d, notes, ok := srv.delegatedHumanMergeDecision(in, req, "writer", true)
			if !ok || d.Rule != tc.wantRule {
				t.Fatalf("ok=%v decision=%+v, want rule %q", ok, d, tc.wantRule)
			}
			if len(notes) != 1 || notes[0] != tc.wantNote {
				t.Fatalf("notes = %q, want %q", notes, tc.wantNote)
			}
			if tc.wantText != "" && !strings.Contains(d.Text, tc.wantText) {
				t.Fatalf("text = %q, want it to contain %q", d.Text, tc.wantText)
			}
			for _, a := range d.Actions {
				if in.PersonAuthored() && a.Kind == delegatedecision.ActionAdoptSupersede {
					t.Fatalf("a person's PR must never carry an adopt action: %+v", d)
				}
			}
			if d.Rule == delegatedHumanMergeAdoptDCORule && d.Verdict != delegatedecision.VerdictAdoptSupersede {
				t.Fatalf("clanker DCO-less head should adopt: %+v", d)
			}
		})
	}
}
