package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
)

func integrationGovernanceHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func integrationGovernanceRow(pr *gh.PullRequest) []any {
	labels := make([]string, 0, len(pr.Labels))
	for _, label := range pr.Labels {
		labels = append(labels, integrationGovernanceHash(label.GetName()))
	}
	sort.Strings(labels)
	return []any{pr.GetNumber(), pr.GetHead().GetSHA(), pr.GetBase().GetRepo().GetFullName(),
		integrationGovernanceHash(pr.GetBase().GetRef()), pr.GetUser().GetID(), pr.GetUser().GetLogin(),
		pr.GetDraft(), integrationGovernanceHash(pr.GetTitle()), integrationGovernanceHash(pr.GetBody()), labels,
		integrationGovernanceHash(pr.GetHead().GetRef())}
}

// VerifyIntegrationCandidateGovernance revalidates mutable member policy, not
// author-head check results. State/base advancement from an exact documented
// prefix merge deliberately does not change the immutable metadata digest.
func (c *Client) VerifyIntegrationCandidateGovernance(ctx context.Context, subject forgestate.IntegrationCandidateSubject, payload []byte) error {
	var receipt integrationCandidateReceipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return err
	}
	if receipt.Schema == 1 {
		return nil // Historical v1 remains readable; new protected names require v2.
	}
	if receipt.Schema != 2 || receipt.Protocol != "integration-producer-receipt-v2" ||
		!receipt.Subject.SameCore(subject) || !forgestate.IntegrationContextsPassed(receipt.Contexts, receipt.MetadataDigest) {
		return fmt.Errorf("complete signed candidate governance/context binding unavailable")
	}
	if c == nil || c.client == nil || len(subject.Members) == 0 || len(subject.Members) > integrationCandidateMaxMembers ||
		len(subject.Members) != len(subject.Prefixes) {
		return fmt.Errorf("bounded candidate governance reader unavailable")
	}
	parts := strings.SplitN(subject.Repo, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid candidate governance repository")
	}
	rows := make([][]any, 0, len(subject.Members))
	for index, member := range subject.Members {
		pr, _, err := c.client.PullRequests.Get(ctx, parts[0], parts[1], member.PR)
		if err != nil {
			return err
		}
		if pr == nil || pr.GetNumber() != member.PR || pr.GetHead().GetSHA() != member.Head ||
			pr.GetHead().GetRepo().GetFullName() != subject.Repo || pr.GetBase().GetRepo().GetFullName() != subject.Repo ||
			pr.GetBase().GetRef() != subject.Base || pr.GetDraft() || pr.GetUser().GetID() <= 0 || pr.GetUser().GetLogin() == "" ||
			!(pr.GetState() == "open" || pr.GetMerged() && pr.GetMergeCommitSHA() == subject.Prefixes[index].Commit) {
			return fmt.Errorf("candidate governance member %d changed or is not its exact known prefix", member.PR)
		}
		rows = append(rows, integrationGovernanceRow(pr))
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if integrationGovernanceHash(string(raw)) != receipt.MetadataDigest {
		return fmt.Errorf("candidate member title/body/labels/author/head policy changed; old proof cannot authorize promotion")
	}
	return nil
}

func bindCandidateContextReceipt(receipt integrationCandidateReceipt, run *gh.WorkflowRun, jobs []*gh.WorkflowJob, checks []*gh.CheckRun, appID int64) error {
	if receipt.Schema == 1 {
		return nil
	}
	if len(receipt.Contexts) != len(forgestate.IntegrationRequiredContexts()) {
		return fmt.Errorf("signed required context cohort incomplete")
	}
	seenJobs, seenChecks := map[int64]bool{}, map[int64]bool{}
	for _, name := range forgestate.IntegrationRequiredContexts() {
		record, exists := receipt.Contexts[name]
		if !exists || record.JobID <= 0 || record.CheckID <= 0 || record.AppID != appID ||
			record.SuiteID != run.GetCheckSuiteID() || seenJobs[record.JobID] || seenChecks[record.CheckID] {
			return fmt.Errorf("signed context %q producer or physical identity differs", name)
		}
		seenJobs[record.JobID], seenChecks[record.CheckID] = true, true
		var actual *gh.WorkflowJob
		for _, job := range jobs {
			if job.GetName() == name {
				if actual != nil {
					return fmt.Errorf("duplicate actual protected context %q", name)
				}
				actual = job
			}
		}
		if actual == nil || actual.GetID() != record.JobID || actual.GetRunID() != run.GetID() ||
			actual.GetHeadSHA() != run.GetHeadSHA() || actual.GetStatus() != "completed" ||
			actual.GetConclusion() != record.Conclusion || !strings.HasSuffix(actual.GetCheckRunURL(), "/"+fmt.Sprint(record.CheckID)) {
			return fmt.Errorf("signed context %q differs from actual exact-run job", name)
		}
		found := false
		for _, check := range checks {
			if check.GetID() == record.CheckID && check.GetName() == name &&
				check.GetHeadSHA() == run.GetHeadSHA() && check.GetCheckSuite().GetID() == record.SuiteID &&
				check.GetApp().GetID() == appID && check.GetStatus() == "completed" && check.GetConclusion() == record.Conclusion {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("signed context %q differs from actual check/app/suite", name)
		}
	}
	return nil
}
