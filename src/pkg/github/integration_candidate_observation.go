package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forgestate"
)

const (
	integrationCandidateWorkflow   = ".github/workflows/integration-candidate.yml"
	integrationCandidateFinalizer  = "integration producer receipt"
	integrationCandidateMaxMembers = 8
)

// IntegrationCandidateExpectation comes from enrollment and the durable
// materializer observation, never from assertions inside a downloaded artifact.
type IntegrationCandidateExpectation struct {
	Subject                                       forgestate.IntegrationCandidateSubject
	RepositoryID, ActorID, WorkflowID, CheckAppID int64
	RunID                                         int64
	Attempt                                       int
	RequiredJobs                                  []string
}

// IntegrationCandidateObservation keeps verified producer provenance separate
// from technical success and live promotion eligibility.
type IntegrationCandidateObservation struct {
	SubjectID                       string
	State, Detail                   string
	ObservedAt                      time.Time
	RunID                           int64
	Attempt                         int
	WorkflowID, SuiteID, ArtifactID int64
	ArtifactDigest                  string
	Identity                        IntegrationProducerIdentity
	Payload                         []byte `json:"-"`
	Token                           string `json:"-"`
	CheckIDs                        []int64
	Jobs                            []forgestate.IntegrationRunnerJob
	JobCompleteness                 string
}

type integrationCandidateReceipt struct {
	Schema     int                                    `json:"schema"`
	Protocol   string                                 `json:"protocol"`
	Subject    forgestate.IntegrationCandidateSubject `json:"subject"`
	RunID      string                                 `json:"runID"`
	RunAttempt string                                 `json:"runAttempt"`
	Outcomes   struct {
		CI       string `json:"ci"`
		Tests    string `json:"tests"`
		Security string `json:"security"`
	} `json:"outcomes"`
}

func (e IntegrationCandidateExpectation) validate() error {
	s := e.Subject
	parts := strings.Split(s.Repo, "/")
	if s.Schema != 1 || s.Protocol != "integration-prefix-v1" || s.Engine != "hive" ||
		len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		strings.ContainsAny(s.Repo, "?#%\\ \t\r\n") || s.Base == "" ||
		!integrationProducerDigest(s.ID) || !integrationProducerDigest(s.Policy) ||
		!integrationOID(s.BaseSHA) || !integrationOID(s.WorkflowTree) ||
		e.RepositoryID <= 0 || e.ActorID <= 0 || e.WorkflowID <= 0 || e.CheckAppID <= 0 ||
		e.RunID <= 0 || e.Attempt < 1 || len(s.Members) == 0 ||
		len(s.Members) > integrationCandidateMaxMembers || len(s.Prefixes) != len(s.Members) ||
		len(e.RequiredJobs) == 0 || len(e.RequiredJobs) >= integrationGroupMaxEntries {
		return fmt.Errorf("incomplete enrolled candidate expectation")
	}
	members := make(map[int]bool)
	heads := make(map[string]bool)
	for i, member := range s.Members {
		prefix := s.Prefixes[i]
		if member.PR <= 0 || members[member.PR] || heads[member.Head] ||
			!integrationOID(member.Head) || !integrationOID(prefix.Commit) || !integrationOID(prefix.Tree) {
			return fmt.Errorf("candidate ordered member/prefix identity invalid")
		}
		members[member.PR], heads[member.Head] = true, true
	}
	names := make(map[string]bool)
	for _, name := range e.RequiredJobs {
		if name == "" || names[name] || name == integrationCandidateFinalizer {
			return fmt.Errorf("qualified required-job cohort is empty or ambiguous")
		}
		names[name] = true
	}
	return nil
}

func (c *Client) integrationCandidateRead(ctx context.Context, path string, out any) error {
	request, err := c.client.NewRequest("GET", path, nil)
	if err != nil {
		return err
	}
	response, err := c.client.Do(ctx, request, out)
	if err != nil {
		return err
	}
	if response != nil && response.NextPage != 0 {
		return fmt.Errorf("candidate observation cohort truncated; completeness unknown")
	}
	return nil
}

// ReadIntegrationCandidateObservation performs bounded cohort reads, not
// per-job polling. The controller must coalesce events/reconciliation by exact
// run/attempt and persist verified receipts before acknowledging observations.
func (c *Client) ReadIntegrationCandidateObservation(ctx context.Context, expected IntegrationCandidateExpectation, verifier *IntegrationProducerVerifier, now time.Time) (IntegrationCandidateObservation, error) {
	out := IntegrationCandidateObservation{
		SubjectID: expected.Subject.ID, RunID: expected.RunID, Attempt: expected.Attempt,
		State: "unknown", ObservedAt: now, JobCompleteness: "unknown",
	}
	if err := expected.validate(); err != nil {
		return out, err
	}
	if c == nil || c.client == nil || now.IsZero() {
		return out, fmt.Errorf("candidate observer unavailable")
	}
	repo := expected.Subject.Repo
	var run gh.WorkflowRun
	runPath := fmt.Sprintf("repos/%s/actions/runs/%d", repo, expected.RunID)
	if err := c.integrationCandidateRead(ctx, runPath, &run); err != nil {
		return out, err
	}
	last := expected.Subject.Prefixes[len(expected.Subject.Prefixes)-1]
	if run.GetRunAttempt() != expected.Attempt {
		out.State, out.Detail = "superseded", "authoritative latest workflow attempt changed"
		return out, nil
	}
	if run.GetID() != expected.RunID || run.GetWorkflowID() != expected.WorkflowID ||
		run.GetCheckSuiteID() <= 0 || run.GetEvent() != "workflow_dispatch" ||
		run.GetPath() != integrationCandidateWorkflow || run.GetHeadSHA() != last.Commit ||
		run.GetActor().GetID() != expected.ActorID ||
		run.GetRepository().GetID() != expected.RepositoryID || run.GetRepository().GetFullName() != repo ||
		run.GetHeadRepository().GetFullName() != repo {
		return out, fmt.Errorf("candidate workflow/actor/repository/head not bound to enrollment")
	}
	out.WorkflowID, out.SuiteID = run.GetWorkflowID(), run.GetCheckSuiteID()
	var jobs struct {
		TotalCount int               `json:"total_count"`
		Jobs       []*gh.WorkflowJob `json:"jobs"`
	}
	path := fmt.Sprintf("%s/attempts/%d/jobs?per_page=%d", runPath, expected.Attempt, integrationGroupMaxEntries)
	if err := c.integrationCandidateRead(ctx, path, &jobs); err != nil {
		return out, err
	}
	if jobs.TotalCount == 0 || jobs.TotalCount != len(jobs.Jobs) {
		return out, fmt.Errorf("job cohort incomplete")
	}
	for _, job := range jobs.Jobs {
		if job == nil || job.GetID() <= 0 || job.GetRunID() != expected.RunID || job.GetHeadSHA() != last.Commit {
			return out, fmt.Errorf("job observation not bound to candidate run/head")
		}
		out.Jobs = append(out.Jobs, forgestate.IntegrationRunnerJob{
			Repo: repo, RunID: expected.RunID, Attempt: expected.Attempt,
			JobID: job.GetID(), RunnerID: job.GetRunnerID(), Name: job.GetName(),
			Status: job.GetStatus(), Conclusion: job.GetConclusion(),
			StartedAt: job.GetStartedAt().Time, CompletedAt: job.GetCompletedAt().Time, ObservedAt: now,
		})
	}
	out.JobCompleteness = "complete"
	if run.GetStatus() != "completed" {
		out.State, out.Detail = "waiting", "authoritative candidate workflow is waiting"
		if run.GetStatus() == "in_progress" {
			out.State, out.Detail = "running", "authoritative candidate workflow is running"
		}
		return out, nil
	}
	parts := strings.Split(repo, "/")
	checks, response, err := c.client.Checks.ListCheckRunsForRef(ctx, parts[0], parts[1], last.Commit,
		&gh.ListCheckRunsOptions{Filter: gh.String("all"), ListOptions: gh.ListOptions{PerPage: integrationGroupMaxEntries}})
	if err != nil {
		return out, err
	}
	if checks == nil || response != nil && response.NextPage != 0 {
		return out, fmt.Errorf("candidate check cohort truncated")
	}
	policy := config.IntegrationPolicy{Jobs: append(append([]string(nil), expected.RequiredJobs...), integrationCandidateFinalizer), AppID: expected.CheckAppID}
	proof := IntegrationWorkflowProof{}
	if err := bindIntegrationWorkflowJobs(policy, &run, jobs.Jobs, checks.CheckRuns, &proof); err != nil {
		return out, err
	}
	var finalizer *gh.WorkflowJob
	for _, job := range jobs.Jobs {
		if job.GetName() != integrationCandidateFinalizer {
			continue
		}
		if finalizer != nil {
			return out, fmt.Errorf("ambiguous candidate receipt finalizer")
		}
		finalizer = job
	}
	if finalizer == nil || finalizer.GetStatus() != "completed" || finalizer.GetConclusion() != "success" {
		return out, fmt.Errorf("isolated producer finalizer did not complete successfully")
	}
	finalizerProof := IntegrationWorkflowProof{}
	if err := bindIntegrationWorkflowJobs(config.IntegrationPolicy{Jobs: []string{integrationCandidateFinalizer}, AppID: expected.CheckAppID},
		&run, jobs.Jobs, checks.CheckRuns, &finalizerProof); err != nil || len(finalizerProof.CheckIDs) != 1 {
		return out, fmt.Errorf("finalizer's actual check identity unavailable: %v", err)
	}
	finalizerPassed := false
	for _, check := range checks.CheckRuns {
		if check.GetID() == finalizerProof.CheckIDs[0] {
			finalizerPassed = check.GetStatus() == "completed" && check.GetConclusion() == "success"
		}
	}
	if !finalizerPassed {
		return out, fmt.Errorf("actual finalizer check did not succeed")
	}
	artifactID, artifactDigest, err := c.integrationCandidateArtifact(ctx, repo, &run)
	if err != nil {
		return out, err
	}
	archive, err := c.downloadIntegrationReceipt(ctx, repo, artifactID, artifactDigest)
	if err != nil {
		return out, err
	}
	payload, token, err := decodeIntegrationReceipt(archive)
	if err != nil {
		return out, err
	}
	var receipt integrationCandidateReceipt
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return out, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return out, fmt.Errorf("candidate receipt has trailing data")
	}
	if receipt.Schema != 1 || receipt.Protocol != "integration-producer-receipt-v1" ||
		receipt.RunID != strconv.FormatInt(expected.RunID, 10) ||
		receipt.RunAttempt != strconv.Itoa(expected.Attempt) || !receipt.Subject.SameCore(expected.Subject) {
		return out, fmt.Errorf("receipt does not bind the enrolled exact immutable subject and attempt")
	}
	digest := sha256.Sum256(payload)
	out.Identity, err = verifier.Verify(ctx, token, payload, IntegrationProducerBinding{
		Repo: repo, RepositoryID: expected.RepositoryID, ActorID: expected.ActorID,
		RunID: expected.RunID, RunAttempt: int64(expected.Attempt), FinalizerCheckRunID: finalizerProof.CheckIDs[0],
		WorkflowPath: integrationCandidateWorkflow, WorkflowSHA: last.Commit,
		SubjectID: expected.Subject.ID, PayloadSHA256: hex.EncodeToString(digest[:]),
		JobStartedAt: finalizer.GetStartedAt().Time, JobCompletedAt: finalizer.GetCompletedAt().Time, ObservedAt: now,
	})
	if err != nil {
		return out, err
	}
	if err := c.verifyIntegrationCandidateObjects(ctx, expected.Subject); err != nil {
		return out, err
	}
	var current gh.WorkflowRun
	if err := c.integrationCandidateRead(ctx, runPath, &current); err != nil {
		return out, err
	}
	if current.GetRunAttempt() != expected.Attempt || current.GetHeadSHA() != last.Commit ||
		current.GetStatus() != run.GetStatus() || current.GetConclusion() != run.GetConclusion() {
		out.State, out.Detail = "superseded", "run changed during receipt verification"
		return out, nil
	}
	out.ArtifactID, out.ArtifactDigest, out.Payload, out.Token = artifactID, artifactDigest, payload, token
	out.CheckIDs = proof.CheckIDs
	out.State, out.Detail = "rejected", "actual technical workflow/jobs rejected; provenance alone is not green"
	if proof.Passed && receipt.Outcomes.CI == "success" && receipt.Outcomes.Tests == "success" && receipt.Outcomes.Security == "success" {
		out.State, out.Detail = "passed", "exact candidate technical proof verified; author review and live promotion gates remain separate"
	}
	return out, nil
}
