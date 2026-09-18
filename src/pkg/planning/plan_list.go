package planning

import (
	"sort"

	"github.com/hivecommons/hive/pkg/beads"
)

// This file implements the plan-LISTING reader behind GET /api/plans: the
// dashboard's plan view needs to enumerate every plan across all bead stores
// before openPlanReview can drill into one, and until now no such reader
// existed — the PLANNING tile could only count plans (BuildPlanning), not name
// them (hivecommons/hive#7537).

// PlanSummary is one row of the dashboard plan list: an epic that has entered
// the planning flow (decomposed, or accepted and queued for the architect).
type PlanSummary struct {
	// EpicID is the epic bead's ID.
	EpicID string `json:"epicId"`
	// EpicTitle is the epic bead's title.
	EpicTitle string `json:"epicTitle"`
	// Agent is the name of the bead store holding the plan (the store key the
	// plan endpoints will locate it in).
	Agent string `json:"agent"`
	// PlanStatus is the epic's plan_status ("draft" / "approved"), or "" for an
	// accepted-but-not-yet-decomposed epic.
	PlanStatus string `json:"planStatus"`
	// PendingDecompose is true while the epic is queued for the architect
	// (children not yet materialized).
	PendingDecompose bool `json:"pendingDecompose"`
	// IssueRepo / IssueNumber / IssueURL trace an issue-sourced epic back to its
	// GitHub issue; zero values for bd-created epics.
	IssueRepo   string `json:"issueRepo,omitempty"`
	IssueNumber string `json:"issueNumber,omitempty"`
	IssueURL    string `json:"issueUrl,omitempty"`
	// ChildrenTotal / ChildrenOpen count the plan's child beads and how many of
	// them are still open or in progress.
	ChildrenTotal int `json:"childrenTotal"`
	// ChildrenOpen counts children with status open or in_progress.
	ChildrenOpen int `json:"childrenOpen"`
}

// listOrder ranks summaries so human-action-required plans surface first:
// drafts awaiting review, then epics queued for the architect, then approved
// (executing) plans.
func listOrder(p PlanSummary) int {
	switch {
	case p.PlanStatus == PlanStatusDraft && !p.PendingDecompose:
		return 0 // decomposed, awaiting human review
	case p.PendingDecompose:
		return 1 // queued for the architect
	default:
		return 2 // approved / executing
	}
}

// ListPlans enumerates every plan across the given bead stores: each epic bead
// that carries a plan_status (set at decompose time, and at mint time for
// issue-sourced epics). The result is ordered drafts-first (see listOrder),
// ties broken by title then ID so the listing is stable across refreshes.
func ListPlans(stores map[string]*beads.Store) []PlanSummary {
	var out []PlanSummary
	for name, store := range stores {
		if store == nil {
			continue
		}
		all := store.List(beads.ListFilter{})

		total := make(map[string]int)
		open := make(map[string]int)
		for _, b := range all {
			epicID := b.Meta(MetaParentEpic)
			if epicID == "" {
				continue
			}
			total[epicID]++
			if b.Status == beads.StatusOpen || b.Status == beads.StatusInProgress {
				open[epicID]++
			}
		}

		for _, b := range all {
			if b.Type != beads.TypeEpic || b.Meta(MetaPlanStatus) == "" {
				continue
			}
			out = append(out, PlanSummary{
				EpicID:           b.ID,
				EpicTitle:        b.Title,
				Agent:            name,
				PlanStatus:       b.Meta(MetaPlanStatus),
				PendingDecompose: DecomposePending(b),
				IssueRepo:        b.Meta(MetaIssueRepo),
				IssueNumber:      b.Meta(MetaIssueNumber),
				IssueURL:         b.Meta(MetaIssueURL),
				ChildrenTotal:    total[b.ID],
				ChildrenOpen:     open[b.ID],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := listOrder(out[i]), listOrder(out[j]); a != b {
			return a < b
		}
		if out[i].EpicTitle != out[j].EpicTitle {
			return out[i].EpicTitle < out[j].EpicTitle
		}
		return out[i].EpicID < out[j].EpicID
	})
	return out
}
