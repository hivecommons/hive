package dashboard

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	integrationExecutionScopesPerCycle     = 2
	integrationExecutionPreparationMembers = 8
	integrationExecutionProtocol           = "integration-prefix-v1"
	integrationExecutionPreparingSource    = "normal-lane-preparation-v1"
)

// ReconcileIntegrationExecution is independent of the legacy convergence flag.
// Its short lock reserves a bounded worker turn, never a remote-I/O section.
func (s *Server) ReconcileIntegrationExecution(ctx context.Context) error {
	cfg := s.integrationConfig().Execution.Effective()
	if cfg.Mode == config.IntegrationOff {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	now := time.Now()
	s.integration.mu.Lock()
	if s.integration.executionReconciling || now.Sub(s.integration.executionLastReconcile) < integrationReconcileInterval {
		s.integration.mu.Unlock()
		return nil
	}
	s.integration.executionReconciling, s.integration.executionLastReconcile = true, now
	offset := s.integration.executionScopeOffset
	s.integration.executionScopeOffset = (offset + integrationExecutionScopesPerCycle) % len(cfg.Scopes)
	s.integration.mu.Unlock()
	defer func() { s.integration.mu.Lock(); s.integration.executionReconciling = false; s.integration.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, integrationReconcileTimeout)
	defer cancel()
	s.integration.mu.Lock()
	s.integration.executionCancel = cancel
	s.integration.mu.Unlock()
	store, err := s.integrationExecutionStore()
	if err != nil {
		return err
	}
	defer func() { s.refreshExecutionTracker(store, cfg, time.Now()) }()
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return fmt.Errorf("integration execution observer has no live GitHub client")
	}
	reads := integrationReconcileMaxRuns
	var problems []error
	for index := 0; index < len(cfg.Scopes) && index < integrationExecutionScopesPerCycle; index++ {
		scope := cfg.Scopes[(offset+index)%len(cfg.Scopes)]
		if err := s.reconcileIntegrationExecutionScope(ctx, store, cfg, scope, now, &reads); err != nil {
			problems = append(problems, fmt.Errorf("%s#%s: %w", scope.Repo, scope.Base, err))
		}
	}
	return errors.Join(problems...)
}

func (s *Server) reconcileIntegrationExecutionScope(ctx context.Context, store *integration.Store, cfg config.IntegrationExecutionConfig, scope config.IntegrationExecutionScope, now time.Time, reads *int) error {
	status := integration.ExecutionStatus{Repo: scope.Repo, Base: scope.Base,
		ExecutorID: s.integration.executionIdentity.ID, DesiredMode: cfg.Mode,
		EffectiveWriter: "observer", SourceVersion: "unknown", RuntimeVersion: versionHash, ObservedAt: now}
	if scope.Authority == "native" {
		status.Reason = "native selection unavailable/unobserved; no direct fallback or new poller"
		s.nativeQueueTracker.mu.Lock()
		observed, known := s.nativeQueueTracker.observations[nativeQueueScopeKey(scope.Repo, scope.Base)]
		s.nativeQueueTracker.mu.Unlock()
		fresh := known && !observed.Stale && observed.EntriesComplete && !observed.ObservedAt.IsZero() && now.Sub(observed.ObservedAt) <= cfg.ObservationAge
		if fresh && s.nativeQueueScopeAllowed(scope.Repo, scope.Base) && cfg.Mode == config.IntegrationEnforce {
			status.EffectiveWriter, status.Reason = "github-native", "GitHub owns promotion and build windows; designated Hive admission uses existing native observations"
		}
		coverage := integration.Coverage{Repo: scope.Repo, Base: scope.Base, From: now.Add(-cfg.Window - cfg.ObservationAge), Through: now,
			Complete: true, Source: "execution-native-no-owned-producer-v2",
			Reason: "native profile cannot dispatch Hive producers; actual native jobs remain in authoritative author/native cohort"}
		if err := store.Append(integration.Event{Version: integration.SchemaVersion,
			ID: fmt.Sprintf("execution/coverage/%s/%s/%d", scope.Repo, scope.Base, now.UnixNano()), At: now, Coverage: &coverage}); err != nil {
			return err
		}
		return appendExecutionStatus(store, status)
	}
	if s.nativeQueueScopeAllowed(scope.Repo, scope.Base) {
		status.Reason = "native authority selected; Hive execution/promotion fallback forbidden"
		return appendExecutionStatus(store, status)
	}
	observationErr := s.observeExecutionPlans(ctx, store, cfg, scope, now, reads)
	authority, err := s.deps.GHClient.ReadIntegrationExecutionAuthority(ctx, scope, s.integration.executionIdentity.ID)
	if err != nil || !authority {
		status.Reason = "designated executor/immutable forge authority unavailable; no takeover"
		_ = appendExecutionStatus(store, status)
		return err
	}
	digest, qualified, err := s.integrationExecutionQualified(ctx, store, scope)
	if err != nil || !qualified {
		status.Reason = "actual protected actor/profile qualification unavailable"
		_ = appendExecutionStatus(store, status)
		return err
	}
	if cfg.Mode == config.IntegrationEnforce {
		if promoted, err := s.promoteExecutionReady(ctx, store, cfg, scope, digest, now); err != nil || promoted {
			status.Reason = "exact protected promotion reconciled; next admission waits for fresh destination/telemetry"
			if err != nil {
				status.Reason = err.Error()
			}
			status.EffectiveWriter = "hive-designated"
			_ = appendExecutionStatus(store, status)
			return err
		}
	}
	if observationErr != nil {
		status.Reason = "actual producer/job cohort incomplete; growth frozen: " + observationErr.Error()
		_ = appendExecutionStatus(store, status)
		return observationErr
	}
	if err := s.refreshExecutionEligibility(ctx, store, scope, now); err != nil {
		status.Reason = "normal lane authoring admission unknown: " + err.Error()
		_ = appendExecutionStatus(store, status)
		return err
	}
	if cfg.Mode == config.IntegrationEnforce {
		for _, projected := range store.ExecutionSnapshot().Plans {
			plan, exists := store.ExecutionPlan(projected.Recipe.ID())
			if !exists || plan.Recipe.Repo != scope.Repo || plan.Recipe.Base != scope.Base ||
				plan.ExecutorID != scope.ExecutorID || plan.AuthorityCommit != scope.AuthorityCommit {
				continue
			}
			unissued := plan.RequestDigest == "" && plan.State == integration.Waiting
			retry := plan.State == integration.Rejected && plan.DispatchOutcome == "not-applied" &&
				plan.Attempt < cfg.MaxAttempts && now.Sub(plan.ObservedAt) >= cfg.Cooldown
			if !unissued && !retry {
				continue
			}
			budget := s.integrationExecutionCost(cfg, store, now)
			if !budget.Complete || budget.RunnerMinutes == nil {
				break
			}
			resumed, err := store.ResumeExecution(cfg, plan.Recipe.ID(), budget, now)
			if err != nil {
				status.Reason = err.Error()
				_ = appendExecutionStatus(store, status)
				return nil
			}
			return s.dispatchExecutionPreparation(ctx, store, cfg, scope, resumed, digest, now)
		}
	}
	baseSHA, err := s.executionDestination(ctx, scope.Repo, scope.Base)
	if err != nil {
		return err
	}
	input := integration.ExecutionAdmissionInput{Repo: scope.Repo, Base: scope.Base, BaseSHA: baseSHA,
		ExecutorID: s.integration.executionIdentity.ID, AuthorityVerified: authority,
		QualificationVerified: qualified, Cost: s.integrationExecutionCost(cfg, store, now), Now: now}
	for _, eligible := range store.ExecutionSnapshot().Eligibility {
		if eligible.Repo == scope.Repo && eligible.Base == scope.Base && now.Sub(eligible.ObservedAt) <= cfg.ObservationAge {
			input.Members = append(input.Members, integration.ExecutionMember{
				PR: eligible.PR, Head: eligible.Head, WorkKey: eligible.WorkKey, Eligible: true,
				DependenciesKnown: eligible.DependenciesKnown, DependenciesSatisfied: eligible.DependenciesSatisfied,
				ScopeKnown: eligible.ScopeKnown, ReadyAt: eligible.ReadyAt,
				IntentGeneration: eligible.IntentGeneration, PlanRevision: eligible.PlanRevision,
			})
		}
	}
	decision, err := store.PlanExecution(cfg, input)
	status.Reason, status.EffectiveWriter = decision.Reason, decision.EffectiveWriter
	if err == nil && cfg.Mode == config.IntegrationEnforce && decision.Plan != nil {
		err = s.dispatchExecutionPreparation(ctx, store, cfg, scope, *decision.Plan, digest, now)
		if err != nil {
			status.Reason = err.Error()
		}
	}
	_ = appendExecutionStatus(store, status)
	return err
}

func appendExecutionStatus(store *integration.Store, status integration.ExecutionStatus) error {
	return store.AppendExecution(fmt.Sprintf("execution/status/%s/%s/%d", status.Repo, status.Base, status.ObservedAt.UnixNano()),
		status.ObservedAt, integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Status: &status})
}

func (s *Server) executionDestination(ctx context.Context, repo, base string) (string, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("execution destination repository invalid")
	}
	ref, _, err := s.deps.GHClient.GoGitHub().Git.GetRef(ctx, parts[0], parts[1], "heads/"+base)
	if err != nil {
		return "", err
	}
	if ref.GetObject().GetSHA() == "" {
		return "", fmt.Errorf("execution current destination unknown")
	}
	return ref.GetObject().GetSHA(), nil
}

func (s *Server) integrationExecutionQualified(ctx context.Context, store *integration.Store, scope config.IntegrationExecutionScope) (string, bool, error) {
	if err := s.deps.GHClient.VerifyIntegrationPromotionActor(scope.PromotionAppID, scope.PromotionInstallationID); err != nil {
		return "", false, err
	}
	digest, err := s.deps.GHClient.ReadIntegrationProtectionDigestForActor(ctx, scope.Repo, scope.Base, scope.PromotionAppID)
	if err != nil {
		return "", false, err
	}
	if scope.Purpose == "qualification" {
		comparison, err := s.deps.GHClient.ReadIntegrationProtectionDigestForActor(ctx, scope.Repo, scope.QualifiedBase, scope.PromotionAppID)
		if err != nil {
			return "", false, err
		}
		// This only permits the isolated one-member probe, not production.
		return digest, digest == comparison, nil
	}
	for _, observed := range store.ExecutionSnapshot().Qualifications {
		if observed.ID == scope.QualificationID && observed.Repo == scope.Repo && observed.Base == scope.Base &&
			observed.ExecutorID == scope.ExecutorID && observed.RequestActorID == scope.RequestActorID &&
			observed.ProfileDigest == scope.ProviderProfileDigest() &&
			observed.Policy == scope.Policy && observed.WorkflowTree == scope.WorkflowTree && observed.ProtectionDigest == digest {
			return digest, true, nil
		}
	}
	return digest, false, nil
}

func (s *Server) refreshExecutionEligibility(ctx context.Context, store *integration.Store, scope config.IntegrationExecutionScope, now time.Time) error {
	s.integration.mu.Lock()
	gate := s.integration.executionPreparationEvaluator
	s.integration.mu.Unlock()
	if gate == nil {
		return fmt.Errorf("complete normal boot lane preparation evaluator unavailable")
	}
	parts := strings.SplitN(scope.Repo, "/", 2)
	prs, response, err := s.deps.GHClient.GoGitHub().PullRequests.List(ctx, parts[0], parts[1],
		&gh.PullRequestListOptions{State: "open", Base: scope.Base, Sort: "created", Direction: "asc",
			ListOptions: gh.ListOptions{PerPage: integrationReconcilePageSize}})
	if err != nil {
		return err
	}
	if response != nil && response.NextPage != 0 {
		return fmt.Errorf("open eligible member discovery truncated")
	}
	// Older canonical work retains its scheduling age across observations.
	sort.SliceStable(prs, func(i, j int) bool { return prs[i].GetCreatedAt().Before(prs[j].GetCreatedAt().Time) })
	for _, candidate := range s.executionEligibilityTurn(scope, prs) {
		pr, _, err := gate(ctx, scope.Repo, scope.Base, candidate.GetNumber(), candidate.GetHead().GetSHA())
		if err != nil {
			return err
		}
		if pr == nil {
			continue
		}
		if err := s.recordExecutionEligibility(store, scope, pr, integrationExecutionPreparingSource, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) executionEligibilityTurn(scope config.IntegrationExecutionScope, prs []*gh.PullRequest) []*gh.PullRequest {
	if len(prs) == 0 {
		return nil
	}
	key := strings.ToLower(scope.Repo) + "#" + scope.Base
	s.integration.mu.Lock()
	if s.integration.executionEligibilityOffsets == nil {
		s.integration.executionEligibilityOffsets = map[string]int{}
	}
	if _, known := s.integration.executionEligibilityOffsets[key]; !known &&
		len(s.integration.executionEligibilityOffsets) >= config.MaxIntegrationScopes {
		s.integration.executionEligibilityOffsets = map[string]int{}
	}
	offset := s.integration.executionEligibilityOffsets[key] % len(prs)
	count := min(len(prs), integrationExecutionPreparationMembers)
	s.integration.executionEligibilityOffsets[key] = (offset + count) % len(prs)
	s.integration.mu.Unlock()
	out := make([]*gh.PullRequest, 0, count)
	for index := range count {
		out = append(out, prs[(offset+index)%len(prs)])
	}
	return out
}

func (s *Server) recordExecutionEligibility(store *integration.Store, scope config.IntegrationExecutionScope, pr *gh.PullRequest, source string, now time.Time) error {
	eligible := integration.ExecutionEligibility{Repo: scope.Repo, Base: scope.Base,
		PR: pr.GetNumber(), Head: pr.GetHead().GetSHA(), ReadyAt: now, ObservedAt: now, Source: source}
	intents, err := s.integrationStore()
	if err != nil {
		return err
	}
	var intent integration.Intent
	for _, current := range intents.Intents() {
		if strings.EqualFold(current.Repo, scope.Repo) && current.Base == scope.Base && current.Head == eligible.Head {
			if intent.Key != "" && intent.Key != current.Key {
				return fmt.Errorf("ambiguous canonical execution work")
			}
			intent = current
		}
	}
	if intent.Key != "" {
		input := s.integrationInput(intent.Key, scope.Repo, scope.Base, now)
		eligible.WorkKey, eligible.IntentGeneration, eligible.PlanRevision = intent.Key, intent.Generation, intent.PlanRevision
		eligible.ScopeKnown = intent.ScopeKnown && intent.PlanRevision == input.PlanRevision
		eligible.DependenciesKnown = input.PlanApproved && input.PlanRevision == intent.PlanRevision
		eligible.DependenciesSatisfied = eligible.DependenciesKnown
		for _, prerequisite := range intent.Prerequisites {
			dependency, exists := input.Dependencies[prerequisite.Key]
			eligible.DependenciesKnown = eligible.DependenciesKnown && exists && dependency.Known
			eligible.DependenciesSatisfied = eligible.DependenciesSatisfied && exists && dependency.Known && dependency.Satisfied
		}

	}
	return store.AppendExecution(fmt.Sprintf("execution/eligible/%s/%d", eligible.Key(), now.UnixNano()), now,
		integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Eligibility: &eligible})
}

func executionPreparationRequest(plan integration.ExecutionPlan) ghpkg.IntegrationPreparationRequest {
	return ghpkg.IntegrationPreparationRequest{Schema: 1, Protocol: integrationExecutionProtocol, Engine: "hive",
		Repo: plan.Recipe.Repo, Base: plan.Recipe.Base, BaseSHA: plan.Recipe.BaseSHA, Policy: plan.Recipe.Policy,
		Generation: int64(plan.Attempt), Members: plan.Recipe.Members, CreatedAt: plan.AttemptAt.UTC().Format(time.RFC3339Nano)}
}

func (s *Server) executionLiveAuthority(ctx context.Context, cfg config.IntegrationExecutionConfig, scope config.IntegrationExecutionScope, digest string) error {
	if s.nativeQueueScopeAllowed(scope.Repo, scope.Base) {
		return fmt.Errorf("native selection forbids Hive execution/promotion fallback")
	}
	current := s.integrationConfig().Execution.Effective()
	liveScope, exists := executionEnrolledScope(current, scope.Repo, scope.Base)
	if current.Mode != config.IntegrationEnforce || !exists || current.Validate() != nil ||
		!reflect.DeepEqual(liveScope, scope) || !reflect.DeepEqual(current, cfg) {
		return fmt.Errorf("live execution mode/profile changed")
	}
	verified, err := s.deps.GHClient.ReadIntegrationExecutionAuthority(ctx, scope, s.integration.executionIdentity.ID)
	if err != nil {
		return err
	}
	if !verified {
		return fmt.Errorf("shared designated execution authority changed")
	}
	liveDigest, qualified, err := s.integrationExecutionQualified(ctx, s.integration.executionStore, scope)
	if err != nil {
		return err
	}
	if !qualified || liveDigest != digest {
		return fmt.Errorf("live protection/actor qualification changed")
	}
	return nil
}

func (s *Server) dispatchExecutionPreparation(ctx context.Context, store *integration.Store, cfg config.IntegrationExecutionConfig, scope config.IntegrationExecutionScope, plan integration.ExecutionPlan, protection string, now time.Time) error {
	request := executionPreparationRequest(plan)
	raw, digest, err := ghpkg.EncodeIntegrationPreparation(request)
	if err != nil {
		return err
	}
	plan.RequestDigest, plan.MutationKey = digest, "preparation/"+digest
	plan.Generation++
	plan.ObservedAt = now
	plan.State, plan.Reason = integration.Unknown, "durable pre-effect preparation intent; no timeout/TTL redispatch"
	if err := appendExecutionPlan(store, plan); err != nil {
		return err
	}
	_, dispatchErr := s.deps.GHClient.DispatchIntegrationPreparation(ctx, request, raw, digest, plan.ExecutorID,
		plan.AuthorityCommit, s.integration.executionBoundary, func(ctx context.Context) error {
			if err := s.executionLiveAuthority(ctx, cfg, scope, protection); err != nil {
				return err
			}
			base, err := s.executionDestination(ctx, scope.Repo, scope.Base)
			if err != nil {
				return err
			}
			if base != plan.Recipe.BaseSHA {
				return fmt.Errorf("preparation destination moved")
			}
			if err := s.executionLiveWork(plan, time.Now()); err != nil {
				return err
			}
			for _, member := range plan.Recipe.Members {
				s.integration.mu.Lock()
				gate := s.integration.executionPreparationEvaluator
				s.integration.mu.Unlock()
				if gate == nil {
					return fmt.Errorf("live full preparation evaluator unavailable")
				}
				if pr, reason, err := gate(ctx, scope.Repo, scope.Base, member.PR, member.Head); err != nil || pr == nil {
					return fmt.Errorf("preparation live member %d refused (%s): %w", member.PR, reason, err)
				}
			}
			return nil
		})
	plan.Generation++
	plan.ObservedAt = time.Now()
	switch {
	case dispatchErr == nil:
		plan.State, plan.Reason = integration.Waiting, "transport accepted; actual materializer observation pending"
		plan.DispatchOutcome = "accepted"
	case mutation.DefinitelyNotApplied(dispatchErr):
		plan.State, plan.Reason = integration.Rejected, "definitely not applied; owner-first repair before bounded retry: "+dispatchErr.Error()
		plan.DispatchOutcome = "not-applied"
	default:
		plan.State, plan.Reason = integration.Unknown, "preparation outcome ambiguous; observer-only, no TTL steal/blind repeat: "+dispatchErr.Error()
		plan.DispatchOutcome = "unknown"
	}
	if err := appendExecutionPlan(store, plan); err != nil {
		return err
	}
	return dispatchErr
}

func appendExecutionPlan(store *integration.Store, plan integration.ExecutionPlan) error {
	return store.AppendExecution(fmt.Sprintf("execution/plan/%s/%d", plan.Recipe.ID(), plan.Generation), plan.ObservedAt,
		integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Plan: &plan})
}
