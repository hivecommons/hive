package panes

import "testing"

// TestModelPickerBackendEchoesConstruction pins the Backend accessor: the app
// routes the eventual set-model request by this value, so the overlay must
// return exactly the backend it was opened for, not a normalized or defaulted
// one.
func TestModelPickerBackendEchoesConstruction(t *testing.T) {
	p := NewModelPicker("agent-1", "Agent One", "copilot", "gpt-5")
	if got := p.Backend(); got != "copilot" {
		t.Errorf("Backend() = %q, want %q", got, "copilot")
	}
	if got := p.Agent(); got != "agent-1" {
		t.Errorf("Agent() = %q, want %q", got, "agent-1")
	}
}
