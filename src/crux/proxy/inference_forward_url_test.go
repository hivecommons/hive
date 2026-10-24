package proxy

import (
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
