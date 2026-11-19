package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// Every corpus repository must yield a lifecycle model that compiles;
// refusals are the ingest's bug, warnings are the inferred elements.
func TestLifecycleCorpusCompiles(t *testing.T) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		seen++
		t.Run(e.Name(), func(t *testing.T) {
			repo := strings.ReplaceAll(e.Name(), "__", "/")
			m := Lifecycle(workflowmodel.Scope{Forge: "github.com", Repository: repo}, readRepo(t, filepath.Join(corpusDir, e.Name())))
			p, diags, err := workflowmodel.Compile(m)
			if err != nil {
				t.Fatalf("compile: %v\n%v", err, diags)
			}
			if p.Mode != workflowmodel.DerivationBuiltin {
				t.Fatalf("mode %q", p.Mode)
			}
			if m.Spec.Meta.Provenance != workflowmodel.ProvenanceIngestedUnreviewed || m.Spec.Meta.Version == "" {
				t.Fatalf("meta %+v", m.Spec.Meta)
			}
			if len(m.Spec.Pipelines) == 0 {
				t.Fatal("corpus repo ingested no pipelines")
			}
			// The hive-default lifecycle is intact underneath.
			for _, s := range hivedefault.Generate().Spec.States {
				if _, ok := p.States[s.ID]; !ok {
					t.Fatalf("hive-default state %q missing", s.ID)
				}
			}
			// Every ingested element is inferred and warned about.
			inferred := 0
			for _, d := range diags {
				if d.Code == workflowmodel.CodeInferred {
					inferred++
				}
			}
			ingested := 0
			for _, s := range m.Spec.States {
				if s.Origin == workflowmodel.OriginIngested {
					ingested++
					if s.Lifecycle.Confidence != workflowmodel.ConfidenceInferred || s.Lifecycle.Column != s.ID {
						t.Fatalf("ingested state %+v", s.Lifecycle)
					}
					if s.ID != workflowmodel.StateDone && len(s.Lifecycle.Citations) == 0 {
						t.Fatalf("ingested state %q has no citations", s.ID)
					}
				}
			}
			if ingested > 0 && inferred == 0 {
				t.Fatalf("%d ingested states but no inferred warnings", ingested)
			}
		})
	}
	if seen == 0 {
		t.Fatal("empty corpus")
	}
}

func TestLifecyclePostMergeChain(t *testing.T) {
	files := map[string][]byte{
		".github/workflows/ci.yml":      []byte("name: CI\non: [pull_request, push]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: make test}]\n"),
		".github/workflows/release.yml": []byte("name: Release\non:\n  push:\n    tags: ['v*']\njobs:\n  publish:\n    runs-on: ubuntu-latest\n    environment: production\n    steps: [{run: make publish}]\n"),
	}
	m := Lifecycle(workflowmodel.Scope{Forge: "github.com", Repository: "acme/widgets"}, files)
	p, diags, err := workflowmodel.Compile(m)
	if err != nil {
		t.Fatalf("%v %v", err, diags)
	}
	if m.Spec.Meta.Source != workflowmodel.SourceGitHubActions {
		t.Fatalf("source %q", m.Spec.Meta.Source)
	}
	// Columns: hive-default's seven, then main CI, release, done.
	var cols []string
	for _, c := range m.Spec.Columns {
		cols = append(cols, c.ID)
	}
	got := strings.Join(cols, " ")
	if !strings.HasSuffix(got, hivedefault.ColumnMerged+" "+workflowmodel.StateMainCI+" "+workflowmodel.StateRelease+" "+workflowmodel.StateDone) {
		t.Fatalf("columns %s", got)
	}
	if !p.Terminal[workflowmodel.StateDone] || !p.Terminal[hivedefault.StateMerged] {
		t.Fatalf("terminal %v", p.Terminal)
	}
	// PR-verify pipelines sit on every CI / Review state with a citation.
	bound := 0
	for _, s := range m.Spec.States {
		if s.Lifecycle.Column == hivedefault.ColumnCIReview {
			if len(s.Pipelines) != 1 || len(s.Lifecycle.Citations) != 1 || s.Lifecycle.Citations[0].Path != ".github/workflows/ci.yml" {
				t.Fatalf("ci state %q: %v %v", s.ID, s.Pipelines, s.Lifecycle.Citations)
			}
			bound++
		}
	}
	if bound == 0 {
		t.Fatal("no CI / Review state bound")
	}
	// The chain fires on the declared pipeline event, merged -> main-ci -> release -> done.
	var chain []string
	for _, tr := range m.Spec.Transitions {
		if tr.On == EventPipelineSucceeded {
			chain = append(chain, tr.From+">"+tr.To)
		}
	}
	if want := "merged>main-ci main-ci>release release>done"; strings.Join(chain, " ") != want {
		t.Fatalf("chain %v", chain)
	}
}

func TestLifecycleNoPipelinesIsHiveDefaultLifecycle(t *testing.T) {
	m := Lifecycle(workflowmodel.Scope{Forge: "github.com", Repository: "acme/empty"}, map[string][]byte{"README.md": []byte("hi")})
	p, _, err := workflowmodel.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, _ := workflowmodel.Compile(hivedefault.Generate())
	if len(p.States) != len(ref.States) || len(m.Spec.Columns) != len(hivedefault.Generate().Spec.Columns) {
		t.Fatalf("states %d/%d", len(p.States), len(ref.States))
	}
	if m.Spec.Meta.Source != "" || m.Spec.Meta.Provenance != workflowmodel.ProvenanceIngestedUnreviewed {
		t.Fatalf("meta %+v", m.Spec.Meta)
	}
}

func scopeFor(repo string) workflowmodel.Scope {
	return workflowmodel.Scope{Forge: "github.com", Repository: repo}
}
