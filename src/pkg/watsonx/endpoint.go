package watsonx

import (
	"fmt"
	"strings"
)

const (
	// BaseURLTemplate is the watsonx model-gateway base URL with a %s
	// placeholder for the region slug (us-south, eu-de, jp-tok, …). hive
	// appends /v1/models and /v1/chat/completions to reach watsonx's
	// OpenAI-compatible surface (.../ml/gateway/v1/...).
	//
	// This lives in pkg/watsonx (a leaf package) rather than in the dashboard
	// so that BOTH surfaces that need it — the Model Gateways UI preset and the
	// AGENT LAUNCH path, which must resolve an endpoint for an agent whose
	// backend is "watsonx" — derive it from one definition. Duplicating the
	// template was how the gateway half and the agent half were free to drift.
	BaseURLTemplate = "https://%s.ml.cloud.ibm.com/ml/gateway"

	// DefaultRegion is the region used when none is configured, so the endpoint
	// template always resolves to a concrete URL out of the box.
	DefaultRegion = "us-south"
)

// EndpointForRegion builds the watsonx model-gateway base URL for a region
// slug, falling back to DefaultRegion when blank.
func EndpointForRegion(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		region = DefaultRegion
	}
	return fmt.Sprintf(BaseURLTemplate, region)
}
