package github

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

const integrationPreparationEffectKind = "integration_candidate_prepare"

type integrationStrictExecutor interface {
	ExecuteJournaled(context.Context, effects.Claim, func(context.Context) (effects.Result, error)) (effects.Result, error)
}

// DispatchIntegrationPreparation chooses repository_dispatch explicitly. It
// never retries/falls back after an Actions permission error and never reruns
// legacy PR CI. A 204 acknowledges transport, not materialization or passing CI.
func (c *Client) DispatchIntegrationPreparation(ctx context.Context, request IntegrationPreparationRequest, requestJSON, requestDigest, executorID, authorityCommit string, boundary effects.Boundary, guard func(context.Context) error) (effects.Result, error) {
	if c == nil || c.client == nil {
		return effects.Result{}, ErrNoGitHubClient
	}
	strict, ok := boundary.(integrationStrictExecutor)
	if !ok || guard == nil {
		return effects.Result{}, fmt.Errorf("preparation requires strict durable journaling and a live authority/admission guard")
	}
	if len(requestJSON) == 0 || len(requestJSON) > 32<<10 || !integrationProducerDigest(requestDigest) ||
		!integrationProducerDigest(executorID) || !integrationOID(authorityCommit) ||
		request.Schema != 1 || request.Protocol != "integration-prefix-v1" || request.Engine != "hive" ||
		!integrationPromotionRepo(request.Repo) || !integrationPromotionBranch(request.Base) || !integrationPromotionOID(request.BaseSHA) ||
		!integrationProducerDigest(request.Policy) || request.Generation < 1 ||
		len(request.Members) == 0 || len(request.Members) > integrationCandidateMaxMembers {
		return effects.Result{}, fmt.Errorf("exact preparation request/authority binding incomplete")
	}
	created, err := time.Parse(time.RFC3339Nano, request.CreatedAt)
	if err != nil || created.IsZero() {
		return effects.Result{}, fmt.Errorf("preparation request timestamp invalid")
	}
	members, heads := map[int]bool{}, map[string]bool{}
	for _, member := range request.Members {
		if member.PR < 1 || !integrationOID(member.Head) || members[member.PR] || heads[member.Head] {
			return effects.Result{}, fmt.Errorf("preparation ordered member invalid/duplicated")
		}
		members[member.PR], heads[member.Head] = true, true
	}
	canonical, digest, err := EncodeIntegrationPreparation(request)
	if err != nil || canonical != requestJSON || digest != requestDigest {
		return effects.Result{}, fmt.Errorf("preparation raw request/digest differs from the exact typed request")
	}
	claim := effects.Claim{
		Repo: request.Repo, Kind: integrationPreparationEffectKind, Target: request.Base + "/" + requestDigest, Actor: executorID,
		Inputs: map[string]string{"protocol": request.Protocol, "request_digest": requestDigest,
			"base_sha": request.BaseSHA, "policy": request.Policy, "authority_commit": authorityCommit},
	}
	return strict.ExecuteJournaled(ctx, claim, func(ctx context.Context) (effects.Result, error) {
		if err := guard(ctx); err != nil {
			return effects.Result{}, effects.NotApplied(err)
		}
		if c.client.BaseURL.Scheme != "https" || c.client.BaseURL.Host != "api.github.com" {
			return effects.Result{}, effects.NotApplied(fmt.Errorf("installed preparation transport supports public GitHub only"))
		}
		body := map[string]any{"event_type": "hive.integration.prepare.v1",
			"client_payload": map[string]string{"request": requestJSON, "request_key": requestDigest}}
		rpc, err := c.client.NewRequest(http.MethodPost, "repos/"+request.Repo+"/dispatches", body)
		if err != nil {
			return effects.Result{}, effects.NotApplied(err)
		}
		response, err := c.client.Do(ctx, rpc, nil)
		if err != nil {
			if response != nil {
				switch response.StatusCode {
				case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
					return effects.Result{}, effects.NotApplied(err)
				}
			}
			return effects.Result{}, err
		}
		if response == nil || response.StatusCode != http.StatusNoContent {
			return effects.Result{}, fmt.Errorf("preparation RPC outcome ambiguous; no TTL steal/blind retry")
		}
		return effects.Result{Provenance: "repository-dispatch-accepted/" + requestDigest}, nil
	})
}
