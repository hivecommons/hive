package dashboard

import "testing"

func TestOmpProbeRunsUnderGivenHome(t *testing.T) {
	t.Setenv("HOME", "/data/home/agents/x")
	var gotHome string
	orig := runOmpModelsProbe
	t.Cleanup(func() { runOmpModelsProbe = orig })
	runOmpModelsProbe = func(home string) ([]string, map[string][]string, error) {
		gotHome = home
		return []string{"anthropic/claude-opus-5"}, map[string][]string{"anthropic/claude-opus-5": {"low"}}, nil
	}
	r := newTestServer().discoverOmpModels()
	if gotHome != "/data/home/agents/x" {
		t.Fatalf("probe HOME = %q, want the agent home", gotHome)
	}
	if r.fallback || len(r.models) != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestOmpStaticEffortsMatchCatalog(t *testing.T) {
	for _, id := range []string{"openai-codex/gpt-5.6-terra", "openai-codex/gpt-5.6-sol", "openai-codex/gpt-5.6-luna"} {
		if got := ompStaticReasoningEfforts[id]; !equalStrings(got, []string{"low", "medium", "high", "xhigh", "max"}) {
			t.Errorf("%s efforts = %v", id, got)
		}
	}
	if got := ompStaticReasoningEfforts["google-antigravity/gemini-3.7-flash"]; !equalStrings(got, []string{"minimal", "low", "medium", "high"}) {
		t.Errorf("gemini-3.7-flash efforts = %v", got)
	}
}
