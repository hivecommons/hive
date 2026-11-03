package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/effects"
)

const (
	IntegrationPromotionProtocol      = "integration-first-parent-v1"
	integrationPromotionMaxLabelBytes = 256
)

// IntegrationPromotion describes one exact, already-tested step. This initial
// protocol preserves both the preceding integration commit and the author head.
// It deliberately does not implement squash, rebase or tree-equivalent reuse.
type IntegrationPromotion struct {
	Repo            string
	Base            string
	ExpectedSHA     string
	TestedSHA       string
	TestedTree      string
	AuthorHead      string
	HeadRepo        string
	HeadRef         string
	SubjectID       string
	Policy          string
	Generation      uint64
	AuthorityRef    string
	AuthorityCommit string
}

// IntegrationPromotionGuard must revalidate the destination's exclusive writer,
// exact evidence and every live member's eligibility. A ledger receipt alone is
// not authorization. The guard runs inside the existing effect boundary,
// immediately before the provider's atomic operation.
type IntegrationPromotionGuard func(context.Context, IntegrationPromotion) error

var ErrIntegrationDestinationChanged = errors.New("integration destination changed; reconcile the candidate before retrying")

var ErrIntegrationHeadFenceUnavailable = errors.New("integration atomic author-head fencing requires a same-repository branch")

func (p IntegrationPromotion) validate() error {
	if !strings.EqualFold(p.HeadRepo, p.Repo) {
		return ErrIntegrationHeadFenceUnavailable
	}
	if !integrationPromotionRepo(p.Repo) || !integrationPromotionBranch(p.Base) ||
		!integrationPromotionBranch(p.HeadRef) || p.HeadRef == p.Base ||
		!integrationPromotionOID(p.ExpectedSHA) || !integrationPromotionOID(p.TestedSHA) ||
		!integrationPromotionOID(p.TestedTree) || !integrationPromotionOID(p.AuthorHead) ||
		p.ExpectedSHA == p.TestedSHA || p.ExpectedSHA == p.AuthorHead ||
		p.SubjectID == "" || p.Policy == "" || p.Generation == 0 ||
		len(p.SubjectID) > integrationPromotionMaxLabelBytes || len(p.Policy) > integrationPromotionMaxLabelBytes ||
		strings.ContainsAny(p.SubjectID+p.Policy, "\r\n\x00") {
		return fmt.Errorf("incomplete exact integration promotion")
	}
	if p.AuthorityRef != "" || p.AuthorityCommit != "" {
		if !strings.HasPrefix(p.AuthorityRef, "refs/heads/hive-integration-authority/") ||
			!integrationPromotionBranch(strings.TrimPrefix(p.AuthorityRef, "refs/heads/")) ||
			!integrationPromotionOID(p.AuthorityCommit) ||
			p.AuthorityRef == "refs/heads/"+p.Base || p.AuthorityRef == "refs/heads/"+p.HeadRef {
			return fmt.Errorf("invalid designated-executor authority epoch fence")
		}
	}
	return nil
}

func integrationPromotionRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[1] == "." || parts[1] == ".." {
		return false
	}
	for i, part := range parts {
		for _, character := range part {
			if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
				character >= '0' && character <= '9' || character == '-' || i == 1 && (character == '.' || character == '_') {
				continue
			}
			return false
		}
	}
	return true
}

func integrationPromotionJournaled(boundary effects.Boundary) bool {
	for {
		switch b := boundary.(type) {
		case effects.LoggingBoundary:
			boundary = b.Next
		case *effects.LoggingBoundary:
			if b == nil {
				return false
			}
			boundary = b.Next
		case nil, effects.NoopBoundary, *effects.NoopBoundary:
			return false
		default:
			_, reconciles := boundary.(effects.Reconciler)
			_, looksUp := boundary.(effects.Lookup)
			_, requiresJournal := boundary.(effects.JournaledBoundary)
			return reconciles && looksUp && requiresJournal
		}
	}
}

func integrationPromotionOID(value string) bool {
	return integrationOID(value) && value == strings.ToLower(value) && strings.Trim(value, "0") != ""
}

func integrationPromotionBranch(base string) bool {
	if base == "" || base == "@" || strings.HasPrefix(base, "/") ||
		strings.HasSuffix(base, "/") || strings.HasSuffix(base, ".") ||
		strings.ContainsAny(base, " ~^:?*[\\\x00\t\r\n") ||
		strings.Contains(base, "..") || strings.Contains(base, "@{") {
		return false
	}
	for _, character := range base {
		if character < ' ' || character == '\x7f' {
			return false
		}
	}
	for _, component := range strings.Split(base, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func (p IntegrationPromotion) claim() effects.Claim {
	claim := effects.Claim{
		Repo: p.Repo, Kind: effects.KindBranchUpdate,
		Target: "refs/heads/" + p.Base, Actor: "integration-aware",
		Inputs: map[string]string{
			"protocol": IntegrationPromotionProtocol, "expected_sha": p.ExpectedSHA,
			"tested_sha": p.TestedSHA, "tested_tree": p.TestedTree,
			"author_head": p.AuthorHead, "subject": p.SubjectID,
			"head_repo": p.HeadRepo, "head_ref": p.HeadRef,
			"policy": p.Policy, "generation": fmt.Sprint(p.Generation),
		},
	}
	if p.AuthorityRef != "" {
		claim.Inputs["authority_ref"], claim.Inputs["authority_commit"] = p.AuthorityRef, p.AuthorityCommit
	}
	return claim
}

// PromoteIntegrationStep is a transport protocol, not an installed execution
// authority. The opt-in controller supplies a durable mutation boundary and
// complete live guard; normal merge lanes route without calling it directly.
//
// GitHub's updateRefs atomically checks the destination and a no-op author-ref
// fence server-side. Cross-repository fork refs cannot participate. REST UpdateRef,
// a PR merge's expected author head, and createCommitOnBranch do not promote
// this existing tested commit with a destination compare-and-swap.
func (c *Client) PromoteIntegrationStep(ctx context.Context, p IntegrationPromotion, boundary effects.Boundary, guard IntegrationPromotionGuard) (effects.Result, error) {
	return c.promoteIntegrationStep(ctx, p, boundary, guard, nil)
}

// PromoteIntegrationStepWithAcknowledgement additionally distinguishes an actual
// successful provider RPC from observing an already-achieved journalled state.
// Only the former can qualify a previously unproven protected actor/profile.
func (c *Client) PromoteIntegrationStepWithAcknowledgement(ctx context.Context, p IntegrationPromotion, boundary effects.Boundary, guard IntegrationPromotionGuard) (effects.Result, bool, error) {
	acknowledged := false
	result, err := c.promoteIntegrationStep(ctx, p, boundary, guard, func() { acknowledged = true })
	return result, acknowledged, err
}

func (c *Client) promoteIntegrationStep(ctx context.Context, p IntegrationPromotion, boundary effects.Boundary, guard IntegrationPromotionGuard, acknowledge func()) (effects.Result, error) {
	if c == nil || c.client == nil {
		return effects.Result{}, ErrNoGitHubClient
	}
	if err := p.validate(); err != nil {
		return effects.Result{}, err
	}
	if boundary == nil || guard == nil {
		return effects.Result{}, fmt.Errorf("integration promotion requires a mutation boundary and live eligibility guard")
	}
	if !integrationPromotionJournaled(boundary) {
		return effects.Result{}, fmt.Errorf("integration promotion refuses an unjournaled mutation boundary")
	}
	owner, repo := c.splitRepo(p.Repo)
	repository, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return effects.Result{}, err
	}
	if repository.GetNodeID() == "" || !strings.EqualFold(repository.GetFullName(), p.Repo) {
		return effects.Result{}, fmt.Errorf("integration repository identity unavailable")
	}
	commit, _, err := c.client.Git.GetCommit(ctx, owner, repo, p.TestedSHA)
	if err != nil {
		return effects.Result{}, err
	}
	if commit.GetSHA() != p.TestedSHA || commit.GetTree().GetSHA() != p.TestedTree ||
		len(commit.Parents) != 2 || commit.Parents[0].GetSHA() != p.ExpectedSHA ||
		commit.Parents[1].GetSHA() != p.AuthorHead {
		return effects.Result{}, fmt.Errorf("integration promotion requires the exact tested tree and ordered merge parents")
	}
	claim := p.claim()
	apply := func(ctx context.Context) (effects.Result, error) {
		if err := guard(ctx, p); err != nil {
			return effects.Result{}, effects.NotApplied(err)
		}
		var out struct {
			UpdateRefs *struct {
				ClientMutationID string `json:"clientMutationId"`
			} `json:"updateRefs"`
		}
		id := effects.StableDigest(claim.Repo, claim.Target, effects.StableInputs(claim.Inputs))
		updates := []map[string]any{{
			"name": "refs/heads/" + p.Base, "beforeOid": p.ExpectedSHA,
			"afterOid": p.TestedSHA, "force": false,
		}, {
			"name": "refs/heads/" + p.HeadRef, "beforeOid": p.AuthorHead,
			"afterOid": p.AuthorHead, "force": false,
		}}
		if p.AuthorityRef != "" {
			updates = append(updates, map[string]any{"name": p.AuthorityRef,
				"beforeOid": p.AuthorityCommit, "afterOid": p.AuthorityCommit, "force": false})
		}
		err := c.graphQL(ctx, `mutation($input: UpdateRefsInput!) {
 updateRefs(input: $input) { clientMutationId }
}`, map[string]any{"input": map[string]any{
			"repositoryId": repository.GetNodeID(), "clientMutationId": id,
			"refUpdates": updates,
		}}, &out)
		if err != nil {
			return effects.Result{}, err
		}
		if out.UpdateRefs == nil || out.UpdateRefs.ClientMutationID != id {
			return effects.Result{}, fmt.Errorf("integration promotion acknowledgement is incomplete; reconciliation required")
		}
		if acknowledge != nil {
			acknowledge()
		}
		return effects.Result{Provenance: p.TestedSHA}, nil
	}
	journaled, ok := boundary.(effects.JournaledBoundary)
	if !ok {
		return effects.Result{}, fmt.Errorf("integration promotion requires journaled execution")
	}
	out, err := journaled.ExecuteJournaled(ctx, claim, apply)
	if !errors.Is(err, effects.ErrNeedsReconciliation) {
		return out, err
	}
	if _, ok := boundary.(effects.Reconciler); !ok {
		return out, err
	}
	ref, _, readErr := c.client.Git.GetRef(ctx, owner, repo, "heads/"+p.Base)
	if readErr != nil {
		return out, fmt.Errorf("%w: %v", err, readErr)
	}
	state := effects.ExternalState{}
	switch ref.GetObject().GetSHA() {
	case p.TestedSHA:
		state = effects.ExternalState{Applied: true, Provenance: p.TestedSHA}
	case p.ExpectedSHA:
		// A duplicate pending request cannot overwrite another destination:
		// every attempt retains exactly the same server-side beforeOid.
	default:
		return out, ErrIntegrationDestinationChanged
	}
	if err := effects.Reconcile(ctx, boundary, claim, state); err != nil {
		return out, err
	}
	return journaled.ExecuteJournaled(ctx, claim, apply)
}
