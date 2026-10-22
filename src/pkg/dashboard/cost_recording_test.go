package dashboard

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

func TestBuildCostRecording_StaleWhenAgentModelsStopGrowing(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	broke := now.Add(-22 * 24 * time.Hour)
	agents := []FrontendAgent{
		{Name: "scanner", State: "running", Model: "claude-sonnet-5.5"},
		{Name: "fixer", State: "running", Model: "auto"},
		{Name: "idle", State: "running", Model: "claude-opus-5.5", Paused: true},
		{Name: "down", State: "stopped", Model: "gpt-5"},
	}
	byAgentModel := map[string]string{"fixer": "claude-sonnet-4.6"}
	byModel := []costModelEntry{{Name: "gpt-4o-mini", Input: 61_000_000, Output: 176_000}}
	history := []CostHistoryEntry{
		{Timestamp: broke.Add(-time.Hour).UnixMilli(), Models: map[string]CostModelSnap{"claude-sonnet-4.6": {Input: 100}}},
		{Timestamp: broke.UnixMilli(), Models: map[string]CostModelSnap{"claude-sonnet-4.6": {Input: 500}, "gpt-4o-mini": {Input: 10}}},
		{Timestamp: now.Add(-time.Hour).UnixMilli(), Models: map[string]CostModelSnap{"claude-sonnet-4.6": {Input: 500}, "gpt-4o-mini": {Input: 900}}},
	}

	rec := buildCostRecording(agents, byAgentModel, byModel, history, now)

	if want := []string{"claude-sonnet-4.6", "claude-sonnet-5.5"}; !reflect.DeepEqual(rec.AgentModels, want) {
		t.Errorf("AgentModels = %v, want %v", rec.AgentModels, want)
	}
	if want := []string{"claude-sonnet-4.6", "claude-sonnet-5.5"}; !reflect.DeepEqual(rec.UnrecordedModels, want) {
		t.Errorf("UnrecordedModels = %v, want %v", rec.UnrecordedModels, want)
	}
	if rec.LastPricedGrowthAt != broke.UnixMilli() {
		t.Errorf("LastPricedGrowthAt = %d, want %d", rec.LastPricedGrowthAt, broke.UnixMilli())
	}
	if rec.LastAgentModelGrowthAt != broke.UnixMilli() {
		t.Errorf("LastAgentModelGrowthAt = %d, want %d", rec.LastAgentModelGrowthAt, broke.UnixMilli())
	}
	if want := []string{"gpt-4o-mini"}; !reflect.DeepEqual(rec.RecentModels, want) {
		t.Errorf("RecentModels = %v, want %v", rec.RecentModels, want)
	}
	if !rec.Stale {
		t.Error("Stale = false, want true when agent models recorded nothing for > 24h")
	}
	if rec.StaleAfterMs != costRecordingStaleAfter.Milliseconds() {
		t.Errorf("StaleAfterMs = %d", rec.StaleAfterMs)
	}
}

func TestBuildCostRecording_HealthyWhenAgentModelGrewRecently(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	agents := []FrontendAgent{{Name: "scanner", State: "running", Model: "anthropic/Claude-Sonnet-4.6"}}
	byModel := []costModelEntry{{Name: "claude-sonnet-4-6", Input: 900}}
	history := []CostHistoryEntry{
		{Timestamp: now.Add(-48 * time.Hour).UnixMilli(), Models: map[string]CostModelSnap{"claude-sonnet-4-6": {Input: 100}}},
		{Timestamp: now.Add(-time.Hour).UnixMilli(), Models: map[string]CostModelSnap{"claude-sonnet-4-6": {Input: 900}}},
	}
	rec := buildCostRecording(agents, nil, byModel, history, now)
	if rec.Stale {
		t.Error("Stale = true, want false when the agent model grew within 24h")
	}
	if len(rec.UnrecordedModels) != 0 {
		t.Errorf("UnrecordedModels = %v, want none (normalized match)", rec.UnrecordedModels)
	}
}

func TestBuildCostRecording_NoFalseAlarmOnShortHistoryOrNoAgents(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	agents := []FrontendAgent{{Name: "scanner", State: "running", Model: "claude-sonnet-4.6"}}
	short := []CostHistoryEntry{{Timestamp: now.Add(-time.Hour).UnixMilli(), Models: map[string]CostModelSnap{"gpt-4o-mini": {Input: 1}}}}
	if rec := buildCostRecording(agents, nil, nil, short, now); rec.Stale {
		t.Error("Stale = true on a history shorter than the stale window")
	}
	long := []CostHistoryEntry{{Timestamp: now.Add(-72 * time.Hour).UnixMilli()}}
	if rec := buildCostRecording(nil, nil, nil, long, now); rec.Stale || rec.AgentModels == nil || rec.UnrecordedModels == nil || rec.RecentModels == nil {
		t.Errorf("no running agents: got %+v, want non-stale with empty (non-nil) lists", rec)
	}
}

func TestHandleCost_IncludesRecordingBlock(t *testing.T) {
	s, _ := covServer(t)
	rec := doGet(s, "/api/cost")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var block costRecording
	if err := json.Unmarshal(raw["recording"], &block); err != nil {
		t.Fatalf("recording block missing or malformed: %v (%s)", err, raw["recording"])
	}
	if block.StaleAfterMs != costRecordingStaleAfter.Milliseconds() || block.AgentModels == nil {
		t.Errorf("recording = %+v, want stale_after_ms set and agent_models present", block)
	}
}

func TestCostRecordingNoticeText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: cost recording notice text was not executed")
	}
	fn, ok := jsFuncOptional(indexHTML(t), "costRecordingNoticeText")
	if !ok {
		t.Fatal("index.html does not define costRecordingNoticeText")
	}
	script := "const assert = require('node:assert/strict');\n" +
		"function fmtCostDateTime(ms) { return 'T' + ms; }\n" + fn + `
assert.equal(costRecordingNoticeText(null), '');
assert.equal(costRecordingNoticeText({stale: false, agent_models: ['claude-sonnet-5.5']}), '');
assert.equal(
  costRecordingNoticeText({stale: true, agent_models: ['claude-fable-5.1', 'claude-sonnet-5.5'], recent_models: ['gpt-4o-mini'], last_agent_model_growth_at: 5}),
  "⚠ No usage recorded from the agents' configured models (claude-fable-5.1, claude-sonnet-5.5) since T5. The figures below only reflect gpt-4o-mini. Live cost recording is likely broken — see");
assert.ok(costRecordingNoticeText({stale: true, agent_models: ['a','b','c','d','e']}).includes('(a, b, c, d, …) in the recorded history.'));
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("cost recording notice JS failed: %v\n%s", err, out)
	}
}

func TestCostRecordingModelKeyMatchesTokensNormalizer(t *testing.T) {
	for _, in := range []string{
		"anthropic/claude-sonnet-5.5",
		"bedrock/us.anthropic.claude-sonnet-4-6-20250514-v1:0",
		"Claude Sonnet__5.5:free",
	} {
		if got, want := costRecordingModelKey(in), tokens.NormalizeModelID(in); got != want {
			t.Errorf("costRecordingModelKey(%q) = %q, want %q", in, got, want)
		}
	}
	if a, b := costRecordingModelKey("anthropic/claude-sonnet-5.5"), costRecordingModelKey("claude-sonnet-5-5"); a != b {
		t.Errorf("prefixed %q != bare %q", a, b)
	}
}
