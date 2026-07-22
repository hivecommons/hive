package integrated

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

var (
	hostedOperatorRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	hostedOperatorDigestPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	hostedOperatorCommitPattern    = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

// HostedOperatorIdentity is the non-authoritative public description of a
// human operator. Constructing this value never grants authority.
type HostedOperatorIdentity struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// HostedOperatorProof contains the immutable GitHub Actions context that must
// be independently verified before hosted human authority can be minted.
// Config is durable signed policy restored by the hosted controller.
type HostedOperatorProof struct {
	Config          Config
	Operation       string
	RequestID       string
	RequestSHA256   string
	DefaultHeadSHA  string
	RunHeadSHA      string
	RunID           int64
	EventActorID    int64
	EventActorLogin string
	RequestedActor  HostedOperatorIdentity
}

// HostedOperatorAuthority is a sealed capability. Code outside this package
// cannot implement it or construct the concrete value accepted by lifecycle
// mutations. It is minted only after immutable workflow-run, workflow-content,
// actor, permission, repository, release-policy, and request verification.
type HostedOperatorAuthority interface {
	hostedOperatorAuthority()
}

type verifiedHostedOperatorAuthority struct {
	identity      hivegithub.AuthenticatedUserIdentity
	repository    string
	operation     string
	requestID     string
	requestSHA256 string
}

func (*verifiedHostedOperatorAuthority) hostedOperatorAuthority() {}

// VerifyHostedOperatorAuthority verifies the current managed workflow run and
// returns an opaque operation-bound capability. Merely naming an existing
// human or resolving their numeric ID is intentionally insufficient.
func VerifyHostedOperatorAuthority(ctx context.Context, client *hivegithub.Client, proof HostedOperatorProof) (HostedOperatorAuthority, error) {
	if client == nil {
		return nil, errors.New("GitHub client is required to verify the hosted operator")
	}
	config := proof.Config
	if config.ExecutionMode != ExecutionHosted || config.Repository == "" || config.RepositoryID == "" || config.DefaultBranch == "" ||
		config.HostedStateBranch == "" || config.HiveReleaseVersion == "" || config.HiveCommit == "" || config.VisualHiveRef == "" || config.DistributionManifestSHA256 == "" {
		return nil, errors.New("hosted operator signed policy is incomplete")
	}
	if !hostedOperatorRequestIDPattern.MatchString(proof.RequestID) || !hostedOperatorDigestPattern.MatchString(strings.ToLower(strings.TrimSpace(proof.RequestSHA256))) ||
		!hostedOperatorCommitPattern.MatchString(strings.ToLower(strings.TrimSpace(proof.DefaultHeadSHA))) ||
		!hostedOperatorCommitPattern.MatchString(strings.ToLower(strings.TrimSpace(proof.RunHeadSHA))) || strings.TrimSpace(proof.Operation) == "" || proof.RunID <= 0 {
		return nil, errors.New("hosted operator request or workflow identity is invalid")
	}
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return nil, errors.New("hosted operator repository identity is invalid")
	}
	numericRepositoryID, err := strconv.ParseInt(config.RepositoryID, 10, 64)
	if err != nil || numericRepositoryID <= 0 {
		return nil, errors.New("hosted operator numeric repository identity is invalid")
	}
	run, _, err := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repository, proof.RunID)
	if err != nil {
		return nil, fmt.Errorf("read hosted operator workflow run: %w", err)
	}
	wantPath := HostedControllerWorkflowPath
	pathOK := run.GetPath() == wantPath || run.GetPath() == wantPath+"@refs/heads/"+config.DefaultBranch
	if run.GetID() != proof.RunID || run.GetEvent() != "workflow_dispatch" || run.GetRunAttempt() != 1 || run.GetHeadBranch() != config.DefaultBranch ||
		strings.ToLower(run.GetHeadSHA()) != strings.ToLower(proof.RunHeadSHA) || !pathOK || run.GetDisplayTitle() != "Hive "+proof.Operation+" "+proof.RequestID ||
		run.GetRepository().GetID() != numericRepositoryID || !strings.EqualFold(run.GetRepository().GetFullName(), config.Repository) {
		return nil, errors.New("hosted operator workflow run is a rerun or has the wrong repository, event, path, title, branch, or head")
	}
	if !humanMatchesHostedOperator(run.GetActor(), proof.RequestedActor) || !humanMatchesHostedOperator(run.GetTriggeringActor(), proof.RequestedActor) ||
		proof.EventActorID != proof.RequestedActor.ID || !strings.EqualFold(strings.TrimSpace(proof.EventActorLogin), strings.TrimSpace(proof.RequestedActor.Login)) {
		return nil, errors.New("hosted operator request actor does not match the immutable workflow run actor")
	}

	expectedWorkflow, err := GenerateHostedControllerWorkflow(hostedWorkflowConfig(config))
	if err != nil {
		return nil, fmt.Errorf("generate trusted hosted controller workflow: %w", err)
	}
	content, directory, _, err := client.GoGitHub().Repositories.GetContents(ctx, owner, repository, HostedControllerWorkflowPath, &gh.RepositoryContentGetOptions{Ref: strings.ToLower(proof.RunHeadSHA)})
	if err != nil {
		return nil, fmt.Errorf("read exact hosted controller workflow: %w", err)
	}
	actualWorkflow, contentErr := content.GetContent()
	if content == nil || directory != nil || content.GetType() != "file" || content.GetPath() != HostedControllerWorkflowPath || contentErr != nil || actualWorkflow != expectedWorkflow {
		return nil, errors.New("hosted operator workflow bytes do not match the exact generated release-bound controller")
	}

	permission, _, err := client.GoGitHub().Repositories.GetPermissionLevel(ctx, owner, repository, proof.RequestedActor.Login)
	if err != nil {
		return nil, fmt.Errorf("verify hosted operator repository permission: %w", err)
	}
	if !hostedOperatorPermissionAllowed(permission.GetPermission()) {
		return nil, errors.New("hosted operator requires current write, maintain, or administrator repository permission")
	}
	resolved, err := client.ResolveHumanNumericUser(ctx, proof.RequestedActor.Login)
	if err != nil || resolved.ID != proof.RequestedActor.ID || !strings.EqualFold(resolved.Login, proof.RequestedActor.Login) {
		return nil, errors.New("hosted operator numeric identity could not be independently verified")
	}
	return &verifiedHostedOperatorAuthority{
		identity: resolved, repository: config.Repository, operation: proof.Operation,
		requestID: proof.RequestID, requestSHA256: strings.ToLower(proof.RequestSHA256),
	}, nil
}

// DescribeHostedOperatorAuthority returns audit-safe identity without exposing
// a constructible authority value.
func DescribeHostedOperatorAuthority(authority HostedOperatorAuthority) (HostedOperatorIdentity, error) {
	verified, ok := authority.(*verifiedHostedOperatorAuthority)
	if !ok || verified == nil || verified.identity.ID <= 0 || strings.TrimSpace(verified.identity.Login) == "" {
		return HostedOperatorIdentity{}, errors.New("hosted operator authority is invalid")
	}
	return HostedOperatorIdentity{ID: verified.identity.ID, Login: verified.identity.Login}, nil
}

func resolveOperatorIdentity(ctx context.Context, client *hivegithub.Client, authority HostedOperatorAuthority, repository, operation string) (hivegithub.AuthenticatedUserIdentity, error) {
	if client == nil {
		return hivegithub.AuthenticatedUserIdentity{}, errors.New("GitHub client is required to resolve the operator")
	}
	if authority == nil {
		return client.AuthenticatedNumericUser(ctx)
	}
	verified, ok := authority.(*verifiedHostedOperatorAuthority)
	if !ok || verified == nil || verified.repository != repository || verified.operation != operation || verified.identity.ID <= 0 || strings.TrimSpace(verified.identity.Login) == "" {
		return hivegithub.AuthenticatedUserIdentity{}, errors.New("hosted operator authority is invalid or bound to a different repository or operation")
	}
	return verified.identity, nil
}

func humanMatchesHostedOperator(user *gh.User, actor HostedOperatorIdentity) bool {
	return user != nil && user.GetID() == actor.ID && strings.EqualFold(strings.TrimSpace(user.GetLogin()), strings.TrimSpace(actor.Login)) &&
		strings.EqualFold(user.GetType(), "User") && !strings.EqualFold(user.GetLogin(), "github-actions[bot]")
}

func hostedOperatorPermissionAllowed(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "admin", "maintain", "write", "push":
		return true
	default:
		return false
	}
}
