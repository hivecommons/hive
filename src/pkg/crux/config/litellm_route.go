package config

import (
	"fmt"
	"strings"
)

// LocalLiteLLMProxyPort is the loopback port the bundled litellm proxy
// listens on when governor.litellm.local_proxy is enabled. Distinct from the
// Go inference translator's port (18444): agents always talk to the
// translator, which forwards to this local litellm instance.
const LocalLiteLLMProxyPort = 18445

// LocalLiteLLMProxyURL is the base URL of the bundled local litellm proxy.
func LocalLiteLLMProxyURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", LocalLiteLLMProxyPort)
}

// LiteLLMRouteSource says which configuration source supplied a resolved
// litellm inference endpoint, so diagnostics can name it to the operator.
type LiteLLMRouteSource int

const (
	// LiteLLMRouteNone means no source yields an endpoint.
	LiteLLMRouteNone LiteLLMRouteSource = iota
	// LiteLLMRouteGateway means the explicit `gateways:` entry named by the backend.
	LiteLLMRouteGateway
	// LiteLLMRouteLegacyEnv means the HIVE_LITELLM_ENDPOINT environment variable.
	LiteLLMRouteLegacyEnv
	// LiteLLMRouteLegacyYAML means the legacy governor.litellm.endpoint field.
	LiteLLMRouteLegacyYAML
)

// LiteLLMChatCompletionsURL is the exact URL the inference translator POSTs
// to for an endpoint: {endpoint}/v1/chat/completions, tolerating a base that
// already ends in /v1 and any trailing slash. The dashboard's Test Connection
// reports this URL so the operator sees the request agents will actually
// make, not a guess. pkg/proxy keeps its own copy (it cannot import
// pkg/inference without a test-only cycle); TestOpenAIChatCompletionsURLMatchesInference
// pins the two together.
func LiteLLMChatCompletionsURL(endpoint string) string {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}
