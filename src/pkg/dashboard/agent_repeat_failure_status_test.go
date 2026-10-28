package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
)

// #14550 (C11, C12, C14): the agent row carries the repeat-failure check
// state and note, and nothing at all while the check is off.

func repeatFailureProc(status crux.RepeatFailureStatus) *crux.Process {
	return &crux.Process{
		Name:          "scanner",
		Config:        config.AgentConfig{Backend: "claude", Enabled: true},
		State:         crux.StateRunning,
		OutputBuffer:  cruxtest.NewRingBuffer(10),
		RepeatFailure: status,
	}
}

func TestBuildAgents_RepeatFailureOffShowsNothing(t *testing.T) {
	a := buildOneAgent(t, repeatFailureProc(crux.RepeatFailureStatus{}))
	if a.RepeatFailureCheck != "" || a.RepeatFailureCheckMessage != "" || a.RepeatFailureNote != "" {
		t.Fatalf("check off: row shows %q / %q / %q, want nothing", a.RepeatFailureCheck, a.RepeatFailureCheckMessage, a.RepeatFailureNote)
	}
}

func TestBuildAgents_RepeatFailureStates(t *testing.T) {
	for _, tc := range []struct {
		check crux.RepeatFailureCheckState
		msg   string
	}{
		{crux.RepeatFailureCheckOn, "repeat-failure check on"},
		{crux.RepeatFailureCheckNotCovered, "repeat-failure check not available for this backend"},
		{crux.RepeatFailureCheckNoData, "repeat-failure check not receiving data"},
	} {
		a := buildOneAgent(t, repeatFailureProc(crux.RepeatFailureStatus{Check: tc.check}))
		if a.RepeatFailureCheck != string(tc.check) || a.RepeatFailureCheckMessage != tc.msg {
			t.Errorf("check %q: row = %q / %q, want %q", tc.check, a.RepeatFailureCheck, a.RepeatFailureCheckMessage, tc.msg)
		}
	}
	note := "Repeating failure: `false` failed 5 times in 10 min (exit 1)"
	a := buildOneAgent(t, repeatFailureProc(crux.RepeatFailureStatus{Check: crux.RepeatFailureCheckOn, Note: note}))
	if a.RepeatFailureNote != note {
		t.Errorf("note = %q, want %q", a.RepeatFailureNote, note)
	}
	if a.Paused || a.StructuredStatus == "BLOCKED" {
		t.Error("a repeat-failure note must not pause or block the agent (monitor only)")
	}
}

// TestRepeatFailureLineWiredInDashboard: the card and both detail-panel paths
// render the line, and the line renders nothing without a state.
func TestRepeatFailureLineWiredInDashboard(t *testing.T) {
	html := indexHTML(t)
	body := jsFunc(t, html, "repeatFailureLineHtml")
	for _, want := range []string{
		"if (!a || (!a.repeatFailureCheck && !a.repeatFailureNote)) return '';",
		"a.repeatFailureCheckMessage",
		"a.repeatFailureNote",
		"'no_data'",
		"'not_covered'",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("repeatFailureLineHtml is missing %q", want)
		}
	}
	if !strings.Contains(jsFunc(t, html, "renderAgents"), "${repeatFailureLineHtml(a)}") {
		t.Error("renderAgents does not render the repeat-failure line on the card")
	}
	if n := strings.Count(jsFunc(t, html, "ocRenderAgentDetail"), "repeatFailureLineHtml(a)"); n != 2 {
		t.Errorf("ocRenderAgentDetail renders the repeat-failure line %d times, want 2 (full and quick-update paths)", n)
	}
}
