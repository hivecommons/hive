package retro

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
)

type AutonomyDecision struct {
	Repo        string
	Direction   string
	From        int
	To          int
	EvidenceIDs []string
	Reason      string
	At          time.Time
}

type AutonomyDecisionSink interface {
	RecordAutonomyDecision(AutonomyDecision)
}

type autonomyClock func() time.Time

type AutonomyPolicyEngine struct {
	cfg   *config.Config
	sink  AutonomyDecisionSink
	now   autonomyClock
	store *beads.Store
}

func NewAutonomyPolicyEngine(cfg *config.Config, store *beads.Store, sink AutonomyDecisionSink) *AutonomyPolicyEngine {
	return &AutonomyPolicyEngine{cfg: cfg, store: store, sink: sink, now: time.Now}
}

func (e *AutonomyPolicyEngine) SetClock(now func() time.Time) {
	if now != nil {
		e.now = now
	}
}

func (e *AutonomyPolicyEngine) Evaluate() []AutonomyDecision {
	if e == nil || e.cfg == nil || e.store == nil {
		return nil
	}
	policy := e.cfg.Autonomy
	if !policy.AutoPromote && !policy.AutoDemote {
		return nil
	}
	now := e.now().UTC()
	byRepo := map[string][]*beads.Bead{}
	for _, b := range e.store.List(beads.ListFilter{}) {
		if b == nil || b.Type != beads.TypeAdvisory || b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
			continue
		}
		if b.Meta(metadataAutonomyScopeType) != "repo" {
			continue
		}
		repo := strings.TrimSpace(b.Meta(metadataAutonomyScopeValue))
		if repo == "" {
			continue
		}
		if !isAutonomyPolicyPattern(b.Meta(metadataPattern)) {
			continue
		}
		byRepo[repo] = append(byRepo[repo], b)
	}
	var decisions []AutonomyDecision
	for repo, facts := range byRepo {
		sort.Slice(facts, func(i, j int) bool {
			ti := facts[i].CreatedAt.Time
			tj := facts[j].CreatedAt.Time
			if !ti.Equal(tj) {
				return ti.Before(tj)
			}
			return facts[i].ID < facts[j].ID
		})
		if d, ok := e.demoteDecision(policy, repo, facts, now); ok {
			decisions = append(decisions, d)
			e.record(d)
			continue
		}
		if d, ok := e.promoteDecision(policy, repo, facts, now); ok {
			decisions = append(decisions, d)
			e.record(d)
		}
	}
	return decisions
}

func (e *AutonomyPolicyEngine) demoteDecision(policy config.AutonomyConfig, repo string, facts []*beads.Bead, now time.Time) (AutonomyDecision, bool) {
	if !policy.AutoDemote || e.cfg.RepoACMMPinned(repo) {
		return AutonomyDecision{}, false
	}
	for i := len(facts) - 1; i >= 0; i-- {
		b := facts[i]
		if !matchesDemoteOn(policy.EffectiveDemoteOn(), b.Meta(metadataPattern)) {
			continue
		}
		if last, ok := e.cfg.RepoACMMLastAutomatic(repo); ok && !b.CreatedAt.Time.After(last.At) {
			continue
		}
		if alreadyUsed(e.cfg, repo, b.ID) {
			return AutonomyDecision{}, false
		}
		from := boundedLevel(e.cfg.EffectiveACMMLevelForRepo(repo))
		if from <= config.MinACMMLevel {
			return AutonomyDecision{}, false
		}
		return AutonomyDecision{
			Repo: repo, Direction: "demote", From: from, To: from - 1, At: now,
			EvidenceIDs: []string{b.ID},
			Reason:      fmt.Sprintf("autonomy %s finding %s", b.Meta(metadataPattern), b.ID),
		}, true
	}
	return AutonomyDecision{}, false
}

func (e *AutonomyPolicyEngine) promoteDecision(policy config.AutonomyConfig, repo string, facts []*beads.Bead, now time.Time) (AutonomyDecision, bool) {
	if !policy.AutoPromote || e.cfg.RepoACMMPinned(repo) {
		return AutonomyDecision{}, false
	}
	if last, ok := e.cfg.RepoACMMLastAutomatic(repo); ok && last.At.Add(time.Duration(policy.EffectiveCooldownDays())*24*time.Hour).After(now) {
		return AutonomyDecision{}, false
	}
	from := boundedLevel(e.cfg.EffectiveACMMLevelForRepo(repo))
	maxLevel := policy.EffectiveMaxLevel(e.cfg.ACMMLevelOrZero())
	if from >= maxLevel {
		return AutonomyDecision{}, false
	}
	threshold := policy.EffectivePromoteAfter()
	var streak []*beads.Bead
	if last, ok := e.cfg.RepoACMMLastAutomatic(repo); ok {
		for _, b := range facts {
			if !b.CreatedAt.Time.After(last.At) {
				continue
			}
			streak = appendPromotionStreak(streak, b)
		}
	} else {
		for _, b := range facts {
			streak = appendPromotionStreak(streak, b)
		}
	}
	if len(streak) < threshold {
		return AutonomyDecision{}, false
	}
	window := streak[len(streak)-threshold:]
	ids := make([]string, 0, len(window))
	for _, b := range window {
		ids = append(ids, b.ID)
	}
	return AutonomyDecision{
		Repo: repo, Direction: "promote", From: from, To: from + 1, At: now,
		EvidenceIDs: ids,
		Reason:      fmt.Sprintf("%d consecutive qualifying autonomy findings", threshold),
	}, true
}

func appendPromotionStreak(streak []*beads.Bead, b *beads.Bead) []*beads.Bead {
	switch b.Meta(metadataPattern) {
	case PatternPlanAcceptedFirstPass, PatternPRMergedNoRework:
		return append(streak, b)
	case PatternRunRolledBack, PatternPRReworkedAfterReview:
		return nil
	default:
		return streak
	}
}

func (e *AutonomyPolicyEngine) record(d AutonomyDecision) {
	change := config.AutonomyLevelChange{
		At: d.At, Direction: d.Direction, From: d.From, To: d.To, Repo: d.Repo,
		EvidenceIDs: d.EvidenceIDs, Reason: d.Reason,
	}
	if _, err := e.cfg.SetRepoACMMAutomaticAndSave(d.Repo, d.To, change); err != nil {
		return
	}
	if e.sink != nil {
		e.sink.RecordAutonomyDecision(d)
	}
}

func isAutonomyPolicyPattern(pattern string) bool {
	switch pattern {
	case PatternPlanAcceptedFirstPass, PatternPRMergedNoRework, PatternPRReworkedAfterReview, PatternRunRolledBack:
		return true
	default:
		return false
	}
}

func matchesDemoteOn(mode, pattern string) bool {
	switch mode {
	case "either":
		return pattern == PatternRunRolledBack || pattern == PatternPRReworkedAfterReview
	case "rework":
		return pattern == PatternPRReworkedAfterReview
	default:
		return pattern == PatternRunRolledBack
	}
}

func alreadyUsed(cfg *config.Config, repo, id string) bool {
	last, ok := cfg.RepoACMMLastAutomatic(repo)
	if !ok {
		return false
	}
	for _, seen := range last.EvidenceIDs {
		if seen == id {
			return true
		}
	}
	return false
}

func boundedLevel(level int) int {
	if level < config.MinACMMLevel {
		return config.MinACMMLevel
	}
	if level > config.MaxACMMLevel {
		return config.MaxACMMLevel
	}
	return level
}
