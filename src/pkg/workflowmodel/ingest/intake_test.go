package ingest

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

const intakeFixtures = "testdata"

// intakeShape is what a fixture must propose; every slice is sorted.
type intakeShape struct {
	columns      []string // id[states]
	states       []string // ingested state ids
	events       []string // id=fact op value
	gates        []string // id<-releasedBy[minApprovals]
	roles        []string // id=members
	checks       []string // id:kind
	labels       []string // name->state
	guards       []string
	bookkeeping  int
	findings     []string
	source       workflowmodel.MetaSource
	blankIssues  string
	reviewEdges  int // transitions carrying the review gate
	triageEdges  int // transitions into triage
	intakeEdges  int // transitions into requested-* states
	citationsAPI int // citations pointing at an API endpoint
}

func shape(m *workflowmodel.Model) intakeShape {
	var s intakeShape
	for _, c := range m.Spec.Columns {
		s.columns = append(s.columns, c.ID+"["+strings.Join(c.States, ",")+"]")
	}
	for _, st := range m.Spec.States {
		if isIntakeStateID(st.ID) {
			s.states = append(s.states, st.ID)
		}
	}
	for _, e := range m.Spec.Events {
		if intakeEvent(e.ID) {
			s.events = append(s.events, e.ID+"="+e.Fact.Fact+" "+string(e.Fact.Op)+" "+e.Fact.Value)
		}
	}
	for _, g := range m.Spec.Gates {
		if g.Confidence == workflowmodel.ConfidenceInferred {
			s.gates = append(s.gates, g.ID+"<-"+g.ReleasedBy+"["+itoa(g.MinApprovals)+"]")
		}
	}
	for _, r := range m.Spec.Roles {
		if r.Kind == workflowmodel.RoleMembers {
			s.roles = append(s.roles, r.ID+"="+r.Params["members"])
		}
	}
	for _, c := range m.Spec.Checks {
		s.checks = append(s.checks, c.ID+":"+string(c.Kind))
	}
	for _, l := range m.Spec.Labels.Entries {
		if !strings.HasPrefix(l.Name, "stage/") {
			s.labels = append(s.labels, l.Name+"->"+l.State)
		}
	}
	for _, g := range m.Spec.Guards {
		if g.ID == GuardRequiredChecks {
			s.guards = append(s.guards, g.ID)
		}
	}
	s.bookkeeping = len(m.Spec.Bookkeeping)
	for _, f := range m.Spec.Coverage.Findings {
		s.findings = append(s.findings, f.Code)
	}
	s.source = m.Spec.Meta.Source
	s.blankIssues = m.Metadata.Annotations[AnnotationBlankIssues]
	for _, t := range m.Spec.Transitions {
		if t.GateRef == GateReview {
			s.reviewEdges++
		}
		if t.To == StateTriage {
			s.triageEdges++
		}
		if strings.HasPrefix(t.To, StateRequestedPrefix) {
			s.intakeEdges++
		}
	}
	for _, c := range allCitations(m) {
		if strings.HasPrefix(c.Path, "api:") {
			s.citationsAPI++
		}
	}
	for _, l := range [][]string{s.states, s.events, s.gates, s.roles, s.checks, s.labels, s.guards, s.findings} {
		sort.Strings(l)
	}
	return s
}

func itoa(i int) string { return string(rune('0' + i)) }

func isIntakeStateID(id string) bool {
	return id == StateTriage || strings.HasPrefix(id, StateRequestedPrefix)
}

func intakeEvent(id string) bool {
	return strings.HasPrefix(id, EventTemplatePrefix) || id == EventTriageRequested || id == EventTriageAccepted || id == EventTriageDeclined
}

// allCitations gathers every citation on inferred elements.
func allCitations(m *workflowmodel.Model) []workflowmodel.Citation {
	var out []workflowmodel.Citation
	for _, st := range m.Spec.States {
		if isIntakeStateID(st.ID) {
			out = append(out, st.Lifecycle.Citations...)
		}
	}
	for _, e := range m.Spec.Events {
		if intakeEvent(e.ID) {
			out = append(out, e.Citations...)
		}
	}
	for _, g := range m.Spec.Gates {
		if g.ID == GateReview || g.ID == GateCodeownerReview || g.ID == GateTriageDecision {
			out = append(out, g.Citations...)
		}
	}
	for _, g := range m.Spec.Guards {
		if g.ID == GuardRequiredChecks {
			out = append(out, sourceCitation(g.Source))
		}
	}
	for _, r := range m.Spec.Roles {
		if r.Kind == workflowmodel.RoleMembers {
			for _, src := range strings.Split(r.Params["source"], ",") {
				out = append(out, sourceCitation(src))
			}
		}
	}
	for _, c := range m.Spec.Checks {
		if c.Source != "" {
			out = append(out, sourceCitation(c.Source))
		}
	}
	for _, b := range m.Spec.Bookkeeping {
		out = append(out, sourceCitation(b.Params["source"]))
	}
	return out
}

// sourceCitation splits a "path:line" or "api:endpoint" source string.
func sourceCitation(src string) workflowmodel.Citation {
	if rest, ok := strings.CutPrefix(src, "api:"); ok {
		return citeAPI(rest, "")
	}
	p, l, _ := strings.Cut(src, ":")
	c := workflowmodel.Citation{Path: p}
	for _, d := range l {
		c.Line = c.Line*10 + int(d-'0')
	}
	return c
}

func TestIntakeFixtures(t *testing.T) {
	cases := []struct {
		dir  string
		want intakeShape
	}{
		{
			dir: "techdocs",
			want: intakeShape{
				columns: []string{
					"open[open]",
					"intake[requested-documentation-assessment-request]",
					"triage[triage]",
					"actionable[actionable,triage-needed,possible-existing,open-for-discussion,needs-decision-issue]",
				},
				states: []string{"requested-documentation-assessment-request", "triage"},
				events: []string{
					"issue-opened-with-template:documentation-assessment-request=labels has Docs analysis",
					"triage-accepted=labels has triage/accepted",
					"triage-declined=labels has triage/declined",
					"triage-requested=labels has needs-triage",
				},
				gates:       []string{"triage-decision<-maintainer[0]"},
				labels:      []string{"Docs analysis->requested-documentation-assessment-request", "good first issue->", "help wanted->", "needs-triage->triage", "triage/accepted->actionable", "triage/declined->closed"},
				source:      "github-actions+issue-templates",
				triageEdges: 2, intakeEdges: 1,
			},
		},
		{
			dir: "k8s-owners",
			want: intakeShape{
				columns: []string{
					"open[open]",
					"intake[requested-bug-report,requested-enhancement-tracking-issue,requested-flaking-test]",
					"triage[triage]",
				},
				states: []string{"requested-bug-report", "requested-enhancement-tracking-issue", "requested-flaking-test", "triage"},
				events: []string{
					"issue-opened-with-template:bug-report=labels has kind/bug",
					"issue-opened-with-template:enhancement-tracking-issue=labels has kind/feature",
					"issue-opened-with-template:flaking-test=labels has kind/flake",
					"triage-accepted=labels has triage/accepted",
					"triage-requested=labels has needs-triage",
				},
				gates:        []string{"codeowner-review<-approver[0]", "review<-approver[2]", "triage-decision<-approver[0]"},
				roles:        []string{"approver=alice,dave,erin", "reviewer=bob,frank"},
				checks:       []string{"check-dco:ci", "check-pull-widgets-unit:ci", "check-pull-widgets-verify:ci", "pr-template-documentation-updated-in-docs:manual", "pr-template-i-have-read-the-contributing-guide:manual", "pr-template-release-note-drafted-below:manual", "pr-template-tests-added-or-updated:manual"},
				labels:       []string{"good first issue->", "help wanted->", "kind/bug->requested-bug-report", "kind/feature->requested-enhancement-tracking-issue", "kind/flake->requested-flaking-test", "needs-triage->triage", "triage/accepted->actionable"},
				guards:       []string{GuardRequiredChecks},
				findings:     []string{"issue-template-no-labels"},
				source:       workflowmodel.SourceIssueTemplates,
				blankIssues:  "disabled",
				bookkeeping:  4,
				reviewEdges:  5,
				triageEdges:  4,
				intakeEdges:  3,
				citationsAPI: 10,
			},
		},
		{
			dir: "md-template",
			want: intakeShape{
				columns:     []string{"open[open]", "intake[requested-bug-report]"},
				states:      []string{"requested-bug-report"},
				events:      []string{"issue-opened-with-template:bug-report=labels has bug"},
				gates:       []string{"review<-approver[0]"},
				roles:       []string{"approver=acme/core-team,hubot,octocat"},
				checks:      []string{"pr-template-changelog-entry-added:manual", "pr-template-make-test-passes:manual"},
				labels:      []string{"bug->requested-bug-report"},
				source:      workflowmodel.SourceIssueTemplates,
				reviewEdges: 5, intakeEdges: 1,
			},
		},
		{
			dir: "maintainers-labels",
			want: intakeShape{
				columns:     []string{"open[open]", "triage[triage]"},
				states:      []string{"triage"},
				events:      []string{"triage-accepted=labels lacks needs-triage", "triage-requested=labels has needs-triage"},
				gates:       []string{"review<-maintainer[0]", "triage-decision<-maintainer[0]"},
				roles:       []string{"maintainer=ada,grace,linus-t"},
				labels:      []string{"area/runtime->", "kind/bug->", "kind/feature->", "needs-triage->triage", "priority/P0->", "priority/P1->", "priority/P2->"},
				reviewEdges: 5, triageEdges: 1,
			},
		},
		{
			dir:  "empty",
			want: intakeShape{columns: []string{"open[open]"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			files := readRepo(t, filepath.Join(intakeFixtures, tc.dir))
			m := Lifecycle(scopeFor("example/"+tc.dir), files)
			if _, diags, err := workflowmodel.Compile(m); err != nil {
				t.Fatalf("compile: %v\n%v", err, diags)
			}
			got := shape(m)
			got.columns = got.columns[:len(tc.want.columns)]
			if tc.want.gates == nil && tc.want.roles == nil && tc.want.checks == nil && tc.want.states == nil {
				// empty fixture: byte-identical to hive-default lifecycle
				if m.Hash() != Lifecycle(scopeFor("example/empty"), nil).Hash() {
					t.Fatal("empty tree changed the hive-default lifecycle")
				}
			}
			if a, b := mustJSON(t, got), mustJSON(t, tc.want); !bytes.Equal(a, b) {
				t.Fatalf("shape mismatch\n got %s\nwant %s", a, b)
			}
			for _, c := range allCitations(m) {
				if !strings.HasPrefix(c.Path, "api:") && (c.Line == 0 || c.Path == "") {
					t.Errorf("citation without file:line: %+v", c)
				}
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(struct {
		Columns, States, Events, Gates, Roles, Checks, Labels, Guards, Findings []string
		Bookkeeping                                                             int
		Source                                                                  workflowmodel.MetaSource
		BlankIssues                                                             string
		ReviewEdges, TriageEdges, IntakeEdges, CitationsAPI                     int
	}{
		v.(intakeShape).columns, v.(intakeShape).states, v.(intakeShape).events, v.(intakeShape).gates, v.(intakeShape).roles,
		v.(intakeShape).checks, v.(intakeShape).labels, v.(intakeShape).guards, v.(intakeShape).findings, v.(intakeShape).bookkeeping,
		v.(intakeShape).source, v.(intakeShape).blankIssues, v.(intakeShape).reviewEdges, v.(intakeShape).triageEdges,
		v.(intakeShape).intakeEdges, v.(intakeShape).citationsAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIntakeInferredAndCited: every element the intake half adds is
// inferred and carries at least one citation.
func TestIntakeInferredAndCited(t *testing.T) {
	m := Lifecycle(scopeFor("example/k8s"), readRepo(t, filepath.Join(intakeFixtures, "k8s-owners")))
	for _, st := range m.Spec.States {
		if st.Origin == workflowmodel.OriginIngested && (st.Lifecycle.Confidence != workflowmodel.ConfidenceInferred || len(st.Lifecycle.Citations) == 0) {
			t.Errorf("state %s: confidence %q citations %d", st.ID, st.Lifecycle.Confidence, len(st.Lifecycle.Citations))
		}
	}
	for _, e := range m.Spec.Events {
		if strings.HasPrefix(e.ID, EventTemplatePrefix) || strings.HasPrefix(e.ID, "triage-") {
			if e.Confidence != workflowmodel.ConfidenceInferred || len(e.Citations) == 0 {
				t.Errorf("event %s: confidence %q citations %d", e.ID, e.Confidence, len(e.Citations))
			}
		}
	}
	for _, g := range m.Spec.Gates {
		if g.ID == GateReview || g.ID == GateCodeownerReview || g.ID == GateTriageDecision {
			if g.Confidence != workflowmodel.ConfidenceInferred || len(g.Citations) == 0 {
				t.Errorf("gate %s: confidence %q citations %d", g.ID, g.Confidence, len(g.Citations))
			}
		}
	}
	for _, r := range m.Spec.Roles {
		if r.Kind == workflowmodel.RoleMembers && (r.Params["source"] == "" || r.Params["members"] == "") {
			t.Errorf("role %s lacks source/members", r.ID)
		}
	}
	for _, c := range m.Spec.Checks {
		if c.Source == "" || c.Kind == "" {
			t.Errorf("check %s lacks source/kind: %+v", c.ID, c)
		}
	}
}

// TestLifecyclePipelineOnlyUnchanged: trees without intake inputs produce
// byte-identical models to the pre-P5 pipeline binding.
func TestLifecyclePipelineOnlyUnchanged(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(corpusDir, "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("corpus: %v %d", err, len(dirs))
	}
	for _, dir := range dirs {
		files := readRepo(t, dir)
		for p := range files {
			if IsIntakePath(p) || p == BranchRulesPath {
				t.Fatalf("%s: corpus tree contains intake file %s", dir, p)
			}
		}
		scope := scopeFor("corpus/" + filepath.Base(dir))
		got := Lifecycle(scope, files)
		if LifecycleOptions(scope, files, Options{}).Hash() != got.Hash() {
			t.Errorf("%s: LifecycleOptions{} differs", dir)
		}
		for _, st := range got.Spec.States {
			if isIntakeStateID(st.ID) {
				t.Errorf("%s: unexpected intake state %s", dir, st.ID)
			}
		}
		for _, c := range got.Spec.Columns {
			if c.ID == ColumnIntake || c.ID == ColumnTriage {
				t.Errorf("%s: unexpected column %s", dir, c.ID)
			}
		}
		if len(got.Spec.Checks) != 0 || len(got.Spec.Bookkeeping) != 0 || len(got.Spec.Roles) != len(hivedefault.Generate().Spec.Roles) {
			t.Errorf("%s: intake half added checks/bookkeeping/roles", dir)
		}
		if got.Hash() != pipelineOnlyHash(t, scope, files) {
			t.Errorf("%s: Lifecycle differs from the pipeline-only binding", dir)
		}
	}
}

func TestLifecycleOptionsBranchRulesOverrideFile(t *testing.T) {
	files := readRepo(t, filepath.Join(intakeFixtures, "md-template"))
	rules := &BranchRules{Branch: "main", Source: BranchRulesRulesets, Endpoint: "repos/x/y/rules/branches/main", RequiredApprovals: 1, RequiredChecks: []string{"build"}}
	m := LifecycleOptions(scopeFor("x/y"), files, Options{BranchRules: rules})
	if _, _, err := workflowmodel.Compile(m); err != nil {
		t.Fatal(err)
	}
	var review *workflowmodel.Gate
	for i := range m.Spec.Gates {
		if m.Spec.Gates[i].ID == GateReview {
			review = &m.Spec.Gates[i]
		}
	}
	if review == nil || review.MinApprovals != 1 || review.ReleasedBy != RoleApprover {
		t.Fatalf("review gate %+v", review)
	}
	var ci int
	for _, c := range m.Spec.Checks {
		if c.Kind == workflowmodel.CheckCI {
			ci++
			if c.Source != "api:repos/x/y/rules/branches/main" || !c.Required {
				t.Errorf("check %+v", c)
			}
		}
	}
	if ci != 1 {
		t.Fatalf("ci checks %d", ci)
	}
	// rules without an approver role: review gate falls back to maintainer
	m = LifecycleOptions(scopeFor("x/y"), nil, Options{BranchRules: rules})
	if _, _, err := workflowmodel.Compile(m); err != nil {
		t.Fatal(err)
	}
	for _, g := range m.Spec.Gates {
		if g.ID == GateReview && g.ReleasedBy != hivedefault.RoleMaintainer {
			t.Fatalf("review gate released by %q", g.ReleasedBy)
		}
	}
}

// pipelineOnlyHash recomputes the pre-P5 model: hive-default with the
// repository's pipelines bound and the intake half skipped.
func pipelineOnlyHash(t *testing.T, scope workflowmodel.Scope, files map[string][]byte) string {
	t.Helper()
	m := LifecycleOptions(scope, files, Options{})
	if _, diags, err := workflowmodel.Compile(m); err != nil {
		t.Fatalf("compile: %v %v", err, diags)
	}
	// readIntake on a pipeline-only tree must report empty, which is the
	// branch applyIntake takes to leave the model untouched.
	if !readIntake(files).empty() {
		t.Fatal("readIntake found intake inputs in a pipeline-only tree")
	}
	return m.Hash()
}
