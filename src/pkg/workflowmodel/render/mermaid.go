package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// MaxDiagramNodes caps how many execution nodes one pipeline subgraph
// draws before the rest are summarised, so a 120-job workflow stays
// legible in the editor.
const MaxDiagramNodes = 40

// Mermaid renders the process graph as a Mermaid stateDiagram-v2, with
// parallel states as concurrent regions, and the pipelines bound to each
// state as notes naming their levels. Deterministic for a given model.
func Mermaid(m *workflowmodel.Model) string {
	var b strings.Builder
	b.WriteString("stateDiagram-v2\n")
	fmt.Fprintf(&b, "    [*] --> %s\n", mid(m.Spec.Initial))
	for _, s := range m.Spec.States {
		writeState(&b, m, s, 1)
		if s.Type == workflowmodel.StateFinal {
			fmt.Fprintf(&b, "    %s --> [*]\n", mid(s.ID))
		}
	}
	for _, t := range m.Spec.Transitions {
		label := t.On
		if t.GuardRef != "" {
			label += " [" + t.GuardRef + "]"
		}
		fmt.Fprintf(&b, "    %s --> %s: %s\n", mid(t.From), mid(t.To), label)
	}
	return b.String()
}

func writeState(b *strings.Builder, m *workflowmodel.Model, s workflowmodel.State, depth int) {
	ind := strings.Repeat("    ", depth)
	fmt.Fprintf(b, "%sstate \"%s\" as %s\n", ind, escape(s.Label+ownerSuffix(s.Owner)), mid(s.ID))
	if len(s.Regions) > 0 {
		fmt.Fprintf(b, "%sstate %s {\n", ind, mid(s.ID))
		for i, r := range s.Regions {
			if i > 0 {
				fmt.Fprintf(b, "%s    --\n", ind)
			}
			writeState(b, m, r, depth+1)
		}
		fmt.Fprintf(b, "%s}\n", ind)
	}
	if len(s.Pipelines) > 0 {
		fmt.Fprintf(b, "%snote right of %s\n", ind, mid(s.ID))
		for _, id := range s.Pipelines {
			if p := m.Pipeline(id); p != nil {
				fmt.Fprintf(b, "%s    %s: %s\n", ind, escape(p.Name), levelsSummary(p))
			}
		}
		fmt.Fprintf(b, "%send note\n", ind)
	}
}

func ownerSuffix(o workflowmodel.Owner) string {
	switch o {
	case workflowmodel.OwnerHuman:
		return " (human)"
	case workflowmodel.OwnerRegisteredAgent:
		return " (agent)"
	case workflowmodel.OwnerExternalService:
		return " (ci)"
	}
	return ""
}

func levelsSummary(p *workflowmodel.Pipeline) string {
	levels := workflowmodel.ByLevel(p.Nodes)
	parts := make([]string, 0, len(levels))
	for _, l := range levels {
		parts = append(parts, substageLabel(l))
	}
	return strings.Join(parts, " → ")
}

// PipelineMermaid renders one pipeline's execution DAG as a Mermaid
// flowchart (left to right), matrix jobs marked with their keys and
// reusable workflows with their ref.
func PipelineMermaid(p *workflowmodel.Pipeline) string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	fmt.Fprintf(&b, "    subgraph %s[\"%s (%s)\"]\n", mid(p.ID), escape(p.Name), p.Role)
	nodes := p.Nodes
	truncated := 0
	if len(nodes) > MaxDiagramNodes {
		truncated = len(nodes) - MaxDiagramNodes
		nodes = nodes[:MaxDiagramNodes]
	}
	shown := map[string]bool{}
	for _, n := range nodes {
		shown[n.ID] = true
		fmt.Fprintf(&b, "        %s[\"%s\"]\n", mid(p.ID+"-"+n.ID), escape(nodeLabel(n)))
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "        %s[\"… %d more\"]\n", mid(p.ID+"-more"), truncated)
	}
	for _, n := range nodes {
		needs := append([]string(nil), n.Needs...)
		sort.Strings(needs)
		for _, need := range needs {
			if shown[need] {
				fmt.Fprintf(&b, "        %s --> %s\n", mid(p.ID+"-"+need), mid(p.ID+"-"+n.ID))
			}
		}
	}
	b.WriteString("    end\n")
	return b.String()
}

func nodeLabel(n workflowmodel.Node) string {
	label := n.ID
	if n.Name != "" && !strings.Contains(n.Name, "${{") {
		label = n.Name
	}
	switch {
	case n.Matrix != nil && n.Matrix.Dynamic:
		label += " ⊗ dynamic"
	case n.Matrix != nil && len(n.Matrix.Keys) > 0:
		label += " ⊗ " + strings.Join(n.Matrix.Keys, ",")
	}
	if n.Kind == workflowmodel.NodeReusableWorkflow {
		label += " ↗ " + n.Ref
	}
	if n.Kind == workflowmodel.NodeFinally {
		label += " (finally)"
	}
	if n.Environment != "" {
		label += " @" + n.Environment
	}
	return label
}

// mid makes a Mermaid-safe identifier.
func mid(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 || (s[0] >= '0' && s[0] <= '9') {
		return "n_" + b.String()
	}
	return b.String()
}

func escape(s string) string {
	return strings.NewReplacer(`"`, "#quot;", "<", "#lt;", ">", "#gt;").Replace(s)
}
