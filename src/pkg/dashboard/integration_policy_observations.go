package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/github"
)

func integrationNativeMembershipCompatible(previous integration.Subject, current github.NativeIntegrationGroup) bool {
	if previous.TestedTree != current.Tree || previous.TestedSHA != current.HeadSHA || previous.Authority != current.HeadRef ||
		!strings.EqualFold(previous.Repo, current.Repo) || previous.Base != current.Base || len(current.Members) == 0 || len(current.Members) > len(previous.Members) {
		return false
	}
	offset := len(previous.Members) - len(current.Members)
	if offset == 0 && previous.BaseSHA != current.BaseSHA || offset > 0 && previous.Members[offset-1].HeadCommit != current.BaseSHA {
		return false
	}
	for index, member := range current.Members {
		old := previous.Members[offset+index]
		if old.PR != member.PR || old.Head != member.Head || old.Entry != member.Entry || member.Position != index+1 ||
			old.BaseCommit != member.BaseCommit || old.HeadCommit != member.HeadCommit {
			return false
		}
	}
	// A documented exact prefix promotion retains its original subject and
	// lineage. It does not copy an old receipt onto a member-removed subject.
	return true
}

func (s *Server) observeIntegrationDirectPolicyRun(parent context.Context, run *gh.WorkflowRun, repo, delivery string, now time.Time) error {
	if s.deps == nil || s.deps.GHClient == nil {
		return nil
	}
	cfg := s.integrationConfig()
	store, err := s.integrationStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, integrationReconcileTimeout)
	defer cancel()
	for _, policy := range cfg.Policies {
		if policy.Engine != "direct" || !strings.EqualFold(policy.Repo, repo) || len(run.PullRequests) != 1 ||
			run.PullRequests[0].GetBase().GetRef() != policy.Base || strings.Split(run.GetPath(), "@")[0] != policy.Workflow {
			continue
		}
		eventID := "policy/" + delivery + "/" + policy.Workflow
		if store.HasEvent(eventID) {
			continue
		}
		proof, proofErr := s.deps.GHClient.ReadIntegrationWorkflowProof(ctx, policy, run)
		if proofErr != nil {
			return proofErr
		}
		attestation, attestationErr := s.deps.GHClient.ReadDirectIntegrationAttestation(ctx, policy, run)
		if attestationErr != nil {
			return attestationErr
		}
		subject := integration.Subject{Version: integration.SchemaVersion, Repo: repo, Base: policy.Base, Engine: "direct", Authority: fmt.Sprintf("pull/%d", run.PullRequests[0].GetNumber()),
			BaseSHA: attestation.BaseSHA, TestedSHA: attestation.TestedSHA, TestedTree: attestation.TestedTree,
			Policy: policy.Fingerprint(), Producer: fmt.Sprintf("github-app/%d", policy.AppID), Workflow: fmt.Sprintf("%s@%d", policy.Workflow, run.GetWorkflowID()),
			WorkflowSourceRepo: run.GetHeadRepository().GetFullName(),
			Attempt:            run.GetRunAttempt(), Generation: uint64(run.GetID())}
		for _, member := range attestation.Members {
			subject.Members = append(subject.Members, integration.Member{PR: member.PR, Head: member.Head, Entry: fmt.Sprintf("pull/%d", member.PR), Position: 1})
		}
		state := integration.Running
		if proof.Passed {
			state = integration.Passed
		} else if proof.Rejected {
			state = integration.Rejected
		}
		observedAt := run.GetUpdatedAt().Time
		if observedAt.IsZero() {
			observedAt = now
		}
		receipt := integration.Evidence{Subject: subject, State: state, ObservedAt: observedAt, Source: "github-actions-attested-v1",
			RunID: proof.RunID, SuiteID: proof.SuiteID, CheckIDs: proof.CheckIDs, Reason: proof.Reason}
		if err := store.Append(integration.Event{Version: integration.SchemaVersion, ID: eventID, At: now, Evidence: &receipt}); err != nil {
			return err
		}
	}
	return nil
}
