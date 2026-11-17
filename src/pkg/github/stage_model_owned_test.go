package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// modelOwnedCycles is how many enumeration cycles the flap regression test
// replays: enough for a strip-then-restore loop to show twice.
const modelOwnedCycles = 4

func techdocsModelForStage(t *testing.T) *workflowmodel.Profile {
	t.Helper()
	b, err := os.ReadFile("../workflowmodel/testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	p, diags, err := workflowmodel.Compile(&m)
	if err != nil {
		t.Fatalf("%v %+v", err, diags)
	}
	return p
}

// hivecommons/hive#18434: on a repository bound to a workflow model whose
// state derives stage/020-claimed (TechDocs "accepted"), the in-progress
// sweep stripped the label for want of a start mark and the stage sync put
// it back, every cycle. Replaying sweep + stage reconcile must converge:
// one add on the first cycle, then no add or remove at all, and the issue
// is never withheld as claimed.
func TestModelOwnedStageLabel_ReconcileIsIdempotent(t *testing.T) {
	p := techdocsModelForStage(t)
	lookup := func(owner, repo string) *workflowmodel.Profile { return p }

	server, removed := sweepServer(t, []string{"a human comment, no claim marker"})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableInProgress(c, "hive-a", 2*time.Hour)
	c.SetStageWorkflowFor(lookup)

	ref := stageengine.Ref{Owner: "acme", Repo: "widgets", Number: 7, Kind: stagelabels.IssueItem}
	w := stageenginetest.NewWriter()
	w.Seed(ref, "techdocs/request", "techdocs/accepted")
	r := stageengine.New(stageengine.Options{
		Mode:        stageengine.ModeEnforce,
		Store:       stagelabels.NewStore("", nil),
		Writer:      w,
		WorkflowFor: lookup,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	for cycle := 0; cycle < modelOwnedCycles; cycle++ {
		issue := claimIssue()
		issue.Labels = w.LabelsOf(ref)
		result := &ActionableResult{Issues: IssueResultFromItems([]Issue{issue})}
		if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, nil); withheld != 0 {
			t.Fatalf("cycle %d: a model state is not a claim, withheld = %d", cycle, withheld)
		}
		if len(result.Issues.Items) != 1 {
			t.Fatalf("cycle %d: issue must stay in the actionable set, got %+v", cycle, result.Issues.Items)
		}
		if len(*removed) != 0 {
			t.Fatalf("cycle %d: in-progress sweep stripped the model's stage label: %v", cycle, *removed)
		}
		f := issueFactsFrom(issueStageInputFrom(result.Issues.Items[0]), func(string, int) stagelabels.Stage { return stagelabels.None })
		f.Ref = ref
		tr, err := r.Reconcile(context.Background(), f)
		if err != nil {
			t.Fatalf("cycle %d: reconcile: %v", cycle, err)
		}
		if tr.To != stagelabels.Claimed {
			t.Fatalf("cycle %d: model state accepted derives %s, want %s", cycle, tr.To, stagelabels.Claimed)
		}
		if cycle > 0 && (len(tr.Outcome.Added) > 0 || len(tr.Outcome.Removed) > 0) {
			t.Fatalf("cycle %d: label churn: added %v removed %v", cycle, tr.Outcome.Added, tr.Outcome.Removed)
		}
	}
	if !issueHasLabel(w.LabelsOf(ref), stagelabels.Claimed.Label()) {
		t.Fatalf("labels = %v, want the model's stage label kept", w.LabelsOf(ref))
	}
}

// Off the model (no binding, or the issue is in a state whose label is not
// the trigger), the sweep clears a stage/020-claimed with no live mark
// exactly as before.
func TestModelOwnedStageLabel_UnboundOrOtherStateStillClears(t *testing.T) {
	p := techdocsModelForStage(t)
	cases := []struct {
		name   string
		lookup func(owner, repo string) *workflowmodel.Profile
		labels []string
	}{
		{"unbound", func(string, string) *workflowmodel.Profile { return nil }, []string{"techdocs/accepted", InProgressLabel}},
		{"model state is not claimed", func(string, string) *workflowmodel.Profile { return p }, []string{"techdocs/request", stagelabels.Claimed.Label()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, removed := sweepServer(t, []string{"just a human comment"})
			c := newTestClient(t, server, "acme", []string{"widgets"})
			enableInProgress(c, "hive-a", 2*time.Hour)
			c.SetStageWorkflowFor(tc.lookup)
			issue := claimIssue()
			issue.Labels = tc.labels
			result := &ActionableResult{Issues: IssueResultFromItems([]Issue{issue})}
			if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, nil); withheld != 0 {
				t.Fatalf("withheld = %d, want 0", withheld)
			}
			if len(*removed) != 1 {
				t.Fatalf("expected the lapsed label removed once, got %v", *removed)
			}
		})
	}
}
