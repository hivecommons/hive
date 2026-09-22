package config

import "testing"

func TestReviewModelsDefaultsAndValidation(t *testing.T) {
	if !((ReviewModelsConfig{}).ExcludeAuthorModelEnabled()) {
		t.Fatal("exclude_author_model must default on")
	}
	if got := (ReviewModelsConfig{}).EffectiveFallback(); got != ReviewModelsFallbackPinned {
		t.Fatalf("fallback = %q, want pinned", got)
	}
	cfg := &Config{
		Project: ProjectConfig{Org: "o"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents:  map[string]AgentConfig{"reviewer": {Backend: "copilot", ReviewModels: ReviewModelsConfig{Pool: []ReviewModelPoolEntry{{Backend: "definitely-not-real", Model: "m"}}}}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an unknown review_models backend")
	}
	cfg.Agents["reviewer"] = AgentConfig{Backend: "copilot", ReviewModels: ReviewModelsConfig{Fallback: "bogus"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an invalid review_models fallback")
	}
}
