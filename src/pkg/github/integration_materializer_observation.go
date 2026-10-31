package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forgestate"
)

const (
	integrationMaterializerWorkflow  = ".github/workflows/integration-candidate-prepare.yml"
	integrationMaterializerFinalizer = "integration materializer receipt"
	integrationMaterializerJob       = "trusted integration materializer"
	integrationMaterializerArtifact  = "integration-materializer-receipt-"
)

// IntegrationPreparationRequest is versioned independently of the older
// single-PR direct attestation. Members are ordered and never inferred by paths.
type IntegrationPreparationRequest struct {
	Schema     int                                     `json:"schema"`
	Protocol   string                                  `json:"protocol"`
	Engine     string                                  `json:"engine"`
	Repo       string                                  `json:"repo"`
	Base       string                                  `json:"base"`
	BaseSHA    string                                  `json:"baseSHA"`
	Policy     string                                  `json:"policy"`
	Generation int64                                   `json:"generation"`
	Members    []forgestate.IntegrationCandidateMember `json:"members"`
	CreatedAt  string                                  `json:"createdAt"`
}

type IntegrationMaterializationReceipt struct {
	Schema     int                                     `json:"schema"`
	Protocol   string                                  `json:"protocol"`
	Request    IntegrationPreparationRequest           `json:"request"`
	Prepared   []IntegrationPreparedSubject            `json:"prepared"`
	Failure    *IntegrationPreparationFailure          `json:"failure"`
	PreparedBy forgestate.IntegrationPreparationSource `json:"preparedBy"`
}

type IntegrationPreparedSubject struct {
	Subject     forgestate.IntegrationCandidateSubject `json:"subject"`
	Publication string                                 `json:"publication"`
	Dispatch    string                                 `json:"dispatch"`
	Ownership   forgestate.IntegrationDispatchClaim    `json:"ownership"`
}

type IntegrationPreparationFailure struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// IntegrationMaterializerExpectation is installed policy plus an exact durable
// bootstrap request. EventName selects a transport; an API error never does.
type IntegrationMaterializerExpectation struct {
	Request                                               IntegrationPreparationRequest
	RequestDigest, DefaultBranch, EventName, WorkflowTree string
	RepositoryID, ActorID, WorkflowID, CheckAppID, RunID  int64
	Attempt                                               int
}

type IntegrationMaterializerObservation struct {
	Receipt  IntegrationMaterializationReceipt
	Evidence IntegrationCandidateObservation
}

func (c *Client) ReadIntegrationMaterializerObservation(ctx context.Context, expected IntegrationMaterializerExpectation, verifier *IntegrationProducerVerifier, now time.Time) (IntegrationMaterializerObservation, error) {
	out := IntegrationMaterializerObservation{Evidence: IntegrationCandidateObservation{
		SubjectID: expected.RequestDigest, RunID: expected.RunID, Attempt: expected.Attempt,
		State: "unknown", ObservedAt: now, JobCompleteness: "unknown",
	}}
	repo := expected.Request.Repo
	parts := strings.Split(repo, "/")
	if c == nil || c.client == nil || now.IsZero() ||
		expected.Request.Schema != 1 || expected.Request.Protocol != "integration-prefix-v1" || expected.Request.Engine != "hive" ||
		expected.Request.Generation < 1 || expected.Request.Base == "" || !integrationOID(expected.Request.BaseSHA) ||
		!integrationProducerDigest(expected.Request.Policy) || len(expected.Request.Members) == 0 ||
		len(expected.Request.Members) > integrationCandidateMaxMembers ||
		len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		strings.ContainsAny(repo, "?#%\\ \t\r\n") || expected.RepositoryID <= 0 ||
		expected.ActorID <= 0 || expected.WorkflowID <= 0 || expected.CheckAppID <= 0 ||
		expected.RunID <= 0 || expected.Attempt < 1 || !integrationProducerDigest(expected.RequestDigest) ||
		!integrationOID(expected.WorkflowTree) || expected.DefaultBranch == "" ||
		expected.EventName != "workflow_dispatch" && expected.EventName != "repository_dispatch" {
		return out, fmt.Errorf("incomplete enrolled materializer observation")
	}
	runPath := fmt.Sprintf("repos/%s/actions/runs/%d", repo, expected.RunID)
	var run gh.WorkflowRun
	if err := c.integrationCandidateRead(ctx, runPath, &run); err != nil {
		return out, err
	}
	if run.GetRunAttempt() != expected.Attempt {
		out.Evidence.State, out.Evidence.Detail = "superseded", "materializer attempt changed"
		return out, nil
	}
	if run.GetID() != expected.RunID || run.GetWorkflowID() != expected.WorkflowID || run.GetCheckSuiteID() <= 0 ||
		run.GetPath() != integrationMaterializerWorkflow || run.GetEvent() != expected.EventName ||
		run.GetRepository().GetID() != expected.RepositoryID || run.GetRepository().GetFullName() != repo ||
		run.GetHeadRepository().GetFullName() != repo || run.GetActor().GetID() != expected.ActorID || !integrationOID(run.GetHeadSHA()) {
		return out, fmt.Errorf("materializer workflow/actor/source identity changed")
	}
	var jobs struct {
		TotalCount int               `json:"total_count"`
		Jobs       []*gh.WorkflowJob `json:"jobs"`
	}
	if err := c.integrationCandidateRead(ctx, fmt.Sprintf("%s/attempts/%d/jobs?per_page=%d", runPath, expected.Attempt, integrationGroupMaxEntries), &jobs); err != nil {
		return out, err
	}
	if jobs.TotalCount == 0 || jobs.TotalCount != len(jobs.Jobs) {
		return out, fmt.Errorf("materializer job cohort incomplete")
	}
	var finalizer *gh.WorkflowJob
	for _, job := range jobs.Jobs {
		if job == nil || job.GetID() <= 0 || job.GetRunID() != expected.RunID || job.GetHeadSHA() != run.GetHeadSHA() {
			return out, fmt.Errorf("materializer job not bound to actual run/head")
		}
		out.Evidence.Jobs = append(out.Evidence.Jobs, forgestate.IntegrationRunnerJob{
			Repo: repo, RunID: expected.RunID, Attempt: expected.Attempt, JobID: job.GetID(), RunnerID: job.GetRunnerID(),
			Name: job.GetName(), Status: job.GetStatus(), Conclusion: job.GetConclusion(),
			StartedAt: job.GetStartedAt().Time, CompletedAt: job.GetCompletedAt().Time, ObservedAt: now,
		})
		if job.GetName() == integrationMaterializerFinalizer {
			if finalizer != nil {
				return out, fmt.Errorf("ambiguous materializer finalizer")
			}
			finalizer = job
		}
	}
	out.Evidence.JobCompleteness = "complete"
	if run.GetStatus() != "completed" {
		out.Evidence.State, out.Evidence.Detail = "waiting", "materializer waiting"
		if run.GetStatus() == "in_progress" {
			out.Evidence.State, out.Evidence.Detail = "running", "materializer running"
		}
		return out, nil
	}
	checks, response, err := c.client.Checks.ListCheckRunsForRef(ctx, parts[0], parts[1], run.GetHeadSHA(),
		&gh.ListCheckRunsOptions{Filter: gh.String("all"), ListOptions: gh.ListOptions{PerPage: integrationGroupMaxEntries}})
	if err != nil {
		return out, err
	}
	if checks == nil || response != nil && response.NextPage != 0 {
		return out, fmt.Errorf("materializer check cohort truncated")
	}
	proof := IntegrationWorkflowProof{}
	if err := bindIntegrationWorkflowJobs(config.IntegrationPolicy{Jobs: []string{integrationMaterializerJob, integrationMaterializerFinalizer}, AppID: expected.CheckAppID},
		&run, jobs.Jobs, checks.CheckRuns, &proof); err != nil {
		return out, err
	}
	if finalizer == nil || len(proof.CheckIDs) != 2 || finalizer.GetStatus() != "completed" || finalizer.GetConclusion() != "success" {
		return out, fmt.Errorf("trusted materializer cohort/finalizer incomplete")
	}
	finalizerProof := IntegrationWorkflowProof{}
	if err := bindIntegrationWorkflowJobs(config.IntegrationPolicy{Jobs: []string{integrationMaterializerFinalizer}, AppID: expected.CheckAppID},
		&run, jobs.Jobs, checks.CheckRuns, &finalizerProof); err != nil || !finalizerProof.Passed || len(finalizerProof.CheckIDs) != 1 {
		return out, fmt.Errorf("actual isolated materializer finalizer did not succeed")
	}
	id, digest, err := c.integrationProducerArtifact(ctx, repo, &run, integrationMaterializerArtifact)
	if err != nil {
		return out, err
	}
	archive, err := c.downloadIntegrationReceipt(ctx, repo, id, digest)
	if err != nil {
		return out, err
	}
	payload, token, err := decodeIntegrationEnvelope(archive, "materialization.json")
	if err != nil {
		return out, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out.Receipt); err != nil {
		return out, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return out, fmt.Errorf("materializer receipt has trailing data")
	}
	if out.Receipt.Schema != 1 || out.Receipt.Protocol != "integration-materializer-receipt-v1" ||
		!reflect.DeepEqual(out.Receipt.Request, expected.Request) ||
		out.Receipt.PreparedBy.SourceSHA != run.GetHeadSHA() || out.Receipt.PreparedBy.RequestDigest != expected.RequestDigest ||
		len(out.Receipt.Prepared) > len(expected.Request.Members) {
		return out, fmt.Errorf("materializer receipt differs from exact durable request")
	}
	payloadDigest := sha256.Sum256(payload)
	out.Evidence.Identity, err = verifier.VerifyMaterializer(ctx, token, payload, IntegrationMaterializerBinding{
		IntegrationProducerBinding: IntegrationProducerBinding{
			Repo: repo, RepositoryID: expected.RepositoryID, ActorID: expected.ActorID,
			RunID: expected.RunID, RunAttempt: int64(expected.Attempt), FinalizerCheckRunID: proof.CheckIDs[1],
			WorkflowPath: integrationMaterializerWorkflow, WorkflowSHA: run.GetHeadSHA(), SubjectID: expected.RequestDigest,
			PayloadSHA256: hex.EncodeToString(payloadDigest[:]), JobStartedAt: finalizer.GetStartedAt().Time,
			JobCompletedAt: finalizer.GetCompletedAt().Time, ObservedAt: now,
		},
		DefaultBranch: expected.DefaultBranch, EventName: expected.EventName,
	})
	if err != nil {
		return out, err
	}
	if err := c.verifyIntegrationProducerClosure(ctx, repo, run.GetHeadSHA(), expected.WorkflowTree); err != nil {
		return out, err
	}
	var published *forgestate.IntegrationCandidateSubject
	for i, prepared := range out.Receipt.Prepared {
		s := prepared.Subject
		if s.Schema != expected.Request.Schema || s.Protocol != expected.Request.Protocol || s.Engine != expected.Request.Engine ||
			s.Repo != repo || s.Base != expected.Request.Base || s.BaseSHA != expected.Request.BaseSHA ||
			s.Policy != expected.Request.Policy || s.WorkflowTree != expected.WorkflowTree ||
			s.Generation != expected.Request.Generation || !integrationProducerDigest(s.ID) ||
			!reflect.DeepEqual(s.Members, expected.Request.Members[:i+1]) || len(s.Prefixes) != i+1 {
			return out, fmt.Errorf("materializer changed ordered prepared prefix %d", i+1)
		}
		if i > 0 && !reflect.DeepEqual(s.Prefixes[:i], out.Receipt.Prepared[i-1].Subject.Prefixes) {
			return out, fmt.Errorf("materializer changed predecessor prefix identity")
		}
		if prepared.Publication != "accepted" && prepared.Publication != "unresolved" {
			return out, fmt.Errorf("unknown prefix publication state")
		}
		switch prepared.Dispatch {
		case "accepted", "unresolved", "observer", "not_requested":
		default:
			return out, fmt.Errorf("unknown prefix dispatch state")
		}
		if prepared.Publication == "accepted" {
			copySubject := s
			published = &copySubject
		}
	}
	if published != nil {
		if err := c.verifyIntegrationCandidateObjects(ctx, *published); err != nil {
			return out, err
		}
	}
	var current gh.WorkflowRun
	if err := c.integrationCandidateRead(ctx, runPath, &current); err != nil {
		return out, err
	}
	if current.GetRunAttempt() != expected.Attempt || current.GetHeadSHA() != run.GetHeadSHA() ||
		current.GetStatus() != "completed" || current.GetConclusion() != run.GetConclusion() {
		out.Evidence.State, out.Evidence.Detail = "superseded", "materializer changed during verification"
		return out, nil
	}
	out.Evidence.WorkflowID, out.Evidence.SuiteID = expected.WorkflowID, run.GetCheckSuiteID()
	out.Evidence.ArtifactID, out.Evidence.ArtifactDigest = id, digest
	out.Evidence.Payload, out.Evidence.Token, out.Evidence.CheckIDs = payload, token, proof.CheckIDs
	out.Evidence.State, out.Evidence.Detail = "passed", "trusted materialization provenance verified; not candidate CI success or promotion permission"
	return out, nil
}
