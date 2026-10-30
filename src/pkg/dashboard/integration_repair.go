package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	enrolled := false
	for _, scope := range cfg.Scopes {
		if strings.EqualFold(scope.Repo, repo) {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return nil, nil, ""
	}
	store, err := s.integrationStore()
	if err != nil {
		return nil, nil, ""
	}
	var selected *integration.Evidence
	now := time.Now()
	for _, receipt := range store.ForFailure(repo, failure.HeadSHA, failure.SuiteID, failure.CheckRunID) {
		if !strings.EqualFold(receipt.Subject.Repo, repo) || !cfg.Allows(repo, receipt.Subject.Base) ||
			receipt.State == integration.Superseded || receipt.State == integration.Unknown || receipt.Subject.Validate() != nil ||
			receipt.ObservedAt.After(now) || now.Sub(receipt.ObservedAt) > cfg.MaxObservationAge {
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
		BaseSHA: subject.BaseSHA, Policy: subject.Policy, Producer: subject.Producer, Attempt: subject.Attempt, RunID: selected.RunID, SuiteID: selected.SuiteID, Source: selected.Source}
	for _, m := range subject.Members {
		context.Members = append(context.Members, fmt.Sprintf("%d:%s", m.PR, m.Head))
	}
	return ref, context, member.Head
}

func (s *Server) integrationFailureIdentity(event string, body []byte, failure *github.WebhookCIFailure) error {
	if failure == nil || s.integrationConfig().Mode != config.IntegrationEnforce || event != "check_run" && event != "check_suite" {
		return nil
	}
	var payload struct {
		CheckRun struct {
			ID         int64 `json:"id"`
			CheckSuite struct {
				ID int64 `json:"id"`
			} `json:"check_suite"`
		} `json:"check_run"`
		CheckSuite struct {
			ID int64 `json:"id"`
		} `json:"check_suite"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	if event == "check_run" {
		failure.CheckRunID, failure.SuiteID = payload.CheckRun.ID, payload.CheckRun.CheckSuite.ID
	}
	if event == "check_suite" {
		failure.SuiteID = payload.CheckSuite.ID
	}
	return nil
}
