package stageengine

import (
	"fmt"
	"sort"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Edge is one from→to stage pair.
type Edge struct {
	From, To stagelabels.Stage
}

// Expected reports whether from→to is a transition the pipeline model
// anticipates. It is consulted AFTER derivation and never changes the
// Decision: an unexpected pair increments
// stage_transition_anomaly_total{kind,from,to} and writes one audit line
// ("stage: unexpected <from>→<to> on <ref>: <reason>"), then the derived
// stage is applied as usual. Reconcile-from-truth therefore always wins.
//
// The table is the mermaid state diagram in
// docs/labels-and-control-signals/stage-labels.md (DiagramEdges) plus the Store's
// IsRegression semantics: `None → *`, `* → None` and a no-op are always
// expected; a hop between two stages the kind can carry is expected when the
// diagram draws it or when IsRegression does not count it as a fall (every
// forward hop, and a non-regression backward hop such as 090 → 072). A hop
// to or from a stage the kind cannot carry is never expected.
func Expected(kind stagelabels.Item, from, to stagelabels.Stage) bool {
	if from == stagelabels.None || to == stagelabels.None || from == to {
		return true
	}
	if !from.Applies(kind) || !to.Applies(kind) {
		return false
	}
	if diagramEdges[Edge{from, to}] {
		return true
	}
	return !stagelabels.IsRegression(from, to)
}

// Anomaly explains why from→to is unexpected for kind, or "" when it is
// expected. The text is the audit-line reason.
func Anomaly(kind stagelabels.Item, from, to stagelabels.Stage) string {
	if Expected(kind, from, to) {
		return ""
	}
	switch {
	case !from.Applies(kind):
		return fmt.Sprintf("%s is not a %s stage", from, kind)
	case !to.Applies(kind):
		return fmt.Sprintf("%s is not a %s stage", to, kind)
	}
	return "regression the pipeline does not draw"
}

// DiagramEdges returns the from→to pairs drawn in the state diagram
// (terminal `[*]` edges excluded), sorted by from then to.
func DiagramEdges() []Edge {
	out := make([]Edge, 0, len(diagramEdges))
	for e := range diagramEdges {
		out = append(out, e)
	}
	sortEdges(out)
	return out
}

func sortEdges(edges []Edge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From.Number() != edges[j].From.Number() {
			return edges[i].From.Number() < edges[j].From.Number()
		}
		return edges[i].To.Number() < edges[j].To.Number()
	})
}

// StageByNumber resolves a Table number (10, 45, 85, 100 …) to its stage;
// ok is false for a number the Table does not define.
func StageByNumber(n int) (stagelabels.Stage, bool) {
	for _, d := range stagelabels.Table {
		if d.Number == n {
			return d.Stage, true
		}
	}
	return stagelabels.None, false
}
