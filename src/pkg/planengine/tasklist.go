package planengine

import (
	"fmt"
	"strings"
)

// RenderTaskList turns an exported plan into the ordered task-list text that
// planning.DecomposeFromOutput parses, so the engine's structure is admitted
// verbatim and no model is asked to redecompose it.
func RenderTaskList(plan Plan) string {
	var b strings.Builder
	for i, task := range plan.Tasks {
		ref := strings.TrimSpace(task.Ref)
		if ref == "" {
			ref = strings.TrimSpace(task.ID)
		}
		if ref == "" {
			ref = fmt.Sprintf("T%d", i+1)
		}
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, ref, strings.TrimSpace(task.Title))
		if repo := strings.TrimSpace(task.Repo); repo != "" {
			fmt.Fprintf(&b, " [repo:%s]", repo)
		}
		if len(task.DependsOn) > 0 {
			fmt.Fprintf(&b, " (depends: %s)", strings.Join(task.DependsOn, ", "))
		}
		execution := strings.TrimSpace(task.Execution)
		if execution == "" {
			execution = "agent_suitable"
		}
		fmt.Fprintf(&b, " [%s]\n", execution)
	}
	return b.String()
}
