package agent

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSubAgentModelRows(t *testing.T) {
	got := subAgentRows([]string{
		"  ● General-purpose (model: claude-opus-5)",
		"  └ ✓ Explore (model: gpt-5.4)",
		"General-purpose (model: claude-opus-5)",
		"Policy says: General-purpose (model: claude-opus-5)",
		"General-purpose (model: )", "General-purpose (model: <script>)",
		"General-purpose (model: claude-opus-5) is an example",
	})
	want := []string{"General-purpose\x00claude-opus-5", "Explore\x00gpt-5.4", "General-purpose\x00claude-opus-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
}

func TestOlderModelGeneration(t *testing.T) {
	for _, tt := range []struct {
		child, parent string
		want          bool
	}{
		{"claude-opus-5", "claude-opus-5.5", true},
		{"claude-haiku-4.5", "claude-sonnet-5.5", true},
		{"claude-sonnet-5.5", "claude-opus-5.5", false},
		{"claude-opus-5.5", "claude-opus-5", false},
		{"claude-opus-5-5", "claude-opus-5.5", false},
		{"claude-opus-5.9", "claude-opus-5.10", true},
		{"opus", "claude-opus-5.5", false},
		{"claude-opus-5", "auto", false},
		{"gpt-5.4", "claude-opus-5.5", false},
		{"claude-opus-5-preview", "claude-opus-5.5", false},
	} {
		if got := olderModelGeneration(tt.child, tt.parent); got != tt.want {
			t.Errorf("older(%q, %q) = %v, want %v", tt.child, tt.parent, got, tt.want)
		}
	}
}

func TestObserveSubAgentModelsKickLifecycle(t *testing.T) {
	kick := time.Now()
	old := "● Explore (model: claude-haiku-4.5)"
	row := "● General-purpose (model: claude-opus-5)"
	a := &AgentProcess{
		Config:        config.AgentConfig{Backend: "copilot", Model: "claude-opus-5"},
		ModelOverride: "claude-opus-5.5", LastKick: &kick,
		lastPaneCapture: []string{old},
	}
	a.observeSubAgentModelsLocked([]string{old, row, row})
	if len(a.SubAgentModels) != 2 || !a.SubAgentModels[0].OlderGeneration {
		t.Fatalf("observations = %+v; want two new dispatches flagged against override", a.SubAgentModels)
	}
	// Completion repaint and an identical poll must not duplicate dispatches.
	a.observeSubAgentModelsLocked([]string{old, "✓ General-purpose (model: claude-opus-5)", row})
	a.observeSubAgentModelsLocked(nil)
	a.observeSubAgentModelsLocked([]string{old, row, row})
	if len(a.SubAgentModels) != 2 {
		t.Fatalf("repaint duplicated rows: %+v", a.SubAgentModels)
	}
	snapshot := a.snapshot()
	snapshot.SubAgentModels[0].Model = "changed"
	if a.SubAgentModels[0].Model == "changed" {
		t.Fatal("snapshot aliases observations")
	}
	// Advancing LastKick immediately hides stale models, even before next poll.
	next := kick.Add(time.Minute)
	a.LastKick = &next
	if got := a.snapshot().SubAgentModels; len(got) != 0 {
		t.Fatalf("stale snapshot: %+v", got)
	}
	a.lastPaneCapture = []string{old, row, row}
	a.observeSubAgentModelsLocked([]string{old, row, row})
	if len(a.SubAgentModels) != 0 {
		t.Fatalf("old scrollback attributed to new kick: %+v", a.SubAgentModels)
	}
	a.observeSubAgentModelsLocked([]string{old, row, row, "Explore (model: claude-sonnet-5.5)"})
	if len(a.SubAgentModels) != 1 || a.SubAgentModels[0].OlderGeneration {
		t.Fatalf("new kick = %+v", a.SubAgentModels)
	}
	a.BackendOverride = "claude"
	if got := a.snapshot().SubAgentModels; len(got) != 0 {
		t.Fatalf("non-Copilot snapshot: %+v", got)
	}
	a.observeSubAgentModelsLocked([]string{row})
	if len(a.SubAgentModels) != 0 {
		t.Fatal("non-Copilot source must be absent")
	}
}

func TestSubAgentModelsAbsentOrBounded(t *testing.T) {
	a := &AgentProcess{Config: config.AgentConfig{Backend: "copilot"}}
	row := "General-purpose (model: claude-opus-5)"
	a.observeSubAgentModelsLocked([]string{row})
	if len(a.SubAgentModels) != 0 {
		t.Fatal("observation without a kick")
	}
	kick := time.Now()
	a.LastKick = &kick
	a.observeSubAgentModelsLocked([]string{"unknown CLI layout"})
	if len(a.SubAgentModels) != 0 {
		t.Fatal("guessed a model from unknown layout")
	}
	var rows []string
	for i := 0; i < 300; i++ {
		rows = append(rows, row)
	}
	a.observeSubAgentModelsLocked(rows)
	if len(a.SubAgentModels) != 256 {
		t.Fatalf("retained %d rows", len(a.SubAgentModels))
	}
}
