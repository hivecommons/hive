package watsonx

import (
	"strings"
	"testing"
)

// The endpoint template moved here from pkg/dashboard so the AGENT LAUNCH path
// resolves the same URL the Model Gateways UI preset does. Two copies is how
// the gateway half and the agent half of watsonx support were free to drift.

func TestEndpointForRegion(t *testing.T) {
	for _, tc := range []struct{ region, want string }{
		{"us-south", "https://us-south.ml.cloud.ibm.com/ml/gateway"},
		{"eu-de", "https://eu-de.ml.cloud.ibm.com/ml/gateway"},
		{"jp-tok", "https://jp-tok.ml.cloud.ibm.com/ml/gateway"},
		// Blank (and whitespace-only) falls back to the default region so the
		// template always yields a concrete, usable URL.
		{"", "https://us-south.ml.cloud.ibm.com/ml/gateway"},
		{"   ", "https://us-south.ml.cloud.ibm.com/ml/gateway"},
	} {
		if got := EndpointForRegion(tc.region); got != tc.want {
			t.Errorf("EndpointForRegion(%q) = %q, want %q", tc.region, got, tc.want)
		}
	}
}

// hive appends /v1/... to the base, so the base must NOT already carry a
// version segment or a trailing slash.
func TestEndpointForRegion_ShapeIsAOpenAIBase(t *testing.T) {
	ep := EndpointForRegion("us-south")
	if strings.HasSuffix(ep, "/") {
		t.Errorf("endpoint %q must not end in a slash", ep)
	}
	if strings.Contains(ep, "/v1") {
		t.Errorf("endpoint %q must not include /v1 — hive appends it", ep)
	}
	if !strings.HasPrefix(ep, "https://") {
		t.Errorf("endpoint %q must be https", ep)
	}
}

func TestDefaultRegionIsSubstitutedIntoTemplate(t *testing.T) {
	if !strings.Contains(EndpointForRegion(DefaultRegion), DefaultRegion) {
		t.Errorf("EndpointForRegion(DefaultRegion) should contain %q", DefaultRegion)
	}
}
