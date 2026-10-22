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
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{"reviewer": {Backend: "copilot", ReviewModels: ReviewModelsConfig{Pool: []ReviewModelPoolEntry{{Backend: "definitely-not-real", Model: "m"}}}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an unknown review_models backend")
	}
	cfg.Agents["reviewer"] = AgentConfig{Backend: "copilot", ReviewModels: ReviewModelsConfig{Fallback: "bogus"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an invalid review_models fallback")
	}
}

func TestReviewModelsSelect(t *testing.T) {
	pool := []ReviewModelPoolEntry{
		{Backend: "copilot", Model: "claude-fable-5.1"},
		{Backend: "copilot", Model: "claude-opus-4-6"},
		{Backend: "copilot", Model: " gpt-6-astra "},
	}
	t.Run("exclude author model only", func(t *testing.T) {
		got := (ReviewModelsConfig{Pool: pool}).Select("claude-fable-5.1", "copilot", "claude-fable-5.1")
		if got.Model != "claude-opus-4-6" || got.Fallback || got.Skip || got.RequiresHuman {
			t.Fatalf("selection = %+v", got)
		}
	})
	t.Run("exclude author family trims and picks cross family", func(t *testing.T) {
		got := (ReviewModelsConfig{Pool: pool, ExcludeAuthorFamily: true}).Select("Claude-Fable-5.1", "copilot", "claude-fable-5.1")
		if got.Backend != "copilot" || got.Model != "gpt-6-astra" || got.Fallback {
			t.Fatalf("selection = %+v", got)
		}
	})
	t.Run("exclude author model disabled allows same model", func(t *testing.T) {
		off := false
		got := (ReviewModelsConfig{Pool: pool, ExcludeAuthorModel: &off}).Select("claude-fable-5.1", "copilot", "x")
		if got.Model != "claude-fable-5.1" {
			t.Fatalf("selection = %+v", got)
		}
	})
	t.Run("pinned fallback", func(t *testing.T) {
		got := (ReviewModelsConfig{Pool: pool[:1]}).Select("claude-fable-5.1", "copilot", "claude-fable-5.1")
		if !got.Fallback || got.Skip || got.RequiresHuman || got.Model != "claude-fable-5.1" || got.Backend != "copilot" {
			t.Fatalf("selection = %+v", got)
		}
	})
	t.Run("skip fallback", func(t *testing.T) {
		got := (ReviewModelsConfig{Pool: pool[:1], Fallback: ReviewModelsFallbackSkip}).Select("claude-fable-5.1", "copilot", "pinned")
		if !got.Fallback || !got.Skip || got.Model != "" {
			t.Fatalf("selection = %+v", got)
		}
	})
	t.Run("requires_human fallback keeps pinned model", func(t *testing.T) {
		got := (ReviewModelsConfig{Pool: pool[:1], Fallback: ReviewModelsFallbackRequiresHuman}).Select("claude-fable-5.1", "copilot", "pinned")
		if !got.Fallback || !got.RequiresHuman || got.Skip || got.Model != "pinned" {
			t.Fatalf("selection = %+v", got)
		}
	})
	t.Run("WithPool keeps exclusions and fallback", func(t *testing.T) {
		base := ReviewModelsConfig{Pool: pool, ExcludeAuthorFamily: true, Fallback: ReviewModelsFallbackSkip}
		got := base.WithPool([]ReviewModelPoolEntry{{Backend: "copilot", Model: "claude-opus-4-6"}}).Select("claude-fable-5.1", "copilot", "pinned")
		if !got.Skip {
			t.Fatalf("override pool should still apply family exclusion + skip fallback: %+v", got)
		}
		if len(base.Pool) != 3 {
			t.Fatal("WithPool mutated the receiver")
		}
	})
}

func TestReviewPerspectiveModelsValidation(t *testing.T) {
	base := func(entries map[string]ReviewModelPoolEntry) *Config {
		return &Config{
			Project: ProjectConfig{Org: "o"},
			GitHub:  GitHubConfig{Token: "x"},
			AgentRuntimeConfig: AgentRuntimeConfig{
				Agents: map[string]AgentConfig{"reviewer": {Backend: "copilot"}},
			},
			Review: ReviewConfig{PerspectiveModels: entries},
		}
	}
	if err := base(map[string]ReviewModelPoolEntry{"security": {Backend: "copilot", Model: "gpt-6-astra"}}).Validate(); err != nil {
		t.Fatalf("valid perspective_models rejected: %v", err)
	}
	for name, entries := range map[string]map[string]ReviewModelPoolEntry{
		"unknown backend": {"security": {Backend: "definitely-not-real", Model: "m"}},
		"missing backend": {"security": {Model: "m"}},
		"missing model":   {"security": {Backend: "copilot"}},
		"blank name":      {" ": {Backend: "copilot", Model: "m"}},
	} {
		if err := base(entries).Validate(); err == nil {
			t.Fatalf("%s: Validate accepted invalid perspective_models", name)
		}
	}
}
