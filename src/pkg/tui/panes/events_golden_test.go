package panes_test

import (
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// TestEventsGolden pins the events pane's complete 48x12 render. The fixture
// deliberately has more events than the ten body rows that fit, so the golden
// also proves the initial viewport shows the newest events.
func TestEventsGolden(t *testing.T) {
	events := []client.Event{
		{Timestamp: "2026-08-29T12:04:02Z", User: "governor", Action: "kick", Agent: "scanner", Detail: "reason=governor"},
		{Timestamp: "2026-08-29T12:03:41Z", User: "Danathar", Action: "pr.open", Detail: "number=4919"},
		{Timestamp: "2026-08-29T12:01:58Z", User: "system", Action: "state.change", Agent: "quality", Detail: "running -> idle"},
		{Timestamp: "2026-08-29T11:58:20Z", User: "oidc:00u1opaque", UserName: "Jane Doe", Action: "pause", Agent: "reviewer"},
		{Timestamp: "2026-08-29T11:57:03Z", User: "system", Action: "bead.close", Detail: "id=bd-1042"},
		{Timestamp: "2026-08-29T11:55:44Z", User: "system", Action: "governor.mode", Detail: "quiet -> busy"},
		{Timestamp: "2026-08-29T11:52:30Z", User: "operator", Action: "set_model", Agent: "quality", Detail: "model=gpt-5"},
		{Timestamp: "2026-08-29T11:49:12Z", User: "system", Action: "agent.complete", Agent: "reviewer"},
		{Timestamp: "2026-08-29T11:45:01Z", User: "operator", Action: "resume", Agent: "ux-discovery"},
		{Timestamp: "2026-08-29T11:40:17Z", User: "system", Action: "queue.refresh", Detail: "actionable=7"},
		{Timestamp: "2026-08-29T11:38:54Z", User: "operator", Action: "config.save", Detail: "file=hive.yaml"},
		{Timestamp: "2026-08-29T11:32:09Z", User: "system", Action: "startup"},
	}

	pane, _ := panes.NewEvents().Update(panes.EventsMsg{Events: events})
	requireGolden(t, []byte(pane.View(48, 12)), filepath.Join("testdata", "events.golden"))
}
