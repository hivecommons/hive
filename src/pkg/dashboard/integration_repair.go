package dashboard

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/github"
)

// integrationFailureOwner enriches the existing owner-first repair route.
// The front is a service owner, not an assertion that it caused the failure.
func (s *Server) integrationFailureOwner(repo string, failure *github.WebhookCIFailure) (*github.CIFailurePR, *github.IntegrationFailureContext, string) {
	cfg := s.integrationConfig()
	if cfg.Mode != config.IntegrationEnforce || failure == nil || s.deps == nil || s.deps.GHClient == nil {
		return nil, nil, ""
	}
	store, err := s.integrationStore()
	if err != nil {
		return nil, nil, ""
	}
	var selected *integration.Evidence
	for _, receipt := range store.ForTestedHead(repo, failure.HeadSHA) {
		if receipt.Subject.Repo != repo || receipt.Subject.TestedSHA != failure.HeadSHA || !cfg.Allows(repo, receipt.Subject.Base) ||
			receipt.State == integration.Superseded || len(receipt.Subject.Members) == 0 {
			continue
		}
		if selected == nil || receipt.ObservedAt.After(selected.ObservedAt) {
			copyReceipt := receipt
			selected = &copyReceipt
		}
	}
	if selected == nil {
		return nil, nil, ""
	}
	subject := selected.Subject
	member := subject.Members[0]
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, nil, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), integrationReconcileTimeout)
	defer cancel()
	pr, _, err := s.deps.GHClient.GoGitHub().PullRequests.Get(ctx, parts[0], parts[1], member.PR)
	if err != nil || pr == nil || pr.GetState() != "open" || pr.GetHead().GetSHA() != member.Head || pr.GetBase().GetRef() != subject.Base {
		return nil, nil, ""
	}
	ref := &github.CIFailurePR{Number: member.PR, Author: pr.GetUser().GetLogin(), Title: pr.GetTitle(), URL: pr.GetHTMLURL(), BaseRef: subject.Base,
		HiveAuthored: s.deps.GHClient.IsHiveAuthorLogin(pr.GetUser().GetLogin())}
	context := &github.IntegrationFailureContext{CandidateID: subject.ID(), TestedSHA: subject.TestedSHA, TestedTree: subject.TestedTree,
		BaseSHA: subject.BaseSHA, Policy: subject.Policy, Producer: subject.Producer, Attempt: subject.Attempt}
	for _, m := range subject.Members {
		context.Members = append(context.Members, fmt.Sprintf("%d:%s", m.PR, m.Head))
	}
	return ref, context, member.Head
}
