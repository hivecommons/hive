package dashboard

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestActivityEntry_EffortJSONSerialization(t *testing.T) {
	entry := ActivityEntry{
		Username: "alice",
		Action:   "picked up",
		Role:     "contributor",
		CLI:      "codex",
		Model:    "gpt-5.6-terra",
		Effort:   "high",
		Task:     "task-123",
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal ActivityEntry failed: %v", err)
	}
	if !strings.Contains(string(raw), `"effort":"high"`) {
		t.Errorf("expected effort in JSON, got %s", string(raw))
	}

	var decoded ActivityEntry
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal ActivityEntry failed: %v", err)
	}
	if decoded.Effort != "high" || decoded.CLI != "codex" || decoded.Model != "gpt-5.6-terra" {
		t.Errorf("decoded = %+v, want effort=high, cli=codex, model=gpt-5.6-terra", decoded)
	}

	// Empty effort should be omitted from JSON
	entryNoEffort := ActivityEntry{
		Username: "bob",
		Action:   "joined",
		CLI:      "bob",
	}
	rawNoEffort, err := json.Marshal(entryNoEffort)
	if err != nil {
		t.Fatalf("marshal ActivityEntry without effort failed: %v", err)
	}
	if strings.Contains(string(rawNoEffort), `"effort"`) {
		t.Errorf("empty effort should be omitted, got %s", string(rawNoEffort))
	}
}

func TestContributeWSHub_AddActivity_EffortStored(t *testing.T) {
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         slog.Default(),
	}
	hub.addActivity("alice", "picked up", "contributor", "codex", "gpt-5.6-terra", "high", "task-1")

	acts := hub.RecentActivity()
	if len(acts) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(acts))
	}
	if acts[0].Effort != "high" || acts[0].CLI != "codex" || acts[0].Model != "gpt-5.6-terra" {
		t.Errorf("activity entry = %+v, want effort=high, cli=codex, model=gpt-5.6-terra", acts[0])
	}
}

func TestContributePage_LoadoutFormattingPresence(t *testing.T) {
	body := renderContributePage(t)

	// ccFormatLoadout function must exist and be exported on window
	if !strings.Contains(body, "function ccFormatLoadout(e,cls){") {
		t.Error("ccFormatLoadout helper is missing from /contribute page")
	}
	if !strings.Contains(body, "window.ccFormatLoadout=ccFormatLoadout;") {
		t.Error("ccFormatLoadout is not published on window")
	}

	// ccNarrate must format loadout and append to all event types
	if !strings.Contains(body, "var loadout=ccFormatLoadout(e,'ref');") {
		t.Error("ccNarrate does not compute loadout via ccFormatLoadout")
	}
	if !strings.Contains(body, "return {ic:ic,body:body+loadout,ts:e.timestamp};") {
		t.Error("ccNarrate does not append loadout to body")
	}

	// Onboarding activity feed must use ccFormatLoadout and escape username/task/role
	if !strings.Contains(body, "ccFormatLoadout)(e,'feed-cli')") {
		t.Error("onboarding activity feed does not use ccFormatLoadout")
	}
	if !strings.Contains(body, "<b>'+esc(e.username)+'</b>") {
		t.Error("onboarding activity feed does not escape username")
	}
}

func TestFleetClanker_ReasoningEffortJSONSerialization(t *testing.T) {
	clanker := FleetClanker{
		ContributorID:   "c1",
		CLIBackend:      "codex",
		Model:           "gpt-5.6-terra",
		ReasoningEffort: "high",
	}
	raw, err := json.Marshal(clanker)
	if err != nil {
		t.Fatalf("marshal FleetClanker failed: %v", err)
	}
	if !strings.Contains(string(raw), `"reasoning_effort":"high"`) {
		t.Errorf("expected reasoning_effort in FleetClanker JSON, got %s", string(raw))
	}
}
