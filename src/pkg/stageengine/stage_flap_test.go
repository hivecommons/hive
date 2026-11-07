package stageengine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A raw human opinion and its normalized remediation must not take turns
// winning just because the previous sweep recorded a different stage (#16880).
func TestNormalizedFixingIsIndependentOfPreviousStage(t *testing.T) {
	obs := stageObservation{Lane: stageLaneSelf, HeadSHA: "head", Detail: stageDetail{
		VerdictKnown: true,
		Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human",
			HumanSummaries: []string{"correctness requires human: field is never populated; add the producer."}},
	}}
	raw := stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true, Reason: StageReasonVerdictRequiresHuman}
	for _, initial := range []stagelabels.Stage{stagelabels.None, stagelabels.NeedsDecision, stagelabels.Fixing} {
		t.Run(initial.String(), func(t *testing.T) {
			prev := stagelabels.Record{Stage: initial, Reason: stagelabels.LaneReasonChangesRequested, ActivityHead: obs.HeadSHA}
			for sweep := 0; sweep < 3; sweep++ {
				got, ok := PreserveNormalizedFixingStage(prev, initial != stagelabels.None || sweep > 0, obs, raw)
				if !ok || got.Stage != stagelabels.Fixing || got.Human || got.Reason != stagelabels.LaneReasonChangesRequested {
					t.Fatalf("sweep %d: got %+v, %v", sweep, got, ok)
				}
				prev.Stage, prev.Reason = got.Stage, got.Reason
			}
		})
	}
	// An explicit policy question is not converted into author work, regardless
	// of a previous normalized fixing record.
	obs.Detail.Verdict.DecisionRequested = true
	obs.Detail.Verdict.DecisionQuestion = "May the relay cross this merge/trust boundary?"
	prev := stagelabels.Record{Stage: stagelabels.Fixing, Reason: stagelabels.LaneReasonChangesRequested, ActivityHead: "head"}
	for sweep := 0; sweep < 3; sweep++ {
		if got, ok := PreserveNormalizedFixingStage(prev, true, obs, raw); ok {
			t.Fatalf("policy decision normalized to %+v", got)
		}
		got, ok := RequiresHumanStage(obs, stageRankOwned)
		if !ok || got.Stage != stagelabels.NeedsDecision {
			t.Fatalf("policy decision = %+v, %v", got, ok)
		}
		prev.Stage, prev.Reason = got.Stage, got.Reason
	}
}

func TestPreserveNeedsDecisionOnSameHead(t *testing.T) {
	head := "687f4cb"
	obs := stageObservation{
		Lane:    stageLaneSelf,
		HeadSHA: head,
		Detail: stageDetail{
			VerdictKnown: true,
			Verdict: ContributorLaneVerdict{
				Configured:        true,
				Verdict:           "requires_human",
				DecisionRequested: true,
				DecisionQuestion:  "@hanthor should confirm pin-only merge",
			},
		},
	}
	prev := stagelabels.Record{
		Stage:        stagelabels.NeedsDecision,
		Reason:       StageReasonDecisionRequested,
		ActivityHead: head,
	}

	// 1. Same head with decision question preserves 085 even when raw verdict would be fixing (070) or review-pending (050).
	rawFixing := stageVerdict{Stage: stagelabels.Fixing, Rank: stageRankOwned, Reason: "check-failing:unit"}
	got, ok := PreserveNeedsDecisionOnSameHead(prev, true, obs, rawFixing)
	if !ok || got.Stage != stagelabels.NeedsDecision || got.Reason != prev.Reason {
		t.Fatalf("expected preserved 085, got %+v, ok=%v", got, ok)
	}

	rawPending := stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned, Reason: "required-check-pending:build"}
	got, ok = PreserveNeedsDecisionOnSameHead(prev, true, obs, rawPending)
	if !ok || got.Stage != stagelabels.NeedsDecision || got.Reason != prev.Reason {
		t.Fatalf("expected preserved 085 from pending, got %+v, ok=%v", got, ok)
	}

	// 2. A new push clears preservation.
	obsNewPush := obs
	obsNewPush.HeadSHA = "newhead999"
	if _, ok := PreserveNeedsDecisionOnSameHead(prev, true, obsNewPush, rawPending); ok {
		t.Fatal("expected new push to clear preservation")
	}

	// 3. A human approval clears preservation.
	obsApproved := obs
	obsApproved.Detail.HumanApprover = "hanthor"
	obsApproved.Detail.HumanApprovalHead = head
	if _, ok := PreserveNeedsDecisionOnSameHead(prev, true, obsApproved, rawPending); ok {
		t.Fatal("expected human approval to clear preservation")
	}

	// 4. A human change request clears preservation.
	obsChanges := obs
	obsChanges.Detail.HumanChangeRequester = "hanthor"
	obsChanges.Detail.HumanChangeRequestHead = head
	if _, ok := PreserveNeedsDecisionOnSameHead(prev, true, obsChanges, rawFixing); ok {
		t.Fatal("expected human change request to clear preservation")
	}

	// 5. Terminal verdict clears preservation.
	terminal := stageVerdict{Terminal: true, Stage: stagelabels.None}
	if _, ok := PreserveNeedsDecisionOnSameHead(prev, true, obs, terminal); ok {
		t.Fatal("expected terminal verdict to clear preservation")
	}

	// 6. A re-review of the same head that no longer asks for a decision
	// releases the hold: approve must reach 090, changes_requested 070.
	obsApprove := obs
	obsApprove.Detail.Verdict = ContributorLaneVerdict{Configured: true, Verdict: "approve", Approved: true}
	rawApproved := stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: stageRankOwned, Reason: "approved"}
	if got, ok := PreserveNeedsDecisionOnSameHead(prev, true, obsApprove, rawApproved); ok {
		t.Fatalf("expected same-head approve verdict to clear preservation, got %+v", got)
	}
	obsChangesVerdict := obs
	obsChangesVerdict.Detail.Verdict = ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}
	if got, ok := PreserveNeedsDecisionOnSameHead(prev, true, obsChangesVerdict, rawFixing); ok {
		t.Fatalf("expected same-head changes_requested verdict to clear preservation, got %+v", got)
	}

	// 7. No verdict for this head yet (the check re-ran because of the
	// hive's own label write): the recorded question still holds 085.
	obsNoVerdict := obs
	obsNoVerdict.Detail = stageDetail{}
	got, ok = PreserveNeedsDecisionOnSameHead(prev, true, obsNoVerdict, rawPending)
	if !ok || got.Stage != stagelabels.NeedsDecision || got.Reason != StageReasonDecisionRequested {
		t.Fatalf("expected recorded question to preserve 085 without a verdict, got %+v, ok=%v", got, ok)
	}
	// … but only when the record says a decision was requested.
	prevOther := prev
	prevOther.Reason = StageReasonVerdictRequiresHuman
	if got, ok := PreserveNeedsDecisionOnSameHead(prevOther, true, obsNoVerdict, rawPending); ok {
		t.Fatalf("expected no preservation without a recorded question, got %+v", got)
	}

	// 8. An explicit /hive admission on the same head releases the hold.
	prevAdmitted := prev
	prevAdmitted.AdmittedHead = head
	if got, ok := PreserveNeedsDecisionOnSameHead(prevAdmitted, true, obs, rawPending); ok {
		t.Fatalf("expected same-head admission to clear preservation, got %+v", got)
	}
	prevAdmitted.AdmittedHead = "olderhead"
	if _, ok := PreserveNeedsDecisionOnSameHead(prevAdmitted, true, obs, rawPending); !ok {
		t.Fatal("expected an admission for another head to leave the hold in place")
	}
}

// Store's per-head transition dampener refuses the 4th move between two
// stages within 30 minutes; Reconcile must report that as a suppressed
// transition at the previous stage and write nothing (#16974).
func TestReconcilerReportsStoreDampenerSuppression(t *testing.T) {
	now := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return now })
	writer := newFakeWriter()
	stage := stagelabels.NeedsDecision
	r := New(Options{Store: store, Writer: writer, Now: func() time.Time { return now },
		PR: DeriverFunc(stagelabels.PRItem, func(Facts) Decision {
			return Decision{Stage: stage, Reason: "decision-requested"}
		}),
	})
	f := Facts{Ref: Ref{Owner: "acme", Repo: "widgets", Number: 42, Kind: stagelabels.PRItem}, Open: true,
		PR: &PRFacts{HeadSHA: "head", Pass: &PRPassFacts{}}}
	// Toggling the check summary is fresh evidence, so the reconciler's
	// own write guard lets each reversal through; only the Store dampens.
	sweep := func(want stagelabels.Stage) Transition {
		now = now.Add(time.Minute)
		stage = want
		if want == stagelabels.Fixing {
			f.PR.Checks = stagelabels.CheckSummary{Known: true, Total: 1, Red: []string{"unit"}}
		} else {
			f.PR.Checks = stagelabels.CheckSummary{Known: true, Total: 1, Green: 1}
		}
		tr, err := r.Reconcile(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	store.ObserveHead(f.Ref.Key(), "head", stagelabels.Fixing, "check-failing:unit")
	writer.labels[f.Ref.Key()] = []string{stagelabels.Fixing.Label()}
	for i, want := range []stagelabels.Stage{stagelabels.NeedsDecision, stagelabels.Fixing, stagelabels.NeedsDecision} {
		if tr := sweep(want); tr.Outcome.Suppressed || tr.To != want {
			t.Fatalf("move %d: %+v", i, tr)
		}
	}
	// Three moves of the {070, 085} pair are booked. Falling back into
	// fixing is never dampened …
	if tr := sweep(stagelabels.Fixing); tr.Outcome.Suppressed || tr.To != stagelabels.Fixing {
		t.Fatalf("move into fixing: %+v", tr)
	}
	writes := writer.Writes
	// … but the climb back to 085 is the pair's 5th move in five minutes.
	tr := sweep(stagelabels.NeedsDecision)
	if !tr.Outcome.Suppressed || tr.From != stagelabels.Fixing || tr.To != stagelabels.Fixing || tr.Outcome.Changed() {
		t.Fatalf("dampened: %+v", tr)
	}
	if writer.Writes != writes {
		t.Fatalf("writer applied a dampened transition: writes=%d, want %d", writer.Writes, writes)
	}
	if rec, _ := store.Get(f.Ref.Key()); rec.Stage != stagelabels.Fixing {
		t.Fatalf("store=%s, want fixing", rec.Stage)
	}
}
func TestRequiresHumanReconcilesToFixedPoint(t *testing.T) {
	for _, decisionRequested := range []bool{false, true} {
		obs := stageObservation{Owner: "acme", Repo: "widgets", Number: 42, Lane: stageLaneSelf, HeadSHA: "head", AuthorKind: authorkind.Hive,
			Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{VerdictKnown: true,
				Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: decisionRequested,
					HumanSummaries: []string{"correctness requires human: field is never populated; add the producer."}},
			}}
		f := FactsFromPRObservations([]stageObservation{obs})
		f.PR.Pass = &PRPassFacts{AuthorKind: authorkind.Hive}
		writer := newFakeWriter()
		store := stagelabels.NewStore("", nil)
		r := New(Options{Store: store, Writer: writer})
		want := stagelabels.Fixing
		if decisionRequested {
			want = stagelabels.NeedsDecision
		}
		for sweep := 0; sweep < 3; sweep++ {
			tr, err := r.Reconcile(context.Background(), f)
			if err != nil || tr.To != want || tr.Outcome.Suppressed {
				t.Fatalf("decisionRequested=%v sweep=%d: %+v, %v", decisionRequested, sweep, tr, err)
			}
			if sweep > 0 && tr.Outcome.Changed() {
				t.Fatalf("idle sweep %d changed labels: %+v", sweep, tr)
			}
			rec, _ := store.Get(f.Ref.Key())
			f.PR.Pass.Prior = &PriorStage{Stage: rec.Stage, Reason: rec.Reason, ActivityHead: obs.HeadSHA}
			f.Labels = writer.stageLabels(f.Ref)
		}
	}
}

func TestReconcilerSuppressesSameHeadStageReversal(t *testing.T) {
	for _, release := range []string{"timeout", "head", "terminal", "checks", "conflict", "review", "adoption", "escalation", "optional-check"} {
		t.Run(release, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
			store := stagelabels.NewStore("", nil)
			writer := newFakeWriter()
			var logs bytes.Buffer
			stage := stagelabels.NeedsDecision
			terminal := false
			r := New(Options{Store: store, Writer: writer, Now: func() time.Time { return now },
				Logger: slog.New(slog.NewTextHandler(&logs, nil)),
				PR: DeriverFunc(stagelabels.PRItem, func(Facts) Decision {
					return Decision{Stage: stage, Reason: "review evidence", Terminal: terminal}
				}),
			})
			f := Facts{Ref: Ref{Owner: "acme", Repo: "widgets", Number: 42, Kind: stagelabels.PRItem}, Open: true, PR: &PRFacts{HeadSHA: "head", Pass: &PRPassFacts{AdoptionLive: true},
				Lanes: []LaneFact{{Lane: "contributor", Reason: "initial opinion"}}}}
			store.Observe(f.Ref.Key(), stagelabels.Fixing, "initial")
			writer.labels[f.Ref.Key()] = []string{stagelabels.Fixing.Label()}
			if _, err := r.Reconcile(context.Background(), f); err != nil {
				t.Fatal(err)
			}
			stage = stagelabels.Fixing
			for sweep := 0; sweep < 3; sweep++ {
				now = now.Add(time.Minute)
				// ApplyStageLabels pre-observes its legacy opinion before
				// invoking the engine; that must not defeat the write guard.
				store.Observe(f.Ref.Key(), stage, "legacy opinion")
				f.PR.Pass.Prior = &PriorStage{Stage: stage, Reason: "legacy opinion"}
				f.PR.Pass.Stalled = true
				f.PR.Lanes[0].Reason = "reversed opinion"
				f.PR.SoakElapsed += time.Minute
				tr, err := r.Reconcile(context.Background(), f)
				if err != nil || !tr.Outcome.Suppressed || tr.To != stagelabels.NeedsDecision || tr.Outcome.Changed() {
					t.Fatalf("sweep %d: %+v, %v", sweep, tr, err)
				}
				rec, _ := store.Get(f.Ref.Key())
				if rec.Stage != stagelabels.NeedsDecision || writer.Writes != 1 {
					t.Fatalf("store=%s writes=%d", rec.Stage, writer.Writes)
				}
			}
			if !strings.Contains(logs.String(), "stage flap suppressed") {
				t.Fatal("missing suppression diagnostic")
			}
			switch release {
			case "timeout":
				// Exactly 15 minutes from the successful write, not the last
				// suppressed attempt: an idle sweep cannot extend the hold.
				now = now.Add(12 * time.Minute)
			case "head":
				f.PR.HeadSHA = "new-head"
			case "terminal":
				terminal = true
			case "checks":
				f.PR.Checks = stagelabels.CheckSummary{Known: true, Total: 1, Green: 1}
			case "conflict":
				f.PR.Conflicting = true
			case "review":
				f.PR.Review.HumanApprover = "maintainer"
			case "adoption":
				f.PR.Pass.AdoptionLive = false
			case "escalation":
				f.PR.Pass.EscalationReason = "adoption-stalled:rebase-failed-twice"
			case "optional-check":
				f.PR.Lanes[0].RedChecks = []string{"unit"}
				f.PR.Lanes[0].RedChecksHead = "head"
			}
			tr, err := r.Reconcile(context.Background(), f)
			if err != nil || tr.Outcome.Suppressed || writer.Writes != 2 {
				t.Fatalf("release: %+v, %v, writes=%d", tr, err, writer.Writes)
			}
		})
	}
}
