package render

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func scope() workflowmodel.Scope {
	return workflowmodel.Scope{Forge: "github.com", Repository: "example/repo"}
}

func pipeline(id string, role workflowmodel.Role, nodes ...workflowmodel.Node) workflowmodel.Pipeline {
	p := workflowmodel.Pipeline{ID: id, Name: id, System: workflowmodel.SystemGitHubActions, Path: id + ".yml", Role: role, Nodes: nodes}
	p.Levels, _ = workflowmodel.Levels(p.Nodes)
	return p
}

// The unbound default must draw exactly the seven columns the strip draws
// today (agent-now.js STAGE_TRACKER_PATH + the merged cap).
func TestHiveDefaultColumnsMatchToday(t *testing.T) {
	cols := HiveDefaultColumns()
	want := []string{"Open", "Actionable", "Claimed", "PR in progress", "CI / Review", "Merge queue", "Merged"}
	if len(cols) != len(want) {
		t.Fatalf("got %d columns, want %d", len(cols), len(want))
	}
	for i, c := range cols {
		if c.Label != want[i] {
			t.Errorf("column %d = %q, want %q", i, c.Label, want[i])
		}
		if len(c.Substages) != 0 || len(c.Pipelines) != 0 {
			t.Errorf("column %q must be bare on the default: %+v", c.Label, c)
		}
	}
	if !cols[len(cols)-1].Terminal {
		t.Error("Merged must be the terminal cap")
	}
	if got := strings.Join(cols[4].Regions, "|"); got != "Review|CI" {
		t.Errorf("verify regions = %q", got)
	}
	if d := Diff(workflowmodel.HiveDefault(scope())); len(d) != 0 {
		t.Errorf("Diff(default) = %v, want none", d)
	}
}

func TestColumnsExpandCIAndAddPostMerge(t *testing.T) {
	ps := []workflowmodel.Pipeline{
		pipeline("ci", workflowmodel.RolePRVerify,
			workflowmodel.Node{ID: "lint", Name: "Lint", Kind: workflowmodel.NodeJob},
			workflowmodel.Node{ID: "build", Kind: workflowmodel.NodeJob, Needs: []string{"lint"}},
			workflowmodel.Node{ID: "unit", Kind: workflowmodel.NodeJob, Needs: []string{"build"}},
			workflowmodel.Node{ID: "integration", Kind: workflowmodel.NodeJob, Needs: []string{"build"}, Matrix: &workflowmodel.Matrix{Keys: []string{"os"}}},
			workflowmodel.Node{ID: "e2e", Kind: workflowmodel.NodeJob, Needs: []string{"unit", "integration"}}),
		pipeline("verify", workflowmodel.RolePRVerify, workflowmodel.Node{ID: "dco", Kind: workflowmodel.NodeJob}),
		pipeline("release", workflowmodel.RoleRelease, workflowmodel.Node{ID: "goreleaser", Kind: workflowmodel.NodeJob}),
	}
	m := workflowmodel.Bind(scope(), ps, nil, workflowmodel.Coverage{})
	cols := Columns(m)
	labels := make([]string, 0, len(cols))
	for _, c := range cols {
		labels = append(labels, c.Label)
	}
	if got := strings.Join(labels, ","); got != "Open,Actionable,Claimed,PR in progress,CI / Review,Merge queue,Merged,Release,Done" {
		t.Fatalf("labels = %s", got)
	}
	ci := cols[4]
	if len(ci.Substages) != 4 {
		t.Fatalf("substages = %+v", ci.Substages)
	}
	// Level 0 merges both PR-verify pipelines; names prefer Node.Name.
	if ci.Substages[0].Label != "Lint · dco" {
		t.Errorf("level 0 = %q", ci.Substages[0].Label)
	}
	if ci.Substages[2].Label != "integration · unit" || ci.Substages[3].Label != "e2e" {
		t.Errorf("levels = %+v", ci.Substages)
	}
	if strings.Join(ci.Pipelines, ",") != "ci,verify" {
		t.Errorf("ci pipelines = %v", ci.Pipelines)
	}
	if cols[6].Terminal || !cols[8].Terminal {
		t.Error("Done must be the cap once Release exists")
	}
	d := strings.Join(Diff(m), "\n")
	for _, want := range []string{`"CI / Review" gains 4 CI substage(s) from 2 pipeline(s)`, `"Merged" is no longer the cap`, `"Release" added after merge`, `"Done" added after merge`} {
		if !strings.Contains(d, want) {
			t.Errorf("Diff missing %q:\n%s", want, d)
		}
	}
}

func TestSubstageLabelFoldsLongLevels(t *testing.T) {
	var nodes []workflowmodel.Node
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		nodes = append(nodes, workflowmodel.Node{ID: id, Kind: workflowmodel.NodeJob})
	}
	m := workflowmodel.Bind(scope(), []workflowmodel.Pipeline{pipeline("ci", workflowmodel.RolePRVerify, nodes...)}, nil, workflowmodel.Coverage{})
	got := Columns(m)[4].Substages[0].Label
	if got != "a · b · c +2" {
		t.Fatalf("label = %q", got)
	}
}

func TestDeepDAGFoldsIntoMaxSubstages(t *testing.T) {
	var nodes []workflowmodel.Node
	prev := ""
	for i := 0; i < MaxSubstages+3; i++ {
		n := workflowmodel.Node{ID: string(rune('a' + i)), Kind: workflowmodel.NodeJob}
		if prev != "" {
			n.Needs = []string{prev}
		}
		prev = n.ID
		nodes = append(nodes, n)
	}
	m := workflowmodel.Bind(scope(), []workflowmodel.Pipeline{pipeline("ci", workflowmodel.RolePRVerify, nodes...)}, nil, workflowmodel.Coverage{})
	sub := Columns(m)[4].Substages
	if len(sub) != MaxSubstages {
		t.Fatalf("got %d substages, want %d", len(sub), MaxSubstages)
	}
	if len(sub[MaxSubstages-1].Nodes) != 4 {
		t.Fatalf("last substage should fold 4 nodes: %+v", sub[MaxSubstages-1])
	}
}

func TestMermaidIsDeterministicAndWellFormed(t *testing.T) {
	ps := []workflowmodel.Pipeline{pipeline("ci", workflowmodel.RolePRVerify,
		workflowmodel.Node{ID: "build", Kind: workflowmodel.NodeJob},
		workflowmodel.Node{ID: "test", Kind: workflowmodel.NodeJob, Needs: []string{"build"}, Matrix: &workflowmodel.Matrix{Dynamic: true}},
		workflowmodel.Node{ID: "call", Kind: workflowmodel.NodeReusableWorkflow, Ref: "org/repo/.github/workflows/x.yml@main", Needs: []string{"build"}})}
	m := workflowmodel.Bind(scope(), ps, nil, workflowmodel.Coverage{})
	a, b := Mermaid(m), Mermaid(m)
	if a != b {
		t.Fatal("Mermaid not deterministic")
	}
	for _, want := range []string{"stateDiagram-v2", "[*] --> open", `state "CI / Review (ci)" as verify`, "state verify {", "--", "note right of ci", "ci: build → call · test", "merged --> [*]", "verify --> merge_queue: verified [required-checks-satisfied]"} {
		if !strings.Contains(a, want) {
			t.Errorf("missing %q in:\n%s", want, a)
		}
	}
	pm := PipelineMermaid(&m.Spec.Pipelines[0])
	for _, want := range []string{"flowchart LR", `subgraph ci["ci (pr-verify)"]`, `ci_test["test ⊗ dynamic"]`, `ci_call["call ↗ org/repo/.github/workflows/x.yml@main"]`, "ci_build --> ci_test"} {
		if !strings.Contains(pm, want) {
			t.Errorf("missing %q in:\n%s", want, pm)
		}
	}
}

func TestPipelineMermaidTruncatesHugeGraphs(t *testing.T) {
	var nodes []workflowmodel.Node
	for i := 0; i < MaxDiagramNodes+5; i++ {
		nodes = append(nodes, workflowmodel.Node{ID: "j" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Kind: workflowmodel.NodeJob})
	}
	p := pipeline("big", workflowmodel.RolePRVerify, nodes...)
	out := PipelineMermaid(&p)
	if !strings.Contains(out, "… 5 more") {
		t.Fatalf("expected truncation marker:\n%s", out)
	}
}
