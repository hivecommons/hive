package planning

import (
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// This file implements Phase 3 stall detection: the PURE, unit-testable core
// that finds approved plans whose execution has stalled. It has NO goroutines,
// NO agent/tmux dependency, and NO governor coupling — it operates entirely on a
// point-in-time snapshot of beads plus an injected `now`, so it can be tested
// with backdated timestamps. The eval-loop wiring (cadence, kicks, alerts) lives
// in replan.go / the ReplanLane and in cmd/hive/main.go.

// MetaReplanCount on an epic records how many times its plan has been
// re-decomposed / re-kicked by the stall-replan loop. It is a decimal string
// ("0", "1", ...). Absent means zero. It is capped by MaxReplans.
const MetaReplanCount = "replan_count"

// MetaLastReplanAt on an epic records the RFC3339 timestamp of the most recent
// replan. BuildPlanning reads it to compute replans_24h purely from bead
// metadata (no in-memory governor state needed).
const MetaLastReplanAt = "last_replan_at"

// MaxReplans caps how many times a single plan may be replanned before the loop
// stops and escalates to a human. It mirrors inception's maxKickRetries=5 so a
// stalled plan cannot thrash the architect indefinitely.
const MaxReplans = 5

// DefaultStallThreshold is the default age (since a plan last made progress)
// beyond which an approved plan with unfinished children is considered stalled.
// It is deliberately generous: a plan must be quiet for a while before we spend
// tokens re-kicking the architect.
const DefaultStallThreshold = 6 * time.Hour

// StallConfig tunes stall detection. The zero value is valid: it falls back to
// DefaultStallThreshold and MaxReplans.
type StallConfig struct {
	// StallThreshold is how long a plan may go without progress (no child
	// closing and no child update) before it is reported stalled. Zero means
	// DefaultStallThreshold.
	StallThreshold time.Duration
	// MaxReplans caps replans per epic. Zero means MaxReplans (the package
	// default). A plan already at or past the cap is still reported (so the
	// caller can escalate), with CapReached=true.
	MaxReplans int
}

func (c StallConfig) threshold() time.Duration {
	if c.StallThreshold > 0 {
		return c.StallThreshold
	}
	return DefaultStallThreshold
}

func (c StallConfig) maxReplans() int {
	if c.MaxReplans > 0 {
		return c.MaxReplans
	}
	return MaxReplans
}

// SnapBead is one bead in a stall-detection snapshot. It carries only the fields
// stall detection reads, with EXPORTED time fields so tests can construct
// snapshots with arbitrary (backdated) timestamps without touching the beads
// package's unexported time wrapper. SnapshotFromStore builds these from a live
// store in production.
type SnapBead struct {
	ID          string
	Title       string
	Type        beads.BeadType
	Status      beads.Status
	ParentEpic  string // parent_epic metadata ("" for non-children)
	PlanStatus  string // plan_status metadata (epics only)
	ReplanCount int    // replan_count metadata parsed to int (epics only)
	// UpdatedAt is the bead's last-update time.
	UpdatedAt time.Time
	// ClosedAt is the bead's close time, or the zero Time if still open.
	ClosedAt time.Time
}

// StalledPlan describes one approved plan whose execution has stalled. Remaining
// holds the still-open (or blocked) children — the unfinished work the caller
// may re-decompose or re-kick.
type StalledPlan struct {
	// EpicID is the stalled epic's bead ID.
	EpicID string
	// EpicTitle is the stalled epic's title.
	EpicTitle string
	// Remaining are the epic's children that are still open, in-progress, or
	// blocked (the unfinished work).
	Remaining []SnapBead
	// ReplanCount is the epic's replan_count BEFORE this detection cycle.
	ReplanCount int
	// CapReached is true when ReplanCount has already reached the configured
	// cap; the caller must escalate rather than replan again.
	CapReached bool
	// StalledFor is how long the plan has gone without progress at detection
	// time (now minus the most recent child activity).
	StalledFor time.Duration
}

// SnapshotFromStore extracts a stall-detection snapshot from a live beads store.
// It reads only the fields DetectStalledPlans needs. This is the ONLY place that
// touches the store; DetectStalledPlans itself stays pure over the snapshot.
func SnapshotFromStore(store *beads.Store) []SnapBead {
	if store == nil {
		return nil
	}
	all := store.List(beads.ListFilter{})
	out := make([]SnapBead, 0, len(all))
	for _, b := range all {
		sb := SnapBead{
			ID:         b.ID,
			Title:      b.Title,
			Type:       b.Type,
			Status:     b.Status,
			ParentEpic: b.Meta(MetaParentEpic),
			PlanStatus: b.Meta(MetaPlanStatus),
			UpdatedAt:  b.UpdatedAt.Time,
		}
		if b.ClosedAt != nil {
			sb.ClosedAt = b.ClosedAt.Time
		}
		sb.ReplanCount = parseReplanCount(b.Meta(MetaReplanCount))
		out = append(out, sb)
	}
	return out
}

// childActive reports whether a child bead still represents unfinished work
// (open, in-progress, or blocked). Done/closed children are finished.
func childActive(s beads.Status) bool {
	switch s {
	case beads.StatusOpen, beads.StatusInProgress, beads.StatusBlocked:
		return true
	default:
		return false
	}
}

// DetectStalledPlans finds approved plans whose execution has stalled as of
// `now`. A plan is stalled when ALL of:
//
//  1. the epic's plan_status is approved (a draft plan is gated, not stalled);
//  2. at least one child is still active (open/in_progress/blocked) — a fully
//     completed plan is done, not stalled;
//  3. no child has made progress within cfg.StallThreshold — "progress" is the
//     most recent child ClosedAt (a child finishing) or UpdatedAt (a child being
//     worked). If the whole child set has been quiet longer than the threshold,
//     the plan is stalled.
//
// It is a PURE function: no I/O, no goroutines, no store access. Callers pass a
// snapshot (SnapshotFromStore) and the current time. Determinism makes it fully
// table-testable with backdated timestamps.
func DetectStalledPlans(snap []SnapBead, now time.Time, cfg StallConfig) []StalledPlan {
	threshold := cfg.threshold()
	cap := cfg.maxReplans()

	// Index approved epics and bucket active children by parent.
	approvedEpics := make(map[string]SnapBead)
	for _, b := range snap {
		if b.Type == beads.TypeEpic && b.PlanStatus == PlanStatusApproved {
			approvedEpics[b.ID] = b
		}
	}
	if len(approvedEpics) == 0 {
		return nil
	}

	type acc struct {
		active     []SnapBead
		lastActive time.Time // most recent activity across ALL children
	}
	byEpic := make(map[string]*acc, len(approvedEpics))
	for _, b := range snap {
		if b.ParentEpic == "" {
			continue
		}
		if _, ok := approvedEpics[b.ParentEpic]; !ok {
			continue // child of a non-approved (or missing) epic
		}
		a := byEpic[b.ParentEpic]
		if a == nil {
			a = &acc{}
			byEpic[b.ParentEpic] = a
		}
		// Track the most recent activity of ANY child (closed or updated).
		if b.ClosedAt.After(a.lastActive) {
			a.lastActive = b.ClosedAt
		}
		if b.UpdatedAt.After(a.lastActive) {
			a.lastActive = b.UpdatedAt
		}
		if childActive(b.Status) {
			a.active = append(a.active, b)
		}
	}

	var out []StalledPlan
	for epicID, epic := range approvedEpics {
		a := byEpic[epicID]
		if a == nil || len(a.active) == 0 {
			continue // no children at all, or all children finished
		}
		// A plan with no recorded activity (lastActive zero) but active children
		// is treated as stalled from the epic's own update time is not available
		// here; the child snapshot always carries an UpdatedAt, so lastActive is
		// non-zero whenever children exist.
		stalledFor := now.Sub(a.lastActive)
		if stalledFor < threshold {
			continue // still making progress within the window
		}
		out = append(out, StalledPlan{
			EpicID:      epicID,
			EpicTitle:   epic.Title,
			Remaining:   a.active,
			ReplanCount: epic.ReplanCount,
			CapReached:  epic.ReplanCount >= cap,
			StalledFor:  stalledFor,
		})
	}

	// Deterministic order (by epic ID) so callers and tests see a stable list.
	sortStalledByID(out)
	return out
}

// sortStalledByID sorts stalled plans by epic ID for deterministic output.
func sortStalledByID(sp []StalledPlan) {
	for i := 1; i < len(sp); i++ {
		for j := i; j > 0 && sp[j-1].EpicID > sp[j].EpicID; j-- {
			sp[j-1], sp[j] = sp[j], sp[j-1]
		}
	}
}

// parseReplanCount parses a replan_count metadata string to an int. A missing,
// empty, or malformed value is zero.
func parseReplanCount(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0 // any non-digit → treat as unset/malformed
		}
		n = n*10 + int(r-'0')
	}
	return n
}
