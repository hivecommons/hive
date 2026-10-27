package proxy

import (
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/inference"
)

// The dashboard's Test Connection prints inference.ChatCompletionsURL as "the
// URL agents will POST to" (hivecommons/hive#9945). That claim is only true
// while the translator's own URL builder agrees with it for every endpoint
// shape, so pin them together here.
func TestOpenAIChatCompletionsURLMatchesInference(t *testing.T) {
	for _, ep := range []string{
		"https://gw.example",
		"https://gw.example/",
		"https://gw.example/v1",
		"https://gw.example/v1/",
		"http://litellm.hive.svc.cluster.local:4000",
	} {
		if got, want := openAIChatCompletionsURL(ep), inference.ChatCompletionsURL(ep); got != want {
			t.Errorf("openAIChatCompletionsURL(%q) = %q, inference.ChatCompletionsURL = %q", ep, got, want)
		}
	}
}

func TestOpenAIChatCompletionsURLToleratesV1Endpoint(t *testing.T) {
	if got, want := openAIChatCompletionsURL("https://api.openai.com/v1"), "https://api.openai.com/v1/chat/completions"; got != want {
		t.Errorf("openAIChatCompletionsURL = %q, want %q", got, want)
	}
}

// The forward paths in github_proxy.go must build the upstream URL through
// openAIChatCompletionsURL; a hand-built "/v1/chat/completions" appended to an
// endpoint ending in /v1 yields /v1/v1 and a 404 (hivecommons/hive#13219).
func TestGitHubProxyDoesNotHandBuildChatCompletionsURL(t *testing.T) {
	src, err := os.ReadFile("github_proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `+ "/v1/chat/completions"`) {
		t.Error(`github_proxy.go hand-builds a "/v1/chat/completions" URL; use openAIChatCompletionsURL`)
	}
}
