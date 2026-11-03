package dashboard

import (
	"context"
	"fmt"
	"github.com/hivecommons/hive/pkg/github"
	"sort"
	"strings"
	"time"
)

func (s *Server) executionFailureOwner(repo string, failure *github.WebhookCIFailure) (*github.CIFailurePR, *github.IntegrationFailureContext, string) {
	cfg := s.integrationConfig().Execution.Effective()
	store, err := s.integrationExecutionStore()
	if err != nil {
		return nil, nil, ""
	}
	proofs := store.ExecutionSnapshot().Proofs
	sort.SliceStable(proofs, func(i, j int) bool { return proofs[i].ObservedAt.After(proofs[j].ObservedAt) })
	now := time.Now()
	for _, projection := range proofs {
		subject := projection.Subject
		scope, enrolled := cfg.Scope(repo, subject.Base)
		if !enrolled || scope.Authority != "hive" || !strings.EqualFold(subject.Repo, repo) || len(subject.Prefixes) == 0 ||
			subject.Prefixes[len(subject.Prefixes)-1].Commit != failure.HeadSHA || projection.ObservedAt.After(now) || now.Sub(projection.ObservedAt) > cfg.ObservationAge {
			continue
		}
		proof, ok := store.ExecutionProof(subject.ID, projection.RunID, projection.Attempt)
		if !ok || proof.Archive == nil {
			continue
		}
		bound := failure.SuiteID > 0 && failure.SuiteID == proof.Archive.SuiteID
		for _, id := range proof.Archive.CheckIDs {
			bound = bound || failure.CheckRunID > 0 && failure.CheckRunID == id
		}
		if !bound {
			continue
		}
		member := subject.Members[len(subject.Members)-1]
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) != 2 {
			return nil, nil, ""
		}
		ctx, cancel := context.WithTimeout(context.Background(), integrationReconcileTimeout)
		pr, _, err := s.deps.GHClient.GoGitHub().PullRequests.Get(ctx, parts[0], parts[1], member.PR)
		cancel()
		if err != nil || pr.GetState() != "open" || pr.GetHead().GetSHA() != member.Head || pr.GetBase().GetRef() != subject.Base {
			continue
		}
		evidence := &github.IntegrationFailureContext{CandidateID: subject.ID, TestedSHA: failure.HeadSHA,
			TestedTree: subject.Prefixes[len(subject.Prefixes)-1].Tree, BaseSHA: subject.BaseSHA, Policy: subject.Policy,
			Producer: fmt.Sprintf("%s#%d@%s", proof.Archive.WorkflowPath, proof.Archive.WorkflowID, proof.Archive.WorkflowSHA),
			Attempt:  proof.Attempt, RunID: proof.RunID, SuiteID: proof.Archive.SuiteID, Source: proof.Source}
		for _, m := range subject.Members {
			evidence.Members = append(evidence.Members, fmt.Sprintf("%d:%s", m.PR, m.Head))
		}
		ref := &github.CIFailurePR{Number: member.PR, Author: pr.GetUser().GetLogin(), Title: pr.GetTitle(), URL: pr.GetHTMLURL(), BaseRef: subject.Base,
			HiveAuthored: s.deps.GHClient.IsHiveAuthorLogin(pr.GetUser().GetLogin())}
		return ref, evidence, member.Head
	}
	return nil, nil, ""
}
