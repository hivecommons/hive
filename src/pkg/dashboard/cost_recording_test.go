package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestCostRecording_UnrecordedConfiguredModelIsStale(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner":  {Enabled: true, Model: "claude-fable-5.1"},
		"reviewer": {Enabled: true, Model: "claude-sonnet-5.5"},
		"retired":  {Enabled: false, Model: "gpt-5.6-luna"},
		"nomodel":  {Enabled: true},
	}}
	est := costEstimated{
		ByModel:     []costModelEntry{{Name: "gpt-4o-mini", Input: 61_000_000, Output: 176_000, Source: "unpriced"}},
		WindowStart: time.Date(2026, 8, 26, 23, 11, 0, 0, time.UTC).UnixMilli(),
	}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	lastGrowth := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	hist := []CostHistoryEntry{
		{Timestamp: lastGrowth.Add(-time.Hour).UnixMilli(), USD: 100},
		{Timestamp: lastGrowth.UnixMilli(), USD: 110},
		{Timestamp: lastGrowth.Add(24 * time.Hour).UnixMilli(), USD: 110},
		{Timestamp: now.Add(-time.Hour).UnixMilli(), USD: 110},
	}
	rec := buildCostRecording(cfg, est, hist, now)
	if !rec.Stale {
		t.Fatalf("expected stale, got %+v", rec)
	}
	if got, want := rec.ConfiguredModels, []string{"claude-fable-5-1", "claude-sonnet-5-5"}; !recEqualStrings(got, want) {
		t.Errorf("configured = %v, want %v (disabled and model-less agents skipped)", got, want)
	}
	if !recEqualStrings(rec.UnrecordedModels, []string{"claude-fable-5-1", "claude-sonnet-5-5"}) {
		t.Errorf("unrecorded = %v", rec.UnrecordedModels)
	}
	if !recEqualStrings(rec.RecordedModels, []string{"gpt-4o-mini"}) {
		t.Errorf("recorded = %v", rec.RecordedModels)
	}
	if rec.LastPricedGrowth != lastGrowth.UnixMilli() || rec.StaleSince != lastGrowth.UnixMilli() {
		t.Errorf("last growth / stale since = %d / %d, want %d", rec.LastPricedGrowth, rec.StaleSince, lastGrowth.UnixMilli())
	}
	if rec.Issue != costRecordingIssue || rec.Reason == "" {
		t.Errorf("issue/reason = %q / %q", rec.Issue, rec.Reason)
	}
}

func TestCostRecording_HealthyWhenConfiguredModelsRecordedAndGrowing(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner": {Enabled: true, Model: "claude-sonnet-4.6"},
	}}
	// Backend reports the dash spelling; punctuation must not count as unrecorded.
	est := costEstimated{ByModel: []costModelEntry{{Name: "claude-sonnet-4-6", Input: 10, Output: 5, Source: "estimated"}}}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	hist := []CostHistoryEntry{
		{Timestamp: now.Add(-3 * time.Hour).UnixMilli(), USD: 10},
		{Timestamp: now.Add(-2 * time.Hour).UnixMilli(), USD: 12},
		{Timestamp: now.Add(-time.Hour).UnixMilli(), USD: 12},
	}
	rec := buildCostRecording(cfg, est, hist, now)
	if rec.Stale {
		t.Fatalf("expected healthy, got %+v", rec)
	}
	if rec.StaleSince != 0 || rec.Issue != "" || rec.Reason != "" {
		t.Errorf("healthy block must not carry stale fields: %+v", rec)
	}
	if rec.LastPricedGrowth != now.Add(-2*time.Hour).UnixMilli() {
		t.Errorf("last priced growth = %d", rec.LastPricedGrowth)
	}
}

func TestCostRecording_FlatTotalBeyondThresholdIsStale(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"a": {Enabled: true, Model: "m"}}}
	est := costEstimated{ByModel: []costModelEntry{{Name: "m", Input: 1, Output: 1}}}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	growth := now.Add(-costRecordingStaleAfter - time.Hour)
	hist := []CostHistoryEntry{
		{Timestamp: growth.Add(-time.Hour).UnixMilli(), USD: 1},
		{Timestamp: growth.UnixMilli(), USD: 2},
		{Timestamp: now.Add(-time.Minute).UnixMilli(), USD: 2},
	}
	rec := buildCostRecording(cfg, est, hist, now)
	if !rec.Stale || rec.StaleSince != growth.UnixMilli() {
		t.Fatalf("expected stale since %d, got %+v", growth.UnixMilli(), rec)
	}
	// A lineage reset (drop) is not growth and does not refresh the clock.
	hist = append(hist, CostHistoryEntry{Timestamp: now.UnixMilli(), USD: 0.5})
	if rec2 := buildCostRecording(cfg, est, hist, now); !rec2.Stale || rec2.LastPricedGrowth != growth.UnixMilli() {
		t.Errorf("reset counted as growth: %+v", rec2)
	}
}

func TestCostRecording_NoConfigNoHistoryIsNotStale(t *testing.T) {
	rec := buildCostRecording(nil, costEstimated{}, nil, time.Now())
	if rec.Stale || len(rec.ConfiguredModels) != 0 || rec.RecordedModels == nil || rec.UnrecordedModels == nil {
		t.Fatalf("empty inputs must yield a healthy, non-nil-slice block: %+v", rec)
	}
}

func TestCostRecording_TokenGrowthTracked(t *testing.T) {
	t1, t2 := int64(1000), int64(2000)
	hist := []CostHistoryEntry{
		{Timestamp: t1, USD: 0, Models: map[string]CostModelSnap{"m": {Input: 10}}},
		{Timestamp: t2, USD: 0, Models: map[string]CostModelSnap{"m": {Input: 20}}},
	}
	priced, tokens := costHistoryGrowth(hist)
	if priced != 0 || tokens != t2 {
		t.Fatalf("growth = (%d,%d), want (0,%d)", priced, tokens, t2)
	}
}

func TestCov_HandleCost_CarriesRecordingBlock(t *testing.T) {
	s, _ := covServer(t)
	rec := doGet(s, "/api/cost")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var block costRecording
	if err := json.Unmarshal(raw["recording"], &block); err != nil {
		t.Fatalf("recording block missing or malformed: %v", err)
	}
	if block.RecordedModels == nil || block.UnrecordedModels == nil || block.ConfiguredModels == nil {
		t.Errorf("slices must serialise as [] not null: %s", raw["recording"])
	}
}

func recEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
