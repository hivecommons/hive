package automerge

import (
	"context"
	"fmt"
	gh "github.com/google/go-github/v72/github"
)

type integrationBaseEvidenceKey struct{}
type integrationPreparationProbeKey struct{}

// IntegrationPreparationEligibility does not authorize advancement. It runs
// the normal quality/lane gates read-only while leaving current-base testing
// to the candidate that preparation is about to construct.
func (c *Engine) IntegrationPreparationEligibility(ctx context.Context, repo, base string, number int, head string) (*gh.PullRequest, string, error) {
	ctx = context.WithValue(ctx, integrationPreparationProbeKey{}, true)
	return c.IntegrationEligibility(ctx, repo, base, number, head)
}

// IntegrationBaseEvidence is an exact combined-subject live verifier, not the
// convergence single-PR BaseSHA receipt or a copied green status. Only the
// integration controller supplies it after verified candidate observation.
type IntegrationBaseEvidence struct {
	Repo, Base, Head, ExpectedSHA, TestedSHA, SubjectID string
	PR                                                  int
	Verify                                              func(context.Context) error
}

func WithIntegrationBaseEvidence(ctx context.Context, evidence IntegrationBaseEvidence) context.Context {
	return context.WithValue(ctx, integrationBaseEvidenceKey{}, evidence)
}

func integrationCombinedBaseProvided(ctx context.Context) bool {
	if preparing, _ := ctx.Value(integrationPreparationProbeKey{}).(bool); preparing && integrationEligibilityProbe(ctx) {
		return true
	}
	_, exists := ctx.Value(integrationBaseEvidenceKey{}).(IntegrationBaseEvidence)
	return exists && integrationEligibilityProbe(ctx)
}

func integrationCombinedBaseCheck(ctx context.Context, repo, base string, number int, head string) (bool, error) {
	if preparing, _ := ctx.Value(integrationPreparationProbeKey{}).(bool); preparing && integrationEligibilityProbe(ctx) {
		return true, nil
	}
	evidence, exists := ctx.Value(integrationBaseEvidenceKey{}).(IntegrationBaseEvidence)
	if !exists {
		return false, nil
	}
	if !integrationEligibilityProbe(ctx) || evidence.Repo != repo || evidence.Base != base ||
		evidence.PR != number || evidence.Head != head || evidence.ExpectedSHA == "" ||
		evidence.TestedSHA == "" || evidence.SubjectID == "" || evidence.Verify == nil {
		return true, fmt.Errorf("combined current-base evidence does not bind the exact live integration member")
	}
	return true, evidence.Verify(ctx)
}
