// Package render derives views from a workflow model: a Mermaid diagram of
// the process and execution graphs, and the Activity Monitor column set
// the dashboard would draw for a repository bound to the model.
package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Column is one Activity Monitor strip column.
type Column struct {
	// ID is the process state the column lights.
	ID    string `json:"id"`
	Label string `json:"label"`
	// Regions are the parallel regions shown inside the column, if any.
	Regions []string `json:"regions,omitempty"`
	// Substages are the CI sub-stages (one per topological level of the
	// bound PR-verify pipelines), shown under the column.
	Substages []Substage `json:"substages,omitempty"`
	// Pipelines bound into the column (process state and its regions).
	Pipelines []string `json:"pipelines,omitempty"`
	// Terminal marks the final cap column.
	Terminal bool `json:"terminal,omitempty"`
	// Origin says whether the column exists on an unbound hive.
	Origin workflowmodel.Origin `json:"origin,omitempty"`
}

// Substage is one level of a CI DAG, named after its nodes.
type Substage struct {
	Level int      `json:"level"`
	Label string   `json:"label"`
	Nodes []string `json:"nodes"`
}

// MaxSubstageNames caps how many node names a substage label lists before
// summarising the rest as "+N".
const MaxSubstageNames = 3

// MaxSubstages caps how many CI levels become substages; deeper DAGs are
// folded into the last one so the strip stays readable.
const MaxSubstages = 6

// Columns derives the strip columns from the model: one per top-level
// state in declaration order, parallel states as one column listing their
// regions, and the CI region's bound pipelines expanded into substages by
// topological level. HiveDefault yields exactly today's seven columns.
func Columns(m *workflowmodel.Model) []Column {
	var out []Column
	for _, s := range m.Spec.States {
		c := Column{ID: s.ID, Label: s.Label, Origin: s.Origin, Terminal: s.Type == workflowmodel.StateFinal}
		c.Pipelines = append(c.Pipelines, s.Pipelines...)
		for _, r := range s.Regions {
			c.Regions = append(c.Regions, r.Label)
			c.Pipelines = append(c.Pipelines, r.Pipelines...)
			if r.ID == workflowmodel.StateCI {
				c.Substages = substages(m, r.Pipelines)
			}
		}
		if s.ID == workflowmodel.StateCI {
			c.Substages = substages(m, s.Pipelines)
		}
		if len(c.Pipelines) == 0 {
			c.Pipelines = nil
		}
		out = append(out, c)
	}
	return out
}

// HiveDefaultColumns is the column set every unbound hive draws today.
func HiveDefaultColumns() []Column {
	return Columns(workflowmodel.HiveDefault(workflowmodel.Scope{Forge: "github.com", Repository: "example/repo"}))
}

// substages merges the levels of every bound pipeline: level k of the
// result holds the level-k nodes of each pipeline.
func substages(m *workflowmodel.Model, ids []string) []Substage {
	var levels [][]string
	for _, id := range ids {
		p := m.Pipeline(id)
		if p == nil {
			continue
		}
		for lvl, nodes := range workflowmodel.ByLevel(p.Nodes) {
			for len(levels) <= lvl {
				levels = append(levels, nil)
			}
			for _, n := range nodes {
				levels[lvl] = append(levels[lvl], nodeDisplayName(p, n))
			}
		}
	}
	if len(levels) > MaxSubstages {
		tail := levels[MaxSubstages-1:]
		var folded []string
		for _, l := range tail {
			folded = append(folded, l...)
		}
		levels = append(levels[:MaxSubstages-1], folded)
	}
	out := make([]Substage, 0, len(levels))
	for i, nodes := range levels {
		nodes = dedupeSorted(nodes)
		out = append(out, Substage{Level: i, Label: substageLabel(nodes), Nodes: nodes})
	}
	return out
}

func nodeDisplayName(p *workflowmodel.Pipeline, id string) string {
	for _, n := range p.Nodes {
		if n.ID == id {
			if n.Name != "" && !strings.Contains(n.Name, "${{") {
				return n.Name
			}
			return n.ID
		}
	}
	return id
}

func dedupeSorted(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func substageLabel(nodes []string) string {
	if len(nodes) <= MaxSubstageNames {
		return strings.Join(nodes, " · ")
	}
	return fmt.Sprintf("%s +%d", strings.Join(nodes[:MaxSubstageNames], " · "), len(nodes)-MaxSubstageNames)
}

// Diff lists how the model's columns differ from the hive default, as
// short human sentences; empty when they are identical.
func Diff(m *workflowmodel.Model) []string {
	base := HiveDefaultColumns()
	cols := Columns(m)
	var out []string
	byID := map[string]Column{}
	for _, c := range cols {
		byID[c.ID] = c
	}
	for _, b := range base {
		c, ok := byID[b.ID]
		if !ok {
			out = append(out, fmt.Sprintf("column %q removed", b.Label))
			continue
		}
		if len(c.Substages) > 0 && len(b.Substages) == 0 {
			out = append(out, fmt.Sprintf("column %q gains %d CI substage(s) from %d pipeline(s)", b.Label, len(c.Substages), len(c.Pipelines)))
		}
		if b.Terminal && !c.Terminal {
			out = append(out, fmt.Sprintf("column %q is no longer the cap", b.Label))
		}
	}
	baseIDs := map[string]bool{}
	for _, b := range base {
		baseIDs[b.ID] = true
	}
	for _, c := range cols {
		if !baseIDs[c.ID] {
			out = append(out, fmt.Sprintf("column %q added after merge", c.Label))
		}
	}
	return out
}
