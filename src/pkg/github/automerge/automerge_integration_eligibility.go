package automerge

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const integrationEligibilityCaptured = "integration eligibility captured without merge/enqueue"

// IntegrationExecutionDecision is routing, not replacement quality evidence.
// Empty preserves legacy behavior; native forbids a direct merge fallback.
type IntegrationExecutionDecision struct {
	Authority string
	Reason    string
}

type IntegrationEligibilityGate func(context.Context, string, string, int, string) (*gh.PullRequest, string, error)
type IntegrationExecutionHook func(context.Context, string, *gh.PullRequest, IntegrationEligibilityGate) (IntegrationExecutionDecision, error)

type integrationEligibilityContextKey struct{}

type integrationEligibilityCapture struct {
	repo, base, head string
	pr               *gh.PullRequest
}

type integrationEligibilityReadOnlyBoundary struct{}

func (integrationEligibilityReadOnlyBoundary) Execute(_ context.Context, claim effects.Claim, _ func(context.Context) (effects.Result, error)) (effects.Result, error) {
	return effects.Result{}, effects.NotApplied(fmt.Errorf("live integration eligibility cannot mutate %s", claim.Kind))
}

func integrationEligibilityProbe(ctx context.Context) bool {
	_, ok := ctx.Value(integrationEligibilityContextKey{}).(*integrationEligibilityCapture)
	return ok
}

func (c *Engine) integrationReadOnlyOptions() Options {
	options := c.integrationOptions
	options.MutationBoundary = integrationEligibilityReadOnlyBoundary{}
	options.StageTracker = NewStageTracker()
	options.BasePreparation = NewBasePreparationStore("")
	// Review requirements remain intact; a diagnostic probe must not enqueue
	// a planner repair or overwrite the real tracker/preparation observations.
	options.HumanChangeRequests = nil
	return options
}

// IntegrationEligibility executes the existing applicable lane's complete
// evaluation with a fresh check cohort. Its private stop occurs after all lane
// gates but before any merge/native enqueue. It does not treat the contributor-
// only relay guard as sufficient for App/trusted authors or issue a fake merge.
func (c *Engine) IntegrationEligibility(ctx context.Context, repo, base string, number int, head string) (*gh.PullRequest, string, error) {
	ctx = hgithub.WithIntegrationReadOnly(ctx)
	if !c.ready() {
		return nil, "integration lane evaluator unavailable", hgithub.ErrNoGitHubClient
	}
	// Do not mutate a shared engine's boundary or copy its mutexes. An isolated
	// evaluator retains the exact live policy callbacks and refuses preparation,
	// branch updates and repair effects while consulting the same lane gates.
	options := c.integrationReadOnlyOptions()
	c = New(c.transport, options)
	if number < 1 || head == "" || base == "" {
		return nil, "exact integration member missing", nil
	}
	owner, name := c.transport.SplitRepo(repo)
	slug := owner + "/" + name
	if !c.repoAutoMergeAllowed(repo) {
		return nil, nativeQueueReasonPaused, nil
	}
	active := false
	for _, watched := range c.activeRepos() {
		o, r := c.transport.SplitRepo(watched)
		if strings.EqualFold(o+"/"+r, slug) {
			active = true
			break
		}
	}
	if !active {
		return nil, nativeQueueReasonPaused, nil
	}
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:integration_live_lane_gate"), owner, name, number)
	if err != nil {
		return nil, "integration member unavailable", err
	}
	if pr.GetHead().GetSHA() != head || pr.GetBase().GetRef() != base ||
		pr.GetBase().GetRepo().GetFullName() != slug {
		return nil, "integration member head/base changed", nil
	}
	capture := &integrationEligibilityCapture{repo: slug, base: base, head: head}
	ctx = context.WithValue(contextWithExpectedCheckCache(ctx, newExpectedCheckCache()), integrationEligibilityContextKey{}, capture)
	var reason string
	queued := false
	for _, label := range pr.Labels {
		if strings.EqualFold(label.GetName(), hgithub.AutoMergeQueuedLabel) {
			queued = true
		}
	}
	switch {
	case queued:
		_, reason, err = c.trySweepQueuedPR(ctx, repo, owner, name, number, hgithub.AutoMergeQueuedLabel)
	default:
		author := hgithub.SafeGetLogin(pr.GetUser())
		if _, ownLane := c.sweepLaneForAuthor(author); ownLane {
			if options.IntegrationSelfAllowed == nil || !options.IntegrationSelfAllowed() {
				return nil, "normal self-authored flag/ACMM admission is disabled or unknown", nil
			}
			_, reason, err = c.trySweepSelfAuthoredPR(ctx, repo, owner, name, number, false)
		} else if policy := c.currentTrustedAuthorPolicy(); policy.repoAllowed(repo) {
			if decision := c.trustedAuthorDecision(author, policy.RequireRole); decision.Allowed {
				_, reason, err = c.trySweepTrustedAuthorPR(ctx, repo, owner, name, number, policy)
			} else {
				contributor := c.currentContributorLanePolicy()
				if !contributor.repoAllowed(repo) {
					return nil, "contributor lane disabled/not enrolled", nil
				}
				_, reason, err = c.trySweepContributorLanePR(ctx, repo, owner, name, number, contributor)
			}
		} else {
			policy := c.currentContributorLanePolicy()
			if !policy.repoAllowed(repo) {
				return nil, "contributor lane disabled/not enrolled", nil
			}
			_, reason, err = c.trySweepContributorLanePR(ctx, repo, owner, name, number, policy)
		}
	}
	if err != nil {
		return nil, reason, err
	}
	if capture.pr == nil || reason != integrationEligibilityCaptured {
		if reason == "" {
			reason = "normal lane did not authorize exact member"
		}
		return nil, reason, nil
	}
	if capture.pr.GetHead().GetSHA() != head {
		return nil, "", fmt.Errorf("captured integration head changed")
	}
	return capture.pr, "", nil
}
