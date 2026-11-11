package automerge

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

func workflowTestProfile(t *testing.T, labels ...workflowmodel.LabelEntry) *workflowmodel.Profile {
	t.Helper()
	b, err := os.ReadFile("../../workflowmodel/testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m.Spec.Labels = workflowmodel.LabelSet{Prefix: "docs", Entries: labels}
	p, _, err := workflowmodel.Compile(&m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newWorkflowStageEngine is newStageEngine with a WorkflowFor lookup the
// test controls per call.
func newWorkflowStageEngine(t *testing.T, apiURL string, lookup func(owner, repo string) *workflowmodel.Profile) (*Engine, *StageTracker) {
	t.Helper()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	tracker := NewStageTracker()
	e := New(client, Options{
		Now:          clock.now,
		StageTracker: tracker,
		StageEngine:  &stageengine.Options{WorkflowFor: lookup},
	})
	return e, tracker
}

func TestEnsureStageLabelsUnboundRepoIsUnchanged(t *testing.T) {
	ctx := context.Background()
	for name, lookup := range map[string]func(owner, repo string) *workflowmodel.Profile{
		"no lookup":    nil,
		"unbound":      func(string, string) *workflowmodel.Profile { return nil },
		"hive-default": func(string, string) *workflowmodel.Profile { return hivedefault.Profile() },
		"other repo": func(_, repo string) *workflowmodel.Profile {
			if repo == "other" {
				return workflowTestProfile(t, workflowmodel.LabelEntry{Name: "docs/x"})
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &stageAPI{labels: map[string]string{"bug": "ff0000"}}
			srv := newStageAPI(t, api)
			defer srv.Close()
			e, tracker := newWorkflowStageEngine(t, srv.URL, lookup)
			if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
				t.Fatal(err)
			}
			if len(api.creates) != len(stagelabels.AllLabelDefs()) || api.lists != 1 || len(api.edits) != 0 {
				t.Fatalf("creates=%d lists=%d edits=%v", len(api.creates), api.lists, api.edits)
			}
			for _, c := range api.creates {
				if c == "docs/x" {
					t.Fatalf("model label leaked into an unbound repo: %v", api.creates)
				}
			}
			// memoised: a second call does nothing
			if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil || api.lists != 1 {
				t.Fatalf("second call: err=%v lists=%d", err, api.lists)
			}
		})
	}
}

func TestEnsureStageLabelsProvisionsBoundModelAndReprovisionsOnRebind(t *testing.T) {
	ctx := context.Background()
	api := &stageAPI{labels: map[string]string{"docs/020-analysing": "000000"}}
	srv := newStageAPI(t, api)
	defer srv.Close()

	v1 := workflowTestProfile(t,
		workflowmodel.LabelEntry{Name: "docs/010-requested", State: "requested"},
		workflowmodel.LabelEntry{Name: "docs/020-analysing", Color: "123456", Description: "analysing", State: "analysing"},
	)
	current := v1
	e, tracker := newWorkflowStageEngine(t, srv.URL, func(string, string) *workflowmodel.Profile { return current })

	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
		t.Fatal(err)
	}
	if api.lists != 1 || !slices.Contains(api.creates, "docs/010-requested") || len(api.creates) != len(stagelabels.AllLabelDefs())+1 {
		t.Fatalf("lists=%d creates=%v", api.lists, api.creates)
	}
	if api.labels["docs/010-requested"] != stagelabels.StageColor || api.labels["docs/020-analysing"] != "123456" || !slices.Contains(api.edits, "docs/020-analysing=123456") {
		t.Fatalf("labels=%v edits=%v", api.labels, api.edits)
	}
	if tracker.ensuredWorkflow["acme/widget"] != v1.Hash {
		t.Fatalf("memo %q", tracker.ensuredWorkflow["acme/widget"])
	}

	// same binding: memoised, no second list
	api.reset()
	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil || api.lists != 1 || api.writes() != 0 || len(api.creates)+len(api.edits) != 0 {
		t.Fatalf("idempotent: err=%v lists=%d writes=%d creates=%v edits=%v", err, api.lists, api.writes(), api.creates, api.edits)
	}

	// rebind to a new version with one more label: re-list, create only the new one
	current = workflowTestProfile(t,
		workflowmodel.LabelEntry{Name: "docs/010-requested", State: "requested"},
		workflowmodel.LabelEntry{Name: "docs/020-analysing", Color: "123456", Description: "analysing", State: "analysing"},
		workflowmodel.LabelEntry{Name: "docs/045-analysis-review", State: "analysis-review"},
	)
	if current.Hash == v1.Hash {
		t.Fatal("fixture: hash did not change")
	}
	api.reset()
	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
		t.Fatal(err)
	}
	if api.lists != 2 || !slices.Equal(api.creates, []string{"docs/045-analysis-review"}) || len(api.edits) != 0 {
		t.Fatalf("rebind: lists=%d creates=%v edits=%v", api.lists, api.creates, api.edits)
	}
	if tracker.ensuredWorkflow["acme/widget"] != current.Hash {
		t.Fatalf("memo %q", tracker.ensuredWorkflow["acme/widget"])
	}

	// unbind: re-list once (hash changes to ""), nothing to create; model labels are left in place
	current = nil
	api.reset()
	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil || api.lists != 3 || len(api.creates)+len(api.edits) != 0 {
		t.Fatalf("unbind: err=%v lists=%d creates=%v edits=%v", err, api.lists, api.creates, api.edits)
	}
	if _, ok := api.labels["docs/045-analysis-review"]; !ok {
		t.Fatal("unbind deleted a model label")
	}
}
