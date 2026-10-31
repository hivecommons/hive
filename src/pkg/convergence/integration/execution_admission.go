package integration

import (
	"fmt"
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forgestate"
)

type ExecutionMember struct {
	PR                    int
	Head                  string
	WorkKey               string
	Eligible              bool
	DependenciesKnown     bool
	DependenciesSatisfied bool
	ScopeKnown            bool
	ReadyAt               time.Time
	Priority              int
	IntentGeneration      uint64
	PlanRevision          int
}

// ExecutionAdmissionInput is obtained outside the ledger lock from real live
// guards, authoritative ownership, qualified workflow cohorts and current refs.
type ExecutionAdmissionInput struct {
	Repo, Base, BaseSHA   string
	ExecutorID            string
	AuthorityVerified     bool
	QualificationVerified bool
	NativeSelected        bool
	Members               []ExecutionMember
	Cost                  Cost
	Now                   time.Time
}

type ExecutionAdmissionDecision struct {
	Allowed         bool           `json:"allowed"`
	WouldBlock      bool           `json:"would_block"`
	Reason          string         `json:"reason"`
	EffectiveWriter string         `json:"effective_writer"`
	Plan            *ExecutionPlan `json:"plan,omitempty"`
}

func executionBlocked(cfg config.IntegrationExecutionConfig, reason string) ExecutionAdmissionDecision {
	return ExecutionAdmissionDecision{Allowed: cfg.Effective().Mode != config.IntegrationEnforce,
		WouldBlock: true, Reason: reason, EffectiveWriter: "observer"}
}

// PlanExecution reserves only the new layer's bounded CI work. It does not
// assume disjoint files are independent or acquire file-exclusive locks.
func (s *Store) PlanExecution(cfg config.IntegrationExecutionConfig, in ExecutionAdmissionInput) (ExecutionAdmissionDecision, error) {
	cfg = cfg.Effective()
	if cfg.Mode == config.IntegrationOff {
		return ExecutionAdmissionDecision{Allowed: true, Reason: "execution OFF; existing flow unchanged", EffectiveWriter: "legacy"}, nil
	}
	if err := cfg.Validate(); err != nil {
		return executionBlocked(cfg, err.Error()), err
	}
	scope, enrolled := cfg.Scope(in.Repo, in.Base)
	if !enrolled {
		return ExecutionAdmissionDecision{Allowed: true, Reason: "destination not enrolled", EffectiveWriter: "legacy"}, nil
	}
	if scope.Authority == "native" {
		if !in.NativeSelected {
			return executionBlocked(cfg, "native-selected scope has no native authority; no direct fallback"), nil
		}
		if !in.Cost.Complete {
			return executionBlocked(cfg, "native admission observations incomplete; no queue/build-window control claimed"), nil
		}
		if !executionCostKnown(in.Cost) || in.Cost.Running >= cfg.MaxActive ||
			in.Cost.Running+scope.ExpectedJobs > cfg.MaxJobs || *in.Cost.RunnerMinutes+scope.ExpectedMinutes > cfg.MaxRunnerMinutes {
			return executionBlocked(cfg, "native Hive admission budget exhausted; GitHub controls active queue groups"), nil
		}
		return ExecutionAdmissionDecision{Allowed: true, Reason: "bounded Hive enqueue admission; GitHub retains queue/promotion authority", EffectiveWriter: "github-native"}, nil
	}
	if in.NativeSelected {
		return executionBlocked(cfg, "native selection forbids Hive promotion fallback"), nil
	}
	if in.ExecutorID != scope.ExecutorID || !in.AuthorityVerified {
		return executionBlocked(cfg, "not the registered designated executor; observer-only, no TTL takeover"), nil
	}
	if !in.QualificationVerified {
		return executionBlocked(cfg, "actual actor/branch-protection qualification unavailable"), nil
	}
	if in.Now.IsZero() || !fullSHA(in.BaseSHA) {
		return executionBlocked(cfg, "current exact destination unknown"), nil
	}
	if !executionCostKnown(in.Cost) {
		return executionBlocked(cfg, "authoritative job telemetry incomplete; growth frozen, unknown is not zero"), nil
	}

	eligible := make([]ExecutionMember, 0, len(in.Members))
	for _, member := range in.Members {
		if member.Eligible && member.DependenciesKnown && member.DependenciesSatisfied &&
			member.ScopeKnown && member.WorkKey != "" && member.PR > 0 && fullSHA(member.Head) && !member.ReadyAt.IsZero() {
			if member.IntentGeneration == 0 {
				continue
			}
			eligible = append(eligible, member)
		}
	}
	// Priority can move a member by at most the configured priority bounds in
	// minutes. Waiting age continues to grow; no unbounded priority lane exists.
	sort.SliceStable(eligible, func(i, j int) bool {
		priority := func(value int) int {
			if value < 0 {
				return 0
			}
			if value > config.MaxIntegrationExecutionPriority {
				return config.MaxIntegrationExecutionPriority
			}
			return value
		}
		a := eligible[i].ReadyAt.Add(-time.Duration(priority(eligible[i].Priority)) * time.Minute)
		b := eligible[j].ReadyAt.Add(-time.Duration(priority(eligible[j].Priority)) * time.Minute)
		if a.Equal(b) {
			return eligible[i].PR < eligible[j].PR
		}
		return a.Before(b)
	})
	if len(eligible) == 0 {
		return executionBlocked(cfg, "no live eligible approved/known-scope members"), nil
	}
	if len(eligible) > scope.MaxMembers {
		eligible = eligible[:scope.MaxMembers]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.writerLock()
	if err != nil {
		return executionBlocked(cfg, "execution journal unavailable"), err
	}
	defer unlock()
	state := s.executionData()
	reserved, activeJobs, active := 0.0, 0, 0
	for _, plan := range state.plans {
		if plan.ReservationsReleased {
			continue
		}
		if plan.State == Unknown || plan.State == Waiting || plan.State == Running {
			// Bounds are global across the explicitly enrolled destinations, not
			// independently reusable in every repository. Removed scopes retain
			// observed cost but no longer reserve new-layer admission slots.
			profile, enrolled := cfg.Scope(plan.Recipe.Repo, plan.Recipe.Base)
			if !enrolled {
				continue
			}
			active += len(plan.Recipe.Members)
			activeJobs += len(plan.Recipe.Members) * profile.ExpectedJobs
			reserved += float64(len(plan.Recipe.Members)) * profile.ExpectedMinutes
		}
	}
	capacity := 0
	for capacity < len(eligible) {
		demand := capacity + 1
		if active+demand > cfg.MaxActive || in.Cost.Running+activeJobs+demand*scope.ExpectedJobs > cfg.MaxJobs ||
			*in.Cost.RunnerMinutes+reserved+float64(demand)*scope.ExpectedMinutes > cfg.MaxRunnerMinutes {
			break
		}
		capacity++
	}
	if capacity == 0 {
		return executionBlocked(cfg, "static candidate/job/runner-minute budget exhausted; no adaptive or live-pilot growth"), nil
	}
	eligible = eligible[:capacity]
	recipe := ExecutionRecipe{Repo: scope.Repo, Base: scope.Base, BaseSHA: in.BaseSHA, Policy: scope.Policy, WorkflowTree: scope.WorkflowTree}
	keys := make([]string, 0, len(eligible))
	bindings := make([]ExecutionWorkBinding, 0, len(eligible))
	for _, member := range eligible {
		recipe.Members = append(recipe.Members, forgestate.IntegrationCandidateMember{PR: member.PR, Head: member.Head})
		keys = append(keys, member.WorkKey)
		bindings = append(bindings, ExecutionWorkBinding{Key: member.WorkKey, IntentGeneration: member.IntentGeneration, PlanRevision: member.PlanRevision})
	}
	if err := recipe.Validate(); err != nil {
		return executionBlocked(cfg, err.Error()), err
	}
	if _, exists := state.plans[recipe.ID()]; exists {
		return executionBlocked(cfg, "exact recipe already owned/observed; ambiguous requests are not redispatched"), nil
	}
	plan := ExecutionPlan{Recipe: recipe, Generation: 1, CreatedAt: in.Now, ObservedAt: in.Now,
		State: Waiting, Reason: "qualified exact-prefix preparation admitted", WorkKeys: keys,
		ExecutorID: in.ExecutorID, AuthorityCommit: scope.AuthorityCommit, Attempt: 1, AttemptAt: in.Now, WorkBindings: bindings}
	decision := ExecutionAdmissionDecision{Allowed: true, Reason: plan.Reason, EffectiveWriter: "hive-designated", Plan: &plan}
	if cfg.Mode == config.IntegrationAdvisory {
		decision.Reason, decision.EffectiveWriter = "advisory only: would prepare; no dispatch/promotion", "observer"
		return decision, nil
	}

	err = s.appendLocked(Event{Version: SchemaVersion, ID: "execution/plan/" + recipe.ID(), At: in.Now,
		Execution: &ExecutionEvent{Version: ExecutionSchemaVersion, Plan: &plan}})
	if err != nil {
		return executionBlocked(cfg, fmt.Sprintf("durable admission failed: %v", err)), err
	}
	return decision, nil
}
