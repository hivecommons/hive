package dashboard

import "testing"

func TestParseOmpModelsJSONCapturesThinkingPerModel(t *testing.T) {
	raw := []byte(`{"models":[{"selector":"anthropic/claude-fable-5","reasoning":true,"thinking":["low","medium","high","xhigh","max"]},{"selector":"anthropic/claude-3-5-sonnet-20240620","reasoning":false,"thinking":null}]}`)
	models, efforts, err := parseOmpModelsJSON(raw)
	if err != nil {
		t.Fatalf("parseOmpModelsJSON error = %v", err)
	}
	if len(models) != 2 || models[0] != "anthropic/claude-fable-5" || models[1] != "anthropic/claude-3-5-sonnet-20240620" {
		t.Fatalf("models = %#v", models)
	}
	levels := efforts["anthropic/claude-fable-5"]
	if len(levels) != 5 || levels[4] != "max" {
		t.Fatalf("fable levels = %#v", levels)
	}
	if got := efforts["anthropic/claude-3-5-sonnet-20240620"]; len(got) != 0 {
		t.Fatalf("non-reasoning model got levels %#v", got)
	}
}

func TestValidateAgentReasoningEffort_OMPUsesSelectedModel(t *testing.T) {
	s := &Server{cliModels: newCLIModelCache()}
	s.cliModels.set(ompBackendID, cliModelResult{models: []string{"anthropic/claude-opus-4-1", "anthropic/claude-fable-5"}, reasoningEfforts: map[string][]string{
		"anthropic/claude-opus-4-1": {"minimal", "low", "medium", "high", "xhigh"},
		"anthropic/claude-fable-5":  {"low", "medium", "high", "xhigh", "max"},
	}})
	if err := s.validateAgentReasoningEffort(ompBackendID, "anthropic/claude-opus-4-1", "minimal"); err != nil {
		t.Fatalf("minimal should be valid for opus 4.1: %v", err)
	}
	if err := s.validateAgentReasoningEffort(ompBackendID, "anthropic/claude-fable-5", "minimal"); err == nil {
		t.Fatal("minimal should be rejected for fable when its discovered list starts at low")
	}
}
