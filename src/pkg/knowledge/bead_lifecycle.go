package knowledge

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

const (
	retentionBaseScore   = 100
	retentionCritBonus   = 50
	retentionHighBonus   = 30
	retentionMedBonus    = 10
	retentionBugBonus    = 20
	retentionDecBonus    = 15
	retentionAdvBonus    = 10
	retentionFeatBonus   = 5
	retentionChorePenalty = 10
	retentionDepPinBonus = 100
	retentionAgeDayDecay = 2
	retentionSynthDecay  = 30
	retentionArchiveThreshold = 0
	retentionHeadroomRatio    = 0.8

	defaultMaxBeads               = 5000
	defaultArchiveAfterSynthDays  = 7
	defaultHighPriorityRetainDays = 30
)

// BeadLifecycleManager applies signal-based archival to bead stores,
// replacing the simple oldest-first eviction with retention scoring.
type BeadLifecycleManager struct {
	stores map[string]*beads.Store
	policy *RetentionPolicy
	logger *slog.Logger
}

// NewBeadLifecycleManager creates a lifecycle manager with the given policy.
func NewBeadLifecycleManager(
	stores map[string]*beads.Store,
	policy *RetentionPolicy,
	logger *slog.Logger,
) *BeadLifecycleManager {
	if policy.MaxBeads == 0 {
		policy.MaxBeads = defaultMaxBeads
	}
	if policy.ArchiveAfterSynthDays == 0 {
		policy.ArchiveAfterSynthDays = defaultArchiveAfterSynthDays
	}
	if policy.HighPriorityRetainDays == 0 {
		policy.HighPriorityRetainDays = defaultHighPriorityRetainDays
	}
	return &BeadLifecycleManager{
		stores: stores,
		policy: policy,
		logger: logger,
	}
}

type scoredBead struct {
	bead      *beads.Bead
	agent     string
	score     float64
}

// RunCulling scores all closed/done beads and archives those below the
// retention threshold, stopping when counts drop to 80% of MaxBeads.
func (lm *BeadLifecycleManager) RunCulling(_ context.Context) (int, error) {
	now := time.Now().UTC()
	target := int(math.Floor(float64(lm.policy.MaxBeads) * retentionHeadroomRatio))

	var scored []scoredBead

	for agentName, store := range lm.stores {
		if store.Count() <= target {
			continue
		}

		for _, b := range store.AllClosed() {
			s := lm.retentionScore(b, agentName, now)
			scored = append(scored, scoredBead{bead: b, agent: agentName, score: s})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})

	archived := 0
	for _, sb := range scored {
		if sb.score > retentionArchiveThreshold {
			break
		}

		store := lm.stores[sb.agent]
		if store.Count() <= target {
			continue
		}

		if err := store.Archive(sb.bead.ID); err != nil {
			lm.logger.Warn("failed to archive bead",
				"bead_id", sb.bead.ID, "agent", sb.agent, "error", err)
			continue
		}
		archived++
	}

	return archived, nil
}

// retentionScore computes how valuable a bead is to keep.
func (lm *BeadLifecycleManager) retentionScore(b *beads.Bead, agent string, now time.Time) float64 {
	score := float64(retentionBaseScore)

	switch b.Priority {
	case beads.PriorityCritical:
		score += retentionCritBonus
	case beads.PriorityHigh:
		score += retentionHighBonus
	case beads.PriorityMedium:
		score += retentionMedBonus
	}

	switch b.Type {
	case beads.TypeBug:
		score += retentionBugBonus
	case beads.TypeDecision:
		score += retentionDecBonus
	case beads.TypeAdvisory:
		score += retentionAdvBonus
	case beads.TypeFeature:
		score += retentionFeatBonus
	case beads.TypeChore:
		score -= retentionChorePenalty
	}

	if lm.policy.PreserveWithDeps && lm.hasOpenDependents(b, agent) {
		score += retentionDepPinBonus
	}

	closedAt := b.UpdatedAt.Time
	if b.ClosedAt != nil {
		closedAt = b.ClosedAt.Time
	}
	daysSinceClosed := now.Sub(closedAt).Hours() / 24
	score -= daysSinceClosed * retentionAgeDayDecay

	if b.Meta("synthesized_at") != "" {
		synthTime, err := time.Parse(time.RFC3339, b.Meta("synthesized_at"))
		if err == nil {
			daysSinceSynth := now.Sub(synthTime).Hours() / 24
			if daysSinceSynth >= float64(lm.policy.ArchiveAfterSynthDays) {
				score -= retentionSynthDecay
			}
		} else {
			score -= retentionSynthDecay
		}
	}

	return score
}

// hasOpenDependents checks if any bead in any store depends on this bead and is still open.
func (lm *BeadLifecycleManager) hasOpenDependents(b *beads.Bead, _ string) bool {
	for _, store := range lm.stores {
		for _, other := range store.AllClosed() {
			if other.Status == beads.StatusDone || other.Status == beads.StatusClosed {
				continue
			}
		}

		for _, dep := range b.DependsOn {
			for _, store2 := range lm.stores {
				if store2.HasOpenBead(dep) {
					return true
				}
			}
		}
	}
	return false
}
