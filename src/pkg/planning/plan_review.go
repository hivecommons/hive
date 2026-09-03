package planning

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/agentparse"
	"github.com/hivecommons/hive/pkg/beads"
)

// This file implements the Phase 2 plan-review gate transitions and readers.
// Phase 1 (decompose.go) drafts a plan (epic plan_status=draft, children tagged
// parent_epic + execution). beads.Store.Ready() withholds a draft epic's
// children. The functions here let a human inspect the plan (PlanTree), edit it
// before approval (RetagChild / RemoveChild), and approve or reject it
// (ApprovePlan / RejectPlan). Approval flips the epic to plan_status=approved,
// which releases the children through Ready().

// PlanChild summarizes one child bead of a plan for the review UI.
type PlanChild struct {
	// ID is the child bead's ID.
	ID string `json:"id"`
	// Title is the child bead's title.
	Title string `json:"title"`
	// Execution is the child's execution tag (agent_suitable / human_required).
	Execution string `json:"execution"`
	// PlanRef is the planner's local task ref (e.g. "T1"), if any.
	PlanRef string `json:"planRef,omitempty"`
	// Status is the child bead's current status.
	Status beads.Status `json:"status"`
	// DependsOn holds the child bead IDs this child depends on. These are the
	// intra-plan dependency edges for the review DAG.
	DependsOn []string `json:"dependsOn,omitempty"`
}

// PlanTree is the review view of a decomposed epic: the epic plus its children
// (in creation order) with their execution tags and dependency edges.
type PlanTree struct {
	// EpicID is the epic bead's ID.
	EpicID string `json:"epicId"`
	// EpicTitle is the epic bead's title.
	EpicTitle string `json:"epicTitle"`
	// PlanStatus is the epic's current plan_status ("draft" / "approved"), or
	// "" if the epic was never decomposed.
	PlanStatus string `json:"planStatus"`
	// Approved is true when PlanStatus == PlanStatusApproved.
	Approved bool `json:"approved"`
	// Children are the epic's child beads, in creation order.
	Children []PlanChild `json:"children"`
}

// loadEpic fetches an epic bead and verifies it is of type epic. It is the
// shared guard for the review transitions below.
func loadEpic(store *beads.Store, epicID string) (*beads.Bead, error) {
	if store == nil {
		return nil, fmt.Errorf("planning: store is nil")
	}
	epic, err := store.Get(epicID)
	if err != nil {
		return nil, fmt.Errorf("planning: epic %s not found: %w", epicID, err)
	}
	if epic.Type != beads.TypeEpic {
		return nil, fmt.Errorf("planning: bead %s is type %q, not an epic", epicID, epic.Type)
	}
	return epic, nil
}

// childrenOf returns the child beads of epicID (those tagged parent_epic ==
// epicID), in creation order.
func childrenOf(store *beads.Store, epicID string) []*beads.Bead {
	var out []*beads.Bead
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(MetaParentEpic) == epicID {
			out = append(out, b)
		}
	}
	return out
}

// PlanTree returns the review view of a decomposed epic. It errors if the epic
// does not exist or is not of type epic. An epic that was never decomposed
// returns an empty PlanStatus and no children (not an error).
func GetPlanTree(store *beads.Store, epicID string) (*PlanTree, error) {
	epic, err := loadEpic(store, epicID)
	if err != nil {
		return nil, err
	}
	status := epic.Meta(MetaPlanStatus)
	tree := &PlanTree{
		EpicID:     epic.ID,
		EpicTitle:  epic.Title,
		PlanStatus: status,
		Approved:   status == PlanStatusApproved,
	}
	for _, c := range childrenOf(store, epicID) {
		tree.Children = append(tree.Children, PlanChild{
			ID:        c.ID,
			Title:     c.Title,
			Execution: c.Meta(MetaExecution),
			PlanRef:   c.Meta(MetaPlanRef),
			Status:    c.Status,
			DependsOn: c.DependsOn,
		})
	}
	return tree, nil
}

// ApprovePlan approves a decomposed epic's plan by setting its plan_status to
// approved, which releases the epic's children through beads.Store.Ready(). It
// errors if the epic does not exist, is not an epic, was never decomposed
// (plan_status unset), or is already approved (so a double-approve is surfaced
// rather than silently succeeding).
func ApprovePlan(store *beads.Store, epicID string) error {
	epic, err := loadEpic(store, epicID)
	if err != nil {
		return err
	}
	switch epic.Meta(MetaPlanStatus) {
	case "":
		return fmt.Errorf("planning: epic %s has no plan to approve (not decomposed)", epicID)
	case PlanStatusApproved:
		return fmt.Errorf("planning: epic %s plan is already approved", epicID)
	}
	if err := store.SetMetadata(epicID, MetaPlanStatus, PlanStatusApproved); err != nil {
		return fmt.Errorf("planning: approving plan for epic %s: %w", epicID, err)
	}
	return nil
}

// RejectPlan returns a plan to draft state (plan_status=draft), re-gating the
// epic's children. It is the inverse of ApprovePlan — used when a reviewer
// approved by mistake or wants to force re-review after editing. It errors if
// the epic does not exist, is not an epic, or was never decomposed.
func RejectPlan(store *beads.Store, epicID string) error {
	epic, err := loadEpic(store, epicID)
	if err != nil {
		return err
	}
	if epic.Meta(MetaPlanStatus) == "" {
		return fmt.Errorf("planning: epic %s has no plan to reject (not decomposed)", epicID)
	}
	if err := store.SetMetadata(epicID, MetaPlanStatus, PlanStatusDraft); err != nil {
		return fmt.Errorf("planning: rejecting plan for epic %s: %w", epicID, err)
	}
	return nil
}

// RetagChild changes a child bead's execution tag before approval. execution
// must be agent_suitable or human_required. It errors if the child does not
// exist, is not a child of epicID, or the execution value is invalid.
func RetagChild(store *beads.Store, epicID, childID, execution string) error {
	switch execution {
	case agentparse.ExecutionAgentSuitable, agentparse.ExecutionHumanRequired:
		// ok
	default:
		return fmt.Errorf("planning: invalid execution %q (want %q or %q)",
			execution, agentparse.ExecutionAgentSuitable, agentparse.ExecutionHumanRequired)
	}
	if err := verifyChild(store, epicID, childID); err != nil {
		return err
	}
	if err := store.SetMetadata(childID, MetaExecution, execution); err != nil {
		return fmt.Errorf("planning: retagging child %s: %w", childID, err)
	}
	return nil
}

// RemoveChild removes a child bead from a plan before approval by closing it.
// The bead is closed rather than deleted so the audit trail (and any dependency
// references from siblings) is preserved. It errors if the child does not exist
// or is not a child of epicID.
func RemoveChild(store *beads.Store, epicID, childID string) error {
	if err := verifyChild(store, epicID, childID); err != nil {
		return err
	}
	if err := store.Close(childID); err != nil {
		return fmt.Errorf("planning: removing child %s: %w", childID, err)
	}
	return nil
}

// verifyChild confirms childID exists and is tagged parent_epic == epicID.
func verifyChild(store *beads.Store, epicID, childID string) error {
	if store == nil {
		return fmt.Errorf("planning: store is nil")
	}
	child, err := store.Get(childID)
	if err != nil {
		return fmt.Errorf("planning: child %s not found: %w", childID, err)
	}
	if child.Meta(MetaParentEpic) != epicID {
		return fmt.Errorf("planning: bead %s is not a child of epic %s", childID, epicID)
	}
	return nil
}
