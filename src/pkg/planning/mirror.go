package planning

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
)

// MirrorMarker opens every checklist comment the dashboard mirrors onto a
// plan's source issue, so a later pass can find and replace the comment
// instead of stacking a new one per approval.
const MirrorMarker = "<!-- hive-plan-mirror -->"

// MirrorChecklist renders the approved plan as a GitHub task list for the
// epic's source issue (hivecommons/hive#8011). It is what people who never
// open the dashboard see: which tasks the plan has, which are done, who is on
// each one and where its PR is. Closed children are ticked; a claimant and PR
// are appended when known. dashboardURL, when non-empty, links back to the
// plan review. The result is deterministic for a given tree so a mirror can be
// compared against the existing comment before re-posting.
func MirrorChecklist(tree *PlanTree, dashboardURL string) string {
	if tree == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(MirrorMarker)
	b.WriteString("\n### 📋 Hive plan: ")
	b.WriteString(strings.TrimSpace(tree.EpicTitle))
	b.WriteString("\n\n")
	done := 0
	for _, c := range tree.Children {
		closed := c.Status == beads.StatusDone || c.Status == beads.StatusClosed
		if closed {
			done++
			b.WriteString("- [x] ")
		} else {
			b.WriteString("- [ ] ")
		}
		if c.PlanRef != "" {
			b.WriteString("**" + c.PlanRef + "** ")
		}
		b.WriteString(strings.TrimSpace(c.Title))
		var tail []string
		if c.Execution == "human_required" {
			tail = append(tail, "human")
		}
		if c.ClaimedBy != "" {
			tail = append(tail, "@"+strings.TrimPrefix(c.ClaimedBy, "@"))
		}
		if c.PRURL != "" {
			tail = append(tail, c.PRURL)
		}
		if len(tail) > 0 {
			b.WriteString(" — " + strings.Join(tail, " · "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n%d/%d tasks done · plan `%s` %s", done, len(tree.Children), tree.EpicID, tree.PlanStatus)
	if dashboardURL != "" {
		fmt.Fprintf(&b, " · [review in the hive dashboard](%s)", dashboardURL)
	}
	b.WriteString("\n\n_Posted by hive when the plan was approved. Tasks are tracked as beads in the hive; this list is a snapshot, not the source of truth._\n")
	return b.String()
}
