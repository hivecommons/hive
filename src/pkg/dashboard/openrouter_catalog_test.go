package dashboard

import "testing"

// OpenRouter's base URL already ends in /api/v1. The generic probe appended
// "/v1/models" and asked for /api/v1/v1/models (404), so OpenRouter gateways
// never discovered their catalog.
func TestOpenAIModelsURLHandlesV1Suffix(t *testing.T) {
	for in, want := range map[string]string{
		"https://openrouter.ai/api/v1":                 "https://openrouter.ai/api/v1/models",
		"https://openrouter.ai/api/v1/":                "https://openrouter.ai/api/v1/models",
		"http://vllm:8000":                             "http://vllm:8000/v1/models",
		"http://vllm:8000/":                            "http://vllm:8000/v1/models",
		"https://us-south.ml.cloud.ibm.com/ml/gateway": "https://us-south.ml.cloud.ibm.com/ml/gateway/v1/models",
	} {
		if got := openAIModelsURL(in); got != want {
			t.Errorf("openAIModelsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAvailableOpenRouterSuggestedHidesRetired(t *testing.T) {
	sug := []OpenRouterSuggestedModel{{ID: "a/one", Label: "One"}, {ID: "b/two", Label: "Two"}}
	got := availableOpenRouterSuggested(sug, []string{"b/two", "c/three"})
	if len(got) != 1 || got[0].ID != "b/two" {
		t.Fatalf("got %+v, want only b/two", got)
	}
	if all := availableOpenRouterSuggested(sug, nil); len(all) != len(sug) {
		t.Fatalf("unreadable catalog must keep every suggestion, got %+v", all)
	}
	if none := availableOpenRouterSuggested(sug, []string{"z/z"}); none == nil || len(none) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", none)
	}
	if d := availableOpenRouterDefault("a/one", sug, []string{"b/two"}); d != "b/two" {
		t.Errorf("retired default must fall to first live suggestion, got %q", d)
	}
	if d := availableOpenRouterDefault("a/one", sug, []string{"a/one"}); d != "a/one" {
		t.Errorf("live default kept, got %q", d)
	}
	if d := availableOpenRouterDefault("a/one", sug, nil); d != "a/one" {
		t.Errorf("unreadable catalog keeps default, got %q", d)
	}
	if d := availableOpenRouterDefault("a/one", sug, []string{"z/z"}); d != "a/one" {
		t.Errorf("no live suggestion keeps default, got %q", d)
	}
}

func TestIsOpenRouterGateway(t *testing.T) {
	if !isOpenRouterGateway("openrouter", "") {
		t.Error("kind openrouter")
	}
	if !isOpenRouterGateway("custom", "https://openrouter.ai/api/v1") {
		t.Error("custom kind pointed at openrouter.ai")
	}
	if isOpenRouterGateway("litellm", "https://litellm.example.com") {
		t.Error("litellm is not openrouter")
	}
}

func TestAttachOpenRouterModelInfoSkipsOtherKinds(t *testing.T) {
	s := &Server{}
	resp := map[string]interface{}{}
	s.attachOpenRouterModelInfo(resp, "litellm", "https://litellm.example.com", []string{"x"})
	if _, ok := resp["model_info"]; ok {
		t.Fatal("model_info attached for a non-OpenRouter gateway")
	}
	// No OpenRouter dependency wired: nothing attached, no panic.
	s.attachOpenRouterModelInfo(resp, "openrouter", "https://openrouter.ai/api/v1", []string{"x"})
	if _, ok := resp["model_info"]; ok {
		t.Fatal("model_info attached without an OpenRouter gateway")
	}
}
