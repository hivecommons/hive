package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
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

// --- label garbage collection (RFC phase 4 follow-up) -------------------

// workflowGCAPI extends stageAPI with the two routes the collector uses:
// GET /search/issues (open items wearing a label, from inUse) and DELETE
// /repos/acme/widget/labels/<name>. searchErr makes every search fail.
type workflowGCAPI struct {
	inUse     map[string]int
	searchErr bool
	deleted   []string
	searches  []string
}

func (g *workflowGCAPI) install(api *stageAPI) {
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			g.searches = append(g.searches, q)
			if g.searchErr {
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
			total := 0
			for name, n := range g.inUse {
				if strings.Contains(q, `label:"`+name+`"`) {
					total = n
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "incomplete_results": false, "items": []any{}})
			return true
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/labels/"):
			name, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/labels/"))
			delete(api.labels, name)
			g.deleted = append(g.deleted, name)
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	}
}

// newWorkflowGCEngine is newWorkflowStageEngine with a provisioned-label
// store on a temp dir and the label_gc knob under test control.
func newWorkflowGCEngine(t *testing.T, apiURL string, lookup func(owner, repo string) *workflowmodel.Profile, store workflowmodel.ProvisionedStore, gc func() bool) (*Engine, *StageTracker, *[]hgithub.AuditRecord) {
	t.Helper()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	audits := &[]hgithub.AuditRecord{}
	client.SetAttributionAuditRecord(func(rec hgithub.AuditRecord) { *audits = append(*audits, rec) })
	tracker := NewStageTracker()
	e := New(client, Options{
		Now:          clock.now,
		StageTracker: tracker,
		StageEngine:  &stageengine.Options{WorkflowFor: lookup, WorkflowProvisioned: store, WorkflowLabelGC: gc},
	})
	return e, tracker, audits
}

func TestEnsureStageLabelsGCRemovesStaleKeepsInUseBuiltinAndForeign(t *testing.T) {
	ctx := context.Background()
	api := &stageAPI{labels: map[string]string{"docs/legacy": "000000"}}
	gcAPI := &workflowGCAPI{inUse: map[string]int{"docs/030-drafting": 2}}
	gcAPI.install(api)
	srv := newStageAPI(t, api)
	defer srv.Close()
	store := wfstore.Open(t.TempDir())

	v1 := workflowTestProfile(t,
		workflowmodel.LabelEntry{Name: "docs/010-requested", State: "requested"},
		workflowmodel.LabelEntry{Name: "docs/020-analysing", State: "analysing"},
		workflowmodel.LabelEntry{Name: "docs/030-drafting", State: "drafting"},
		workflowmodel.LabelEntry{Name: stagelabels.NeedsDecision.Label(), State: "analysis-review"},
	)
	current := v1
	e, tracker, audits := newWorkflowGCEngine(t, srv.URL, func(string, string) *workflowmodel.Profile { return current }, store, nil)

	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
		t.Fatal(err)
	}
	rec, err := store.LoadProvisioned("acme/widget")
	if err != nil || rec == nil {
		t.Fatalf("record after bind: %v %v", rec, err)
	}
	if rec.Name != v1.Name || rec.Hash != v1.Hash || !slices.Equal(rec.Labels, []string{"docs/010-requested", "docs/020-analysing", "docs/030-drafting", stagelabels.NeedsDecision.Label()}) {
		t.Fatalf("record %+v", rec)
	}
	if len(gcAPI.deleted) != 0 || len(gcAPI.searches) != 0 {
		t.Fatalf("first bind must not search or delete: %v %v", gcAPI.searches, gcAPI.deleted)
	}

	// Rebind to a version that drops 020 (unused → removed), 030 (worn by
	// two open items → kept) and the built-in 045 spelling (never deleted).
	current = workflowTestProfile(t, workflowmodel.LabelEntry{Name: "docs/010-requested", State: "requested"})
	api.reset()
	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gcAPI.deleted, []string{"docs/020-analysing"}) {
		t.Fatalf("deleted %v", gcAPI.deleted)
	}
	if _, ok := api.labels["docs/030-drafting"]; !ok {
		t.Fatal("in-use label deleted")
	}
	if _, ok := api.labels[stagelabels.NeedsDecision.Label()]; !ok {
		t.Fatal("built-in label deleted")
	}
	if _, ok := api.labels["docs/legacy"]; !ok {
		t.Fatal("foreign label deleted")
	}
	if len(gcAPI.searches) != 2 {
		t.Fatalf("searches %v (built-in and foreign labels must not be searched)", gcAPI.searches)
	}
	rec, _ = store.LoadProvisioned("acme/widget")
	if rec == nil || rec.Hash != current.Hash || !slices.Equal(rec.Labels, []string{"docs/010-requested", "docs/030-drafting"}) {
		t.Fatalf("record after rebind %+v", rec)
	}
	if len(*audits) != 1 || (*audits)[0].Action != hgithub.AuditActionWorkflowLabelRemoved || (*audits)[0].Repo != "acme/widget" || !strings.Contains((*audits)[0].Detail, "label=docs/020-analysing") || !strings.Contains((*audits)[0].Detail, "workflow="+current.Name+"@"+current.Version) {
		t.Fatalf("audits %+v", *audits)
	}

	// Unbind: 010 goes, 030 is still worn so the record survives with it.
	current = nil
	gcAPI.deleted = nil
	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gcAPI.deleted, []string{"docs/010-requested"}) {
		t.Fatalf("unbind deleted %v", gcAPI.deleted)
	}
	rec, _ = store.LoadProvisioned("acme/widget")
	if rec == nil || rec.Name != "" || !slices.Equal(rec.Labels, []string{"docs/030-drafting"}) {
		t.Fatalf("record after unbind %+v", rec)
	}
	if !strings.Contains((*audits)[1].Detail, "workflow=unbound") {
		t.Fatalf("unbind audit %+v", (*audits)[1])
	}

	// A new process (fresh tracker) on the unbound repo retries the kept
	// label once nothing wears it, then drops the record.
	delete(gcAPI.inUse, "docs/030-drafting")
	gcAPI.deleted = nil
	fresh := NewStageTracker()
	if err := e.ensureStageLabels(ctx, "acme", "widget", fresh, authorkind.DefaultNames); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gcAPI.deleted, []string{"docs/030-drafting"}) {
		t.Fatalf("restart deleted %v", gcAPI.deleted)
	}
	if rec, _ = store.LoadProvisioned("acme/widget"); rec != nil {
		t.Fatalf("record should be gone, got %+v", rec)
	}
	if _, ok := api.labels["docs/legacy"]; !ok || len(api.labels) != len(stagelabels.AllLabelDefs())+1 {
		t.Fatalf("labels left: %v", api.labels)
	}
}

func TestEnsureStageLabelsGCHonoursKnobAndSearchFailure(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		gc        bool
		searchErr bool
	}{
		"knob off":      {gc: false},
		"search failed": {gc: true, searchErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			api := &stageAPI{}
			gcAPI := &workflowGCAPI{searchErr: tc.searchErr}
			gcAPI.install(api)
			srv := newStageAPI(t, api)
			defer srv.Close()
			store := wfstore.Open(t.TempDir())
			current := workflowTestProfile(t, workflowmodel.LabelEntry{Name: "docs/020-analysing", State: "analysing"})
			e, tracker, audits := newWorkflowGCEngine(t, srv.URL, func(string, string) *workflowmodel.Profile { return current }, store, func() bool { return tc.gc })
			if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
				t.Fatal(err)
			}
			current = nil
			if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
				t.Fatal(err)
			}
			if len(gcAPI.deleted) != 0 || len(*audits) != 0 {
				t.Fatalf("deleted %v audits %v", gcAPI.deleted, *audits)
			}
			if _, ok := api.labels["docs/020-analysing"]; !ok {
				t.Fatal("label deleted")
			}
			if tc.gc != (len(gcAPI.searches) == 1) {
				t.Fatalf("knob off must not search: %v", gcAPI.searches)
			}
			rec, _ := store.LoadProvisioned("acme/widget")
			if rec == nil || !slices.Equal(rec.Labels, []string{"docs/020-analysing"}) {
				t.Fatalf("kept label must stay remembered: %+v", rec)
			}
		})
	}
}

// Without a provisioned store nothing is remembered and nothing deleted —
// the pre-P4-follow-up behaviour TestEnsureStageLabelsProvisionsBoundModelAndReprovisionsOnRebind
// pins; an unbound repository with no record makes no extra call either.
func TestEnsureStageLabelsGCNoStoreOrRecordIsInert(t *testing.T) {
	ctx := context.Background()
	api := &stageAPI{}
	gcAPI := &workflowGCAPI{}
	gcAPI.install(api)
	srv := newStageAPI(t, api)
	defer srv.Close()
	e, tracker, _ := newWorkflowGCEngine(t, srv.URL, func(string, string) *workflowmodel.Profile { return nil }, wfstore.Open(t.TempDir()), nil)
	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil {
		t.Fatal(err)
	}
	if len(gcAPI.searches)+len(gcAPI.deleted) != 0 || api.lists != 1 {
		t.Fatalf("unbound repo touched GC routes: %v %v", gcAPI.searches, gcAPI.deleted)
	}
}

// EnsureStageLabels is the dashboard's on-demand entry into the same
// memoised path (PUT /api/workflows/enabled turning derivation on).
func TestEnsureStageLabelsExportedUsesEngineTrackerAndPolicy(t *testing.T) {
	ctx := context.Background()
	api := &stageAPI{labels: map[string]string{}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	profile := workflowTestProfile(t, workflowmodel.LabelEntry{Name: "docs/010-requested", State: "requested"})
	e, tracker := newWorkflowStageEngine(t, srv.URL, func(string, string) *workflowmodel.Profile { return profile })

	if err := e.EnsureStageLabels(ctx, "acme", "widget"); err != nil {
		t.Fatal(err)
	}
	if api.lists != 1 || !slices.Contains(api.creates, "docs/010-requested") || tracker.ensuredWorkflow["acme/widget"] != profile.Hash {
		t.Fatalf("lists=%d creates=%v memo=%q", api.lists, api.creates, tracker.ensuredWorkflow["acme/widget"])
	}
	// The sweep's own path then finds the memo and does nothing.
	if err := e.ensureStageLabels(ctx, "acme", "widget", tracker, authorkind.DefaultNames); err != nil || api.lists != 1 {
		t.Fatalf("sweep after dashboard provisioning: err=%v lists=%d", err, api.lists)
	}

	var nilEngine *Engine
	if err := nilEngine.EnsureStageLabels(ctx, "acme", "widget"); err == nil {
		t.Fatal("nil engine must report an error")
	}
	if err := New(nil, Options{}).EnsureStageLabels(ctx, "acme", "widget"); err == nil {
		t.Fatal("engine without a transport must report an error")
	}
}
