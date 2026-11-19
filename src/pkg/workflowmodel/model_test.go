package workflowmodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func testScope() Scope { return Scope{Forge: "github.com", Repository: "example/repo"} }

func TestHiveDefaultValidatesAndIsStable(t *testing.T) {
	m := HiveDefault(testScope())
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if m.Hash() != HiveDefault(testScope()).Hash() {
		t.Fatal("hash differs between two identical defaults")
	}
	var ids []string
	for _, s := range m.Spec.States {
		ids = append(ids, s.ID)
	}
	want := []string{StateOpen, StateActionable, StateClaimed, StatePRInProgress, StateVerify, StateMergeQueue, StateMerged}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("default states = %v, want %v", ids, want)
	}
	if got := m.FindState(StateCI); got == nil || got.GuardRef != GuardIDRequiredChecks {
		t.Fatalf("ci region = %+v", got)
	}
}

func TestBindNoPipelinesIsHiveDefault(t *testing.T) {
	d := HiveDefault(testScope())
	b := Bind(testScope(), nil, nil, Coverage{})
	d.Metadata.Name = b.Metadata.Name
	if d.Hash() != b.Hash() {
		t.Fatal("Bind with no pipelines must not change the default spec")
	}
}

func pipeline(id string, role Role, nodes ...Node) Pipeline {
	p := Pipeline{ID: id, Name: id, System: SystemGitHubActions, Path: id + ".yml", Role: role, Nodes: nodes}
	p.Levels, _ = Levels(p.Nodes)
	return p
}

func TestBindFillsCIAndAddsPostMergeChain(t *testing.T) {
	ps := []Pipeline{
		pipeline("ci", RolePRVerify, Node{ID: "lint", Kind: NodeJob}, Node{ID: "test", Kind: NodeJob, Needs: []string{"lint"}}),
		pipeline("nightly", RoleScheduled, Node{ID: "scan", Kind: NodeJob}),
		pipeline("main", RolePushVerify, Node{ID: "build", Kind: NodeJob}),
		pipeline("release", RoleRelease, Node{ID: "publish", Kind: NodeJob}),
		pipeline("deploy", RoleDeploy, Node{ID: "prod", Kind: NodeJob, Environment: "production"}, Node{ID: "stg", Kind: NodeJob, Environment: "staging"}),
	}
	m := Bind(testScope(), ps, nil, Coverage{Files: 5, Parsed: 5})
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if ci := m.FindState(StateCI); len(ci.Pipelines) != 1 || ci.Pipelines[0] != "ci" || ci.Origin != OriginIngested {
		t.Fatalf("ci region = %+v", ci)
	}
	var ids []string
	for _, s := range m.Spec.States {
		ids = append(ids, s.ID)
	}
	want := "open,actionable,claimed,pr-in-progress,verify,merge-queue,merged,main-ci,release,deploy-production,deploy-staging,done"
	if got := strings.Join(ids, ","); got != want {
		t.Fatalf("states = %s\nwant     %s", got, want)
	}
	if m.FindState(StateMerged).Type == StateFinal {
		t.Fatal("merged must stop being final once a post-merge chain exists")
	}
	if m.FindState(StateDone).Type != StateFinal {
		t.Fatal("done must be final")
	}
	if g := m.FindState(DeployStateID("production")).GuardRef; g != "environment-approved-production" {
		t.Fatalf("deploy guard = %q", g)
	}
	for _, s := range m.Spec.States {
		for _, id := range s.Pipelines {
			if id == "nightly" {
				t.Fatal("scheduled pipeline must bind to no state")
			}
		}
	}
}

func TestLevelsAndByLevel(t *testing.T) {
	nodes := []Node{
		{ID: "e2e", Needs: []string{"unit", "integration"}},
		{ID: "unit", Needs: []string{"build"}},
		{ID: "integration", Needs: []string{"build"}},
		{ID: "build", Needs: []string{"lint"}},
		{ID: "lint"},
		{ID: "docs"},
	}
	n, err := Levels(nodes)
	if err != nil || n != 4 {
		t.Fatalf("Levels = %d, %v", n, err)
	}
	got := ByLevel(nodes)
	want := [][]string{{"docs", "lint"}, {"build"}, {"integration", "unit"}, {"e2e"}}
	for i := range want {
		if strings.Join(got[i], ",") != strings.Join(want[i], ",") {
			t.Fatalf("level %d = %v, want %v", i, got[i], want[i])
		}
	}
	if _, err := Levels([]Node{{ID: "a", Needs: []string{"b"}}, {ID: "b", Needs: []string{"a"}}}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle err = %v", err)
	}
	if _, err := Levels([]Node{{ID: "a", Needs: []string{"nope"}}}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown err = %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	m := HiveDefault(testScope())
	m.Spec.Initial = "nowhere"
	m.Spec.States = append(m.Spec.States, State{ID: StateOpen, Label: "dup", Owner: OwnerHuman})
	m.Spec.Transitions = append(m.Spec.Transitions, Transition{From: "x", To: "y", GuardRef: "g"})
	m.Spec.Lanes = append(m.Spec.Lanes, Lane{ID: "l", StateRef: "zzz", Actor: OwnerHuman})
	m.Spec.Pipelines = []Pipeline{{ID: "p", Nodes: []Node{{ID: "a", Needs: []string{"a"}}}}}
	err := m.Validate()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{`initial "nowhere"`, `duplicate state id "open"`, `from unknown state "x"`, `guardRef "g"`, `stateRef "zzz"`, `pipeline "p"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	m := Bind(testScope(), []Pipeline{pipeline("ci", RolePRVerify, Node{ID: "a", Kind: NodeJob, Matrix: &Matrix{Keys: []string{"go", "os"}}})}, []Source{{System: SystemGitHubActions, Path: "ci.yml", SHA256: "x"}}, Coverage{Files: 1, Parsed: 1})
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Model
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Hash() != m.Hash() || back.APIVersion != APIVersion || back.Kind != KindWorkflowModel {
		t.Fatal("round trip changed the model")
	}
}

func TestSlugAndPipelineID(t *testing.T) {
	cases := map[string]string{"Hello World!": "hello-world", "---": "x", "kubernetes/kubernetes": "kubernetes-kubernetes"}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := PipelineID(SystemGitHubActions, ".github/workflows/CI Build.yaml"); got != "gha-github-workflows-ci-build" {
		t.Errorf("PipelineID = %q", got)
	}
	if got := PipelineID(SystemTekton, ".tekton/pull-request.yaml"); got != "tkn-tekton-pull-request" {
		t.Errorf("PipelineID = %q", got)
	}
}

func TestDedupeIDsAndDynamicEnvironment(t *testing.T) {
	ps := []Pipeline{
		pipeline("gha-stale", RoleScheduled, Node{ID: "a", Kind: NodeJob}),
		pipeline("gha-stale", RoleScheduled, Node{ID: "a", Kind: NodeJob}),
		pipeline("gha-stale", RoleScheduled, Node{ID: "a", Kind: NodeJob}),
		pipeline("deploy", RoleDeploy, Node{ID: "d", Kind: NodeJob, Environment: "${{ matrix.env }}"}),
	}
	m := Bind(testScope(), ps, nil, Coverage{})
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := m.Spec.Pipelines[1].ID + "," + m.Spec.Pipelines[2].ID; got != "gha-stale-2,gha-stale-3" {
		t.Fatalf("ids = %s", got)
	}
	if s := m.FindState("deploy"); s == nil || len(s.Pipelines) != 1 {
		t.Fatalf("expected one plain deploy state: %+v", s)
	}
	for _, s := range m.Spec.States {
		if strings.Contains(s.Label, "${{") {
			t.Fatalf("expression leaked into a state label: %q", s.Label)
		}
	}
	if len(m.Spec.Coverage.Findings) != 1 || m.Spec.Coverage.Findings[0].Code != CodeEnvironmentDynamic {
		t.Fatalf("findings = %+v", m.Spec.Coverage.Findings)
	}
}

func TestBindingRolesFollowTriggers(t *testing.T) {
	p := pipeline("ci", RolePRVerify, Node{ID: "a", Kind: NodeJob})
	p.Triggers = []Trigger{{Event: "pull_request"}, {Event: "push", Branches: []string{"main"}, Tags: []string{"v*"}}}
	if got := fmt.Sprint(BindingRoles(p)); got != "[pr-verify release push-verify]" {
		t.Fatalf("roles = %s", got)
	}
	p.Triggers = []Trigger{{Event: "push", Tags: []string{"v*"}}}
	p.Role = RoleRelease
	if got := fmt.Sprint(BindingRoles(p)); got != "[release]" {
		t.Fatalf("tags-only push = %s", got)
	}
	p.Triggers = []Trigger{{Event: "push", Tags: []string{"!v*"}}}
	p.Role = RolePushVerify
	if got := fmt.Sprint(BindingRoles(p)); got != "[push-verify]" {
		t.Fatalf("tags-ignore push = %s", got)
	}
	m := Bind(testScope(), []Pipeline{{ID: "both", Role: RolePRVerify, Triggers: []Trigger{{Event: "pull_request"}, {Event: "push", Branches: []string{"main"}}}, Nodes: []Node{{ID: "a", Kind: NodeJob}}}}, nil, Coverage{})
	if ci, main := m.FindState(StateCI), m.FindState(StateMainCI); len(ci.Pipelines) != 1 || main == nil || len(main.Pipelines) != 1 {
		t.Fatalf("ci=%+v main=%+v", ci, main)
	}
}
