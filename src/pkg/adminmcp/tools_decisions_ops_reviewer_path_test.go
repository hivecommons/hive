package adminmcp

import (
	"strings"
	"testing"
)

// The decisions.delegate_settings preview names the path the decider turn
// takes, from the same llm_reviewer_path / llm_reviewer_reason fields the
// dashboard's Let hive decide settings return (settings_read section
// decisions_delegate is that endpoint). On the old code the preview always
// said "through the governor reviewer endpoint", even on a hive whose turn
// runs on the Claude CLI.
func TestDecisionsDelegateSettingsNamesReviewerPath(t *testing.T) {
	cli := delegateSettingsFixture()
	cli["llm_reviewer_path"] = "claude-cli"
	cli["llm_reviewer_reason"] = ""
	cli["llm_model_effective"] = ""
	preview, _ := delegateSettingsPreview(t, delegateSettingsHandler(&delegateSettingsProvider{settings: cli}), map[string]any{"llm": map[string]any{"enabled": true}})
	d := preview.WideningDisclosure
	if !strings.Contains(d, "through the Claude CLI") || !strings.Contains(d, "every tool off") || !strings.Contains(d, "the Claude CLI's default model") {
		t.Fatalf("disclosure must name the Claude CLI path and its default model: %s", d)
	}
	if strings.Contains(d, "governor reviewer endpoint") || strings.Contains(d, "cannot run yet") {
		t.Fatalf("a ready CLI path is neither the endpoint nor missing: %s", d)
	}

	notReady := delegateSettingsFixture()
	notReady["llm_reviewer_ready"] = false
	notReady["llm_reviewer_path"] = "none"
	notReady["llm_reviewer_reason"] = "Claude is not signed in on this hive"
	preview, _ = delegateSettingsPreview(t, delegateSettingsHandler(&delegateSettingsProvider{settings: notReady}), map[string]any{"llm": map[string]any{"enabled": true}})
	if d := preview.WideningDisclosure; !strings.Contains(d, "cannot run yet (Claude is not signed in on this hive)") {
		t.Fatalf("disclosure must carry the dashboard's reason: %s", d)
	}
}
