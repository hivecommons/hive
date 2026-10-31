package dashboard

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

func (s *Server) SetIntegrationExecutionEvaluators(prepare, promote automerge.IntegrationEligibilityGate) {
	s.integration.mu.Lock()
	defer s.integration.mu.Unlock()
	s.integration.executionEvaluator = promote
	s.integration.executionPreparationEvaluator = prepare
}

func (s *Server) integrationExecutionStore() (*integration.Store, error) {
	s.integration.executionOnce.Do(func() {
		path := s.integration.executionPath
		if path == "" {
			path = filepath.Join(filepath.Dir(taskLeasesFile), "integration-execution-v2.jsonl")
		}
		s.integration.executionStore, s.integration.executionErr = integration.OpenExecution(path)
		if s.integration.executionErr == nil {
			s.integration.executionIdentity, s.integration.executionErr =
				integration.OpenExecutionIdentity(path + ".identity")
		}
		if s.integration.executionErr == nil {
			ledger, err := mutation.OpenLedgerScoped(path+".claims", 1, mutation.TargetWriterScope)
			if err != nil {
				s.integration.executionErr = err
				return
			}
			journal, err := mutation.OpenJournal(path + ".mutations")
			if err != nil {
				s.integration.executionErr = err
				return
			}
			s.integration.executionBoundary = &mutation.Boundary{
				Holder: s.integration.executionIdentity.ID, TTL: integrationReconcileTimeout,
				Executor: mutation.Executor{Ledger: ledger, Journal: journal, Mode: "enforce"},
				Mode: func() string {
					if s.integrationConfig().Execution.Effective().Mode == config.IntegrationEnforce {
						return "enforce"
					}
					return "off"
				},
			}
			s.integration.executionVerifier = ghpkg.NewIntegrationProducerVerifier()
		}
	})
	return s.integration.executionStore, s.integration.executionErr
}

func executionEnrolledScope(cfg config.IntegrationExecutionConfig, repo, base string) (config.IntegrationExecutionScope, bool) {
	for _, scope := range cfg.Scopes {
		if strings.EqualFold(scope.Repo, repo) && scope.Base == base {
			return scope, true
		}
	}
	return config.IntegrationExecutionScope{}, false
}

// IntegrationExecutionRoute runs only after a normal merge lane has passed its
// existing gates. Relays pass a nil live evaluator: they route but cannot create
// a false author/review receipt from their narrower contributor-only check.
func (s *Server) IntegrationExecutionRoute(_ context.Context, repo string, pr *gh.PullRequest, gate automerge.IntegrationEligibilityGate) (automerge.IntegrationExecutionDecision, error) {
	cfg := s.integrationConfig().Execution.Effective()
	if cfg.Mode == config.IntegrationOff || pr == nil {
		return automerge.IntegrationExecutionDecision{}, nil
	}
	scope, enrolled := executionEnrolledScope(cfg, repo, pr.GetBase().GetRef())
	if !enrolled {
		return automerge.IntegrationExecutionDecision{}, nil
	}
	block := func(reason string, err error) (automerge.IntegrationExecutionDecision, error) {
		if cfg.Mode != config.IntegrationEnforce {
			if err != nil {
				s.webhookLogger().Warn("advisory integration execution unavailable; existing flow unaffected", "reason", reason, "error", err)
			}
			return automerge.IntegrationExecutionDecision{}, nil
		}
		return automerge.IntegrationExecutionDecision{Authority: "hold", Reason: reason}, err
	}
	if err := cfg.Validate(); err != nil {
		return block("integration execution configuration invalid; no fallback", err)
	}
	if scope.Authority == "native" {
		if cfg.Mode == config.IntegrationAdvisory {
			return automerge.IntegrationExecutionDecision{}, nil
		}
		return automerge.IntegrationExecutionDecision{Authority: "native",
			Reason: "integration native authority selected; only ordinary bounded enqueue admission is permitted"}, nil
	}
	if s.nativeQueueScopeAllowed(scope.Repo, scope.Base) {
		return block("native authority selected; Hive execution has no direct fallback", nil)
	}
	store, err := s.integrationExecutionStore()
	if err != nil {
		return block("integration execution durable identity/ledger unavailable", err)
	}
	if gate != nil {
		now := time.Now()
		eligible := integration.ExecutionEligibility{Repo: scope.Repo, Base: scope.Base,
			PR: pr.GetNumber(), Head: pr.GetHead().GetSHA(), ReadyAt: now, ObservedAt: now,
			Source: "normal-automerge-lane-v1"}
		if intents, intentErr := s.integrationStore(); intentErr == nil {
			ambiguous := false
			var intent integration.Intent
			for _, current := range intents.Intents() {
				if strings.EqualFold(current.Repo, repo) && current.Base == scope.Base && current.Head == eligible.Head {
					if intent.Key != "" && intent.Key != current.Key {
						ambiguous = true
						break
					}
					intent = current
				}
			}
			if intent.Key != "" && !ambiguous {
				input := s.integrationInput(intent.Key, scope.Repo, scope.Base, now)
				eligible.WorkKey, eligible.IntentGeneration, eligible.PlanRevision = intent.Key, intent.Generation, intent.PlanRevision
				eligible.ScopeKnown = intent.ScopeKnown
				eligible.DependenciesKnown = !input.PlanRequired || input.PlanApproved
				eligible.DependenciesSatisfied = eligible.DependenciesKnown
				for _, prerequisite := range intent.Prerequisites {
					dependency, exists := input.Dependencies[prerequisite.Key]
					eligible.DependenciesKnown = eligible.DependenciesKnown && exists && dependency.Known
					eligible.DependenciesSatisfied = eligible.DependenciesSatisfied && exists && dependency.Known && dependency.Satisfied
				}
			}
		}
		if err := store.AppendExecution(fmt.Sprintf("execution/eligible/%s/%d", eligible.Key(), now.UnixNano()), now,
			integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Eligibility: &eligible}); err != nil {
			return block("integration normal-lane receipt unavailable; no fallback", err)
		}
	}
	if cfg.Mode == config.IntegrationAdvisory {
		return automerge.IntegrationExecutionDecision{}, nil
	}
	return automerge.IntegrationExecutionDecision{Authority: "hive",
		Reason: "integration-execution: normal gates passed; exact qualified-prefix authority owns promotion"}, nil
}
