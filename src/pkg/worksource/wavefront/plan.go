package wavefront

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/agentparse"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
)

// PlanOutput renders the graph in the already-structured task-list shape that
// planning.DecomposeFromOutput consumes, one line per node in wave order:
//
//  1. [<node>] <title> (depends: <dep>, <dep>) [agent_suitable]
//
// This is the whole "plan import": the graph IS the plan, so admitting it is a
// format conversion, not a decomposition. No prompt is built and no planner is
// invoked. Titles are flattened to one line; the node ID is the plan_ref.
func PlanOutput(g Graph) (string, error) {
	waves, err := g.Waves()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Imported Wavefront plan %s@%s\n", g.Name, g.Revision)
	i := 0
	for _, wave := range waves {
		for _, n := range wave {
			i++
			title := strings.Join(strings.Fields(n.Title), " ")
			if title == "" {
				title = n.ID
			}
			fmt.Fprintf(&b, "%d. [%s] %s", i, n.ID, title)
			if len(n.DependsOn) > 0 {
				fmt.Fprintf(&b, " (depends: %s)", strings.Join(n.DependsOn, ", "))
			}
			fmt.Fprintf(&b, " [%s]\n", agentparse.ExecutionAgentSuitable)
		}
	}
	return b.String(), nil
}

// ImportPlan admits the graph into the beads store as the children of epic
// through planning.DecomposeFromOutput, with zero LLM calls. It refuses a
// graph whose revision is not expectedRevision (ErrStaleRevision), so a plan
// approved at one revision cannot be materialized from another. The returned
// result's Tasks carry the node IDs as Ref, in wave order.
func ImportPlan(store *beads.Store, epic *beads.Bead, g Graph, expectedRevision string, opts planning.Options) (*planning.Result, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	if g.Revision != expectedRevision {
		return nil, fmt.Errorf("%w: plan approved at %q, graph at %q", ErrStaleRevision, expectedRevision, g.Revision)
	}
	output, err := PlanOutput(g)
	if err != nil {
		return nil, err
	}
	res, err := planning.DecomposeFromOutput(store, epic, output, opts)
	if err != nil {
		return nil, err
	}
	if len(res.Tasks) != len(g.Nodes) {
		return nil, fmt.Errorf("wavefront: plan import produced %d tasks for %d nodes", len(res.Tasks), len(g.Nodes))
	}
	return res, nil
}
