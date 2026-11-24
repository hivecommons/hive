package curated_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/hub/marketplace/curated"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
)

// TestLoadEmbedded is the gate every curated file must pass: strict
// decode, provenance curated, a version, zero compile diagnostics of any
// severity, nothing left inferred, and a clean bundle export at tier
// curated.
func TestLoadEmbedded(t *testing.T) {
	entries, errs := curated.Load()
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	if len(entries) == 0 {
		t.Fatal("no curated models embedded")
	}
	seen := map[string]bool{}
	for _, e := range entries {
		m := e.Model
		key := m.Metadata.Name + "@" + m.Spec.Meta.Version
		if seen[key] {
			t.Fatalf("%s: duplicate %s", e.Path, key)
		}
		seen[key] = true
		if m.Spec.Meta.Provenance != workflowmodel.ProvenanceCurated {
			t.Fatalf("%s: provenance %q", e.Path, m.Spec.Meta.Provenance)
		}
		if n := workflowmodel.Inferred(m); n != 0 {
			t.Fatalf("%s: %d inferred elements unconfirmed", e.Path, n)
		}
		_, diags, err := workflowmodel.Compile(m)
		if err != nil || len(diags) != 0 {
			t.Fatalf("%s: compile err=%v diags=%+v", e.Path, err, diags)
		}
		b, err := bundle.Export(m, "test", testNow)
		if err != nil {
			t.Fatalf("%s: export: %v", e.Path, err)
		}
		if b.Tier != bundle.TierCurated {
			t.Fatalf("%s: tier %q", e.Path, b.Tier)
		}
	}
	for _, key := range []string{techdocsKey2026_11_1, techdocsKey2026_11_2, techdocsKey2026_11_3} {
		if !seen[key] {
			t.Fatalf("techdocs model %s missing: %v", key, seen)
		}
	}
}

const (
	techdocsKey2026_11_1 = "cncf-techdocs-assessment@curated-2026.11.1"
	techdocsKey2026_11_2 = "cncf-techdocs-assessment@curated-2026.11.2"
	techdocsKey2026_11_3 = "cncf-techdocs-assessment@curated-2026.11.3"
)

// techdocsVersion returns the embedded cncf-techdocs-assessment model at one
// spec.meta.version.
func techdocsVersion(t *testing.T, version string) *workflowmodel.Model {
	t.Helper()
	entries, errs := curated.Load()
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, e := range entries {
		if e.Model.Metadata.Name == "cncf-techdocs-assessment" && e.Model.Spec.Meta.Version == version {
			return e.Model
		}
	}
	t.Fatalf("cncf-techdocs-assessment@%s not embedded", version)
	return nil
}

// TestTechDocsIntakeHook pins hivecommons/hive#18139 step 4: curated-2026.11.2
// declares the intake hook with the rendered assessment-request.yml headings,
// curated-2026.11.1 stays as first published and declares nothing, and the
// two versions differ in nothing else.
func TestTechDocsIntakeHook(t *testing.T) {
	v1 := techdocsVersion(t, "curated-2026.11.1")
	v2 := techdocsVersion(t, "curated-2026.11.2")
	if v1.Spec.Intake.DerivesReferenceRepos() || len(v1.Spec.Intake.OutOfScopeFields) != 0 {
		t.Fatalf("2026.11.1 must declare no intake fields (never edited in place): %+v", v1.Spec.Intake)
	}
	if !v2.Spec.Intake.DerivesReferenceRepos() {
		t.Fatalf("2026.11.2 must declare intake.referenceRepoFields: %+v", v2.Spec.Intake)
	}
	wantRefs := []string{"Website or documentation repository", "Other repositories or sites in scope"}
	wantOut := []string{"Out of scope"}
	if !reflect.DeepEqual(v2.Spec.Intake.ReferenceRepoFields, wantRefs) || !reflect.DeepEqual(v2.Spec.Intake.OutOfScopeFields, wantOut) {
		t.Fatalf("2026.11.2 intake = %+v; want refs %v out %v", v2.Spec.Intake, wantRefs, wantOut)
	}
	if _, ok := v2.Metadata.Annotations["workflow.hivecommons.dev/changes"]; !ok {
		t.Fatal("2026.11.2 must carry a workflow.hivecommons.dev/changes annotation")
	}
	// Everything but the version, the changes annotation and the intake block
	// is identical, so a hive moving from 11.1 to 11.2 changes nothing in its
	// lifecycle.
	a, b := *v1, *v2
	a.Spec.Meta.Version, b.Spec.Meta.Version = "", ""
	a.Spec.Intake, b.Spec.Intake = workflowmodel.Intake{}, workflowmodel.Intake{}
	a.Metadata.Annotations, b.Metadata.Annotations = nil, nil
	if a.Hash() != b.Hash() {
		t.Fatal("2026.11.2 must differ from 2026.11.1 only in version, annotations and intake")
	}
	if !reflect.DeepEqual(a.Metadata, b.Metadata) {
		t.Fatalf("metadata differs beyond annotations:\n%+v\n%+v", a.Metadata, b.Metadata)
	}
	// The rules evaluator derives the same stages from both versions.
	for _, m := range []*workflowmodel.Model{v1, v2} {
		p, diags, err := workflowmodel.Compile(m)
		if err != nil || len(diags) != 0 {
			t.Fatalf("%s: compile err=%v diags=%+v", m.Spec.Meta.Version, err, diags)
		}
		got := stageengine.DeriveFromModel(p, stagelabels.IssueItem).Derive(issue("triage/accepted"))
		if got.Abstain || got.Stage != stagelabels.Claimed {
			t.Fatalf("%s: accepted issue derived %+v", m.Spec.Meta.Version, got)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want error
	}{
		{"bad yaml", "a: [", nil},
		{"unknown field", "apiVersion: workflow.hivecommons.dev/v1alpha1\nkind: WorkflowModel\nbogus: 1\n", nil},
		{"not curated", "apiVersion: workflow.hivecommons.dev/v1alpha1\nkind: WorkflowModel\nspec: {meta: {version: v1, provenance: ingested}}\n", curated.ErrNotCurated},
		{"no version", "apiVersion: workflow.hivecommons.dev/v1alpha1\nkind: WorkflowModel\nspec: {meta: {provenance: curated}}\n", curated.ErrNoVersion},
		{"inferred", "apiVersion: workflow.hivecommons.dev/v1alpha1\nkind: WorkflowModel\nspec: {meta: {version: v1, provenance: curated}, states: [{id: x, lifecycle: {confidence: inferred}}]}\n", curated.ErrInferred},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := curated.Decode([]byte(tc.doc))
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}
}

// techdocs compiles the latest embedded cncf-techdocs-assessment version
// (curated-2026.11.3);
// TestTechDocsIntakeHook pins that every version derives the same stages.
func techdocs(t *testing.T) *workflowmodel.Profile {
	t.Helper()
	p, _, err := workflowmodel.Compile(techdocsVersion(t, "curated-2026.11.3"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTechDocsStageLabels pins hivecommons/hive#19611: curated-2026.11.3
// spells the four states whose built-in label says the wrong thing in their
// own slug, in the same number band; everything else is curated-2026.11.2,
// so the stages derived (and everything keyed on the number) are unchanged
// and only the label the writer puts on the item differs.
func TestTechDocsStageLabels(t *testing.T) {
	v2 := techdocsVersion(t, "curated-2026.11.2")
	v3 := techdocsVersion(t, "curated-2026.11.3")
	want := map[string]string{
		"accepted":           "stage/020-accepted",
		"drafting":           "stage/070-drafting",
		"in-review":          "stage/050-reviewer-check",
		"stakeholder-review": "stage/060-stakeholder-review",
	}
	got := map[string]string{}
	for _, st := range v3.Spec.States {
		if st.Lifecycle.StageLabel != "" {
			got[st.ID] = st.Lifecycle.StageLabel
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("2026.11.3 stage labels = %v, want %v", got, want)
	}
	for _, st := range v2.Spec.States {
		if st.Lifecycle.StageLabel != "" {
			t.Fatalf("2026.11.2 must declare no stage labels (never edited in place): %s %q", st.ID, st.Lifecycle.StageLabel)
		}
	}
	if v3.Metadata.Annotations["workflow.hivecommons.dev/changes"] == v2.Metadata.Annotations["workflow.hivecommons.dev/changes"] {
		t.Fatal("2026.11.3 must carry its own workflow.hivecommons.dev/changes annotation")
	}
	a, b := workflowmodel.Clone(v2), workflowmodel.Clone(v3)
	a.Spec.Meta.Version, b.Spec.Meta.Version = "", ""
	a.Metadata.Annotations, b.Metadata.Annotations = nil, nil
	for i := range b.Spec.States {
		b.Spec.States[i].Lifecycle.StageLabel = ""
	}
	if a.Hash() != b.Hash() {
		t.Fatal("2026.11.3 must differ from 2026.11.2 only in version, annotations and stage labels")
	}

	p2, _, err := workflowmodel.Compile(v2)
	if err != nil {
		t.Fatal(err)
	}
	p3 := techdocs(t)
	accepted := issue("triage/accepted")
	d2 := stageengine.DeriveFromModel(p2, stagelabels.IssueItem).Derive(accepted)
	d3 := stageengine.DeriveFromModel(p3, stagelabels.IssueItem).Derive(accepted)
	if d2.Abstain || d2.Stage != stagelabels.Claimed || d2.Label() != "stage/020-claimed" {
		t.Fatalf("2026.11.2: accepted issue derived stage %v label %q", d2.Stage, d2.Label())
	}
	if d3.Abstain || d3.Stage != stagelabels.Claimed || d3.Label() != "stage/020-accepted" {
		t.Fatalf("2026.11.3: accepted issue derived stage %v label %q", d3.Stage, d3.Label())
	}
	// An item still wearing the built-in spelling swaps to the model's on
	// the next write (a rebind from 11.2), and keeps its stage meanwhile.
	worn := []string{"stage/020-claimed", "triage/accepted"}
	if add, remove := stagelabels.Plan(worn, d2.Desired()); len(stagelabels.StageLabels(add))+len(stagelabels.StageLabels(remove)) != 0 {
		t.Fatalf("2026.11.2: plan add=%v remove=%v, want no stage change", add, remove)
	}
	add, remove := stagelabels.Plan(worn, d3.Desired())
	if !reflect.DeepEqual(stagelabels.StageLabels(add), []string{"stage/020-accepted"}) || !reflect.DeepEqual(stagelabels.StageLabels(remove), []string{"stage/020-claimed"}) {
		t.Fatalf("2026.11.3: plan add=%v remove=%v", add, remove)
	}
	for _, l := range remove {
		if l == "triage/accepted" {
			t.Fatalf("2026.11.3: plan removed the accept gate's input label: %v", remove)
		}
	}
	if got := stagelabels.Current([]string{"stage/020-accepted", "triage/accepted"}); got != stagelabels.Claimed {
		t.Fatalf("stage/020-accepted reads as %v, want 020", got)
	}
	// The labels are provisioned on bind with the model's other labels.
	defs := map[string]workflowmodel.LabelDef{}
	for _, d := range p3.LabelDefs(stagelabels.StageColor) {
		defs[d.Name] = d
	}
	for state, label := range want {
		d, ok := defs[label]
		if !ok || d.State != state || d.Color != stagelabels.StageColor {
			t.Fatalf("label %s not provisioned for %s: %+v", label, state, d)
		}
	}
}

func issue(labels ...string) stageengine.Facts {
	return stageengine.Facts{
		Ref:    stageengine.Ref{Owner: "cncf", Repo: "techdocs", Number: 1, Kind: stagelabels.IssueItem},
		Open:   true,
		Labels: labels,
		Author: stageengine.AuthorFacts{Kind: authorkind.Human, Login: "maintainer"},
		Issue:  &stageengine.IssueFacts{},
	}
}

func pr(labels ...string) stageengine.Facts {
	return stageengine.Facts{
		Ref:    stageengine.Ref{Owner: "cncf", Repo: "techdocs", Number: 2, Kind: stagelabels.PRItem},
		Open:   true,
		Labels: append([]string{"Docs analysis"}, labels...),
		Author: stageengine.AuthorFacts{Kind: authorkind.Hive, Login: "hive"},
		PR:     &stageengine.PRFacts{Checks: stagelabels.CheckSummary{Known: true, Total: 3, Green: 3}, DCO: stagelabels.DCOYes},
	}
}

// TestTechDocsRules walks an assessment through the rules evaluator the
// way Nate's spec describes it: request → accepted → draft → verified →
// stakeholder review → confirmed → merged, plus the declined and held
// branches.
func TestTechDocsRules(t *testing.T) {
	p := techdocs(t)
	issues := stageengine.DeriveFromModel(p, stagelabels.IssueItem)
	prs := stageengine.DeriveFromModel(p, stagelabels.PRItem)

	cases := []struct {
		name  string
		d     stageengine.Deriver
		f     func() stageengine.Facts
		stage stagelabels.Stage
		term  bool
	}{
		{"new request", issues, func() stageengine.Facts { return issue("needs-triage") }, stagelabels.Actionable, false},
		{"accepted", issues, func() stageengine.Facts { return issue("triage/accepted") }, stagelabels.Claimed, false},
		{"draft posted covers the request", issues, func() stageengine.Facts {
			f := issue("triage/accepted", "phase/analysis")
			f.Issue.CoveringPR = &stageengine.Ref{Owner: "cncf", Repo: "techdocs", Number: 2, Kind: stagelabels.PRItem}
			return f
		}, stagelabels.CoveredByPR, false},
		{"escalated holds", issues, func() stageengine.Facts {
			f := issue("triage/accepted")
			f.Park.Holds = []string{"hold"}
			return f
		}, stagelabels.IssueNeedsDecision, false},
		{"declined", issues, func() stageengine.Facts { f := issue("triage/declined"); f.Open = false; return f }, stagelabels.None, true},
		{"closed", issues, func() stageengine.Facts { f := issue("triage/accepted"); f.Open = false; return f }, stagelabels.None, true},
		{"drafting", prs, func() stageengine.Facts { f := pr(); f.Draft = true; return f }, stagelabels.Fixing, false},
		{"verified draft in review", prs, func() stageengine.Facts { f := pr("verified"); f.Draft = true; return f }, stagelabels.ReviewPending, false},
		{"ready but unverified stays in review", prs, func() stageengine.Facts { return pr() }, stagelabels.ReviewPending, false},
		{"checks failing", prs, func() stageengine.Facts {
			f := pr("verified")
			f.PR.Checks.Red = []string{"spell-check"}
			return f
		}, stagelabels.NeedsAuthor, false},
		{"stakeholder review", prs, func() stageengine.Facts { return pr("verified") }, stagelabels.Reviewed, false},
		{"approved awaits merge", prs, func() stageengine.Facts { return pr("verified", "confirmed") }, stagelabels.ReadyForHumanMerge, false},
		{"merged", prs, func() stageengine.Facts { f := pr(); f.Open, f.Merged = false, true; return f }, stagelabels.None, true},
		{"discarded", prs, func() stageengine.Facts { f := pr(); f.Open = false; return f }, stagelabels.None, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.d.Derive(tc.f())
			if got.Abstain {
				t.Fatalf("abstained: %s", got.Reason)
			}
			if got.Terminal != tc.term || got.Stage != tc.stage {
				t.Fatalf("got %+v; want stage %s terminal %v", got, tc.stage, tc.term)
			}
		})
	}
	t.Run("unrelated issue abstains", func(t *testing.T) {
		if got := issues.Derive(issue("bug")); !got.Abstain {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("unlabelled PR abstains", func(t *testing.T) {
		f := pr()
		f.Labels = nil
		if got := prs.Derive(f); !got.Abstain {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("replies resolve to gates", func(t *testing.T) {
		for state, verb := range map[string]string{"requested": "/accept", "stakeholder-review": "/confirm", "awaiting-merge": "/approve"} {
			r, ok := p.Reply(state, verb)
			if !ok || r.To == "" {
				t.Fatalf("%s %s: ok=%v %+v", state, verb, ok, r)
			}
		}
		if _, ok := p.Reply("requested", "/approve"); ok {
			t.Fatal("approve offered out of requested")
		}
	})
}

var testNow = time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC)

func TestEntriesSorted(t *testing.T) {
	entries, _ := curated.Load()
	for i := 1; i < len(entries); i++ {
		if strings.Compare(entries[i-1].Path, entries[i].Path) > 0 {
			t.Fatalf("not sorted: %s > %s", entries[i-1].Path, entries[i].Path)
		}
	}
}

// TestTechDocsDispatch pins the agent hand-off the hive derives from the
// curated model without any new field (cncf/techdocs#395): the drafter is
// dispatched once when a request is Accepted and must open a draft PR
// labeled Docs analysis; the verifier runs on the reviewer's /verify in
// Drafting.
func TestTechDocsDispatch(t *testing.T) {
	p := techdocs(t)
	d, ok := p.DispatchFor("accepted")
	if !ok || d.Profile != "techdocs-drafter" || d.Reply != "" || !d.AppliesTo(workflowmodel.KindIssue) {
		t.Fatalf("accepted dispatch = %+v %v", d, ok)
	}
	want := &workflowmodel.Deliverable{State: "drafting", Draft: true, Labels: []string{"Docs analysis"}}
	if !reflect.DeepEqual(d.Deliverable, want) {
		t.Fatalf("drafter deliverable %+v want %+v", d.Deliverable, want)
	}
	v, ok := p.DispatchFor("drafting")
	if !ok || v.Profile != "techdocs-verifier" || v.Reply != "/verify" || !v.AppliesTo(workflowmodel.KindPR) || v.Deliverable != nil {
		t.Fatalf("drafting dispatch = %+v %v", v, ok)
	}
	if len(p.Dispatch) != 2 {
		t.Fatalf("dispatch targets %+v", p.Dispatch)
	}
}
