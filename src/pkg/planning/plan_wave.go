package planning

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
)

// WaveText renders the plan for a reviewer (hivecommons/hive#8317): the epic
// on one line, then one line per planned item with its plan ref, execution
// tag, status, and dependencies. It is the reference the plan_match review
// perspective judges a PR's diff against, so it is deliberately a flat list of
// what was approved rather than the planner's prose -- a reviewer needs to
// tick items off, not re-read a design.
func (t *PlanTree) WaveText() string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	status := t.PlanStatus
	if status == "" {
		status = "not decomposed"
	}
	fmt.Fprintf(&b, "Plan: %s (%s, %s)\n", t.EpicTitle, t.EpicID, status)
	for _, c := range t.Children {
		b.WriteString("- ")
		if c.PlanRef != "" {
			fmt.Fprintf(&b, "[%s] ", c.PlanRef)
		}
		b.WriteString(c.Title)
		if c.Execution != "" {
			fmt.Fprintf(&b, " [%s]", c.Execution)
		}
		if c.Status != "" {
			fmt.Fprintf(&b, " (%s)", c.Status)
		}
		if len(c.DependsOn) > 0 {
			fmt.Fprintf(&b, " depends: %s", strings.Join(c.DependsOn, ", "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// FindPlanTree resolves a plan reference across every bead store. The ref is
// whatever a PR's Hive-Plan or Hive-Run trailer carried: an epic bead ID, or
// the "owner/repo#N" of the issue the plan was minted from. It returns false
// when nothing matches; a missing plan is the reviewer's to report, not this
// function's to invent.
func FindPlanTree(stores map[string]*beads.Store, ref string) (*PlanTree, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, false
	}
	for _, store := range stores {
		if store == nil {
			continue
		}
		if tree, err := GetPlanTree(store, ref); err == nil {
			return tree, true
		}
	}
	for _, p := range ListPlans(stores) {
		if p.IssueRepo == "" || p.IssueNumber == "" || p.IssueRepo+"#"+p.IssueNumber != ref {
			continue
		}
		store := stores[p.Agent]
		if store == nil {
			continue
		}
		if tree, err := GetPlanTree(store, p.EpicID); err == nil {
			return tree, true
		}
	}
	return nil, false
}
