package ingest

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/render"
)

var update = flag.Bool("update", false, "rewrite corpus goldens")

// corpusDir holds real CI definitions copied from public CNCF-landscape
// repositories (the proving corpus for hivecommons/hive#17620), one
// directory per repository named owner__repo.
const corpusDir = "../testdata/corpus"

// golden is the stable summary a repository's model must keep producing.
type golden struct {
	Columns   []string                   `json:"columns"`
	Roles     map[workflowmodel.Role]int `json:"roles"`
	Pipelines []goldenPipeline           `json:"pipelines"`
	Substages []string                   `json:"ciSubstages"`
	Findings  map[string]int             `json:"findings"`
	Diff      []string                   `json:"columnDiff"`
}

type goldenPipeline struct {
	ID     string             `json:"id"`
	Role   workflowmodel.Role `json:"role"`
	Nodes  int                `json:"nodes"`
	Levels int                `json:"levels"`
	Events []string           `json:"events"`
}

func readRepo(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".golden.json") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func summarize(m *workflowmodel.Model) golden {
	g := golden{Roles: map[workflowmodel.Role]int{}, Findings: map[string]int{}, Diff: render.Diff(m)}
	for _, c := range render.Columns(m) {
		g.Columns = append(g.Columns, c.Label)
		for _, s := range c.Substages {
			g.Substages = append(g.Substages, s.Label)
		}
	}
	for _, p := range m.Spec.Pipelines {
		g.Roles[p.Role]++
		gp := goldenPipeline{ID: p.ID, Role: p.Role, Nodes: len(p.Nodes), Levels: p.Levels}
		for _, t := range p.Triggers {
			gp.Events = append(gp.Events, t.Event)
		}
		g.Pipelines = append(g.Pipelines, gp)
	}
	for _, f := range m.Spec.Coverage.Findings {
		g.Findings[f.Code]++
	}
	return g
}

// Every corpus repository must ingest into a valid model whose summary
// matches its golden; run with -update after an intended change.
func TestCorpusGoldens(t *testing.T) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			repo := strings.Replace(e.Name(), "__", "/", 1)
			m := Repository(workflowmodel.Scope{Forge: "github.com", Repository: repo}, readRepo(t, filepath.Join(corpusDir, e.Name())))
			if err := m.Validate(); err != nil {
				t.Fatal(err)
			}
			// Deterministic: a second ingest hashes the same.
			if again := Repository(m.Spec.Scope, readRepo(t, filepath.Join(corpusDir, e.Name()))); again.Hash() != m.Hash() {
				t.Fatal("ingest is not deterministic")
			}
			got, _ := json.MarshalIndent(summarize(m), "", "  ")
			got = append(got, '\n')
			goldenPath := filepath.Join(corpusDir, e.Name()+".golden.json")
			if *update {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if string(want) != string(got) {
				t.Errorf("golden mismatch for %s (run with -update if intended)\n--- want\n%s\n--- got\n%s", repo, want, got)
			}
		})
	}
}

// Specific claims the corpus must keep proving, independent of goldens.
func TestCorpusClaims(t *testing.T) {
	load := func(name string) *workflowmodel.Model {
		return Repository(workflowmodel.Scope{Forge: "github.com", Repository: strings.Replace(name, "__", "/", 1)}, readRepo(t, filepath.Join(corpusDir, name)))
	}
	t.Run("envoy underscore workflows keep distinct ids", func(t *testing.T) {
		m := load("envoyproxy__envoy")
		ids := map[string]bool{}
		for _, p := range m.Spec.Pipelines {
			if ids[p.ID] {
				t.Fatalf("duplicate id %s", p.ID)
			}
			ids[p.ID] = true
		}
		if !ids["gha-github-workflows-request"] || !ids["gha-github-workflows-request-2"] {
			t.Fatalf("ids = %v", keys(ids))
		}
	})
	t.Run("konflux PaC runs classify by event", func(t *testing.T) {
		m := load("konflux-ci__konflux-ci")
		// pull-request and push runs both reference build-pipeline: one
		// pipeline, bound to both CI and Main CI.
		if n := len(m.Spec.Pipelines); n != 1 {
			t.Fatalf("pipelines = %d, want 1 shared", n)
		}
		for _, id := range []string{workflowmodel.StateCI, workflowmodel.StateMainCI} {
			s := m.FindState(id)
			if s == nil || len(s.Pipelines) != 1 {
				t.Fatalf("state %s = %+v", id, s)
			}
		}
	})
	t.Run("serverless-operator runs share pipelines with deduped triggers", func(t *testing.T) {
		m := load("openshift-knative__serverless-operator")
		var tk int
		for _, p := range m.Spec.Pipelines {
			if p.System != workflowmodel.SystemTekton {
				continue
			}
			tk++
			seen := map[string]bool{}
			for _, tr := range p.Triggers {
				k := tr.Event + strings.Join(tr.Branches, ",") + strings.Join(tr.Types, ",")
				if seen[k] {
					t.Fatalf("%s: duplicate trigger %q", p.ID, k)
				}
				seen[k] = true
			}
		}
		if tk != 1 {
			t.Fatalf("expected one shared tekton pipeline, got %d", tk)
		}
	})
	t.Run("cilium environments become deploy columns and merged stops being the cap", func(t *testing.T) {
		m := load("cilium__cilium")
		cols := render.Columns(m)
		labels := make([]string, 0, len(cols))
		for _, c := range cols {
			labels = append(labels, c.Label)
		}
		joined := strings.Join(labels, "|")
		if !strings.Contains(joined, "Release|") || !strings.Contains(joined, "Deploy: ") || !strings.HasSuffix(joined, "Done") {
			t.Fatalf("columns = %s", joined)
		}
		if m.FindState(workflowmodel.StateMerged).Type == workflowmodel.StateFinal {
			t.Fatal("merged must not be final")
		}
	})
	t.Run("hive default is untouched by an empty repository", func(t *testing.T) {
		m := Repository(workflowmodel.Scope{Forge: "github.com", Repository: "x/y"}, nil)
		cols := render.Columns(m)
		if len(cols) != len(render.HiveDefaultColumns()) || len(render.Diff(m)) != 0 {
			t.Fatalf("columns = %+v", cols)
		}
	})
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
